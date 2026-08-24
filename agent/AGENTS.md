# image-cache-agent: notes for coding agents

Read [DESIGN.md](DESIGN.md) before changing anything here. It carries the model
this module implements, and most review comments trace back to it. The project
level split between this agent and the preload RPM is in
[../DESIGN.md](../DESIGN.md), the workflow and conventions in
[../CONTRIBUTING.md](../CONTRIBUTING.md), and what a review looks for in
[../.claude/REVIEW.md](../.claude/REVIEW.md).

## What this module is

A DaemonSet reconciling the node it runs on. `ImageCache` resources declare
what a set of nodes should cache; the agent pulls those images, extracts the
tarballs they carry under `<cachePath>/<name>/`, removes what is no longer
declared, and writes the per-node result as labels on the Node object.

The kubebuilder scaffold is real: `PROJECT` tracks group `image-cache`, domain
`scality.com`, kind `ImageCache`, version `v1alpha1`, and the resource is
cluster-scoped.

## Layout

```
api/v1alpha1/        the ImageCache types and the generated deepcopy
cmd/main.go          flags, manager setup, what gets registered
internal/controller/ the node reconciler, the filesystem watcher, the labels
internal/cache/      the on-disk store: state, extraction, garbage collection
internal/puller/     pulling an image and streaming its layers
config/              CRD, RBAC and DaemonSet manifests (kustomize)
test/e2e/            kind-based smoke test, builds and deploys for real
```

## Generated files

Never hand-edit these; regenerate them instead:

- `api/v1alpha1/zz_generated.deepcopy.go`
- `config/crd/bases/image-cache.scality.com_imagecaches.yaml`
- `config/rbac/role.yaml`
- `PROJECT`, written by the kubebuilder CLI and read by it for later
  scaffolding

They come from the markers in the Go sources, so change the marker, run
`make manifests generate`, and commit the result. Nothing in CI catches a stale
one; [../CONTRIBUTING.md](../CONTRIBUTING.md#agent-go) says why.

Leave the `+kubebuilder:scaffold:*` comments alone. They are insertion points
for the kubebuilder CLI, in `cmd/main.go`, `internal/controller/suite_test.go`
and several kustomizations.

## Verifying a change

```console
make fmt vet     # both run as prerequisites of test, listed here for clarity
make test        # unit tests plus the envtest suite
make lint        # custom golangci-lint build, logcheck included
make test-e2e    # needs kind and an idle cluster name, see below
```

`make test-e2e` creates the cluster `image-cache-agent-test-e2e`, builds the
image, loads it and deploys the manifests. It needs `kind` and `kubectl` on
your `PATH`; neither is downloaded by the `Makefile`.

A green `make lint` right after a dependency bump is not proof. Run
`bin/golangci-lint cache clean` first, for the reason
[../CONTRIBUTING.md](../CONTRIBUTING.md#agent-go) gives.

`make run` runs the manager against your current kubeconfig. It needs
`NODE_NAME` pointing at a real node and a writable cache path, because it
converges that node for real.

## Conventions this module actually follows

- **Errors**: `github.com/scality/go-errors`. Wrap with the owning package's
  sentinel, add `errors.CausedBy` for the cause and `errors.WithProperty` for
  what locates it. Stamp the sentinel where a foreign error enters, since
  wrapping a cause first retitles it "unknown error". Per-resource failures are
  aggregated with `utilerrors.NewAggregate`, which keeps `errors.Is` working.
- **Logging**: `logr` through controller-runtime. `logcheck` verifies the shape
  of the calls, key-value pairs balanced, not the wording, so follow
  Kubernetes' own convention for messages: a capital first letter, no trailing
  period, the object type named. The reconciler logs outcomes, the packages it
  calls return errors.
- **Node labels**: patched, never updated wholesale. Several agents write the
  same Node object at once, and everything they did not touch has to survive.
- **Vendor neutrality**: this code knows about Kubernetes and containerd, never
  about a distribution or product that consumes it. No such name in code,
  tests, manifests, comments or docs. The one exception is
  `.github/CODEOWNERS`, which has to name a GitHub team that actually exists.

## What this project deliberately does not have

Do not scaffold them back in:

- **No webhooks.** Validation is CEL on the CRD. Adding a webhook would mean
  certificates, a Service and an ordering problem at bootstrap.
- **No status subresource.** The per-node result lives in Node labels, which is
  what makes it greppable and gateable from outside the cluster.
- **No leader election.** `cmd/main.go` never enables it, so nothing is gated
  today. A runnable still declares `NeedLeaderElection() bool { return false }`,
  because controller-runtime defaults to `true` and would gate it the day
  someone turns leader election on. Every agent converges its own node, so
  there is no leader to elect.
- **No helm chart.** `make build-installer` renders the manifests into
  `dist/install.yaml`, which is gitignored and attached to nothing: today the
  way to deploy is this checkout's `make deploy`. Publishing an artifact is a
  separate piece of work, not a missing chart.

## Traps worth knowing

- A `manager.Runnable` whose `Start` returns `nil` is treated as finished
  normally. Returning `nil` when the component died on its own leaves an agent
  that looks healthy and silently stopped watching.
- A path from a resource is compared and used as a map key, so normalize it
  once with `filepath.Clean` at the entry point. Two spellings of the same
  directory made the garbage collector delete what the same pass extracted.
- The cache is shared with whatever else writes into it. The agent only
  deletes a directory carrying its own sentinel, or one of its interrupted
  extractions. Keep it that way.
