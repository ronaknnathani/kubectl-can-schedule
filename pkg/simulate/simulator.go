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
	factory.WaitForCacheSync(ctx.Done())

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
		priorityClasses: priorityClasses,
		defaultPriority: defaultPriority,
	}, nil
}

// Run executes the greedy cumulative packing simulation over the workloads in
// input order and returns the aggregated result.
func (s *Simulator) Run(workloads []*input.Workload) *Result {
	res := &Result{TotalNodes: len(s.nodes), AllSchedulable: true}

	for _, w := range workloads {
		wr := WorkloadResult{
			Kind:              w.Kind,
			Name:              w.Name,
			Namespace:         w.Namespace,
			Source:            w.Source,
			ReplicasRequested: w.Replicas,
		}
		stopped := false
		for i := 0; i < int(w.Replicas); i++ {
			rr := ReplicaResult{Ordinal: i}
			if stopped {
				// Identical template against an unchanged-or-tighter snapshot:
				// if an earlier replica could not be placed, neither can this one.
				wr.Replicas = append(wr.Replicas, rr)
				continue
			}
			pod, pvcs := w.Replica(i)
			s.injectPVCs(pvcs)

			node, viaPreempt, victims, reasons := s.tryPlace(pod)
			if node != "" {
				pod.Spec.NodeName = node
				if len(victims) > 0 {
					s.evict(victims)
				}
				s.scheduledPods = append(s.scheduledPods, pod)
				s.refreshSnapshot()
				rr.Fit = true
				rr.Node = node
				rr.ViaPreemption = viaPreempt
				wr.ReplicasFit++
			} else {
				rr.Reasons = reasons
				stopped = true
			}
			wr.Replicas = append(wr.Replicas, rr)
		}
		wr.Schedulable = wr.ReplicasFit == wr.ReplicasRequested
		if !wr.Schedulable {
			res.AllSchedulable = false
		}
		res.Workloads = append(res.Workloads, wr)
	}
	return res
}

// tryPlace runs PreFilter once then Filter on each node in name order, returning
// the first node that passes all filter plugins. If none fit and preemption is
// enabled (and meaningful for this pod), it attempts a preemption pass.
func (s *Simulator) tryPlace(pod *corev1.Pod) (node string, viaPreemption bool, victims []*corev1.Pod, reasons map[string]string) {
	s.resolvePriority(pod)
	state := framework.NewCycleState()
	reasons = map[string]string{}

	preRes, preStatus, _ := s.framework.RunPreFilterPlugins(s.ctx, state, pod)
	if !preStatus.IsSuccess() {
		reasons[preStatus.Plugin()] = preStatus.Message()
		return "", false, nil, reasons
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
			return name, false, nil, nil
		}
		nodeStatuses.Set(name, status)
		reasons[status.Plugin()] = status.Message()
	}

	if s.opts.ConsiderPreemption && s.preemptionMeaningful(pod) {
		if node, victims := s.tryPreemption(pod, nodeStatuses); node != "" {
			return node, true, victims, nil
		}
	}
	return "", false, nil, reasons
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

func (s *Simulator) injectPVCs(pvcs []*corev1.PersistentVolumeClaim) {
	for _, pvc := range pvcs {
		_ = s.pvcStore.Add(pvc)
	}
}

// preemptionMeaningful reports whether preemption could possibly help: it is a
// no-op for pods at or below the default priority, since there is nothing
// lower-priority to evict.
func (s *Simulator) preemptionMeaningful(pod *corev1.Pod) bool {
	return helpers.PodPriority(pod) > s.defaultPriority
}

// resolvePriority fills in pod.Spec.Priority from its PriorityClassName (or the
// global default) when it is not already set, mirroring how priority admission
// resolves priority before scheduling.
func (s *Simulator) resolvePriority(pod *corev1.Pod) {
	if pod.Spec.Priority != nil {
		return
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
		if v, ok := s.priorityClasses[name]; ok {
			prio = v
		} else {
			prio = s.defaultPriority
		}
	}
	pod.Spec.Priority = &prio
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
	m := make(map[string]int32, len(list.Items))
	var def int32
	for i := range list.Items {
		pc := &list.Items[i]
		m[pc.Name] = pc.Value
		if pc.GlobalDefault {
			def = pc.Value
		}
	}
	return m, def, nil
}
