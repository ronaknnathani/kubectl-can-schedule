package input

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ParseResourceFlags converts repeatable "<name>=<quantity>" flag values into a
// ResourceList of per-replica requests. Names may be any resource.Quantity-typed
// resource: cpu, memory, ephemeral-storage, nvidia.com/gpu, amd.com/gpu, etc.
func ParseResourceFlags(pairs []string) (corev1.ResourceList, error) {
	rl := corev1.ResourceList{}
	for _, p := range pairs {
		key, val, ok := strings.Cut(p, "=")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if !ok || key == "" || val == "" {
			return nil, fmt.Errorf("invalid --resource %q, expected <name>=<quantity> (e.g. cpu=2, memory=4Gi, nvidia.com/gpu=1)", p)
		}
		q, err := resource.ParseQuantity(val)
		if err != nil {
			return nil, fmt.Errorf("invalid quantity for %q: %w", key, err)
		}
		name := corev1.ResourceName(key)
		if _, exists := rl[name]; exists {
			return nil, fmt.Errorf("resource %q specified more than once", key)
		}
		rl[name] = q
	}
	if len(rl) == 0 {
		return nil, fmt.Errorf("at least one --resource <name>=<quantity> is required for flag-based input")
	}
	return rl, nil
}

// FromFlags builds a synthetic Workload of `replicas` identical pods, each
// requesting `requests`.
func FromFlags(requests corev1.ResourceList, replicas int32, namespace, name string) *Workload {
	if name == "" {
		name = "can-schedule-probe"
	}
	if namespace == "" {
		namespace = "default"
	}
	base := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "container",
				Resources: corev1.ResourceRequirements{Requests: requests},
			}},
		},
	}
	return &Workload{
		Kind:      "flags",
		Name:      name,
		Namespace: namespace,
		Replicas:  replicas,
		Source:    "flags",
		base:      base,
	}
}
