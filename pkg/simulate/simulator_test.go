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
	return &corev1.Pod{
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

// only returns the single workload result, failing if there is not exactly one.
func only(t *testing.T, res *Result) WorkloadResult {
	t.Helper()
	if len(res.Workloads) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(res.Workloads))
	}
	return res.Workloads[0]
}

func parseManifest(t *testing.T, manifest string) []*input.Workload {
	t.Helper()
	wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	return wls
}

func resourceStatus(t *testing.T, wl WorkloadResult, name corev1.ResourceName) ResourceStatus {
	t.Helper()
	for _, r := range wl.Resources {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("resource %q not found in %+v", name, wl.Resources)
	return ResourceStatus{}
}

func TestFlagsPackingAcrossNodesAndCapacity(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "4", "8Gi"),
		newNode("node-b", "4", "8Gi"),
		newNode("node-c", "4", "8Gi"),
	}
	reqs := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	wl := input.FromFlags(reqs, 8, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	if res.NodeCount != 3 {
		t.Fatalf("NodeCount = %d, want 3", res.NodeCount)
	}
	w := only(t, res)
	// Each node fits 2 (4 cpu / 2). 3 nodes => 6 fit, 2 do not.
	if w.ReplicasFit != 6 || w.Replicas != 8 || w.Schedulable() {
		t.Fatalf("fit/replicas/schedulable = %d/%d/%v, want 6/8/false", w.ReplicasFit, w.Replicas, w.Schedulable())
	}
	if w.FeasibleNodes != 3 {
		t.Fatalf("FeasibleNodes = %d, want 3", w.FeasibleNodes)
	}
	cpu := resourceStatus(t, w, corev1.ResourceCPU)
	if cpu.Allocatable.String() != "12" || cpu.Requested.String() != "16" {
		t.Errorf("cpu allocatable/requested = %s/%s, want 12/16", cpu.Allocatable.String(), cpu.Requested.String())
	}
	// Cluster-wide there is less cpu (12) than requested (16), so the per-resource
	// status is INSUFFICIENT even though packing still places 6 replicas.
	if cpu.Fit != ResourceInsufficient {
		t.Errorf("cpu fit = %v, want Insufficient", cpu.Fit)
	}
}

func TestResourceAbsent(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "8", "16Gi")}
	reqs := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
	wl := input.FromFlags(reqs, 2, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	w := only(t, res)
	if w.Schedulable() || w.ReplicasFit != 0 {
		t.Fatalf("fit=%d schedulable=%v, want 0/false", w.ReplicasFit, w.Schedulable())
	}
	if w.FeasibleNodes != 0 {
		t.Fatalf("FeasibleNodes = %d, want 0", w.FeasibleNodes)
	}
	gpu := resourceStatus(t, w, "nvidia.com/gpu")
	if gpu.Fit != ResourceAbsent {
		t.Errorf("gpu fit = %v, want Absent", gpu.Fit)
	}
}

func TestResourceInsufficient(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	reqs := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100")}
	wl := input.FromFlags(reqs, 1, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	w := only(t, res)
	if w.Schedulable() {
		t.Fatal("expected not schedulable for 100 cpu on a 4-cpu node")
	}
	cpu := resourceStatus(t, w, corev1.ResourceCPU)
	if cpu.Fit != ResourceInsufficient {
		t.Errorf("cpu fit = %v, want Insufficient", cpu.Fit)
	}
}

func TestExistingPodsCountAsAllocated(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "4", "8Gi"),
		newPod("busy", "node-a", "3", "1Gi", 0), // 3 cpu already used
	}
	reqs := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	wl := input.FromFlags(reqs, 1, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	w := only(t, res)
	if w.ReplicasFit != 0 {
		t.Fatalf("fit=%d, want 0 (only 1 cpu free)", w.ReplicasFit)
	}
	cpu := resourceStatus(t, w, corev1.ResourceCPU)
	if cpu.Allocated.String() != "3" {
		t.Errorf("allocated cpu = %s, want 3", cpu.Allocated.String())
	}
	// 4 allocatable, 3 allocated => 1 free < 2 requested.
	if cpu.Fit != ResourceInsufficient {
		t.Errorf("cpu fit = %v, want Insufficient", cpu.Fit)
	}
}

func TestDeploymentManifestAllFit(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "8", "16Gi")}
	manifest := `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default}
spec:
  replicas: 3
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: app, image: nginx, resources: {requests: {cpu: "1", memory: 256Mi}}}]
`
	res := runSim(t, objs, Options{}, parseManifest(t, manifest))
	w := only(t, res)
	if !w.Schedulable() || w.ReplicasFit != 3 {
		t.Fatalf("deployment: fit=%d schedulable=%v, want 3/true", w.ReplicasFit, w.Schedulable())
	}
	if w.Kind != "Deployment" || w.Name != "web" {
		t.Errorf("kind/name = %q/%q, want Deployment/web", w.Kind, w.Name)
	}
}

func TestStatefulSetUsesDefaultStorageClass(t *testing.T) {
	defaultSC := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
		Provisioner:       "example.com/local",
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	objs := []runtime.Object{newNode("node-a", "8", "16Gi"), defaultSC}
	manifest := `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: default}
spec:
  replicas: 1
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers: [{name: db, image: postgres, resources: {requests: {cpu: "1"}}, volumeMounts: [{name: data, mountPath: /data}]}]
  volumeClaimTemplates:
  - metadata: {name: data}
    spec: {accessModes: ["ReadWriteOnce"], resources: {requests: {storage: 1Gi}}}
`
	res := runSim(t, objs, Options{}, parseManifest(t, manifest))
	w := only(t, res)
	if !w.Schedulable() || w.ReplicasFit != 1 {
		t.Fatalf("statefulset: fit=%d schedulable=%v reasons=%v, want 1/true", w.ReplicasFit, w.Schedulable(), w.FilterReasons)
	}
}

func TestCumulativeBatchCompetesForCapacity(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	// First workload consumes all 4 cpu (2 replicas x 2 cpu); second needs 1 more.
	a := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, 2, "default")
	b := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 1, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{a, b})
	if len(res.Workloads) != 2 {
		t.Fatalf("want 2 workloads, got %d", len(res.Workloads))
	}
	if res.Workloads[0].ReplicasFit != 2 || !res.Workloads[0].Schedulable() {
		t.Fatalf("workload a fit=%d schedulable=%v, want 2/true", res.Workloads[0].ReplicasFit, res.Workloads[0].Schedulable())
	}
	if res.Workloads[1].ReplicasFit != 0 || res.Workloads[1].Schedulable() {
		t.Fatalf("workload b fit=%d schedulable=%v, want 0/false (no capacity left)", res.Workloads[1].ReplicasFit, res.Workloads[1].Schedulable())
	}
	if res.Schedulable {
		t.Fatal("overall should be not schedulable")
	}
}

func TestNodeSelectorFeasibility(t *testing.T) {
	nodeA := newNode("node-a", "8", "16Gi")
	nodeA.Labels = map[string]string{"disktype": "ssd"}
	nodeB := newNode("node-b", "8", "16Gi")
	objs := []runtime.Object{nodeA, nodeB}

	manifest := func(disktype string) string {
		return `
apiVersion: v1
kind: Pod
metadata: {name: needs-disk, namespace: default}
spec:
  nodeSelector: {disktype: ` + disktype + `}
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "1"}}}]
`
	}

	// Only node-a is labelled ssd: 1 feasible node, schedulable.
	fits := only(t, runSim(t, objs, Options{}, parseManifest(t, manifest("ssd"))))
	if !fits.Schedulable() || fits.FeasibleNodes != 1 {
		t.Fatalf("ssd: schedulable=%v feasible=%d, want true/1", fits.Schedulable(), fits.FeasibleNodes)
	}

	// No node is labelled nvme: 0 feasible, not schedulable, filter reason recorded.
	none := only(t, runSim(t, objs, Options{}, parseManifest(t, manifest("nvme"))))
	if none.Schedulable() || none.FeasibleNodes != 0 {
		t.Fatalf("nvme: schedulable=%v feasible=%d, want false/0", none.Schedulable(), none.FeasibleNodes)
	}
	if _, ok := none.FilterReasons["NodeAffinity"]; !ok {
		t.Errorf("expected a NodeAffinity filter reason, got %v", none.FilterReasons)
	}
}

func TestPreemption(t *testing.T) {
	highManifest := `
apiVersion: v1
kind: Pod
metadata: {name: high, namespace: default}
spec:
  priorityClassName: high
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "4"}}}]
`
	defaultManifest := `
apiVersion: v1
kind: Pod
metadata: {name: plain, namespace: default}
spec:
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "4"}}}]
`
	mkObjs := func() []runtime.Object {
		return []runtime.Object{
			newNode("node-a", "4", "8Gi"),
			newPod("low", "node-a", "4", "1Gi", 0),
			&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000},
		}
	}

	resNo := only(t, runSim(t, mkObjs(), Options{}, parseManifest(t, highManifest)))
	if resNo.ReplicasFit != 0 {
		t.Fatalf("no-preempt: fit=%d, want 0", resNo.ReplicasFit)
	}

	resYes := only(t, runSim(t, mkObjs(), Options{ConsiderPreemption: true}, parseManifest(t, highManifest)))
	if !resYes.Schedulable() || !resYes.UsedPreemption {
		t.Fatalf("preempt: schedulable=%v usedPreemption=%v, want true/true", resYes.Schedulable(), resYes.UsedPreemption)
	}

	resDefault := only(t, runSim(t, mkObjs(), Options{ConsiderPreemption: true}, parseManifest(t, defaultManifest)))
	if resDefault.ReplicasFit != 0 || resDefault.UsedPreemption {
		t.Fatalf("preempt no-op: fit=%d usedPreemption=%v, want 0/false", resDefault.ReplicasFit, resDefault.UsedPreemption)
	}
}

func TestPreemptionEvictsMinimalVictimSet(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "10", "32Gi"),
		newPod("small", "node-a", "1", "1Gi", 0),
		newPod("big", "node-a", "9", "1Gi", 1),
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000},
	}
	highManifest := `
apiVersion: v1
kind: Pod
metadata: {name: high, namespace: default}
spec:
  priorityClassName: high
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "9"}}}]
`
	high := parseManifest(t, highManifest)
	probe := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 1, "default")

	res := runSim(t, objs, Options{ConsiderPreemption: true}, []*input.Workload{high[0], probe})
	if !res.Workloads[0].Schedulable() || !res.Workloads[0].UsedPreemption {
		t.Fatalf("high: schedulable=%v usedPreemption=%v, want true/true", res.Workloads[0].Schedulable(), res.Workloads[0].UsedPreemption)
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
	low := parseManifest(t, lowManifest)
	high := parseManifest(t, highManifest)
	res := runSim(t, objs, Options{ConsiderPreemption: true}, []*input.Workload{low[0], high[0]})
	if !res.Workloads[0].Schedulable() {
		t.Fatalf("low should fit, got fit=%d", res.Workloads[0].ReplicasFit)
	}
	if res.Workloads[1].ReplicasFit != 0 || res.Workloads[1].UsedPreemption {
		t.Fatalf("high fit=%d usedPreemption=%v, want 0/false (must not evict a simulated replica)",
			res.Workloads[1].ReplicasFit, res.Workloads[1].UsedPreemption)
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
	wls := parseManifest(t, manifest)
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
