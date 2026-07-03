package cli

import (
	"bytes"
	"strings"
	"testing"
)

// runArgs executes the command with the given args and returns the process exit
// code and combined stderr/stdout. These cases all fail during input validation,
// before any cluster connection is attempted.
func runArgs(t *testing.T, args ...string) string {
	t.Helper()
	o := &options{replicas: 1}
	cmd := newCommand(o)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected validation error for args %v, got nil", args)
	}
	return err.Error()
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"both inputs", []string{"-f", "x.yaml", "--resource", "cpu=1"}, "mutually exclusive"},
		{"no input", nil, "either -f/--filename"},
		{"replicas with files", []string{"-f", "x.yaml", "--replicas", "3"}, "--replicas is not valid with -f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runArgs(t, tc.args...)
			if !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
