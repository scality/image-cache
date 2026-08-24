# Review criteria

Read by the `/review-pr` skill, which the Code Review workflow runs on every pull
request, and by anyone reviewing by hand.
Flag problems only, and report them the way "Reporting a finding" describes at the
end. "What not to flag" closes the file.

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
never about a specific downstream distribution or product. Its docs are read by
people outside the team, so they are part of what ships.

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
| Go error handling | `github.com/scality/go-errors`, wrapped with the owning package's sentinel plus `errors.CausedBy` and `errors.WithProperty`; the sentinel is stamped where a foreign error enters. No dropped errors, no error text that duplicates the caller's context. |
| Context and cancellation | Pulls and extractions are long: `context.Context` threaded through, cancellation honoured mid-transfer, no goroutine without an exit path, no blocking call in a reconcile loop that can outlive its context. |
| Registry client | Platform explicitly pinned, digests verified, retries and auth failures distinguished from permanent errors, layers streamed rather than buffered. |
| Shell half | `set -euo pipefail`, every expansion quoted, `ctr` called with the right containerd namespace and address, import idempotent (re-import of a present image is not an error), exit status reflecting real failure only. |
| Packaging | Spec version and changelog consistent with the tag, `%config(noreplace)` on the sysconfig, systemd unit ordering and dependencies correct (after containerd, before anything needing the images), file paths unchanged unless the PR says so. |
| EL8 and EL9 | Both are supported targets: no bash, systemd or `ctr` feature that exists on only one of them. |
| Tests | Every new logic path has a test. Extraction and path-safety changes need a hostile-input case. Script changes need bats cases. Behaviour visible through the API needs an e2e case, and e2e cleanup must run even when the test fails. |
| CI workflows | Reusable callers pinned by commit digest with the version in a trailing comment. Secrets are **not** inherited by a `uses:` job — every secret the shared workflow needs must be listed under `secrets:`. `permissions:` least-privilege. A new job must also be wired into whatever gates the merge. |
| Docs sync | A change to behaviour, flags, paths, labels or the CRD is reflected in `agent/DESIGN.md` and, when user-facing, in `README.md` and `agent/README.md`. |
| Doc claims | Every command, path, filename, flag, default and version a doc states is checked against what implements it: the `Makefile` target, the spec file, the manifest, the flag definition. A finding here names that source. A wrong command costs a reader more than a wrong comment does. |
| Documented commands | A command the docs tell a reader to run says what it destroys. `make undeploy` deletes the CRD and every `ImageCache` object with it; a page presenting it as removing the DaemonSet is a defect, not a wording preference. |
| Doc set consistency | The docs are read as a set: `README.md`, `agent/README.md`, `DESIGN.md`, `agent/DESIGN.md`, `CONTRIBUTING.md`, `agent/AGENTS.md`, this file. The same fact stated twice in different words is a defect, and so is a rule one file sets and another breaks. Prefer a link to a second copy. |
| Hazard written down instead of fixed | A pull request that documents a footgun a small change would remove (an unpinned version, a missing CI gate, a manual step nothing enforces) gets that said once. Prefer the fix. A deliberate deferral belongs in the commit message, not only in the doc. |
| Security | No secret or token in code, tests or logs. No value taken from a custom resource interpolated into a shell command. Image references validated before use. |
| Breaking changes | Call these out explicitly: CRD schema, label keys, cache path layout, installed file paths, unit names, agent flags and environment variables. Consumers pin to them. |

## Reporting a finding

- One finding per defect. Several findings sharing a root cause are one finding,
  reported where the cause lives.
- Name the file and line that proves it, in the implementation rather than in the
  diff under review. "The Makefile never installs it" is a claim; the line where
  `KIND ?= kind` is declared is proof.
- State the failure concretely: what someone does, and what they get instead. A
  finding nobody can reproduce from its own text is not actionable.
- Rank by what it costs whoever hits it, not by how surprising it is.
- Say so when a finding rests on something you could not check.

## What not to flag

- Anything the linters already own: `golangci-lint` for the Go half (`agent/.golangci.yml`
  — errcheck, staticcheck, revive, depguard, lll, logcheck…), `shellcheck` for the shell
  half, `rpmlint` for the spec, `gofmt`/`goimports` for formatting.
- Generated files (`zz_generated.*`, CRD manifests) except when they are stale with
  respect to the sources in the same PR.
- Markdown or comment wording preferences.
- Refactors unrelated to the PR's purpose.
- A trade-off the commit message states and argues for, unless the argument is
  wrong on the facts.
- A hazard the pull request already documents and defers, beyond the single
  finding that says so.
- Missing documentation for something the repository does not have yet.
