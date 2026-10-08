# image-cache-agent

A DaemonSet that keeps the container image cache of each node in sync with the
`ImageCache` resources selecting it: it pulls the declared images, extracts the
tarballs they carry into the cache directory, garbage-collects what is no
longer declared, and reports per-node progress as node labels.

It is one of the three pieces of [image-cache](../README.md). The
`containerd-image-preload` RPM imports those tarballs into containerd, and
`imagecachectl`, whose source lives in this module under
[cmd/imagecachectl](cmd/imagecachectl), does one resource's worth of the fill
from a command, for a node with no Kubernetes on it yet. They share a
directory and nothing else. See [DESIGN.md](DESIGN.md) for this module's
model and [../DESIGN.md](../DESIGN.md) for the split between them.

All the commands below run from this directory.

## Prerequisites

- Go 1.26+ for building and testing. controller-gen, kustomize, envtest and
  golangci-lint are downloaded into `bin/` at pinned versions by the
  `Makefile`.
- `kubectl`, and `kind` for the e2e suite. Neither is downloaded: the targets
  expect them on your `PATH`.
- Docker, or another `CONTAINER_TOOL`, for the image.
- A cluster on Kubernetes 1.25+, which is where CEL validation of custom
  resources landed. The CRD relies on it.

## Deploying

Build and push the image, then deploy. `deploy` builds `config/default`, which
includes the CRD, so it covers what `install` does on its own.

```console
make docker-build docker-push IMG=<your-registry>/image-cache-agent:<tag>
make deploy IMG=<your-registry>/image-cache-agent:<tag>
```

`docker-build` runs a plain `docker build`, so the image takes the architecture
of the machine that builds it while the DaemonSet only schedules onto amd64.
From anything else, use `make docker-buildx IMG=<...>`: it pins `PLATFORMS`,
`linux/amd64` by default, and pushes in the same step.

`make deploy` writes the image reference into `config/manager/kustomization.yaml`,
a tracked file. Expect a dirty worktree afterwards, and do not commit your own
registry into it.

`make build-installer IMG=...` renders the same manifests into
`dist/install.yaml` instead, for a cluster you deploy to with `kubectl apply`
rather than with this checkout.

The manifests under [`config/`](config) are a working example rather than a
product. The agent mounts `/var/lib/image-cache` from the host and an init
container hands that directory to the agent's UID, because `hostPath` ignores
`fsGroup`, so the namespace has to enforce the `privileged` Pod Security
Standard. The namespace these manifests create does not carry the label: add
`pod-security.kubernetes.io/enforce=privileged` to it, or deploy into a
namespace that already has it. `test/e2e` does the former and is the shortest
working reference. The DaemonSet also pins itself to
`kubernetes.io/arch: amd64`, the architecture the image is meant to target.

`make undeploy` deletes everything `deploy` created, the CRD included, and
Kubernetes garbage-collects every `ImageCache` object with it. There is no
DaemonSet-only target: to swap the image, run `deploy` again. `make uninstall`
removes the CRD alone, with the same consequence for the objects.

## Declaring a cache

`ImageCache` is cluster-scoped:

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

- `source` is required: the image whose layers carry the `*.tar` exports to
  cache, as `registry[:port]/repository[:tag][@sha256:<digest>]`.
- `nodeSelector` matches node labels exactly, like a pod's own selector. Empty
  selects every node.

The agent extracts each resource into `<cache path>/<name>/`, the cache path
being its `--cache-path` flag (see [Configuration](#configuration)). It deletes
only what it owns: a directory carrying its sentinel, or one of its own
interrupted extractions.

The name ends up in a node label, so it is capped at 63 characters and
`generateName` is a bad idea. Watch progress on the label:

```console
kubectl get nodes -l image-cache.scality.com/worker-1-0-0=synced
```

The samples in [`config/samples/`](config/samples) point at
`registry.example.com`, so they parse but do not pull. Edit the source before
applying them.

## Configuration

The agent reads its node name from `NODE_NAME`, filled from the downward API
in the manifests, and exits at startup without it. `--help` lists the flags.

`--cache-path` is the host directory the agent fills, `/var/lib/image-cache` by
default. It must be absolute and must not contain `..`, or the agent exits at
startup. One agent writes one cache path. The DaemonSet mounts the default
one: change the mount with the flag.

`--resync-period` is one hour by default: it bounds how long a drift that
raised no event at all can last. Resource changes and tampering with the cache
directory each trigger a pass of their own, and a failed pass is retried with
backoff, so the periodic pass is a safety net rather than the main loop. Zero
turns it off and leaves the agent purely event driven, which is only safe while
the filesystem watcher registers. When it cannot, on a node that has hit its
inotify limit or a cache path whose mount is missing, the agent says so in its
logs and leans on the periodic pass. With zero there is no pass to lean on.

A registry signed by a private CA needs `--ca-file`, a PEM file of CA
certificates the agent trusts on top of the system ones. Uncomment the
`[REGISTRY-CA]` patch in
[`config/default/kustomization.yaml`](config/default/kustomization.yaml) and
create a `registry-ca` ConfigMap holding the CA under `ca.crt` in the agent's
namespace. The file is read once at startup, and an unusable one stops the
agent there, so restart the DaemonSet after rotating the CA. Without it the
pull fails on `x509: certificate signed by unknown authority`.

On a test cluster set up by hand, `--insecure-skip-tls-verify` accepts any
registry certificate instead. The agent logs a warning at startup. It excludes
`--ca-file`, and no manifest here sets it: never use it on a real node.

Leader election is deliberately absent. Every agent converges the node it runs
on, so there is nothing to elect a leader for.

## Development

```console
make test          # unit tests and the envtest suite, imagecachectl included
make test-e2e      # end-to-end tests on a kind cluster
make lint          # golangci-lint, custom build with the logcheck plugin
make run           # run the controller against your current kubeconfig
make build-imagecachectl OUT=bin/imagecachectl   # the command
make help          # everything else
```

`make run` needs `NODE_NAME` set to a node of the cluster and a writable cache
path, since it reconciles that node for real.

The API types and the RBAC markers drive generated code: run
`make manifests generate` after touching them, and commit the result.

See [../CONTRIBUTING.md](../CONTRIBUTING.md) for the conventions and the pull
request workflow.

## License

Apache 2.0, see [LICENSE](../LICENSE).
