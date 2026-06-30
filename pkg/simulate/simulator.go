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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
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

// ReplicaResult is the outcome for a single replica.
type ReplicaResult struct {
	Ordinal       int               `json:"ordinal"`
	Fit           bool              `json:"fit"`
	Node          string            `json:"node,omitempty"`
	ViaPreemption bool              `json:"viaPreemption,omitempty"`
	Reasons       map[string]string `json:"reasons,omitempty"`
}

// WorkloadResult aggregates the outcome for one input object.
type WorkloadResult struct {
	Kind              string          `json:"kind"`
	Name              string          `json:"name"`
	Namespace         string          `json:"namespace"`
	Source            string          `json:"source"`
	ReplicasRequested int32           `json:"replicasRequested"`
	ReplicasFit       int32           `json:"replicasFit"`
	Schedulable       bool            `json:"schedulable"`
	Replicas          []ReplicaResult `json:"replicas,omitempty"`
}

// Result is the overall simulation outcome.
type Result struct {
	TotalNodes     int              `json:"totalNodes"`
	AllSchedulable bool             `json:"allSchedulable"`
	Workloads      []WorkloadResult `json:"workloads"`
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

	return &Simulator{
		ctx:             ctx,
		framework:       f,
		lister:          lister,
		opts:            opts,
		nodes:           nodes,
		nodeOrder:       order,
		scheduledPods:   pods,
		pvcStore:        pvcInformer.GetStore(),
		simulated:       map[*corev1.Pod]bool{},
		priorityClasses: priorityClasses,
		defaultPriority: defaultPriority,
	}, nil
}

// Run executes the greedy cumulative packing simulation over the workloads in
// input order and returns the aggregated result.
func (s *Simulator) Run(workloads []*input.Workload) (*Result, error) {
	result := &Result{TotalNodes: len(s.nodes), AllSchedulable: true}

	for _, w := range workloads {
		workloadResult := WorkloadResult{
			Kind:              w.Kind,
			Name:              w.Name,
			Namespace:         w.Namespace,
			Source:            w.Source,
			ReplicasRequested: w.Replicas,
		}
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
				// can either, so stop and leave the rest unscheduled.
				workloadResult.Replicas = append(workloadResult.Replicas, ReplicaResult{Ordinal: i, Reasons: reasons})
				break
			}

			pod.Spec.NodeName = node
			if len(victims) > 0 {
				s.evict(victims)
			}
			s.scheduledPods = append(s.scheduledPods, pod)
			s.simulated[pod] = true
			s.refreshSnapshot()
			workloadResult.Replicas = append(workloadResult.Replicas, ReplicaResult{
				Ordinal:       i,
				Fit:           true,
				Node:          node,
				ViaPreemption: viaPreempt,
			})
			workloadResult.ReplicasFit++
		}
		workloadResult.Schedulable = workloadResult.ReplicasFit == workloadResult.ReplicasRequested
		if !workloadResult.Schedulable {
			result.AllSchedulable = false
		}
		result.Workloads = append(result.Workloads, workloadResult)
	}
	return result, nil
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
// to the PVC informer store so volume filter plugins can evaluate them.
func (s *Simulator) injectPVCs(pvcs []*corev1.PersistentVolumeClaim) error {
	for _, pvc := range pvcs {
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
