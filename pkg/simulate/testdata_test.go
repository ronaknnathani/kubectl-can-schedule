package simulate

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/input"
)

// TestTestdataManifests runs every manifest under ../../testdata through the
// simulator against a fake three-worker cluster and asserts the expected
// schedulability outcome and, where relevant, the reason category.
func TestTestdataManifests(t *testing.T) {
	// Three schedulable nodes, none advertising GPUs, plus a default StorageClass
	// so StatefulSet volume claims bind realistically.
	baseObjs := func() []runtime.Object {
		return []runtime.Object{
			newNode("node-a", "8", "16Gi"),
			newNode("node-b", "8", "16Gi"),
			newNode("node-c", "8", "16Gi"),
			&storagev1.StorageClass{
				ObjectMeta:        metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
				Provisioner:       "example.com/local",
				VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
			},
		}
	}

	cases := []struct {
		file           string
		schedulable    bool
		wantAbsent     corev1.ResourceName // a resource expected NOT PRESENT (optional)
		wantFilterPlug string              // a filter plugin expected to reject (optional)
	}{
		{file: "pod-fits.yaml", schedulable: true},
		{file: "deployment-fits.yaml", schedulable: true},
		{file: "statefulset-fits.yaml", schedulable: true},
		{file: "multi-object-fits.yaml", schedulable: true},
		{file: "pod-gpu-absent.yaml", schedulable: false, wantAbsent: "nvidia.com/gpu"},
		{file: "deployment-insufficient-cpu.yaml", schedulable: false},
		{file: "multi-object-mixed.yaml", schedulable: false, wantAbsent: "nvidia.com/gpu"},
		{file: "pod-nodeselector-unmatched.yaml", schedulable: false, wantFilterPlug: "NodeAffinity"},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open("../../testdata/" + tc.file)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()
			wls, err := input.ParseFiles([]string{"-"}, "default", f)
			if err != nil {
				t.Fatalf("ParseFiles: %v", err)
			}
			res := runSim(t, baseObjs(), Options{}, wls)

			if res.Schedulable != tc.schedulable {
				t.Fatalf("schedulable = %v, want %v (workloads=%+v)", res.Schedulable, tc.schedulable, res.Workloads)
			}
			if tc.wantAbsent != "" && !hasAbsentResource(res, tc.wantAbsent) {
				t.Errorf("expected resource %q to be NOT PRESENT in some workload", tc.wantAbsent)
			}
			if tc.wantFilterPlug != "" && !hasFilterReason(res, tc.wantFilterPlug) {
				t.Errorf("expected filter %q to reject nodes in some workload", tc.wantFilterPlug)
			}
		})
	}
}

func hasAbsentResource(res *Result, name corev1.ResourceName) bool {
	for _, wl := range res.Workloads {
		for _, r := range wl.Resources {
			if r.Name == name && r.Fit == ResourceAbsent {
				return true
			}
		}
	}
	return false
}

func hasFilterReason(res *Result, plugin string) bool {
	for _, wl := range res.Workloads {
		if _, ok := wl.FilterReasons[plugin]; ok {
			return true
		}
	}
	return false
}
