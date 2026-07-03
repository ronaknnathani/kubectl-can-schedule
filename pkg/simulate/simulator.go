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

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-helpers/resource"
	helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

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

// Result is the outcome of a schedulability check: what the cluster can offer,
// what the workload asked for, whether it fits, and — when it does not — why.
type Result struct {
	// NodeCount is the number of nodes in the cluster.
	NodeCount int
	// Allocatable is the total allocatable capacity summed across all nodes,
	// restricted to the resources the workload actually requests.
	Allocatable corev1.ResourceList
	// Requested is the total resources requested across every replica of every
	// input workload.
	Requested corev1.ResourceList

	// ReplicasRequested and ReplicasFit report how many pods were asked for and
	// how many the simulation could place.
	ReplicasRequested int
	ReplicasFit       int
	// Schedulable is true when every requested replica was placed.
	Schedulable bool
	// UsedPreemption is true when at least one replica only fit by preempting
	// lower-priority pods.
	UsedPreemption bool

	// Reasons maps each filter plugin that rejected the workload to an example
	// message, explaining why replicas could not be placed. Empty when Schedulable.
	Reasons map[string]string
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

// Run executes the greedy cumulative packing simulation over the workloads in
// input order and returns the aggregated result: cluster capacity, total
// requested resources, whether every replica fits, and rejection reasons.
func (s *Simulator) Run(workloads []*input.Workload) (*Result, error) {
	result := &Result{
		NodeCount:   len(s.nodes),
		Allocatable: corev1.ResourceList{},
		Requested:   corev1.ResourceList{},
		Reasons:     map[string]string{},
		Schedulable: true,
	}

	for _, w := range workloads {
		result.ReplicasRequested += int(w.Replicas)
		addRequested(result.Requested, w)

		for i := 0; i < int(w.Replicas); i++ {
			pod, pvcs := w.Replica(i)
			if err := s.injectPVCs(pvcs); err != nil {
				return nil, err
			}

			node, viaPreempt, victims, reasons, err := s.tryPlace(pod)
			if err != nil {
				return nil, err
			}
			if node == "" {
				// Identical template against an unchanged-or-tighter snapshot: if
				// this replica cannot be placed, no later replica of this workload
				// can either, so stop and record why.
				addReasons(result.Reasons, reasons)
				result.Schedulable = false
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
				result.UsedPreemption = true
			}
			result.ReplicasFit++
		}
	}

	result.Allocatable = s.allocatableFor(result.Requested)
	return result, nil
}

// addRequested adds one workload's total requests (per-pod requests scaled by
// its replica count) to the running total. Per-pod requests use the same
// accounting the scheduler applies (init containers, sidecars, pod overhead).
func addRequested(total corev1.ResourceList, w *input.Workload) {
	pod, _ := w.Replica(0)
	perPod := resource.PodRequests(pod, resource.PodResourcesOptions{})
	for name, q := range perPod {
		scaled := q.DeepCopy()
		scaled.Mul(int64(w.Replicas))
		if existing, ok := total[name]; ok {
			existing.Add(scaled)
			total[name] = existing
		} else {
			total[name] = scaled
		}
	}
}

// addReasons merges per-plugin rejection messages, keeping the first seen.
func addReasons(total, reasons map[string]string) {
	for plugin, msg := range reasons {
		if _, ok := total[plugin]; !ok {
			total[plugin] = msg
		}
	}
}

// allocatableFor sums allocatable capacity across all nodes for exactly the
// given resources, so allocatable and requested share the same resource keys.
func (s *Simulator) allocatableFor(requested corev1.ResourceList) corev1.ResourceList {
	allocatable := corev1.ResourceList{}
	for name := range requested {
		total := apiresource.Quantity{}
		for _, node := range s.nodes {
			if q, ok := node.Status.Allocatable[name]; ok {
				total.Add(q)
			}
		}
		allocatable[name] = total
	}
	return allocatable
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
