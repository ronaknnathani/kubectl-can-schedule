// Package cli implements the `kubectl can-schedule` command.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/input"
	"github.com/ronaknnathani/kubectl-can-schedule/pkg/output"
	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

const longDescription = `Report whether a workload fits in a cluster right now.

can-schedule runs the default scheduler's filter plugins (PreFilter + Filter) for
each candidate pod against a live snapshot of the cluster, greedily placing
replicas first-fit and decrementing capacity as each one lands. It answers
"can this land right now?" — it does not score nodes or pick an optimal fit.

Input is either manifest files or resource flags (mutually exclusive):

  # Manifests (Pod, Deployment, StatefulSet; multi-doc and multiple files OK)
  kubectl can-schedule -f deploy.yaml -f sts.yaml
  cat pod.yaml | kubectl can-schedule -f -

  # Synthetic workload from per-replica resource requests
  kubectl can-schedule --resource cpu=2 --resource memory=4Gi \
      --resource nvidia.com/gpu=1 --replicas 10

Multiple input objects are treated as one cumulative batch competing for the
same capacity, in input order. Exit code is non-zero unless every replica fits.`

type options struct {
	kubeconfig string
	context    string
	namespace  string

	filenames  []string
	resources  []string
	replicas   int32
	preemption bool

	exitCode int
}

// Execute runs the command and returns the process exit code.
func Execute() int {
	o := &options{replicas: 1}
	cmd := newCommand(o)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	return o.exitCode
}

// newCommand builds the cobra command bound to the given options.
func newCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "kubectl-can_schedule",
		Short:         "Check whether pods/deployments/statefulsets can be scheduled in a cluster",
		Long:          longDescription,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to the kubeconfig file to use (defaults to the standard loading rules)")
	flags.StringVar(&o.context, "context", "", "Name of the kubeconfig context to use (defaults to the current context)")
	flags.StringVarP(&o.namespace, "namespace", "n", "", "Namespace for manifests/pods that don't specify one")
	flags.StringArrayVarP(&o.filenames, "filename", "f", nil,
		"Manifest file containing Pod/Deployment/StatefulSet objects; repeatable; '-' for stdin")
	flags.StringArrayVar(&o.resources, "resource", nil,
		"Per-replica resource request as <name>=<quantity> (e.g. cpu=2, memory=4Gi, nvidia.com/gpu=1); repeatable")
	flags.Int32Var(&o.replicas, "replicas", 1, "Replica count for flag-based input")
	flags.BoolVar(&o.preemption, "consider-preemption", false,
		"Also consider preemption of lower-priority pods (no-op for pods at/below default priority)")
	return cmd
}

func (o *options) run(cmd *cobra.Command) error {
	if err := o.validateFlags(cmd); err != nil {
		return err
	}

	clientConfig := o.clientConfig()
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		return fmt.Errorf("resolving namespace: %w", err)
	}
	workloads, err := o.buildWorkloads(namespace, cmd.InOrStdin())
	if err != nil {
		return err
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building Kubernetes client: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sim, err := simulate.New(ctx, client, simulate.Options{ConsiderPreemption: o.preemption})
	if err != nil {
		return err
	}
	result, err := sim.Run(workloads)
	if err != nil {
		return err
	}
	if err := output.Render(cmd.OutOrStdout(), result); err != nil {
		return err
	}
	if !result.Schedulable {
		o.exitCode = 1
	}
	return nil
}

// clientConfig builds a kubeconfig loader honoring --kubeconfig, --context, and
// --namespace on top of the standard loading rules (KUBECONFIG env, default path).
func (o *options) clientConfig() clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.kubeconfig != "" {
		rules.ExplicitPath = o.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if o.context != "" {
		overrides.CurrentContext = o.context
	}
	if o.namespace != "" {
		overrides.Context.Namespace = o.namespace
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}

// validateFlags enforces that exactly one input source is given and that
// --replicas is not combined with manifest files.
func (o *options) validateFlags(cmd *cobra.Command) error {
	useFiles := len(o.filenames) > 0
	useFlags := len(o.resources) > 0
	switch {
	case useFiles && useFlags:
		return fmt.Errorf("-f/--filename and --resource are mutually exclusive")
	case !useFiles && !useFlags:
		return fmt.Errorf("provide either -f/--filename (manifests) or --resource (synthetic workload)")
	case useFiles && cmd.Flags().Changed("replicas"):
		return fmt.Errorf("--replicas is not valid with -f; each manifest uses its own replica count")
	}
	return nil
}

// buildWorkloads assembles the workloads to check from either manifest files or
// the synthetic --resource flags. validateFlags must have passed first.
func (o *options) buildWorkloads(namespace string, stdin io.Reader) ([]*input.Workload, error) {
	if len(o.filenames) > 0 {
		return input.ParseFiles(o.filenames, namespace, stdin)
	}
	if o.replicas < 1 {
		return nil, fmt.Errorf("--replicas must be >= 1")
	}
	requests, err := input.ParseResourceFlags(o.resources)
	if err != nil {
		return nil, err
	}
	return []*input.Workload{input.FromFlags(requests, o.replicas, namespace)}, nil
}
