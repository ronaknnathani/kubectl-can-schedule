// Package output renders simulation results as a table, JSON, or YAML.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"sigs.k8s.io/yaml"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

// Format is an output format.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat validates and normalizes an output format string.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatJSON, FormatYAML:
		return Format(s), nil
	case "":
		return FormatTable, nil
	default:
		return "", fmt.Errorf("invalid output format %q (want table, json, or yaml)", s)
	}
}

// Render writes the result to w in the requested format.
func Render(w io.Writer, res *simulate.Result, format Format, verbose bool) error {
	switch format {
	case FormatJSON:
		return renderJSON(w, res)
	case FormatYAML:
		return renderYAML(w, res)
	default:
		return renderTable(w, res, verbose)
	}
}

func renderJSON(w io.Writer, res *simulate.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func renderYAML(w io.Writer, res *simulate.Result) error {
	b, err := yaml.Marshal(res)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func renderTable(w io.Writer, res *simulate.Result, verbose bool) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAMESPACE\tNAME\tREPLICAS\tFIT\tSCHEDULABLE\tSOURCE")
	var totalReq, totalFit int32
	for _, wl := range res.Workloads {
		totalReq += wl.ReplicasRequested
		totalFit += wl.ReplicasFit
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			wl.Kind, wl.Namespace, wl.Name,
			wl.ReplicasRequested, wl.ReplicasFit, yesNo(wl.Schedulable), wl.Source)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	if res.AllSchedulable {
		fmt.Fprintf(w, "Verdict: SCHEDULABLE — all %d replica(s) across %d workload(s) fit on %d node(s).\n",
			totalReq, len(res.Workloads), res.TotalNodes)
	} else {
		fmt.Fprintf(w, "Verdict: NOT SCHEDULABLE — %d of %d replica(s) fit across %d node(s); %d of %d workload(s) fully schedulable.\n",
			totalFit, totalReq, res.TotalNodes, countSchedulable(res), len(res.Workloads))
	}

	if verbose {
		renderVerbose(w, res)
	}
	return nil
}

func renderVerbose(w io.Writer, res *simulate.Result) {
	printedHeader := false
	for _, wl := range res.Workloads {
		if wl.Schedulable {
			continue
		}
		reasons := firstFailureReasons(wl)
		if len(reasons) == 0 {
			continue
		}
		if !printedHeader {
			fmt.Fprintln(w, "\nRejection reasons (per filter plugin, first unschedulable replica):")
			printedHeader = true
		}
		fmt.Fprintf(w, "\n  %s/%s (%s): replica %d could not be placed on any node\n",
			wl.Namespace, wl.Name, wl.Kind, firstFailureOrdinal(wl))
		for _, plugin := range sortedKeys(reasons) {
			fmt.Fprintf(w, "    %-26s %s\n", plugin+":", reasons[plugin])
		}
	}
}

func firstFailureReasons(wl simulate.WorkloadResult) map[string]string {
	for _, r := range wl.Replicas {
		if !r.Fit && len(r.Reasons) > 0 {
			return r.Reasons
		}
	}
	return nil
}

func firstFailureOrdinal(wl simulate.WorkloadResult) int {
	for _, r := range wl.Replicas {
		if !r.Fit && len(r.Reasons) > 0 {
			return r.Ordinal
		}
	}
	return 0
}

func countSchedulable(res *simulate.Result) int {
	n := 0
	for _, wl := range res.Workloads {
		if wl.Schedulable {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
