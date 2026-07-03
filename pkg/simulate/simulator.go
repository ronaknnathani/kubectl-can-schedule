// Package simulate runs the default scheduler's filter plugins against a live
// cluster snapshot to determine how many replicas of one or more workloads can
// be scheduled. Placement is greedy and cumulative: replicas are placed
// first-fit (filters only) in node-name order, capacity is decremented as each
// replica lands, and later workloads compete for the remaining capacity.
package simulate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	resourcehelper "k8s.io/component-helpers/resource"
	helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/input"
	"github.com/ronaknnathani/kubectl-can-schedule/pkg/scheduling"
	"github.com/ronaknnathani/kubectl-can-schedule/pkg/snapshot"
)

// Options controls simulation behavior.
type Options struct {
	// ConsiderPreemption enables a preemption pass when a replica fails all
	// nodes on filters. It is a no-op for pods at or below the default priority.
	ConsiderPreemption bool
}

// ResourceFit classifies how a single requested resource fares against the
// cluster's capacity.
type ResourceFit int

const (
	// ResourceOK means the cluster has enough unallocated capacity of this resource.
	ResourceOK ResourceFit = iota
	// ResourceInsufficient means some node advertises this resource but the
	// cluster-wide unallocated capacity is less than requested.
	ResourceInsufficient
	// ResourceAbsent means no node advertises this resource at all.
	ResourceAbsent
)

// ResourceStatus is the per-resource view of demand vs cluster capacity.
type ResourceStatus struct {
	Name        corev1.ResourceName
	Allocatable resource.Quantity // total across all nodes
	Allocated   resource.Quantity // requested by pods already running in the cluster
	Requested   resource.Quantity // this workload's total (per-pod x replicas)
	Fit         ResourceFit
}

// WorkloadResult is the outcome for a single input object.
type WorkloadResult struct {
	Label          string           // e.g. "Deployment/web" or "workload"
	Replicas       int              // requested
	ReplicasFit    int              // placed by the simulation
	FeasibleNodes  int              // nodes a single replica passes all filters on
	Resources      []ResourceStatus // one per resource the workload requests
	UsedPreemption bool
	// FilterReasons maps non-resource filter plugins that rejected nodes to an
	// example message (resource shortfalls are conveyed by Resources instead).
	FilterReasons map[string]string
}

// Schedulable reports whether every requested replica of this workload fit.
func (w WorkloadResult) Schedulable() bool { return w.ReplicasFit == w.Replicas }

// Result is the outcome of a schedulability check across all input workloads.
type Result struct {
	NodeCount   int
	Workloads   []WorkloadResult
	Schedulable bool // every workload fully fits
}

// Simulator holds the constructed framework and the mutable cluster view.
type Simulator struct {
	ctx       context.Context
	framework framework.Framework
	lister    *snapshot.Swappable
	opts      Options

	nodes         []*corev1.Node
	nodeOrder     []string
	scheduledPods []*corev1.Pod
	pvcStore      cache.Store

	// defaultStorageClass is the cluster's default StorageClass name (empty if
	// none). Synthesized PVCs that omit a storageClassName inherit it, mirroring
	// the PVC admission plugin, so StatefulSet volume claims schedule realistically.
	defaultStorageClass string

	// simulated holds pods this run has placed (as opposed to pods already
	// running in the cluster). Preemption must never evict these, otherwise a
	// later workload could cannibalize an earlier one already counted as fit.
	simulated map[*corev1.Pod]bool

	priorityClasses map[string]int32
	defaultPriority int32
}

// New builds a Simulator: it constructs the default-profile framework, lists the
// live nodes and scheduled pods, starts the informer factory, and waits for its
// caches to sync.
func New(ctx context.Context, client clientset.Interface, opts Options) (*Simulator, error) {
	factory := informers.NewSharedInformerFactory(client, 0)
	lister := snapshot.NewSwappable()

	f, err := scheduling.BuildDefaultFramework(ctx, client, lister, factory)
	if err != nil {
		return nil, err
	}

	nodes, err := listNodes(ctx, client)
	if err != nil {
		return nil, err
	}
	pods, err := listScheduledPods(ctx, client)
	if err != nil {
		return nil, err
	}
	lister.Set(nodes, pods)

	// Ensure the PVC informer is instantiated, then start and sync all informers
	// the framework registered (PV, PVC, StorageClass, CSINode, DRA, ...).
	pvcInformer := factory.Core().V1().PersistentVolumeClaims().Informer()
	factory.Start(ctx.Done())
	for informerType, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return nil, fmt.Errorf("informer cache failed to sync: %s", informerType)
		}
	}

	order := make([]string, 0, len(nodes))
	for _, n := range nodes {
		order = append(order, n.Name)
	}
	sort.Strings(order)

	priorityClasses, defaultPriority, err := listPriorityClasses(ctx, client)
	if err != nil {
		return nil, err
	}

	defaultStorageClass, err := defaultStorageClassName(ctx, client)
	if err != nil {
		return nil, err
	}

	return &Simulator{
		ctx:                 ctx,
		framework:           f,
		lister:              lister,
		opts:                opts,
		nodes:               nodes,
		nodeOrder:           order,
		scheduledPods:       pods,
		pvcStore:            pvcInformer.GetStore(),
		defaultStorageClass: defaultStorageClass,
		simulated:           map[*corev1.Pod]bool{},
		priorityClasses:     priorityClasses,
		defaultPriority:     defaultPriority,
	}, nil
}

// Run evaluates each input workload against the cluster in order (cumulative:
// replicas placed for earlier workloads consume capacity seen by later ones) and
// returns a per-workload report of capacity, demand, feasibility, and reasons.
func (s *Simulator) Run(workloads []*input.Workload) (*Result, error) {
	result := &Result{NodeCount: len(s.nodes), Schedulable: true}
	allocated := s.allocatedByResource()

	for _, w := range workloads {
		wr, err := s.runWorkload(w, allocated)
		if err != nil {
			return nil, err
		}
		if !wr.Schedulable() {
			result.Schedulable = false
		}
		result.Workloads = append(result.Workloads, *wr)
	}
	return result, nil
}

func (s *Simulator) runWorkload(w *input.Workload, allocated corev1.ResourceList) (*WorkloadResult, error) {
	wr := &WorkloadResult{
		Label:         w.Label(),
		Replicas:      int(w.Replicas),
		FilterReasons: map[string]string{},
	}

	// Feasibility: how many nodes a single replica passes all filters on, plus
	// the non-resource filter reasons for the nodes it does not.
	probe, probePVCs := w.Replica(0)
	if err := s.injectPVCs(probePVCs); err != nil {
		return nil, err
	}
	feasible, filterReasons, err := s.feasibility(probe)
	if err != nil {
		return nil, err
	}
	wr.FeasibleNodes = feasible
	wr.FilterReasons = filterReasons

	// Per-resource demand vs cluster capacity.
	requested := s.requestedByWorkload(w)
	wr.Resources = s.resourceStatuses(requested, allocated)

	// Greedy placement of every replica.
	for i := 0; i < wr.Replicas; i++ {
		pod, pvcs := w.Replica(i)
		if err := s.injectPVCs(pvcs); err != nil {
			return nil, err
		}
		node, viaPreempt, victims, _, err := s.tryPlace(pod)
		if err != nil {
			return nil, err
		}
		if node == "" {
			// Identical template against an unchanged-or-tighter snapshot: if this
			// replica cannot be placed, no later replica of this workload can either.
			break
		}
		pod.Spec.NodeName = node
		if len(victims) > 0 {
			s.evict(victims)
		}
		s.scheduledPods = append(s.scheduledPods, pod)
		s.simulated[pod] = true
		s.refreshSnapshot()
		if viaPreempt {
			wr.UsedPreemption = true
		}
		wr.ReplicasFit++
	}
	return wr, nil
}

// feasibility runs PreFilter once then Filter on every node for a single pod,
// returning how many nodes pass and an example message per non-resource filter
// plugin that rejected nodes. It places nothing.
func (s *Simulator) feasibility(pod *corev1.Pod) (int, map[string]string, error) {
	if err := s.resolvePriority(pod); err != nil {
		return 0, nil, err
	}
	reasons := map[string]string{}
	state := framework.NewCycleState()
	preRes, preStatus, preRejectedBy := s.framework.RunPreFilterPlugins(s.ctx, state, pod)
	if !preStatus.IsSuccess() {
		reasons[preStatus.Plugin()] = sanitizeReason(preStatus.Message())
		return 0, reasons, nil
	}

	feasible := 0
	for _, name := range s.nodeOrder {
		if !preRes.AllNodes() && !preRes.NodeNames.Has(name) {
			continue // narrowed out by a PreFilter plugin (e.g. node affinity)
		}
		nodeInfo, err := s.lister.NodeInfos().Get(name)
		if err != nil {
			continue
		}
		status := s.framework.RunFilterPlugins(s.ctx, state, pod, nodeInfo)
		if status.IsSuccess() {
			feasible++
			continue
		}
		if plugin := status.Plugin(); plugin != names.NodeResourcesFit {
			reasons[plugin] = sanitizeReason(status.Message())
		}
	}
	// A PreFilter plugin that narrowed the candidate set to nothing (e.g. a
	// nodeSelector matching no node) leaves no per-node reason, so surface it.
	if feasible == 0 && len(reasons) == 0 {
		for plugin := range preRejectedBy {
			reasons[plugin] = "no node satisfied this requirement"
		}
	}
	return feasible, reasons, nil
}

// requestedByWorkload returns the total resources requested across all replicas
// (per-pod requests, using the scheduler's accounting, scaled by replica count).
func (s *Simulator) requestedByWorkload(w *input.Workload) corev1.ResourceList {
	pod, _ := w.Replica(0)
	total := corev1.ResourceList{}
	for name, q := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
		scaled := q.DeepCopy()
		scaled.Mul(int64(w.Replicas))
		total[name] = scaled
	}
	return total
}

// resourceStatuses classifies each requested resource against cluster capacity.
func (s *Simulator) resourceStatuses(requested, allocated corev1.ResourceList) []ResourceStatus {
	var statuses []ResourceStatus
	for name, req := range requested {
		allocatable := s.allocatableOf(name)
		alloc := allocated[name]
		statuses = append(statuses, ResourceStatus{
			Name:        name,
			Allocatable: allocatable,
			Allocated:   alloc,
			Requested:   req,
			Fit:         classifyResource(allocatable, alloc, req),
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func classifyResource(allocatable, allocated, requested resource.Quantity) ResourceFit {
	if allocatable.IsZero() {
		return ResourceAbsent
	}
	free := allocatable.DeepCopy()
	free.Sub(allocated)
	if free.Cmp(requested) < 0 {
		return ResourceInsufficient
	}
	return ResourceOK
}

// allocatableOf sums a single resource's allocatable capacity across all nodes.
func (s *Simulator) allocatableOf(name corev1.ResourceName) resource.Quantity {
	total := resource.Quantity{}
	for _, node := range s.nodes {
		if q, ok := node.Status.Allocatable[name]; ok {
			total.Add(q)
		}
	}
	return total
}

// allocatedByResource sums the requests of all pods already running in the
// cluster (the real, pre-existing pods), giving current cluster utilization.
func (s *Simulator) allocatedByResource() corev1.ResourceList {
	total := corev1.ResourceList{}
	for _, pod := range s.scheduledPods {
		for name, q := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
			if existing, ok := total[name]; ok {
				existing.Add(q)
				total[name] = existing
			} else {
				total[name] = q.DeepCopy()
			}
		}
	}
	return total
}

// sanitizeReason removes semicolons from scheduler messages so output never
// contains them.
func sanitizeReason(msg string) string {
	return strings.ReplaceAll(msg, ";", ",")
}

// tryPlace runs PreFilter once then Filter on each node in name order, returning
// the first node that passes all filter plugins. If none fit and preemption is
// enabled (and meaningful for this pod), it attempts a preemption pass. An error
// is returned only for invalid input (e.g. an unknown PriorityClass).
func (s *Simulator) tryPlace(pod *corev1.Pod) (node string, viaPreemption bool, victims []*corev1.Pod, reasons map[string]string, err error) {
	if err := s.resolvePriority(pod); err != nil {
		return "", false, nil, nil, err
	}
	state := framework.NewCycleState()
	reasons = map[string]string{}

	preRes, preStatus, _ := s.framework.RunPreFilterPlugins(s.ctx, state, pod)
	if !preStatus.IsSuccess() {
		reasons[preStatus.Plugin()] = preStatus.Message()
		return "", false, nil, reasons, nil
	}

	nodeStatuses := framework.NewDefaultNodeToStatus()
	for _, name := range s.nodeOrder {
		if !preRes.AllNodes() && !preRes.NodeNames.Has(name) {
			continue
		}
		nodeInfo, err := s.lister.NodeInfos().Get(name)
		if err != nil {
			continue
		}
		status := s.framework.RunFilterPlugins(s.ctx, state, pod, nodeInfo)
		if status.IsSuccess() {
			return name, false, nil, nil, nil
		}
		nodeStatuses.Set(name, status)
		reasons[status.Plugin()] = status.Message()
	}

	if s.opts.ConsiderPreemption && s.preemptionMeaningful(pod) {
		if node, victims := s.tryPreemption(pod, nodeStatuses); node != "" {
			return node, true, victims, nil, nil
		}
	}
	return "", false, nil, reasons, nil
}

// evict removes the given pods from the scheduled set, simulating preemption.
func (s *Simulator) evict(victims []*corev1.Pod) {
	drop := make(map[*corev1.Pod]bool, len(victims))
	for _, v := range victims {
		drop[v] = true
	}
	kept := s.scheduledPods[:0]
	for _, p := range s.scheduledPods {
		if drop[p] {
			continue
		}
		kept = append(kept, p)
	}
	s.scheduledPods = kept
}

func (s *Simulator) refreshSnapshot() {
	s.lister.Set(s.nodes, s.scheduledPods)
}

// injectPVCs adds synthesized PVCs (e.g. from StatefulSet volumeClaimTemplates)
// to the PVC informer store so volume filter plugins can evaluate them. A PVC
// that omits a storageClassName inherits the cluster default, mirroring the PVC
// admission plugin so volume binding is evaluated as it would be in practice.
func (s *Simulator) injectPVCs(pvcs []*corev1.PersistentVolumeClaim) error {
	for _, pvc := range pvcs {
		if pvc.Spec.StorageClassName == nil && s.defaultStorageClass != "" {
			pvc.Spec.StorageClassName = &s.defaultStorageClass
		}
		if err := s.pvcStore.Add(pvc); err != nil {
			return fmt.Errorf("injecting synthetic PVC %s/%s into informer store: %w", pvc.Namespace, pvc.Name, err)
		}
	}
	return nil
}

// preemptionMeaningful reports whether preemption could possibly help: it is a
// no-op for pods at or below the default priority, since there is nothing
// lower-priority to evict.
func (s *Simulator) preemptionMeaningful(pod *corev1.Pod) bool {
	return helpers.PodPriority(pod) > s.defaultPriority
}

// resolvePriority fills in pod.Spec.Priority from its PriorityClassName (or the
// global default) when it is not already set, mirroring how priority admission
// resolves priority before scheduling. It returns an error when the pod
// references a PriorityClass that does not exist in the cluster, which the API
// server would itself reject.
func (s *Simulator) resolvePriority(pod *corev1.Pod) error {
	if pod.Spec.Priority != nil {
		return nil
	}
	var prio int32
	switch name := pod.Spec.PriorityClassName; name {
	case "":
		prio = s.defaultPriority
	case "system-cluster-critical":
		prio = systemClusterCritical
	case "system-node-critical":
		prio = systemNodeCritical
	default:
		v, ok := s.priorityClasses[name]
		if !ok {
			return fmt.Errorf("pod %s/%s references unknown PriorityClass %q", pod.Namespace, pod.Name, name)
		}
		prio = v
	}
	pod.Spec.Priority = &prio
	return nil
}

func listNodes(ctx context.Context, client clientset.Interface) ([]*corev1.Node, error) {
	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	nodes := make([]*corev1.Node, 0, len(list.Items))
	for i := range list.Items {
		nodes = append(nodes, &list.Items[i])
	}
	return nodes, nil
}

func listScheduledPods(ctx context.Context, client clientset.Interface) ([]*corev1.Pod, error) {
	list, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.NodeName == "" {
			continue // unscheduled; does not consume node capacity
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue // terminated; does not consume node capacity
		}
		pods = append(pods, p)
	}
	return pods, nil
}

// Well-known priorities for the built-in system PriorityClasses.
const (
	systemClusterCritical int32 = 2000000000
	systemNodeCritical    int32 = 2000001000
)

// listPriorityClasses returns a name->value map of cluster PriorityClasses and
// the global-default priority value (0 when no class is marked globalDefault).
func listPriorityClasses(ctx context.Context, client clientset.Interface) (map[string]int32, int32, error) {
	list, err := client.SchedulingV1().PriorityClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("listing priorityclasses: %w", err)
	}
	priorities := make(map[string]int32, len(list.Items))
	var defaultPriority int32
	for i := range list.Items {
		pc := &list.Items[i]
		priorities[pc.Name] = pc.Value
		if pc.GlobalDefault {
			defaultPriority = pc.Value
		}
	}
	return priorities, defaultPriority, nil
}

// defaultStorageClassName returns the name of the cluster's default
// StorageClass, or "" if none is marked default.
func defaultStorageClassName(ctx context.Context, client clientset.Interface) (string, error) {
	list, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing storageclasses: %w", err)
	}
	for i := range list.Items {
		sc := &list.Items[i]
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return sc.Name, nil
		}
	}
	return "", nil
}
