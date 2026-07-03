package input

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ParseResourceFlags converts repeatable "<name>=<quantity>" flag values into a
// ResourceList of per-replica requests. Names may be any resource.Quantity-typed
// resource: cpu, memory, ephemeral-storage, nvidia.com/gpu, amd.com/gpu, etc.
// All malformed values are reported together.
func ParseResourceFlags(pairs []string) (corev1.ResourceList, error) {
	if len(pairs) == 0 {
		return nil, fmt.Errorf("at least one --resource <name>=<quantity> is required for flag-based input")
	}
	requests := corev1.ResourceList{}
	var errs []error
	for _, p := range pairs {
		key, val, ok := strings.Cut(p, "=")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if !ok || key == "" || val == "" {
			errs = append(errs, fmt.Errorf("invalid --resource %q, expected <name>=<quantity> (e.g. cpu=2, memory=4Gi, nvidia.com/gpu=1)", p))
			continue
		}
		q, err := resource.ParseQuantity(val)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid quantity for %q: %w", key, err))
			continue
		}
		if q.Sign() < 0 {
			errs = append(errs, fmt.Errorf("resource %q must not be negative, got %s", key, val))
			continue
		}
		name := corev1.ResourceName(key)
		if _, exists := requests[name]; exists {
			errs = append(errs, fmt.Errorf("resource %q specified more than once", key))
			continue
		}
		requests[name] = q
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return requests, nil
}

// FromFlags builds a synthetic Workload of `replicas` identical pods, each
// requesting `requests`.
func FromFlags(requests corev1.ResourceList, replicas int32, namespace string) *Workload {
	const name = "can-schedule-probe"
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
		Name:      name,
		Namespace: namespace,
		Replicas:  replicas,
		base:      base,
	}
}
