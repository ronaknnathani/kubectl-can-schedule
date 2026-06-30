// Package snapshot provides a mutable, swappable scheduler SharedLister built
// from a point-in-time view of cluster nodes and pods.
package snapshot

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
)

// Swappable is a scheduler-framework SharedLister whose backing snapshot can be
// atomically replaced. The scheduler Framework is constructed once with this
// lister; the simulation refreshes the backing snapshot after each replica is
// placed so that later filter passes observe the updated cluster state
// (resource usage, pod affinity/anti-affinity, topology spread, PVC usage).
type Swappable struct {
	mu    sync.RWMutex
	inner fwk.SharedLister
}

// NewSwappable returns a Swappable backed by an empty snapshot.
func NewSwappable() *Swappable {
	return &Swappable{inner: internalcache.NewEmptySnapshot()}
}

// Set rebuilds the backing snapshot from the given nodes and pods. Pods are the
// full set considered "scheduled" for this view (existing cluster pods plus any
// replicas already placed in the simulation).
func (s *Swappable) Set(nodes []*v1.Node, pods []*v1.Pod) {
	snap := internalcache.NewSnapshot(pods, nodes)
	s.mu.Lock()
	s.inner = snap
	s.mu.Unlock()
}

func (s *Swappable) current() fwk.SharedLister {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner
}

// NodeInfos implements fwk.SharedLister.
func (s *Swappable) NodeInfos() fwk.NodeInfoLister { return s.current().NodeInfos() }

// StorageInfos implements fwk.SharedLister.
func (s *Swappable) StorageInfos() fwk.StorageInfoLister { return s.current().StorageInfos() }

// PodGroupStates implements fwk.SharedLister.
func (s *Swappable) PodGroupStates() fwk.PodGroupStateLister { return s.current().PodGroupStates() }
