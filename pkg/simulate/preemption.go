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

	for _, name := range s.nodeOrder {
		st := nodeStatuses.Get(name)
		if st == nil || st.Code() != fwk.Unschedulable {
			// Absent, success, or unresolvable (e.g. node affinity / taint): preemption cannot help.
			continue
		}
		if victims := s.preemptionVictimsOnNode(pod, name, podPrio); victims != nil {
			return name, victims
		}
	}
	return "", nil
}

// preemptionVictimsOnNode returns the minimal set of lower-priority pods on
// nodeName whose removal lets pod pass all filters there, or nil if no such set
// exists.
func (s *Simulator) preemptionVictimsOnNode(pod *corev1.Pod, nodeName string, podPrio int32) []*corev1.Pod {
	var candidates []*corev1.Pod
	for _, p := range s.scheduledPods {
		if p.Spec.NodeName == nodeName && helpers.PodPriority(p) < podPrio {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Evict lowest-priority pods first to minimize disruption.
	sort.SliceStable(candidates, func(i, j int) bool {
		return helpers.PodPriority(candidates[i]) < helpers.PodPriority(candidates[j])
	})

	removed := map[*corev1.Pod]bool{}
	for i, v := range candidates {
		removed[v] = true
		remaining := make([]*corev1.Pod, 0, len(s.scheduledPods))
		for _, p := range s.scheduledPods {
			if removed[p] {
				continue
			}
			remaining = append(remaining, p)
		}
		s.lister.Set(s.nodes, remaining)
		if s.fitsOnNode(pod, nodeName) {
			return candidates[:i+1]
		}
	}
	return nil
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
