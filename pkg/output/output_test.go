package output

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

func sampleResult() *simulate.Result {
	return &simulate.Result{
		TotalNodes:     5,
		AllSchedulable: false,
		Workloads: []simulate.WorkloadResult{
			{
				Kind: "Deployment", Name: "web", Namespace: "default", Source: "deploy.yaml",
				ReplicasRequested: 3, ReplicasFit: 3, Schedulable: true,
				Replicas: []simulate.ReplicaResult{
					{Ordinal: 0, Fit: true, Node: "node-a"},
					{Ordinal: 1, Fit: true, Node: "node-b"},
					{Ordinal: 2, Fit: true, Node: "node-c"},
				},
			},
			{
				Kind: "flags", Name: "probe", Namespace: "default", Source: "flags",
				ReplicasRequested: 4, ReplicasFit: 1, Schedulable: false,
				Replicas: []simulate.ReplicaResult{
					{Ordinal: 0, Fit: true, Node: "node-d"},
					{Ordinal: 1, Fit: false, Reasons: map[string]string{
						"NodeResourcesFit": "Insufficient cpu",
						"TaintToleration":  "node(s) had untolerated taint",
					}},
				},
			},
		},
	}
}

func TestRenderTable(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, sampleResult(), FormatTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"KIND", "SCHEDULABLE", "web", "probe", "NOT SCHEDULABLE", "4 of 7 replica"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n%s", want, out)
		}
	}
}

func TestRenderTableVerbose(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, sampleResult(), FormatTable, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Rejection reasons", "NodeResourcesFit", "Insufficient cpu", "TaintToleration"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output missing %q\n%s", want, out)
		}
	}
}

func TestRenderJSON(t *testing.T) {
	want := sampleResult()
	var buf bytes.Buffer
	if err := Render(&buf, want, FormatJSON, false); err != nil {
		t.Fatal(err)
	}
	// The rendered JSON must round-trip back to the exact same Result, and use
	// the documented camelCase wire keys.
	if !strings.Contains(buf.String(), `"allSchedulable"`) {
		t.Errorf("JSON output missing camelCase key %q\n%s", "allSchedulable", buf.String())
	}
	var got simulate.Result
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal rendered JSON: %v", err)
	}
	if !reflect.DeepEqual(&got, want) {
		t.Errorf("round-tripped JSON mismatch:\n got=%+v\nwant=%+v", got, *want)
	}
}

func TestParseFormat(t *testing.T) {
	for _, in := range []string{"", "table", "json", "yaml"} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q) unexpected error: %v", in, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("ParseFormat(\"xml\") expected error")
	}
}
