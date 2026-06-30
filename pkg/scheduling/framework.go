// Package scheduling wires up an in-process Kubernetes scheduler Framework that
// runs the default scheduler profile's PreFilter and Filter plugins. It is a
// trimmed-down, dependency-free adaptation of the pattern the kube-scheduler
// itself uses to construct a Framework from the in-tree plugin registry.
package scheduling

import (
	"context"
	"fmt"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	resourceslicetracker "k8s.io/dynamic-resource-allocation/resourceslice/tracker"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/dynamicresources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodevolumelimits"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedulermetrics "k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
)

// DefaultSchedulerName is the scheduler name of the default profile.
const DefaultSchedulerName = "default-scheduler"

// BuildDefaultFramework constructs a Framework configured with the default
// scheduler profile (and therefore the default set of filter plugins).
//
// sharedLister supplies the node/pod view that filter plugins evaluate against
// (NodeResourcesFit, PodTopologySpread, InterPodAffinity, ...). informerFactory
// supplies the cluster objects that volume-related plugins need (PV, PVC,
// StorageClass, CSINode). The caller is responsible for starting the informer
// factory and waiting for its caches to sync before running any plugins.
func BuildDefaultFramework(
	ctx context.Context,
	client clientset.Interface,
	sharedLister fwk.SharedLister,
	informerFactory informers.SharedInformerFactory,
) (framework.Framework, error) {
	// Register scheduler metrics once; the underlying registration is guarded by
	// a sync.Once so repeated calls are safe.
	schedulermetrics.Register()

	cfg, err := latest.Default()
	if err != nil {
		return nil, fmt.Errorf("building default scheduler configuration: %w", err)
	}
	profile, err := defaultProfile(cfg)
	if err != nil {
		return nil, err
	}

	csiManager := nodevolumelimits.NewCSIManager(informerFactory.Storage().V1().CSINodes().Lister())

	draManager, err := buildDRAManager(ctx, client, informerFactory)
	if err != nil {
		return nil, err
	}

	opts := []frameworkruntime.Option{
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSnapshotSharedLister(sharedLister),
		frameworkruntime.WithSharedCSIManager(csiManager),
		frameworkruntime.WithSharedDRAManager(draManager),
	}

	f, err := frameworkruntime.NewFramework(ctx, plugins.NewInTreeRegistry(), profile, opts...)
	if err != nil {
		return nil, fmt.Errorf("constructing scheduler framework: %w", err)
	}
	return f, nil
}

// buildDRAManager constructs the Dynamic Resource Allocation manager exactly as
// the kube-scheduler does. DRA is GA (on by default) in this Kubernetes version,
// so the DynamicResources plugin is part of the default profile and dereferences
// this manager during PreFilter even for pods that declare no resource claims.
// Returns nil when the feature gate is disabled.
func buildDRAManager(
	ctx context.Context,
	client clientset.Interface,
	informerFactory informers.SharedInformerFactory,
) (fwk.SharedDRAManager, error) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		return nil, nil
	}
	logger := klog.FromContext(ctx)
	resourceClaimInformer := informerFactory.Resource().V1().ResourceClaims().Informer()
	resourceClaimCache := assumecache.NewAssumeCache(logger, resourceClaimInformer, "ResourceClaim", "", nil)

	opts := resourceslicetracker.Options{
		EnableDeviceTaintRules:   utilfeature.DefaultFeatureGate.Enabled(features.DRADeviceTaintRules),
		EnableConsumableCapacity: utilfeature.DefaultFeatureGate.Enabled(features.DRAConsumableCapacity),
		SliceInformer:            informerFactory.Resource().V1().ResourceSlices(),
		KubeClient:               client,
	}
	if opts.EnableDeviceTaintRules {
		opts.TaintInformer = informerFactory.Resource().V1beta2().DeviceTaintRules()
		opts.ClassInformer = informerFactory.Resource().V1().DeviceClasses()
	}
	tracker, err := resourceslicetracker.StartTracker(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("starting resource slice tracker: %w", err)
	}
	return dynamicresources.NewDRAManager(ctx, resourceClaimCache, tracker, informerFactory), nil
}
