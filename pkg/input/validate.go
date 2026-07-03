package input

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// validateResourceName reports whether name is a resource a container may
// request. This mirrors the Kubernetes API's container-resource validation: an
// unqualified name must be a standard resource (cpu, memory, ephemeral-storage,
// hugepages-*), otherwise it must be a fully-qualified extended resource such as
// nvidia.com/gpu. Without this check, an unqualified name like "gpu" is silently
// dropped by the scheduler's resource accounting and the workload wrongly fits.
func validateResourceName(name corev1.ResourceName) error {
	n := string(name)
	if !strings.Contains(n, "/") {
		if isStandardContainerResource(n) {
			return nil
		}
		return fmt.Errorf("invalid resource %q: must be a standard resource (cpu, memory, ephemeral-storage, hugepages-*) or a fully-qualified extended resource like nvidia.com/gpu", n)
	}
	if errs := validation.IsQualifiedName(n); len(errs) > 0 {
		return fmt.Errorf("invalid resource name %q: %s", n, strings.Join(errs, ", "))
	}
	return nil
}

func isStandardContainerResource(name string) bool {
	switch corev1.ResourceName(name) {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	}
	return strings.HasPrefix(name, corev1.ResourceHugePagesPrefix)
}

// validatePodResourceNames validates every resource name a pod's containers
// (regular and init) request or limit.
func validatePodResourceNames(pod *corev1.Pod) error {
	containers := append([]corev1.Container{}, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for _, ctr := range containers {
		for _, list := range []corev1.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
			for name := range list {
				if err := validateResourceName(name); err != nil {
					return fmt.Errorf("container %q: %w", ctr.Name, err)
				}
			}
		}
	}
	return nil
}
