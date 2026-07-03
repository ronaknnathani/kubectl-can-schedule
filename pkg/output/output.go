// Package output renders a schedulability Result as a bordered, colorized
// report: cluster capacity, per-resource demand vs capacity, feasible nodes, the
// fit verdict, and — when a workload does not fit — the reasons why.
package output

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

// Pastel 256-color foreground codes, chosen to read well on dark terminals.
const (
	colorGreen = 151 // fits / ok
	colorAmber = 222 // partial / insufficient
	colorRed   = 210 // does not fit / absent
	colorDim   = 245 // secondary text
)

// Render writes a report of res to w. Colors are emitted only when useColor is true.
func Render(w io.Writer, res *simulate.Result, useColor bool) error {
	p := painter{useColor: useColor}
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "Cluster: %d node(s)\n", res.NodeCount)

	for _, wl := range res.Workloads {
		buf.WriteString("\n")
		renderWorkload(&buf, wl, res.NodeCount, p)
	}

	if len(res.Workloads) > 1 {
		buf.WriteString("\n")
		if res.Schedulable {
			fmt.Fprintln(&buf, p.paint(colorGreen, "Overall: SCHEDULABLE — every workload fits."))
		} else {
			fmt.Fprintln(&buf, p.paint(colorRed, "Overall: NOT SCHEDULABLE — at least one workload does not fit."))
		}
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}

func renderWorkload(buf *bytes.Buffer, wl simulate.WorkloadResult, nodeCount int, p painter) {
	fmt.Fprintf(buf, "%s (%s)\n", wl.Label, countNoun(wl.Replicas, "replica"))

	table := tablewriter.NewTable(buf, tablewriter.WithSymbols(tw.NewSymbols(tw.StyleRounded)))
	table.Header([]string{"RESOURCE", "ALLOCATABLE", "ALLOCATED", "REQUESTED", "STATUS"})
	for _, r := range wl.Resources {
		_ = table.Append([]string{
			string(r.Name),
			formatQuantity(r.Allocatable),
			withPercent(r.Allocated, r.Allocatable),
			withPercent(r.Requested, r.Allocatable),
			p.paint(resourceColor(r.Fit), resourceText(r.Fit)),
		})
	}
	_ = table.Render()

	fmt.Fprintf(buf, "Feasible nodes: %d of %d\n", wl.FeasibleNodes, nodeCount)
	fmt.Fprintln(buf, verdictLine(wl, p))
	for _, reason := range workloadReasons(wl) {
		fmt.Fprintf(buf, "  - %s\n", reason)
	}
}

func verdictLine(wl simulate.WorkloadResult, p painter) string {
	switch {
	case wl.Schedulable():
		msg := fmt.Sprintf("SCHEDULABLE — %d of %d replicas fit", wl.ReplicasFit, wl.Replicas)
		if wl.UsedPreemption {
			msg += " (some only by preempting lower-priority pods)"
		}
		return p.paint(colorGreen, "Result: "+msg)
	case wl.ReplicasFit == 0:
		return p.paint(colorRed, fmt.Sprintf("Result: NOT SCHEDULABLE — 0 of %d replicas fit", wl.Replicas))
	default:
		return p.paint(colorAmber, fmt.Sprintf("Result: PARTIAL — %d of %d replicas fit", wl.ReplicasFit, wl.Replicas))
	}
}

// countNoun renders a count with a singular/plural noun, e.g. "1 replica" or
// "3 replicas".
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// workloadReasons explains, in priority order, why a workload does not fully fit.
func workloadReasons(wl simulate.WorkloadResult) []string {
	if wl.Schedulable() {
		return nil
	}
	var reasons []string
	resourceShortfall := false
	for _, r := range wl.Resources {
		switch r.Fit {
		case simulate.ResourceAbsent:
			resourceShortfall = true
			reasons = append(reasons, fmt.Sprintf("no node provides resource type %q", r.Name))
		case simulate.ResourceInsufficient:
			resourceShortfall = true
			reasons = append(reasons, fmt.Sprintf("not enough %s (requested %s, %s allocatable, %s already in use)",
				r.Name, formatQuantity(r.Requested), formatQuantity(r.Allocatable), formatQuantity(r.Allocated)))
		}
	}
	for _, plugin := range sortedKeys(wl.FilterReasons) {
		reasons = append(reasons, fmt.Sprintf("filter %s rejected nodes: %s", plugin, wl.FilterReasons[plugin]))
	}
	// Some replicas fit but not all, and no single resource is cluster-wide short:
	// the feasible nodes ran out of room as replicas were packed onto them.
	if wl.ReplicasFit > 0 && !resourceShortfall {
		reasons = append(reasons, fmt.Sprintf("cluster capacity is exhausted after %d replicas once packed across feasible nodes", wl.ReplicasFit))
	}
	// Nothing fit and nothing above explained it (e.g. a selector matched no node).
	if len(reasons) == 0 {
		reasons = append(reasons, "no node satisfies the workload's scheduling requirements")
	}
	return reasons
}

func resourceText(f simulate.ResourceFit) string {
	switch f {
	case simulate.ResourceAbsent:
		return "NOT PRESENT"
	case simulate.ResourceInsufficient:
		return "INSUFFICIENT"
	default:
		return "OK"
	}
}

func resourceColor(f simulate.ResourceFit) int {
	switch f {
	case simulate.ResourceAbsent:
		return colorRed
	case simulate.ResourceInsufficient:
		return colorAmber
	default:
		return colorGreen
	}
}

// withPercent renders "value (p% of allocatable)"; the percent is omitted when
// there is no allocatable capacity to compare against.
func withPercent(value, allocatable resource.Quantity) string {
	v := formatQuantity(value)
	whole := allocatable.AsApproximateFloat64()
	if whole == 0 {
		return v
	}
	pct := value.AsApproximateFloat64() / whole * 100
	switch {
	case pct == 0:
		return fmt.Sprintf("%s (0%%)", v)
	case pct < 1:
		return fmt.Sprintf("%s (<1%%)", v)
	default:
		return fmt.Sprintf("%s (%.0f%%)", v, pct)
	}
}

// formatQuantity renders a resource quantity for human comparison. Binary
// quantities (memory, ephemeral-storage) are shown in the largest sensible
// binary unit (e.g. "31Gi"); everything else (cpu cores, GPU counts) is shown as
// a plain decimal without SI multiplier suffixes (e.g. "2000" rather than "2k").
func formatQuantity(q resource.Quantity) string {
	if q.Format == resource.BinarySI {
		return humanBinary(q.Value())
	}
	return trimDecimalZeros(q.AsDec().String())
}

func humanBinary(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return strconv.FormatInt(bytes, 10)
	}
	units := []string{"Ki", "Mi", "Gi", "Ti", "Pi", "Ei"}
	value := float64(bytes)
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return trimDecimalZeros(strconv.FormatFloat(value, 'f', 2, 64)) + units[i]
}

func trimDecimalZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// painter applies ANSI color when enabled, and is a no-op otherwise.
type painter struct{ useColor bool }

func (p painter) paint(code int, s string) string {
	if !p.useColor {
		return s
	}
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", code, s)
}
