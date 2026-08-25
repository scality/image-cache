# Contributing

Thanks for contributing to image-cache. This guide covers the workflow and the
conventions the repository follows. For what the project does see the
[README](README.md); for how it is built and why see [DESIGN.md](DESIGN.md)
and [agent/DESIGN.md](agent/DESIGN.md).

Be respectful and assume good faith in issues, reviews, and discussions.
Technical disagreement is welcome; personal attacks, harassment, and
dismissive behaviour are not, and maintainers will moderate accordingly.

## Repository layout

The repository holds two independent components, each with its own toolchain:

- [`agent/`](agent): the `image-cache-agent` Go module (kubebuilder), its
  `ImageCache` CRD and its deployment manifests.
- [`rpm/`](rpm): the `containerd-image-preload` package, with shell sources,
  systemd units, spec file, build script and tests. Its
  [README](rpm/README.md) covers the toolchain.

Keep a change inside one component when you can. Reviewers read the repository
that way, and the toolchains have nothing in common. Releases do not follow
that split yet: a tag cuts one version for the whole repository, and only the
RPM is attached to it.

## Development environment

The agent needs Go 1.26+. controller-gen, kustomize, envtest and golangci-lint
are downloaded into `agent/bin/` at pinned versions by the `Makefile`, so those
need no manual install. `kubectl` and `kind` do: the deploy and e2e targets
expect them on your `PATH`.

One version escapes the pinning. The `logcheck` plugin of the custom
golangci-lint build resolves to `latest` in `agent/.custom-gcl.yml`, so a new
`sigs.k8s.io/logtools` release can turn an unchanged tree red.

The RPM side needs only Docker: builds and tests run inside a Rocky Linux
container, pinned by digest, so the result does not depend on your host.

```console
make -C agent test          # unit tests and the envtest suite
make -C agent lint          # golangci-lint (custom build with the logcheck plugin)
make -C agent test-e2e      # end-to-end tests on a kind cluster
make -C rpm test EL=9       # shellcheck, bats and rpmlint (EL=8 or EL=9)
make -C rpm rpm EL=9        # build the RPM into rpm/_build/
```

`make -C agent help` lists the rest.

## Coding conventions

### Agent (Go)

- **Generated code is committed.** After touching the API types or the RBAC
  markers, run `make -C agent manifests generate` and commit the result. CI
  regenerates it before running the tests but does not fail on an uncommitted
  diff, so a stale CRD reaches `main` unnoticed if you skip this.
- **Logging**: `logr` through controller-runtime, with the `logcheck` linter
  enforcing balanced key-value pairs. Log a value once, at the level that owns
  it: the reconciler logs outcomes, the packages it calls return errors.
- **Errors**: `github.com/scality/go-errors`, wrapped with the sentinel of the
  package that owns the failure (`ErrNode`, `ErrSync`, `ErrExtract`, and so
  on), plus `errors.CausedBy` for the underlying error and
  `errors.WithProperty` for what locates it, such as the resource name or the
  cache path. Stamp the sentinel where a foreign error enters the agent:
  wrapping a non-`*errors.Error` cause first retitles it "unknown error" and
  loses the classification `errors.Is` relies on. The reconciler aggregates
  per-resource failures with `utilerrors.NewAggregate`, so one bad resource
  does not stall the others.
- **Tests**: Go's standard `testing` for the packages, Ginkgo + Gomega with
  envtest for the reconciler, and a kind-based smoke test under `test/e2e`.
  A behaviour that can fail on a node deserves a test that reproduces it, not
  only a unit test of the helper it calls: a partial extraction, a missing
  mount, a manual deletion.
- **Lint**: `make -C agent lint` must pass. It builds a custom golangci-lint
  binary; if a finding looks stale, clear the analysis cache
  (`agent/bin/golangci-lint cache clean`) before trusting a green run.

### RPM (shell, spec)

- The script stays POSIX-friendly bash with `set -euo pipefail`, and must pass
  `shellcheck`; the spec must pass `rpmlint` (both are part of
  `make -C rpm test`).
- Anything configurable goes to `/etc/sysconfig/containerd-image-preload` as
  `%config(noreplace)`, never hardcoded in the unit files.
- New behaviour comes with a bats test in `rpm/test/`, exercised on both
  EL 8 and EL 9.

## Commits and pull requests

- Conventional-commit subjects scoped by component, e.g. `feat(agent): …`,
  `fix(rpm): …`, `ci: …`, `docs: …`. Write the subject in the imperative.
- Keep each commit a single coherent change, and keep the branch rebased on
  `main`. A pull request is read commit by commit, so a commit that only fixes
  an earlier commit of the same branch should be squashed into it.
- Explain *why* in the body when the change is not obvious from the diff;
  reference an issue with a trailing `Refs: #<issue>` or `Fixes: #<issue>`.
- Run the relevant `make` targets before opening the pull request. Both CI
  workflows run on every pull request, and the code owners listed in
  [.github/CODEOWNERS](.github/CODEOWNERS) are requested for review.
- Fill in the pull request template. Its sections are what a reviewer needs to
  read the change: which half it touches, the problem, the decisions, what you
  ran, and what it leaves alone.
- Reviewers work from [.claude/REVIEW.md](.claude/REVIEW.md), which spells out
  what this repository cares about in a review. Reading it before you open the
  pull request saves a round trip.

## Keep the docs in sync

Treat the docs as part of the change, not an afterthought. In the same pull
request:

- a change to behaviour, flags, or output → update the [README](README.md), and
  [agent/README.md](agent/README.md) when it is the agent's;
- a change to the model or a design decision → update [DESIGN.md](DESIGN.md)
  or [agent/DESIGN.md](agent/DESIGN.md), whichever owns it;
- a change to conventions or workflow → update this file.

## Reporting issues

Open an [issue](https://github.com/scality/image-cache/issues) with the
component involved, the version (RPM version, agent image tag), what you
expected, what happened, and how to reproduce it. For the agent, the output of
`kubectl describe imagecache <name>` and the agent logs from the affected node
are usually what a maintainer needs first.

Report a suspected vulnerability privately rather than in a public issue,
through GitHub's security advisories or to Scality's security response team at
`secalert@scality.com`.

## License

By contributing you agree your contribution is licensed under the repository's
[LICENSE](LICENSE) (Apache 2.0).
