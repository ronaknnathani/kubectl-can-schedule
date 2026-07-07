# kubectl-can-schedule

Go-based kubectl plugin that reports whether one or more workloads can be
scheduled in a Kubernetes cluster. It runs the upstream default scheduler's
filter plugins (PreFilter + Filter) against a live, read-only snapshot of the
cluster and prints a per-workload capacity and fit report to stdout. It never
mutates the cluster — even preemption is simulated in-memory.

## Build, test, and lint

Use the Makefile targets; they pin and bootstrap tooling where needed.

```bash
make build          # go mod tidy, tests, lint, then build
make test           # unit tests
make lint           # pinned golangci-lint v2.12.2
make ci             # local equivalent of GitHub Actions checks
make test-coverage  # coverage report
make snapshot       # local GoReleaser snapshot
```

Run `make ci` before pushing meaningful code, workflow, or release changes. For
docs-only changes, at least run `git diff --check`; run more if docs touch
commands, install, or release instructions.

Note: the module depends on `k8s.io/kubernetes`, so a cold `make ci` (which runs
`go test -race`) downloads a large module graph and can take several minutes.

## Development conventions

- Follow standard Go conventions and keep changes focused and surgical.
- Add or update tests for behavior changes; a bug fix needs a test that fails
  without the fix.
- Prefer table-driven tests and the existing fixtures; construct fake clusters
  with the fake Kubernetes client. Do not require a live cluster for unit tests.
- Keep new manifests for scenario tests under `testdata/`.
- Preserve the tool's **read-only** behavior. It only lists nodes, pods,
  PriorityClasses, and StorageClasses; it must never create, update, delete, or
  evict real objects. Preemption is a non-destructive in-memory simulation.
- Keep stdout clean for the report; errors and diagnostics go to stderr.
- Emit ANSI color only when stdout is a terminal.
- Fidelity matters: when the scheduler or an admission plugin would treat input
  a certain way (default StorageClass injection, resource-name validation,
  priority resolution), mirror that rather than approximating.
- Do not commit generated artifacts such as `kubectl-can_schedule`,
  `coverage.out`, `dist/`, `bin/`, or `.tools/`.

## kubectl plugin conventions

- Help and usage text show the kubectl form (`kubectl can-schedule`), not just
  the binary name.
- Support common kubectl flags: `-h`/`--help`, `-n`/`--namespace`, `--context`,
  and `--kubeconfig`.
- Import client-go auth plugins with `_ "k8s.io/client-go/plugin/pkg/client/auth"`
  so cloud-provider kubeconfigs (GKE, EKS, AKS, OIDC) authenticate.
- The binary is named `kubectl-can_schedule` (underscore) so kubectl exposes it
  as `kubectl can-schedule`.

## Git, commits, and PRs

- Keep PRs focused on one logical change.
- Use descriptive commit messages, e.g. `Fix GPU resource false-positive` or
  `Consolidate workloads into one table`.
- Include an appropriate co-author trailer when committing from an AI-assisted
  session, using the identity of the agent that made the change:

  ```text
  Co-authored-by: <Agent Name> <agent-email@example.com>
  ```

- Before changing GitHub repo metadata, release text, or other user-facing
  descriptions, show the exact proposed text first.
- Prefer `gh` for GitHub operations.
- After pushing, check GitHub Actions and report whether CI passed.

## Release and distribution notes

- Tags are annotated semver tags (`vX.Y.Z`) and trigger the Release workflow.
- The Release workflow runs the tests, then GoReleaser publishes cross-platform
  archives and `checksums.txt`. The archive is named after the project and tag
  (`kubectl-can-schedule_<vTag>_<os>_<arch>`) with the `kubectl-can_schedule`
  binary inside. The archive tag matches the git tag verbatim (with the leading
  `v`) so `krew-release-bot` can resolve asset URLs.
- `install.sh` downloads the latest release archive and installs the binary onto
  the user's `PATH`.
- krew distribution is driven by the `.krew.yaml` template at the repo root.
  After GoReleaser publishes, the Release workflow runs `krew-release-bot`, which
  opens a PR to `krew-index` to bump the version. The very first submission to
  `krew-index` is manual; the bot only automates subsequent updates.

## krew best-practices checklist

Before updating the krew template (`.krew.yaml`), confirm the plugin follows the
krew developer best practices:

- Help and usage text show the kubectl form (`kubectl can-schedule`), not just
  the binary name.
- Support common kubectl flags: `-h`/`--help`, `-n`/`--namespace`, `--context`,
  and `--kubeconfig`.
- Import client-go auth plugins with `_ "k8s.io/client-go/plugin/pkg/client/auth"`
  so cloud-provider kubeconfigs work.
- Keep manifest `metadata.name` as the plugin name without the `kubectl-` prefix
  (`can-schedule`).
- Use `{{ .TagName }}` for the version and `addURIAndSha` for each platform so
  URLs point to immutable versioned artifacts and SHA256 sums are filled in
  automatically at release time.
- The `bin` must match the binary in the archive (`kubectl-can_schedule`, or
  `kubectl-can_schedule.exe` on Windows).
- Keep `caveats` accurate about required RBAC; this plugin needs read/list access
  to nodes, pods, priorityclasses, and storageclasses, and performs no writes.
- Render the template before tagging a release to confirm it parses, for example:
  `docker run -v "$PWD/.krew.yaml:/tmp/template-file.yaml"
  ghcr.io/rajatjindal/krew-release-bot:v0.0.51 krew-release-bot template
  --tag <tag> --template-file /tmp/template-file.yaml`.
