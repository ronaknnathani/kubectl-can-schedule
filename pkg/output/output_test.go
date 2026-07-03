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
			Kind:          "Deployment",
			Name:          "web",
			Replicas:      3,
			ReplicasFit:   3,
			FeasibleNodes: 3,
			Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("12"), Allocated: resource.MustParse("1"), Requested: resource.MustParse("3"), Fit: simulate.ResourceOK},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"Cluster: 3 node(s)", "Cluster capacity:", "Workloads:", "Deployment", "web", "SCHEDULABLE — 3 of 3 replicas fit"} {
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
			Kind:          "flags",
			Replicas:      2,
			ReplicasFit:   0,
			FeasibleNodes: 0,
			Resources: []simulate.ResourceStatus{
				{Name: "nvidia.com/gpu", Allocatable: resource.MustParse("0"), Allocated: resource.MustParse("0"), Requested: resource.MustParse("2"), Fit: simulate.ResourceAbsent},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"nvidia.com/gpu", "NOT SCHEDULABLE — 0 of 2 replicas fit", "missing nvidia.com/gpu"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	assertNoSemicolons(t, out)
}

func TestRenderPartial(t *testing.T) {
	res := &simulate.Result{
		NodeCount: 4,
		Workloads: []simulate.WorkloadResult{{
			Kind:          "Deployment",
			Name:          "hungry",
			Replicas:      6,
			ReplicasFit:   3,
			FeasibleNodes: 3,
			Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("72"), Allocated: resource.MustParse("1"), Requested: resource.MustParse("60"), Fit: simulate.ResourceInsufficient},
			},
		}},
	}
	out := renderString(t, res)
	for _, want := range []string{"PARTIAL — 3 of 6 replicas fit", "insufficient cpu", "3/6"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	assertNoSemicolons(t, out)
}

func TestRenderMultipleWorkloadsShareOneCapacityTable(t *testing.T) {
	res := &simulate.Result{
		NodeCount:   3,
		Schedulable: false,
		Workloads: []simulate.WorkloadResult{
			{Kind: "Deployment", Name: "a", Replicas: 2, ReplicasFit: 2, FeasibleNodes: 3, Resources: []simulate.ResourceStatus{
				{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("12"), Requested: resource.MustParse("2"), Fit: simulate.ResourceOK},
			}},
			{Kind: "Pod", Name: "b", Replicas: 1, ReplicasFit: 0, FeasibleNodes: 0, Resources: []simulate.ResourceStatus{
				{Name: "nvidia.com/gpu", Allocatable: resource.MustParse("0"), Requested: resource.MustParse("1"), Fit: simulate.ResourceAbsent},
			}},
		},
	}
	out := renderString(t, res)
	// One cluster capacity table regardless of workload count.
	if n := strings.Count(out, "ALLOCATABLE"); n != 1 {
		t.Errorf("ALLOCATABLE should appear once, got %d\n%s", n, out)
	}
	// Both objects appear as rows in a single workloads table (one header).
	if n := strings.Count(out, "FEASIBLE"); n != 1 {
		t.Errorf("workloads table header should appear once, got %d\n%s", n, out)
	}
	for _, want := range []string{"Deployment", "Pod", "missing nvidia.com/gpu", "Overall: NOT SCHEDULABLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	assertNoSemicolons(t, out)
}

func TestShortReasonSuppressesIncidentalTaint(t *testing.T) {
	// A resource shortfall is the binding constraint, so the control-plane taint
	// (recorded as a filter reason) must not appear in the concise reason.
	wl := simulate.WorkloadResult{
		Kind: "Pod", Name: "t", Replicas: 1, ReplicasFit: 0,
		Resources:     []simulate.ResourceStatus{{Name: "nvidia.com/gpu", Requested: resource.MustParse("1"), Fit: simulate.ResourceAbsent}},
		FilterReasons: map[string]string{"TaintToleration": "node(s) had untolerated taint(s)"},
	}
	got := shortReason(wl)
	if got != "missing nvidia.com/gpu" {
		t.Errorf("shortReason = %q, want %q", got, "missing nvidia.com/gpu")
	}
}

func TestShortReasonFilterOnly(t *testing.T) {
	// A real filter (node affinity) plus the incidental control-plane taint: only
	// the specific filter is surfaced.
	wl := simulate.WorkloadResult{
		Kind: "Pod", Name: "s", Replicas: 1, ReplicasFit: 0,
		Resources:     []simulate.ResourceStatus{{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("8"), Requested: resource.MustParse("1"), Fit: simulate.ResourceOK}},
		FilterReasons: map[string]string{"NodeAffinity": "didn't match", "TaintToleration": "untolerated"},
	}
	if got := shortReason(wl); got != "node affinity/selector" {
		t.Errorf("shortReason = %q, want %q", got, "node affinity/selector")
	}
}

func TestShortReasonTaintOnly(t *testing.T) {
	// When the taint is the only signal, it is surfaced.
	wl := simulate.WorkloadResult{
		Kind: "Pod", Name: "s", Replicas: 1, ReplicasFit: 0,
		Resources:     []simulate.ResourceStatus{{Name: corev1.ResourceCPU, Allocatable: resource.MustParse("8"), Requested: resource.MustParse("1"), Fit: simulate.ResourceOK}},
		FilterReasons: map[string]string{"TaintToleration": "untolerated"},
	}
	if got := shortReason(wl); got != "untolerated taints" {
		t.Errorf("shortReason = %q, want %q", got, "untolerated taints")
	}
}

func TestRenderColorEmitsAnsiOnlyWhenEnabled(t *testing.T) {
	res := &simulate.Result{
		NodeCount:   1,
		Schedulable: true,
		Workloads: []simulate.WorkloadResult{{
			Kind: "flags", Replicas: 1, ReplicasFit: 1, FeasibleNodes: 1,
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
	cases := []struct{ value, allocatable, want string }{
		{"20", "72", "20 (28%)"},
		{"2Mi", "30Gi", "2Mi (<1%)"},
		{"0", "10", "0 (0%)"},
		{"2", "0", "2"},
	}
	for _, tc := range cases {
		got := withPercent(resource.MustParse(tc.value), resource.MustParse(tc.allocatable))
		if got != tc.want {
			t.Errorf("withPercent(%q,%q) = %q, want %q", tc.value, tc.allocatable, got, tc.want)
		}
	}
}

func TestOrderResourceNames(t *testing.T) {
	m := map[corev1.ResourceName]bool{
		"nvidia.com/gpu":                true,
		corev1.ResourceMemory:           true,
		corev1.ResourceCPU:              true,
		corev1.ResourceEphemeralStorage: true,
	}
	got := orderResourceNames(m)
	want := []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, "nvidia.com/gpu"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v", got, want)
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
