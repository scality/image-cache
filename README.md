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

The project is three pieces that meet on a directory:

| Component | What it does |
| --- | --- |
| [`containerd-image-preload`](rpm/) (RPM) | A systemd service and timer that import every tarball of the cache directory into containerd. Runs on boot and every 10 minutes. Needs no Kubernetes, no registry, no network. |
| [`image-cache-agent`](agent/) (DaemonSet) | A Kubernetes agent that fills the cache: it pulls the images declared by `ImageCache` custom resources, extracts the tarballs they carry into the cache directory, garbage-collects what is no longer declared, and reports per-node progress as node labels. |
| [`imagecachectl`](agent/cmd/imagecachectl/) (command, RPM) | The same fill, once, for a node that has no Kubernetes yet. Reads a registry or a docker archive and writes the tarballs where the agent would have. |

The contract between them is the filesystem, `/var/lib/image-cache` by
default: the agent or the command writes tarballs, the preload service imports
them. Each works without the others. A node provisioned with tarballs copied at
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
building for amd64 from another architecture, and the label the manifests leave
off the namespace they create. The flags, `--cache-path` and `--resync-period`
included, are under [Configuration](agent/README.md#configuration).

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
```

Every selected node is labelled with the progress of that resource, which
makes the cache state greppable and gateable:

```console
kubectl get nodes -l image-cache.scality.com/worker-1-0-0=synced
```

[agent/README.md](agent/README.md#declaring-a-cache) has the fields and what
deleting a resource removes. Two resources can select the same node, so an
upgrade can stage new content next to the old one and drop the old one once
every node is done.

### The import command

A node being installed has no Kubernetes to run the agent in, and its kubelet
needs images before it starts. `imagecachectl` does the agent's work once, from
a registry or from a docker archive, which is what a first node has instead of
a registry.

```console
dnf install imagecachectl-<version>-1.el9.x86_64.rpm
mkdir -p /var/lib/image-cache
imagecachectl import --name worker-1-0-0 registry.example.com/my-boot-cache-worker:1.0.0
imagecachectl import --name worker-1-0-0 /mnt/iso/images/my-boot-cache-worker.tar
```

Make the directory first: no package creates it, and on the node this runs on
there is no agent yet to create it either.

`--name` is the name of the `ImageCache` resource this content belongs to. It
is the directory the tarballs land in, and it is how the agent recognises the
resource later: give it the name the resource will carry and the agent adopts
what the command wrote instead of pulling the same image again. Until a
resource claims it, the agent leaves the directory alone, so it does not
matter whether the agent or the resources reach the node first.

The source is read as a path when it starts with a separator or a dot, or ends
in `.tar`, and as an image reference otherwise. What decides is the shape of
what you pass, never what happens to sit in the current directory, so an
archive named something else has to be given as `./that-name`.

A second run over a resource that is already complete writes nothing and
reaches no registry, so the command is safe to call on every convergence
rather than only at install. Don't use it to repair an incomplete resource on
a node where the agent runs: the agent repairs it, and two writers swapping
the same directory into place can make one of them fail.

`--cache-path` overrides the directory. Give it the same value as the agent's
`--cache-path`, or the agent never finds what the command wrote. Both follow
the same rule: absolute, and without `..`.
`--ca-file` names a PEM file of CA certificates to trust for a registry signed
by a private CA, on top of the system ones. It is checked on every run, and
never read for an archive. `--insecure-skip-tls-verify` accepts any
certificate instead, for a test cluster only, and prints a warning.

The import refuses to replace a directory without a sentinel. The command runs
as root, so a wrong name or cache path that lands on a directory the store did
not write stops rather than emptying it. Remove that directory by hand if you
meant it.

Before it adopts a directory, the agent checks that it holds the image the
resource asks for, by comparing layers. It reads no layer to do so. The same
image is taken over without a pull, another image is replaced, and an
unreachable source leaves the directory as it is. See
[agent/DESIGN.md](agent/DESIGN.md#adopting-a-seeded-directory). A directory
imported under a name no resource ever carries stays until you remove it.

### Building a cache image

A cache image is an ordinary container image whose layers carry the tarballs.
Nothing else makes it special, so whatever already builds your images builds
one. Export what you want cached, then package it:

```console
docker save registry.example.com/my-app:1.0.0 -o my-app.tar
docker save registry.example.com/my-other-app:2.1.0 -o my-other-app.tar
```

```dockerfile
FROM scratch
COPY *.tar /
```

```console
docker build --platform linux/amd64 -t registry.example.com/my-boot-cache-worker:1.0.0 .
docker push registry.example.com/my-boot-cache-worker:1.0.0
```

Four things the agent expects:

- **A `linux/amd64` image.** It resolves the reference for that platform and
  no other, which is also why the build above pins it. An archive read by
  `imagecachectl` is checked the same way, so an image saved on an arm64
  machine is refused instead of filling an x86_64 node's cache.
- **Unique file names.** Every file lands flat, under its base name, so two
  files called `app.tar`, in different directories or written by different
  layers, fail the extraction instead of overwriting each other.
- **`FROM scratch`, or something equally empty.** Every regular file of the
  image lands in the cache directory, so a conventional base image would pour
  its whole filesystem in there. A layer that deletes a file of an earlier one
  is refused.
- **Regular files only.** A symbolic link, a hard link or a device in the
  image refuses it whole, naming the entry. `docker build` copies a link as it
  finds it, so an archive linked into the build context from elsewhere would
  otherwise reach the node as a link to nothing, and the cache would look
  complete without it. Directories are fine.

The layout inside the image does not matter, since only base names survive.
Files that do not end in `.tar` are extracted too, but the preload service
ignores them.

## Repository layout

```
agent/   the Go module (kubebuilder): the agent, imagecachectl, the CRD and manifests
rpm/     the two packages: sources, specs, build and tests
```

Each component is built and tested independently, and both CI workflows run on
every pull request, since a required check that never runs leaves the pull
request waiting forever; on pushes to `main` they are scoped by path.

Releases are not independent yet: a tag cuts one version for the repository,
and the artifacts attached are the two RPMs, for both EL versions. Publishing
the agent image is still to come.

## Development

The agent needs Go 1.26+. The RPM tooling needs Docker, and Go for the
`imagecachectl` package.

```console
make -C agent test          # unit tests and the envtest suite
make -C agent lint          # golangci-lint (custom build, plugins included)
make -C agent test-e2e      # end-to-end tests on a kind cluster
make -C rpm test EL=9       # shellcheck, bats and rpmlint, in a Rocky 9 container
make -C rpm rpm EL=9        # build both RPMs into rpm/_build/
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow and the conventions.

## Getting help

Open an [issue](https://github.com/scality/image-cache/issues) for a bug, a
question, or a feature request. The project is maintained by Scality, and the
team listed in [CODEOWNERS](.github/CODEOWNERS) reviews every pull request.

## License

Apache 2.0, see [LICENSE](LICENSE).
