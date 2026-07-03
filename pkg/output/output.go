// Package output renders a schedulability Result as a human-readable report:
// cluster capacity, requested resources, the fit verdict, and — when a workload
// does not fit — the filter-plugin reasons why.
package output

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

// Render writes a report of res to w.
func Render(w io.Writer, res *simulate.Result) error {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "Cluster: %d node(s)\n\n", res.NodeCount)

	tw := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tALLOCATABLE\tREQUESTED")
	for _, name := range resourceNames(res.Requested) {
		allocatable := res.Allocatable[name]
		requested := res.Requested[name]
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, formatQuantity(allocatable), formatQuantity(requested))
	}
	tw.Flush()

	fmt.Fprintf(&buf, "\nRequested %d replica(s); %d fit.\n", res.ReplicasRequested, res.ReplicasFit)

	if res.Schedulable {
		fmt.Fprintln(&buf, "\nResult: SCHEDULABLE — all requested replicas fit.")
		if res.UsedPreemption {
			fmt.Fprintln(&buf, "Note: some replicas fit only by preempting lower-priority pods.")
		}
	} else {
		fmt.Fprintln(&buf, "\nResult: NOT SCHEDULABLE — not all requested replicas fit.")
		fmt.Fprintln(&buf, "Reasons:")
		for _, plugin := range sortedKeys(res.Reasons) {
			fmt.Fprintf(&buf, "  %-22s %s\n", plugin, res.Reasons[plugin])
		}
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}

// resourceNames returns the requested resource names in a stable display order:
// cpu, memory, and ephemeral-storage first (when requested), then the rest
// sorted alphabetically.
func resourceNames(requested corev1.ResourceList) []corev1.ResourceName {
	priority := []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage}
	var ordered []corev1.ResourceName
	seen := map[corev1.ResourceName]bool{}
	for _, name := range priority {
		if _, ok := requested[name]; ok {
			ordered = append(ordered, name)
			seen[name] = true
		}
	}
	var rest []corev1.ResourceName
	for name := range requested {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(ordered, rest...)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// humanBinary formats a byte count using binary (power-of-1024) units, keeping
// up to two decimal places and trimming trailing zeros.
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

// trimDecimalZeros removes trailing fractional zeros (and a dangling decimal
// point) from a decimal string: "0.500" -> "0.5", "3.00" -> "3", "2000" -> "2000".
func trimDecimalZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}
