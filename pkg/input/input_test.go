package input

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseFilesMultiDoc(t *testing.T) {
	manifest := `
apiVersion: v1
kind: Pod
metadata:
  name: solo
spec:
  containers:
  - name: c
    image: busybox
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 4
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: c
        image: nginx
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
spec:
  replicas: 2
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers: [{name: c, image: postgres}]
  volumeClaimTemplates:
  - metadata: {name: data}
    spec:
      accessModes: ["ReadWriteOnce"]
      resources: {requests: {storage: 1Gi}}
`
	wls, err := ParseFiles([]string{"-"}, "myns", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if len(wls) != 3 {
		t.Fatalf("got %d workloads, want 3", len(wls))
	}
	// Replica counts distinguish the three objects: a bare Pod is always 1, and
	// the Deployment/StatefulSet carry their own spec.replicas.
	if wls[0].Replicas != 1 {
		t.Errorf("pod: got replicas=%d, want 1", wls[0].Replicas)
	}
	if wls[1].Replicas != 4 {
		t.Errorf("deployment: got replicas=%d, want 4", wls[1].Replicas)
	}
	if wls[2].Replicas != 2 {
		t.Errorf("statefulset: got replicas=%d, want 2", wls[2].Replicas)
	}
	// Namespace defaulting applies when the manifest omits a namespace.
	if wls[0].Namespace != "myns" {
		t.Errorf("namespace defaulting: got %q want myns", wls[0].Namespace)
	}
}

func TestStatefulSetReplicaInjectsPVC(t *testing.T) {
	manifest := `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: default}
spec:
  replicas: 3
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers: [{name: c, image: postgres}]
  volumeClaimTemplates:
  - metadata: {name: data}
    spec:
      accessModes: ["ReadWriteOnce"]
      resources: {requests: {storage: 1Gi}}
`
	wls, err := ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	pod, pvcs := wls[0].Replica(1)
	if len(pvcs) != 1 {
		t.Fatalf("got %d pvcs, want 1", len(pvcs))
	}
	wantPVC := "data-db-1"
	if pvcs[0].Name != wantPVC {
		t.Errorf("pvc name = %q, want %q", pvcs[0].Name, wantPVC)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == wantPVC {
			found = true
		}
	}
	if !found {
		t.Errorf("pod missing volume referencing %q", wantPVC)
	}
}

func TestParseResourceFlags(t *testing.T) {
	rl, err := ParseResourceFlags([]string{"cpu=2", "memory=4Gi", "nvidia.com/gpu=1"})
	if err != nil {
		t.Fatalf("ParseResourceFlags: %v", err)
	}
	if rl[corev1.ResourceCPU] != resource.MustParse("2") {
		t.Errorf("cpu = %v", rl[corev1.ResourceCPU])
	}
	if rl["nvidia.com/gpu"] != resource.MustParse("1") {
		t.Errorf("gpu = %v", rl["nvidia.com/gpu"])
	}

	for _, bad := range [][]string{{"cpu"}, {"cpu="}, {"=2"}, {"cpu=notaquantity"}, {"cpu=-1"}, {"gpu=1"}, {}} {
		if _, err := ParseResourceFlags(bad); err == nil {
			t.Errorf("ParseResourceFlags(%v) expected error", bad)
		}
	}
	if _, err := ParseResourceFlags([]string{"cpu=1", "cpu=2"}); err == nil {
		t.Error("duplicate resource expected error")
	}
	// A fully-qualified extended resource is valid.
	if _, err := ParseResourceFlags([]string{"nvidia.com/gpu=1"}); err != nil {
		t.Errorf("nvidia.com/gpu should be valid: %v", err)
	}
}

func TestInvalidResourceNameInManifest(t *testing.T) {
	// "gpu" is unqualified and not a standard resource; the API would reject it
	// and the scheduler silently ignores it, so parsing must reject it.
	manifest := `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: default}
spec:
  containers: [{name: c, image: busybox, resources: {requests: {gpu: "1"}}}]
`
	if _, err := ParseFiles([]string{"-"}, "default", strings.NewReader(manifest)); err == nil {
		t.Error("expected error for unqualified resource name gpu in manifest")
	}
}

func TestNegativeReplicasRejected(t *testing.T) {
	manifest := `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default}
spec:
  replicas: -2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: c, image: nginx}]
`
	if _, err := ParseFiles([]string{"-"}, "default", strings.NewReader(manifest)); err == nil {
		t.Error("expected error for negative spec.replicas")
	}
}

func TestGenerateNameResolvedForDeployment(t *testing.T) {
	manifest := `
apiVersion: apps/v1
kind: Deployment
metadata: {generateName: web-, namespace: default}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: c, image: nginx}]
`
	wls, err := ParseFiles([]string{"-"}, "default", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if wls[0].Name == "" {
		t.Error("Deployment workload name should be derived from generateName, got empty")
	}
}

func TestUnsupportedKind(t *testing.T) {
	manifest := `
apiVersion: batch/v1
kind: Job
metadata: {name: j}
spec:
  template:
    spec:
      containers: [{name: c, image: busybox}]
      restartPolicy: Never
`
	if _, err := ParseFiles([]string{"-"}, "default", strings.NewReader(manifest)); err == nil {
		t.Error("expected error for unsupported kind Job")
	}
}
