[![agent-ci](https://github.com/scality/image-cache/actions/workflows/agent-ci.yaml/badge.svg)](https://github.com/scality/image-cache/actions/workflows/agent-ci.yaml)
[![rpm-ci](https://github.com/scality/image-cache/actions/workflows/rpm-ci.yaml/badge.svg)](https://github.com/scality/image-cache/actions/workflows/rpm-ci.yaml)
[![Go version](https://img.shields.io/github/go-mod/go-version/scality/image-cache?filename=agent%2Fgo.mod)](agent/go.mod)
[![License](https://img.shields.io/github/license/scality/image-cache)](LICENSE)

# image-cache

A local cache of container images for Kubernetes nodes, and the machinery to
keep it filled and to restore it into containerd.

A node that loses its images cannot pull them back if the registry itself runs
on that cluster. It happens on a pruned image store, a corrupted disk, a
reinstall, or a cluster brought up with no registry reachable. image-cache
breaks the circular dependency: the images live on the node as plain tarballs,
and a systemd service imports them into containerd without asking anything of
Kubernetes.

The project is two independent halves that meet on a directory:

| Component | What it does |
| --- | --- |
| [`containerd-image-preload`](rpm/) (RPM) | A systemd service and timer that import every tarball of the cache directory into containerd. Runs on boot and every 10 minutes. Needs no Kubernetes, no registry, no network. |
| [`image-cache-agent`](agent/) (DaemonSet) | A Kubernetes agent that fills the cache: it pulls the images declared by `ImageCache` custom resources, extracts the tarballs they carry into the cache directory, garbage-collects what is no longer declared, and reports per-node progress as node labels. |

The contract between them is the filesystem, `/var/lib/image-cache` by
default: the agent writes tarballs, the preload service imports them. Either
half works without the other. A node provisioned with tarballs copied at
install time gets them imported with no agent running, and the agent keeps a
cache up to date on a cluster that imports it some other way.

## How it works

```
  ImageCache CR                  registry
       │                            │
       │  desired state             │  cache image
       ▼                            ▼
  ┌──────────────────────────────────────┐
  │  image-cache-agent (DaemonSet)       │   pulls, extracts, garbage-collects
  └──────────────────┬───────────────────┘
                     │ writes *.tar
                     ▼
        /var/lib/image-cache/<name>/
                     │ reads *.tar
                     ▼
  ┌──────────────────────────────────────┐
  │  containerd-image-preload (systemd)  │   ctr images import, every 10 min
  └──────────────────┬───────────────────┘
                     ▼
                 containerd
```

The images the agent pulls are ordinary container images whose layers carry
the tarballs to cache. Publishing new cache content means pushing a new image
and pointing a new `ImageCache` at it. The registry is the transport, and
nothing in the cluster has to be templated or reconfigured.

For the design and the reasoning behind it, see [DESIGN.md](DESIGN.md) and,
for the agent specifically, [agent/DESIGN.md](agent/DESIGN.md).

## Getting started

### The preload service

Install the RPM (Enterprise Linux 8 and 9 are supported), then enable the
timer:

```console
dnf install containerd-image-preload-<version>-1.el9.noarch.rpm
systemctl enable --now containerd-image-preload.timer
```

The package does not create the cache directory, so make it first. Then drop
any `*.tar` image export into it and it is imported into containerd's `k8s.io`
namespace on the next tick, or right away with:

```console
mkdir -p /var/lib/image-cache
systemctl start containerd-image-preload.service
```

Both the directory and the platform are configurable in
`/etc/sysconfig/containerd-image-preload`:

```sh
IMAGE_CACHE_DIR=/var/lib/image-cache
IMAGE_PLATFORM=linux/amd64
```

Releases carry the RPM for both EL versions as assets, see
[Releases](https://github.com/scality/image-cache/releases).
[rpm/README.md](rpm/README.md) has the rest: what the glob covers, what
happens when an import fails, and how the package is built.

### The agent

Build the image, then deploy: `deploy` applies the CRD along with the rest. No
image is published yet, so the first step is yours.

```console
make -C agent docker-build docker-push IMG=<your-registry>/image-cache-agent:<tag>
make -C agent deploy IMG=<your-registry>/image-cache-agent:<tag>
```

Those two commands are enough from an amd64 machine, deploying into a
namespace that enforces the `privileged` Pod Security Standard. Anything else
takes a step or two, and [agent/README.md](agent/README.md#deploying) has them:
building for amd64 from another architecture, the label the manifests leave off
the namespace they create, and the flags, `--resync-period` included.

Then declare what each node should cache:

```yaml
apiVersion: image-cache.scality.com/v1alpha1
kind: ImageCache
metadata:
  name: worker-1-0-0
spec:
  nodeSelector:
    kubernetes.io/os: linux
  source: registry.example.com/my-boot-cache-worker:1.0.0
  cachePath: /var/lib/image-cache
```

Every selected node is labelled with the progress of that resource, which
makes the cache state greppable and gateable:

```console
kubectl get nodes -l image-cache.scality.com/worker-1-0-0=synced
```

Deleting the resource removes its tarballs and its labels while the agent is
running; [agent/README.md](agent/README.md#declaring-a-cache) has the case where
a directory outlives it. Two resources can select the same node, so an upgrade
can stage new content next to the old one and drop the old one once every node
is done.

## Repository layout

```
agent/   the image-cache-agent Go module (kubebuilder), its CRD and manifests
rpm/     the containerd-image-preload package: sources, spec, build and tests
```

Each component is built and tested independently, and both CI workflows run on
every pull request, since a required check that never runs leaves the pull
request waiting forever; on pushes to `main` they are scoped by path.

Releases are not independent yet: a tag cuts one version for the repository,
and the only artifact attached is the RPM for both EL versions. Publishing the
agent image is still to come.

## Development

The agent needs Go 1.26+; the RPM tooling only needs Docker.

```console
make -C agent test          # unit tests and the envtest suite
make -C agent lint          # golangci-lint (custom build, plugins included)
make -C agent test-e2e      # end-to-end tests on a kind cluster
make -C rpm test EL=9       # shellcheck, bats and rpmlint, in a Rocky 9 container
make -C rpm rpm EL=9        # build the RPM into rpm/_build/
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow and the conventions.

## Getting help

Open an [issue](https://github.com/scality/image-cache/issues) for a bug, a
question, or a feature request. The project is maintained by Scality, and the
team listed in [CODEOWNERS](.github/CODEOWNERS) reviews every pull request.

## License

Apache 2.0, see [LICENSE](LICENSE).
