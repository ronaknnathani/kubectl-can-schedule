// Package output renders a schedulability Result as a bordered, colorized
// report. Cluster capacity (allocatable and already-allocated per resource) is
// shown once; a single table then lists each workload as a row with the
// requested resources as columns, its feasible-node count, and the fit result.
// Reasons for anything that does not fit follow the table.
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

// Pastel 256-color foreground codes, chosen to read well on dark terminals.
const (
	colorGreen = 151 // fits / ok
	colorAmber = 222 // partial / insufficient
	colorRed   = 210 // does not fit / absent
)

// Render writes a report of res to w. Colors are emitted only when useColor is true.
func Render(w io.Writer, res *simulate.Result, useColor bool) error {
	p := painter{useColor: useColor}
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "Cluster: %d node(s)\n\n", res.NodeCount)

	resources := clusterResources(res)
	renderCapacityTable(&buf, resources)
	renderWorkloadsTable(&buf, res, resources, p)
	renderVerdict(&buf, res, p)

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}

// resourceCapacity is the cluster-wide capacity of one resource, independent of
// any workload.
type resourceCapacity struct {
	name        corev1.ResourceName
	allocatable resource.Quantity
	allocated   resource.Quantity
}

// clusterResources collects the union of resources requested across all
// workloads, in display order (cpu, memory, ephemeral-storage first). The
// allocatable/allocated values are cluster-wide and identical across workloads.
func clusterResources(res *simulate.Result) []resourceCapacity {
	caps := map[corev1.ResourceName]resourceCapacity{}
	for _, wl := range res.Workloads {
		for _, r := range wl.Resources {
			if _, ok := caps[r.Name]; !ok {
				caps[r.Name] = resourceCapacity{name: r.Name, allocatable: r.Allocatable, allocated: r.Allocated}
			}
		}
	}
	ordered := make([]resourceCapacity, 0, len(caps))
	for _, name := range orderResourceNames(caps) {
		ordered = append(ordered, caps[name])
	}
	return ordered
}

func renderCapacityTable(buf *bytes.Buffer, resources []resourceCapacity) {
	fmt.Fprintln(buf, "Cluster capacity:")
	table := newTable(buf)
	table.Header([]string{"RESOURCE", "ALLOCATABLE", "ALLOCATED"})
	for _, r := range resources {
		_ = table.Append([]string{
			string(r.name),
			formatQuantity(r.allocatable),
			withPercent(r.allocated, r.allocatable),
		})
	}
	_ = table.Render()
}

func renderWorkloadsTable(buf *bytes.Buffer, res *simulate.Result, resources []resourceCapacity, p painter) {
	fmt.Fprintln(buf, "\nWorkloads:")
	header := []string{"KIND", "NAME", "FEASIBLE"}
	for _, r := range resources {
		header = append(header, string(r.name))
	}
	header = append(header, "FIT", "REASON")

	table := newTable(buf)
	table.Header(header)
	for _, wl := range res.Workloads {
		byResource := map[corev1.ResourceName]simulate.ResourceStatus{}
		for _, r := range wl.Resources {
			byResource[r.Name] = r
		}

		row := []string{wl.Kind, displayName(wl), fmt.Sprintf("%d/%d", wl.FeasibleNodes, res.NodeCount)}
		for _, cap := range resources {
			r, requested := byResource[cap.name]
			if !requested {
				row = append(row, "-")
				continue
			}
			row = append(row, p.paint(resourceColor(r.Fit), withPercent(r.Requested, r.Allocatable)))
		}
		row = append(row, p.paint(fitColor(wl), fmt.Sprintf("%d/%d", wl.ReplicasFit, wl.Replicas)), shortReason(wl))
		_ = table.Append(row)
	}
	_ = table.Render()
}

// displayName is the object name, or "-" for a flag-based workload that has none.
func displayName(wl simulate.WorkloadResult) string {
	if wl.Kind == "flags" {
		return "-"
	}
	return wl.Name
}

func renderVerdict(buf *bytes.Buffer, res *simulate.Result, p painter) {
	buf.WriteString("\n")
	if len(res.Workloads) == 1 {
		fmt.Fprintln(buf, verdictLine(res.Workloads[0], p))
		return
	}
	if res.Schedulable {
		fmt.Fprintln(buf, p.paint(colorGreen, "Overall: SCHEDULABLE — every workload fits."))
	} else {
		fmt.Fprintln(buf, p.paint(colorRed, "Overall: NOT SCHEDULABLE — at least one workload does not fit."))
	}
}

func verdictLine(wl simulate.WorkloadResult, p painter) string {
	switch {
	case wl.Schedulable():
		msg := fmt.Sprintf("Result: SCHEDULABLE — %d of %d replicas fit", wl.ReplicasFit, wl.Replicas)
		if wl.UsedPreemption {
			msg += " (some only by preempting lower-priority pods)"
		}
		return p.paint(colorGreen, msg)
	case wl.ReplicasFit == 0:
		return p.paint(colorRed, fmt.Sprintf("Result: NOT SCHEDULABLE — 0 of %d replicas fit", wl.Replicas))
	default:
		return p.paint(colorAmber, fmt.Sprintf("Result: PARTIAL — %d of %d replicas fit", wl.ReplicasFit, wl.Replicas))
	}
}

// shortReason is a concise, comma-separated explanation for the REASON column,
// or "-" when the workload fits. Incidental filter rejections — chiefly the
// control-plane taint, which excludes a node that a more specific reason already
// rules out — are only surfaced when nothing more specific explains the failure.
func shortReason(wl simulate.WorkloadResult) string {
	if wl.Schedulable() {
		return "-"
	}
	var parts []string
	resourceShortfall := false
	for _, r := range wl.Resources {
		switch r.Fit {
		case simulate.ResourceAbsent:
			resourceShortfall = true
			parts = append(parts, "missing "+string(r.Name))
		case simulate.ResourceInsufficient:
			resourceShortfall = true
			parts = append(parts, "insufficient "+string(r.Name))
		}
	}
	hasOtherFilter := false
	for _, plugin := range sortedKeys(wl.FilterReasons) {
		if plugin == "TaintToleration" {
			continue // handled below only when it is the sole signal
		}
		parts = append(parts, shortPlugin(plugin))
		hasOtherFilter = true
	}
	if !resourceShortfall && !hasOtherFilter {
		switch {
		case wl.ReplicasFit > 0:
			parts = append(parts, "capacity exhausted")
		case len(wl.FilterReasons) > 0:
			parts = append(parts, shortPlugin("TaintToleration"))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "unschedulable")
	}
	return strings.Join(parts, ", ")
}

// shortPlugin maps a scheduler filter plugin to a short, human phrase.
func shortPlugin(plugin string) string {
	switch plugin {
	case "TaintToleration":
		return "untolerated taints"
	case "NodeAffinity":
		return "node affinity/selector"
	case "InterPodAffinity":
		return "pod (anti)affinity"
	case "PodTopologySpread":
		return "topology spread"
	case "VolumeBinding", "VolumeZone", "VolumeRestrictions", "NodeVolumeLimits":
		return "volume constraints"
	case "NodePorts":
		return "host port conflict"
	case "NodeUnschedulable":
		return "node cordoned"
	default:
		return plugin
	}
}

// newTable returns a rounded-border table writer into buf. Header auto-format is
// disabled so literal column names such as "nvidia.com/gpu" are not rewritten.
func newTable(buf *bytes.Buffer) *tablewriter.Table {
	return tablewriter.NewTable(buf,
		tablewriter.WithSymbols(tw.NewSymbols(tw.StyleRounded)),
		tablewriter.WithHeaderAutoFormat(tw.Off),
	)
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

func fitColor(wl simulate.WorkloadResult) int {
	switch {
	case wl.Schedulable():
		return colorGreen
	case wl.ReplicasFit == 0:
		return colorRed
	default:
		return colorAmber
	}
}

// orderResourceNames returns the resource names in a stable display order: cpu,
// memory, and ephemeral-storage first, then the rest sorted alphabetically.
func orderResourceNames[V any](m map[corev1.ResourceName]V) []corev1.ResourceName {
	priority := []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage}
	var ordered []corev1.ResourceName
	seen := map[corev1.ResourceName]bool{}
	for _, name := range priority {
		if _, ok := m[name]; ok {
			ordered = append(ordered, name)
			seen[name] = true
		}
	}
	var rest []corev1.ResourceName
	for name := range m {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(ordered, rest...)
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
