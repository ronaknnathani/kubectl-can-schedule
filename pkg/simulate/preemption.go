package simulate

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	helpers "k8s.io/component-helpers/scheduling/corev1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// tryPreemption performs a non-destructive preemption simulation. For each node
// that failed filters with a *resolvable* (Unschedulable) status, it removes the
// node's lower-priority pods one at a time (lowest priority first) and re-checks
// whether the incoming pod then fits. It never deletes or mutates real pods; it
// only temporarily swaps the in-memory snapshot.
//
// This is an approximation: it considers only pod priority (PodDisruptionBudgets
// are not consulted) and evaluates filter plugins only, consistent with the rest
// of the tool. It returns the node and the minimal victim set that makes the pod
// fit, or ("", nil) if preemption cannot help on any node.
func (s *Simulator) tryPreemption(pod *corev1.Pod, nodeStatuses *framework.NodeToStatus) (string, []*corev1.Pod) {
	podPrio := helpers.PodPriority(pod)
	defer s.refreshSnapshot() // restore the real snapshot after temporary swaps

	for _, nodeName := range s.nodeOrder {
		status := nodeStatuses.Get(nodeName)
		if status == nil || status.Code() != fwk.Unschedulable {
			// Absent, success, or unresolvable (e.g. node affinity / taint): preemption cannot help.
			continue
		}
		if victims := s.preemptionVictimsOnNode(pod, nodeName, podPrio); victims != nil {
			return nodeName, victims
		}
	}
	return "", nil
}

// preemptionVictimsOnNode returns the minimal set of lower-priority pods on
// nodeName whose removal lets pod pass all filters there, or nil if no such set
// exists. Only pods already running in the cluster are eligible victims —
// replicas this run has placed are never evicted, so a later workload cannot
// cannibalize an earlier one that was already counted as schedulable.
func (s *Simulator) preemptionVictimsOnNode(pod *corev1.Pod, nodeName string, podPrio int32) []*corev1.Pod {
	var candidates []*corev1.Pod
	for _, scheduledPod := range s.scheduledPods {
		if s.simulated[scheduledPod] {
			continue
		}
		if scheduledPod.Spec.NodeName != nodeName {
			continue
		}
		if helpers.PodPriority(scheduledPod) >= podPrio {
			continue
		}
		candidates = append(candidates, scheduledPod)
	}
	if len(candidates) == 0 {
		return nil
	}
	// Evict lowest-priority pods first to minimize disruption.
	sort.SliceStable(candidates, func(i, j int) bool {
		return helpers.PodPriority(candidates[i]) < helpers.PodPriority(candidates[j])
	})

	removed := map[*corev1.Pod]bool{}
	for i, victim := range candidates {
		removed[victim] = true
		s.setSnapshotWithout(removed)
		if s.fitsOnNode(pod, nodeName) {
			return s.minimizeVictims(pod, nodeName, candidates[:i+1])
		}
	}
	return nil
}

// minimizeVictims drops any victim that is not actually required for pod to fit
// on nodeName, so the simulation frees only the capacity preemption truly needs
// (avoiding over-eviction that would make later workloads look schedulable).
func (s *Simulator) minimizeVictims(pod *corev1.Pod, nodeName string, victims []*corev1.Pod) []*corev1.Pod {
	removed := map[*corev1.Pod]bool{}
	for _, victim := range victims {
		removed[victim] = true
	}
	// Try to reprieve higher-priority victims first (least desirable to evict).
	for i := len(victims) - 1; i >= 0; i-- {
		victim := victims[i]
		delete(removed, victim)
		s.setSnapshotWithout(removed)
		if !s.fitsOnNode(pod, nodeName) {
			removed[victim] = true // still needed; keep evicted
		}
	}
	requiredVictims := make([]*corev1.Pod, 0, len(removed))
	for _, victim := range victims {
		if removed[victim] {
			requiredVictims = append(requiredVictims, victim)
		}
	}
	return requiredVictims
}

// setSnapshotWithout installs a temporary snapshot with the given pods removed.
func (s *Simulator) setSnapshotWithout(removed map[*corev1.Pod]bool) {
	remaining := make([]*corev1.Pod, 0, len(s.scheduledPods))
	for _, scheduledPod := range s.scheduledPods {
		if removed[scheduledPod] {
			continue
		}
		remaining = append(remaining, scheduledPod)
	}
	s.lister.Set(s.nodes, remaining)
}

// fitsOnNode runs PreFilter + Filter for pod against a single node using the
// snapshot currently installed in the lister.
func (s *Simulator) fitsOnNode(pod *corev1.Pod, nodeName string) bool {
	state := framework.NewCycleState()
	preRes, preStatus, _ := s.framework.RunPreFilterPlugins(s.ctx, state, pod)
	if !preStatus.IsSuccess() {
		return false
	}
	if !preRes.AllNodes() && !preRes.NodeNames.Has(nodeName) {
		return false
	}
	nodeInfo, err := s.lister.NodeInfos().Get(nodeName)
	if err != nil {
		return false
	}
	return s.framework.RunFilterPlugins(s.ctx, state, pod, nodeInfo).IsSuccess()
}
