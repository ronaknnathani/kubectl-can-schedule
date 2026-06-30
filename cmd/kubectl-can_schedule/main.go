// Command kubectl-can_schedule is a kubectl plugin that reports how many nodes /
// replicas of a workload can be scheduled in a cluster using the default
// scheduler's filter plugins. Installed on PATH as `kubectl-can_schedule`, it is
// invoked as `kubectl can-schedule`.
package main

import (
	"os"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/cli"
)

func main() {
	os.Exit(cli.Execute())
}
