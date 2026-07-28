# Review criteria

Read by the `/review-pr` skill (Scality agent hub) and by anyone reviewing by hand.
Flag problems only — see "What not to flag" at the end.

## What this repo is

`image-cache` keeps a local, self-healing cache of container images on a Kubernetes
node, so the node can boot and recover without reaching a registry. Two halves that
never call each other:

- **`agent/`** — Go controller (kubebuilder, controller-runtime), shipped as a
  DaemonSet. Watches `ImageCache` custom resources, pulls the listed images and
  extracts them as tar archives into the node cache directory. Reports per-node
  progress through Node labels. Spec: `agent/DESIGN.md`.
- **`rpm/`** — `containerd-image-preload` package: a shell script plus a systemd
  service and timer that (re)import the cached archives into containerd. Runs with
  no Kubernetes API, no network. Tests are bats.
- **`.github/workflows/`** — thin callers of the reusable workflows in
  `scality/workflows`.

The two halves meet on **one contract: the cache directory layout** (per-resource
subdirectory, archive names, completion sentinel). The agent writes, the package
imports.

This code is generic and open-source: it knows about Kubernetes and containerd,
never about a specific downstream distribution or product.

## Criteria

| Area | What to check |
| ---- | ------------- |
| Cross-half contract | A change to the cache layout — subdirectory scheme, archive naming, sentinel, permissions — must land in both halves and in `agent/DESIGN.md` in the same PR. One half moving alone is a silent break. |
| Vendor neutrality | No product or distribution name from a consumer of this tool, anywhere: code, tests, manifests, comments, docs. Consumer-specific values belong in configuration (flags, env, sysconfig), not in defaults hardcoded in logic. |
| Archive extraction safety | Entries are attacker-controlled (any image can be listed in a CR). Check: extraction by base name only, no traversal, regular files only, symlinks and devices rejected, the agent's own sentinel name never taken from the archive, and no `io.ReadAll` of a layer — extraction streams. |
| Crash safety and idempotency | A killed agent must never leave a half-written cache that looks complete: sentinel written last, temporary paths renamed into place, re-running a sync is a no-op when the content already matches. Same for the import script: partial cache means skip, not fail. |
| Reconciler behaviour | Idempotent, no API write when nothing changed, `IgnoreNotFound` on deleted resources, errors returned (not swallowed into a bare requeue), requeue delays bounded, watch predicates narrow enough to avoid hot loops. State lives in Node labels — there is no status subresource. |
| Node label writes | Labels are patched, never updated wholesale — several agents write the same Node object concurrently. Keys stay under the project's label domain and values stay valid Kubernetes label values. |
| RBAC least privilege | kubebuilder markers must match the verbs the code actually uses, nothing wider; `config/rbac` regenerated and committed. Events go through the `events.k8s.io` API. |
| CRD compatibility | `v1alpha1` is served: schema changes must stay additive (no field removal, no new required field, no narrowed validation) unless the PR says how existing objects migrate. `make manifests generate` output committed; samples updated. |
| DaemonSet manifest | Resource requests and limits justified by a measured footprint, not guessed; security context minimal and matched to the distroless user; host mounts scoped to the cache directory; tolerations wide enough to cover every node the cache is meant for. |
| Go error handling | `fmt.Errorf("...: %w", err)` for wrapping, no dropped errors, no error text that duplicates the caller's context. |
| Context and cancellation | Pulls and extractions are long: `context.Context` threaded through, cancellation honoured mid-transfer, no goroutine without an exit path, no blocking call in a reconcile loop that can outlive its context. |
| Registry client | Platform explicitly pinned, digests verified, retries and auth failures distinguished from permanent errors, layers streamed rather than buffered. |
| Shell half | `set -euo pipefail`, every expansion quoted, `ctr` called with the right containerd namespace and address, import idempotent (re-import of a present image is not an error), exit status reflecting real failure only. |
| Packaging | Spec version and changelog consistent with the tag, `%config(noreplace)` on the sysconfig, systemd unit ordering and dependencies correct (after containerd, before anything needing the images), file paths unchanged unless the PR says so. |
| EL8 and EL9 | Both are supported targets: no bash, systemd or `ctr` feature that exists on only one of them. |
| Tests | Every new logic path has a test. Extraction and path-safety changes need a hostile-input case. Script changes need bats cases. Behaviour visible through the API needs an e2e case, and e2e cleanup must run even when the test fails. |
| CI workflows | Reusable callers pinned by commit digest with the version in a trailing comment. Secrets are **not** inherited by a `uses:` job — every secret the shared workflow needs must be listed under `secrets:`. `permissions:` least-privilege. A new job must also be wired into whatever gates the merge. |
| Docs sync | A change to behaviour, flags, paths, labels or the CRD is reflected in `agent/DESIGN.md` and, when user-facing, in `README.md`. |
| Security | No secret or token in code, tests or logs. No value taken from a custom resource interpolated into a shell command. Image references validated before use. |
| Breaking changes | Call these out explicitly: CRD schema, label keys, cache path layout, installed file paths, unit names, agent flags and environment variables. Consumers pin to them. |

## What not to flag

- Anything the linters already own: `golangci-lint` for the Go half (`agent/.golangci.yml`
  — errcheck, staticcheck, revive, depguard, lll, logcheck…), `shellcheck` for the shell
  half, `rpmlint` for the spec, `gofmt`/`goimports` for formatting.
- Generated files (`zz_generated.*`, CRD manifests) except when they are stale with
  respect to the sources in the same PR.
- Markdown or comment wording preferences.
- Refactors unrelated to the PR's purpose.
