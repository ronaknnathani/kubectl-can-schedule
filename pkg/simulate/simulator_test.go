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

func parseManifest(t *testing.T, manifest string) []*input.Workload {
	t.Helper()
	wls, err := input.ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	return wls
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
	// Each node fits 2 (4 cpu / 2). 3 nodes => 6 fit, 2 do not.
	if res.ReplicasFit != 6 || res.ReplicasRequested != 8 {
		t.Fatalf("fit/requested = %d/%d, want 6/8", res.ReplicasFit, res.ReplicasRequested)
	}
	if res.Schedulable {
		t.Fatalf("expected Schedulable=false")
	}
	// Allocatable is summed across all nodes (3 x 4 = 12 cpu); requested is
	// per-replica x replicas (2 x 8 = 16 cpu).
	if got := res.Allocatable.Cpu().String(); got != "12" {
		t.Errorf("allocatable cpu = %s, want 12", got)
	}
	if got := res.Requested.Cpu().String(); got != "16" {
		t.Errorf("requested cpu = %s, want 16", got)
	}
}

func TestExistingPodsReduceCapacity(t *testing.T) {
	objs := []runtime.Object{
		newNode("node-a", "4", "8Gi"),
		// 3 cpu already used on node-a -> only 1 cpu free.
		newPod("busy", "node-a", "3", "1Gi", 0),
	}
	reqs := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	wl := input.FromFlags(reqs, 1, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	if res.ReplicasFit != 0 || res.Schedulable {
		t.Fatalf("fit=%d schedulable=%v, want 0/false (only 1 cpu free, request 2)", res.ReplicasFit, res.Schedulable)
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
	res := runSim(t, objs, Options{}, parseManifest(t, manifest))
	if !res.Schedulable || res.ReplicasFit != 3 {
		t.Fatalf("deployment: fit=%d schedulable=%v, want 3/true", res.ReplicasFit, res.Schedulable)
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
	res := runSim(t, objs, Options{}, parseManifest(t, manifest))
	if !res.Schedulable || res.ReplicasFit != 2 {
		t.Fatalf("statefulset: fit=%d schedulable=%v reasons=%v, want 2/true",
			res.ReplicasFit, res.Schedulable, res.Reasons)
	}
}

func TestCumulativeBatchCompetesForCapacity(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	// First workload consumes all 4 cpu (2 replicas x 2 cpu).
	a := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, 2, "default")
	// Second workload needs 1 cpu but none remains after the first consumed it.
	b := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 1, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{a, b})
	// 2 (a) fit, the single b replica does not -> 2 of 3.
	if res.ReplicasRequested != 3 || res.ReplicasFit != 2 || res.Schedulable {
		t.Fatalf("fit/requested/schedulable = %d/%d/%v, want 2/3/false", res.ReplicasFit, res.ReplicasRequested, res.Schedulable)
	}
}

func TestImpossibleWorkloadReportsReason(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	wl := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100")}, 1, "default")

	res := runSim(t, objs, Options{}, []*input.Workload{wl})
	if res.Schedulable {
		t.Fatal("expected not schedulable for a 100-cpu request on a 4-cpu node")
	}
	if _, ok := res.Reasons["NodeResourcesFit"]; !ok {
		t.Fatalf("expected a NodeResourcesFit reason, got %v", res.Reasons)
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
			newPod("low", "node-a", "4", "1Gi", 0), // fills the node, default priority
			&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "high"}, Value: 1000},
		}
	}

	// Without preemption: high-priority pod cannot fit on the full node.
	resNo := runSim(t, mkObjs(), Options{}, parseManifest(t, highManifest))
	if resNo.ReplicasFit != 0 {
		t.Fatalf("no-preempt: fit=%d, want 0", resNo.ReplicasFit)
	}

	// With preemption + a class above default: fits by evicting the low-priority pod.
	resYes := runSim(t, mkObjs(), Options{ConsiderPreemption: true}, parseManifest(t, highManifest))
	if resYes.ReplicasFit != 1 || !resYes.Schedulable {
		t.Fatalf("preempt: fit=%d schedulable=%v, want 1/true", resYes.ReplicasFit, resYes.Schedulable)
	}
	if !resYes.UsedPreemption {
		t.Fatalf("expected UsedPreemption=true")
	}

	// With preemption flag but a default-priority incoming pod: no-op, cannot fit.
	resDefault := runSim(t, mkObjs(), Options{ConsiderPreemption: true}, parseManifest(t, defaultManifest))
	if resDefault.ReplicasFit != 0 || resDefault.UsedPreemption {
		t.Fatalf("preempt no-op: fit=%d usedPreemption=%v, want 0/false (incoming pod at default priority)",
			resDefault.ReplicasFit, resDefault.UsedPreemption)
	}
}

func TestPartialFitReportsCount(t *testing.T) {
	objs := []runtime.Object{newNode("node-a", "4", "8Gi")}
	// 2 cpu per replica on a 4 cpu node => only 2 of 5 fit.
	wl := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, 5, "default")
	res := runSim(t, objs, Options{}, []*input.Workload{wl})

	if res.ReplicasRequested != 5 || res.ReplicasFit != 2 || res.Schedulable {
		t.Fatalf("fit/requested/schedulable = %d/%d/%v, want 2/5/false", res.ReplicasFit, res.ReplicasRequested, res.Schedulable)
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
  containers: [{name: c, image: busybox, resources: {requests: {cpu: "9"}}}]
`
	high := parseManifest(t, highManifest)
	// A follow-on, default-priority workload needing 1 cpu. It must NOT fit:
	// minimal preemption evicts only `big` (freeing 9), leaving `small` using the
	// last cpu. Over-eviction (evicting small too) would wrongly free room for it.
	probe := input.FromFlags(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 1, "default")

	res := runSim(t, objs, Options{ConsiderPreemption: true}, []*input.Workload{high[0], probe})
	// Only the high-priority replica fits (via preemption); the probe does not.
	if res.ReplicasFit != 1 || !res.UsedPreemption || res.Schedulable {
		t.Fatalf("fit=%d usedPreemption=%v schedulable=%v, want 1/true/false (minimal preemption must not over-free)",
			res.ReplicasFit, res.UsedPreemption, res.Schedulable)
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
	// `low` is placed first (fills the node). `high` must NOT preempt it: a later
	// workload may not cannibalize an earlier one already counted as schedulable.
	res := runSim(t, objs, Options{ConsiderPreemption: true}, []*input.Workload{low[0], high[0]})
	if res.ReplicasFit != 1 || res.Schedulable || res.UsedPreemption {
		t.Fatalf("fit=%d schedulable=%v usedPreemption=%v, want 1/false/false (must not evict a simulated replica)",
			res.ReplicasFit, res.Schedulable, res.UsedPreemption)
	}
}

func TestStatefulSetUsesDefaultStorageClass(t *testing.T) {
	// A default StorageClass with WaitForFirstConsumer binding, mirroring kind.
	defaultSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "standard",
			Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"},
		},
		Provisioner:       "example.com/local",
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	objs := []runtime.Object{newNode("node-a", "8", "16Gi"), defaultSC}
	// volumeClaimTemplates omits storageClassName, so it must inherit the default.
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
    spec:
      accessModes: ["ReadWriteOnce"]
      resources: {requests: {storage: 1Gi}}
`
	res := runSim(t, objs, Options{}, parseManifest(t, manifest))
	if !res.Schedulable || res.ReplicasFit != 1 {
		t.Fatalf("statefulset with default SC: fit=%d schedulable=%v reasons=%v, want 1/true",
			res.ReplicasFit, res.Schedulable, res.Reasons)
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

func TestNodeSelectorExcludesNodes(t *testing.T) {
	newObjs := func() []runtime.Object {
		nodeA := newNode("node-a", "8", "16Gi")
		nodeA.Labels = map[string]string{"disktype": "ssd"}
		nodeB := newNode("node-b", "8", "16Gi")
		return []runtime.Object{nodeA, nodeB}
	}
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

	// Only node-a is labelled ssd, so the pod fits.
	fits := runSim(t, newObjs(), Options{}, parseManifest(t, manifest("ssd")))
	if !fits.Schedulable {
		t.Fatalf("selector disktype=ssd should be schedulable on node-a, reasons=%v", fits.Reasons)
	}

	// No node is labelled nvme, so the selector excludes every node.
	excluded := runSim(t, newObjs(), Options{}, parseManifest(t, manifest("nvme")))
	if excluded.Schedulable {
		t.Fatal("selector disktype=nvme should exclude all nodes")
	}
	if len(excluded.Reasons) == 0 {
		t.Fatal("expected a rejection reason when the selector matches no node")
	}
}
