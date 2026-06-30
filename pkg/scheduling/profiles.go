package scheduling

import (
	"fmt"

	schedconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
)

// defaultProfile returns a pointer to the default-scheduler profile from a
// defaulted KubeSchedulerConfiguration. The defaulted configuration enables the
// standard set of plugins (including feature-gated ones such as DynamicResources
// and NodeDeclaredFeatures that are on by default in this Kubernetes version).
func defaultProfile(cfg *schedconfig.KubeSchedulerConfiguration) (*schedconfig.KubeSchedulerProfile, error) {
	for i := range cfg.Profiles {
		if cfg.Profiles[i].SchedulerName == DefaultSchedulerName {
			return &cfg.Profiles[i], nil
		}
	}
	return nil, fmt.Errorf("default scheduler profile %q not found in defaulted configuration", DefaultSchedulerName)
}
