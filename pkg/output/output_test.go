package output

import (
	"bytes"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

func TestRenderSchedulable(t *testing.T) {
	res := &simulate.Result{
		NodeCount: 3,
		Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("12"),
			corev1.ResourceMemory: resource.MustParse("24Gi"),
		},
		Requested: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("6"),
			corev1.ResourceMemory: resource.MustParse("6Gi"),
		},
		ReplicasRequested: 3,
		ReplicasFit:       3,
		Schedulable:       true,
	}
	var buf bytes.Buffer
	if err := Render(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Cluster: 3 node(s)", "ALLOCATABLE", "REQUESTED", "cpu", "SCHEDULABLE", "3 replica(s); 3 fit"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT SCHEDULABLE") {
		t.Errorf("schedulable result should not say NOT SCHEDULABLE\n%s", out)
	}
}

func TestRenderNotSchedulableShowsReasons(t *testing.T) {
	res := &simulate.Result{
		NodeCount:         3,
		Allocatable:       corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("12")},
		Requested:         corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("40")},
		ReplicasRequested: 10,
		ReplicasFit:       4,
		Schedulable:       false,
		Reasons: map[string]string{
			"NodeResourcesFit": "Insufficient cpu",
			"TaintToleration":  "node(s) had untolerated taint",
		},
	}
	var buf bytes.Buffer
	if err := Render(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"NOT SCHEDULABLE", "Reasons:", "NodeResourcesFit", "Insufficient cpu", "TaintToleration", "10 replica(s); 4 fit"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestRenderPreemptionNote(t *testing.T) {
	res := &simulate.Result{
		NodeCount:         1,
		Allocatable:       corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
		Requested:         corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
		ReplicasRequested: 1,
		ReplicasFit:       1,
		Schedulable:       true,
		UsedPreemption:    true,
	}
	var buf bytes.Buffer
	if err := Render(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "preempting lower-priority pods") {
		t.Errorf("expected preemption note\n%s", buf.String())
	}
}

func TestFormatQuantity(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"3Gi", "3Gi"},
		{"32497696Ki", "30.99Gi"},
		{"512Mi", "512Mi"},
		{"2000", "2000"}, // cpu cores, not "2k"
		{"500m", "0.5"},  // half a core
		{"1", "1"},       // gpu count
	}
	for _, tc := range cases {
		got := formatQuantity(resource.MustParse(tc.in))
		if got != tc.want {
			t.Errorf("formatQuantity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResourceOrdering(t *testing.T) {
	requested := corev1.ResourceList{
		"nvidia.com/gpu":                resource.MustParse("1"),
		corev1.ResourceMemory:           resource.MustParse("1Gi"),
		corev1.ResourceCPU:              resource.MustParse("1"),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	got := resourceNames(requested)
	want := []corev1.ResourceName{
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourceEphemeralStorage,
		"nvidia.com/gpu",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resource order: got %v, want %v", got, want)
		}
	}
}
