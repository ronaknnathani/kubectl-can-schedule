package simulate

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/input"
)

func newNode(name, cpu, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
	}
}

func newPod(name, node, cpu, mem string, prio int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: node,
			Priority: ptr.To(prio),
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(mem),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return p
}

func runSim(t *testing.T, objs []runtime.Object, opts Options, workloads []*input.Workload) *Result {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fakeclient.NewSimpleClientset(objs...)
	sim, err := New(ctx, client, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := sim.Run(workloads)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestFlagsPackingAcrossNodes(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "4", "8Gi"),
		newNode("node-b", "4", "8Gi"),
		newNode("node-c", "4", "8Gi"),
	}
	reqs := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	wl := input.FromFlags(reqs, 8, "default", "probe")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})

	if res.TotalNodes != 3 {
		t.Fatalf("TotalNodes = %d, want 3", res.TotalNodes)
	}
	// Each node fits 2 (4 cpu / 2). 3 nodes => 6 fit, 2 do not.
	got := res.Workloads[0].ReplicasFit
	if got != 6 {
		t.Fatalf("ReplicasFit = %d, want 6", got)
	}
	if res.AllSchedulable {
		t.Fatalf("expected AllSchedulable=false")
	}
}

func TestExistingPodsReduceCapacity(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "4", "8Gi"),
		// 3 cpu already used on node-a -> only 1 cpu free.
		newPod("busy", "node-a", "3", "1Gi", 0),
	}
	reqs := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	wl := input.FromFlags(reqs, 1, "default", "probe")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	if res.Workloads[0].ReplicasFit != 0 {
		t.Fatalf("ReplicasFit = %d, want 0 (only 1 cpu free, request 2)", res.Workloads[0].ReplicasFit)
	}
}

func TestDeploymentManifestAllFit(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "8", "16Gi")}
	manifest := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: app
        image: nginx
        resources:
          requests:
            cpu: "1"
            memory: 256Mi
`
	wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	res := runSim(t, objs, Options{}, wls)
	if !res.AllSchedulable || res.Workloads[0].ReplicasFit != 3 {
		t.Fatalf("deployment: fit=%d schedulable=%v, want 3/true", res.Workloads[0].ReplicasFit, res.AllSchedulable)
	}
}

func TestStatefulSetWithVolumeClaimTemplates(t *testing.T) {
	sc := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "fast"},
		Provisioner:       "example.com/fast",
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	objs := []runtime.Object{newNode("node-a", "8", "16Gi"), sc}
	manifest := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels: {app: db}
  template:
    metadata:
      labels: {app: db}
    spec:
      containers:
      - name: db
        image: postgres
        resources:
          requests:
            cpu: "1"
            memory: 512Mi
        volumeMounts:
        - name: data
          mountPath: /data
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: fast
      resources:
        requests:
          storage: 1Gi
`
	wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	res := runSim(t, objs, Options{}, wls)
	if !res.AllSchedulable || res.Workloads[0].ReplicasFit != 2 {
		t.Fatalf("statefulset: fit=%d schedulable=%v, want 2/true; reasons=%v",
			res.Workloads[0].ReplicasFit, res.AllSchedulable, firstReasons(res.Workloads[0]))
	}
}

func TestCumulativeBatchCompetesForCapacity(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	// First workload consumes all 4 cpu (2 replicas x 2 cpu).
	a := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, 2, "default", "a")
	// Second workload needs 1 cpu but none remains.
	b := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 1, "default", "b")

	res := runSim(t, objs, Options{}, []*input.Workload{a, b})
	if res.Workloads[0].ReplicasFit != 2 {
		t.Fatalf("workload a fit=%d, want 2", res.Workloads[0].ReplicasFit)
	}
	if res.Workloads[1].ReplicasFit != 0 {
		t.Fatalf("workload b fit=%d, want 0 (no capacity left)", res.Workloads[1].ReplicasFit)
	}
}

func TestPreemption(t *testing.T) {
	highManifest := `
apiVersion: v1
kind: Pod
metadata:
  name: high
  namespace: default
spec:
  priorityClassName: high
  containers:
  - name: c
    image: busybox
    resources:
      requests:
        cpu: "4"
`
	defaultManifest := `
apiVersion: v1
kind: Pod
metadata:
  name: plain
  namespace: default
spec:
  containers:
  - name: c
    image: busybox
    resources:
      requests:
        cpu: "4"
`
	mkObjs := func() []runtime.Object {
		return []runtime.Object{
			newNode("node-a", "4", "8Gi"),
			newPod("low", "node-a", "4", "1Gi", 0), // fills the node, default priority
			&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000},
		}
	}
	parse := func(manifest string) []*input.Workload {
		t.Helper()
		wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
		if err != nil {
			t.Fatalf("ParseFiles: %v", err)
		}
		return wls
	}

	// Without preemption: high-priority pod cannot fit on the full node.
	resNo := runSim(t, mkObjs(), Options{}, parse(highManifest))
	if resNo.Workloads[0].ReplicasFit != 0 {
		t.Fatalf("no-preempt: fit=%d, want 0", resNo.Workloads[0].ReplicasFit)
	}

	// With preemption + a class above default: fits by evicting the low-priority pod.
	resYes := runSim(t, mkObjs(), Options{ConsiderPreemption: true}, parse(highManifest))
	if resYes.Workloads[0].ReplicasFit != 1 {
		t.Fatalf("preempt: fit=%d, want 1", resYes.Workloads[0].ReplicasFit)
	}
	if !resYes.Workloads[0].Replicas[0].ViaPreemption {
		t.Fatalf("expected ViaPreemption=true")
	}

	// With preemption flag but a default-priority incoming pod: no-op, cannot fit.
	resDefault := runSim(t, mkObjs(), Options{ConsiderPreemption: true}, parse(defaultManifest))
	if resDefault.Workloads[0].ReplicasFit != 0 {
		t.Fatalf("preempt no-op: fit=%d, want 0 (incoming pod at default priority)", resDefault.Workloads[0].ReplicasFit)
	}
}

func firstReasons(wl WorkloadResult) map[string]string {
	for _, r := range wl.Replicas {
		if !r.Fit {
			return r.Reasons
		}
	}
	return nil
}

func TestShortCircuitStopsAfterFirstFailure(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	// 2 cpu per replica on a 4 cpu node => only 2 fit; ask for 5.
	wl := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, 5, "default", "probe")
	res := runSim(t, objs, Options{}, []*input.Workload{wl})

	if res.Workloads[0].ReplicasFit != 2 {
		t.Fatalf("ReplicasFit = %d, want 2", res.Workloads[0].ReplicasFit)
	}
	// The replica list must stop at the first failure (ordinals 0,1 fit + 2 failed),
	// not contain an entry for every requested replica.
	if got := len(res.Workloads[0].Replicas); got != 3 {
		t.Fatalf("len(Replicas) = %d, want 3 (short-circuit after first failure)", got)
	}
}

func TestPreemptionEvictsMinimalVictimSet(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "10", "32Gi"),
		newPod("small", "node-a", "1", "1Gi", 0), // lower priority, tiny
		newPod("big", "node-a", "9", "1Gi", 1),   // lower priority, large
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000},
	}
	highManifest := `
apiVersion: v1
kind: Pod
metadata: {name: high, namespace: default}
spec:
  priorityClassName: high
  containers:
  - name: c
    image: busybox
    resources: {requests: {cpu: "9"}}
`
	high, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(highManifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	// A follow-on, default-priority workload needing 1 cpu. It must NOT fit:
	// minimal preemption evicts only `big` (freeing 9), leaving `small` using the
	// last cpu. Over-eviction (evicting small too) would wrongly free room for it.
	probe := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 1, "default", "probe")

	res := runSim(t, objs, Options{ConsiderPreemption: true}, []*input.Workload{high[0], probe})
	if res.Workloads[0].ReplicasFit != 1 || !res.Workloads[0].Replicas[0].ViaPreemption {
		t.Fatalf("high: fit=%d viaPreempt=%v, want 1/true", res.Workloads[0].ReplicasFit, res.Workloads[0].Replicas[0].ViaPreemption)
	}
	if res.Workloads[1].ReplicasFit != 0 {
		t.Fatalf("probe fit=%d, want 0 (minimal preemption must not over-free capacity)", res.Workloads[1].ReplicasFit)
	}
}

func TestPreemptionDoesNotEvictSimulatedReplicas(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "4", "8Gi"),
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000},
	}
	lowManifest := `
apiVersion: v1
kind: Pod
metadata: {name: low, namespace: default}
spec:
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "4"}}}]
`
	highManifest := `
apiVersion: v1
kind: Pod
metadata: {name: high, namespace: default}
spec:
  priorityClassName: high
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "4"}}}]
`
	low, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(lowManifest))
	if err != nil {
		t.Fatalf("ParseFiles low: %v", err)
	}
	high, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(highManifest))
	if err != nil {
		t.Fatalf("ParseFiles high: %v", err)
	}
	// `low` is placed first (fills the node). `high` must NOT preempt it: a later
	// workload may not cannibalize an earlier one already counted as schedulable.
	res := runSim(t, objs, Options{ConsiderPreemption: true}, []*input.Workload{low[0], high[0]})
	if res.Workloads[0].ReplicasFit != 1 {
		t.Fatalf("low fit=%d, want 1", res.Workloads[0].ReplicasFit)
	}
	if res.Workloads[1].ReplicasFit != 0 {
		t.Fatalf("high fit=%d, want 0 (must not evict a simulated replica)", res.Workloads[1].ReplicasFit)
	}
}

func TestUnknownPriorityClassErrors(t *testing.T) {
	manifest := `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: default}
spec:
  priorityClassName: does-not-exist
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "1"}}}]
`
	wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fakeclient.NewSimpleClientset(newNode("node-a", "4", "8Gi"))
	sim, err := New(ctx, client, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sim.Run(wls); err == nil {
		t.Fatal("expected error for unknown PriorityClass")
	}
}

func TestNodeSelectorExcludesNodes(t *testing.T) {
	nodeA := newNode("node-a", "8", "16Gi")
	nodeA.Labels = map[string]string{"disktype": "ssd"}
	nodeB := newNode("node-b", "8", "16Gi")
	objs := []runtime.Object{nodeA, nodeB}

	manifest := `
apiVersion: v1
kind: Pod
metadata:
  name: needs-ssd
  namespace: default
spec:
  nodeSelector:
    disktype: ssd
  containers:
  - name: c
    image: busybox
    resources:
      requests:
        cpu: "1"
`
	wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	res := runSim(t, objs, Options{}, wls)
	if !res.AllSchedulable || res.Workloads[0].Replicas[0].Node != "node-a" {
		t.Fatalf("nodeSelector: expected placement on node-a, got node=%q schedulable=%v",
			res.Workloads[0].Replicas[0].Node, res.AllSchedulable)
	}
}
