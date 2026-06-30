// Package cli implements the `kubectl can-schedule` command.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/input"
	"github.com/ronaknnathani/kubectl-can-schedule/pkg/output"
	"github.com/ronaknnathani/kubectl-can-schedule/pkg/simulate"
)

const longDescription = `Report how many nodes / replicas can be scheduled in a cluster.

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
	configFlags *genericclioptions.ConfigFlags

	filenames   []string
	resources   []string
	replicas    int32
	preemption  bool
	outputFmt   string
	verbose     bool
	name        string

	exitCode int
}

// Execute runs the command and returns the process exit code.
func Execute() int {
	o := &options{
		configFlags: genericclioptions.NewConfigFlags(true),
		replicas:    1,
	}
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
	flags.StringArrayVarP(&o.filenames, "filename", "f", nil,
		"Manifest file containing Pod/Deployment/StatefulSet objects; repeatable; '-' for stdin")
	flags.StringArrayVar(&o.resources, "resource", nil,
		"Per-replica resource request as <name>=<quantity> (e.g. cpu=2, memory=4Gi, nvidia.com/gpu=1); repeatable")
	flags.Int32Var(&o.replicas, "replicas", 1, "Replica count for flag-based input")
	flags.BoolVar(&o.preemption, "consider-preemption", false,
		"Also consider preemption of lower-priority pods (no-op for pods at/below default priority)")
	flags.StringVarP(&o.outputFmt, "output", "o", "table", "Output format: table, json, or yaml")
	flags.BoolVar(&o.verbose, "verbose", false, "Show per-filter-plugin rejection reasons for unschedulable workloads")
	flags.StringVar(&o.name, "name", "", "Name for the synthetic flag-based workload")
	o.configFlags.AddFlags(flags)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	return o.exitCode
}

func (o *options) run(cmd *cobra.Command) error {
	format, err := output.ParseFormat(o.outputFmt)
	if err != nil {
		return err
	}

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

	restConfig, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	namespace, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("resolving namespace: %w", err)
	}

	var workloads []*input.Workload
	if useFiles {
		workloads, err = input.ParseFiles(o.filenames, namespace, cmd.InOrStdin())
		if err != nil {
			return err
		}
	} else {
		if o.replicas < 1 {
			return fmt.Errorf("--replicas must be >= 1")
		}
		requests, err := input.ParseResourceFlags(o.resources)
		if err != nil {
			return err
		}
		workloads = []*input.Workload{input.FromFlags(requests, o.replicas, namespace, o.name)}
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
	result := sim.Run(workloads)

	if err := output.Render(cmd.OutOrStdout(), result, format, o.verbose); err != nil {
		return err
	}
	if !result.AllSchedulable {
		o.exitCode = 1
	}
	return nil
}
