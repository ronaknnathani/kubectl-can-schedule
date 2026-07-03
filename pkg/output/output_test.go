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
		NodeCount:   3,
		Schedulable: true,
		Workloads: []simulate.WorkloadResult{{
			Label:         "Deployment/web",
			Replicas:      3,
			ReplicasFit:   3,
			FeasibleNodes: 3,
			Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("12"), Allocated: resource.MustParse("1"), Requested: resource.MustParse("3"), Fit: simulate.ResourceOK},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"Cluster: 3 node(s)", "Deployment/web (3 replicas)", "RESOURCE", "ALLOCATABLE", "Feasible nodes: 3 of 3", "SCHEDULABLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT SCHEDULABLE") {
		t.Errorf("schedulable output should not contain NOT SCHEDULABLE\n%s", out)
	}
	assertNoSemicolons(t, out)
}

func TestRenderAbsentResource(t *testing.T) {
	res := &simulate.Result{
		NodeCount: 3,
		Workloads: []simulate.WorkloadResult{{
			Label:         "workload",
			Replicas:      2,
			ReplicasFit:   0,
			FeasibleNodes: 0,
			Resources: []simulate.ResourceStatus{
				{Name: "nvidia.com/gpu", Allocatable: resource.MustParse("0"), Allocated: resource.MustParse("0"), Requested: resource.MustParse("2"), Fit: simulate.ResourceAbsent},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"NOT PRESENT", "NOT SCHEDULABLE", `no node provides resource type "nvidia.com/gpu"`, "Feasible nodes: 0 of 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	assertNoSemicolons(t, out)
}

func TestRenderPartialWithPackingReason(t *testing.T) {
	res := &simulate.Result{
		NodeCount: 4,
		Workloads: []simulate.WorkloadResult{{
			Label:         "workload",
			Replicas:      6,
			ReplicasFit:   3,
			FeasibleNodes: 3,
			Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("72"), Allocated: resource.MustParse("1"), Requested: resource.MustParse("60"), Fit: simulate.ResourceOK},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"PARTIAL", "3 of 6", "cluster capacity is exhausted after 3 replicas"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	assertNoSemicolons(t, out)
}

func TestRenderInsufficientResource(t *testing.T) {
	res := &simulate.Result{
		NodeCount: 1,
		Workloads: []simulate.WorkloadResult{{
			Label:       "workload",
			Replicas:    1,
			ReplicasFit: 0,
			Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("4"), Allocated: resource.MustParse("0"), Requested: resource.MustParse("100"), Fit: simulate.ResourceInsufficient},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"INSUFFICIENT", "not enough cpu", "100", "allocatable"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	assertNoSemicolons(t, out)
}

func TestRenderColorEmitsAnsiOnlyWhenEnabled(t *testing.T) {
	res := &simulate.Result{
		NodeCount:   1,
		Schedulable: true,
		Workloads: []simulate.WorkloadResult{{
			Label: "workload", Replicas: 1, ReplicasFit: 1, FeasibleNodes: 1,
			Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("4"), Requested: resource.MustParse("1"), Fit: simulate.ResourceOK},
			},
		}},
	}
	var colored bytes.Buffer
	if err := Render(&colored, res, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colored.String(), "\x1b[38;5;") {
		t.Error("expected ANSI color codes when useColor=true")
	}
	if strings.Contains(renderString(t, res), "\x1b[") {
		t.Error("did not expect ANSI codes when useColor=false")
	}
}

func TestFormatQuantity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"3Gi", "3Gi"},
		{"32497696Ki", "30.99Gi"},
		{"512Mi", "512Mi"},
		{"2000", "2000"},
		{"500m", "0.5"},
		{"1", "1"},
	}
	for _, tc := range cases {
		if got := formatQuantity(resource.MustParse(tc.in)); got != tc.want {
			t.Errorf("formatQuantity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWithPercent(t *testing.T) {
	cases := []struct {
		value, allocatable, want string
	}{
		{"20", "72", "20 (28%)"},
		{"2Mi", "30Gi", "2Mi (<1%)"},
		{"0", "10", "0 (0%)"},
		{"2", "0", "2"}, // no allocatable => no percentage
	}
	for _, tc := range cases {
		got := withPercent(resource.MustParse(tc.value), resource.MustParse(tc.allocatable))
		if got != tc.want {
			t.Errorf("withPercent(%q,%q) = %q, want %q", tc.value, tc.allocatable, got, tc.want)
		}
	}
}

func renderString(t *testing.T, res *simulate.Result) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, res, false); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func assertNoSemicolons(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, ";") {
		t.Errorf("output must not contain semicolons\n%s", out)
	}
}
