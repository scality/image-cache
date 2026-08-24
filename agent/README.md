# image-cache-agent

A DaemonSet that keeps the container image cache of each node in sync with the
`ImageCache` resources selecting it: it pulls the declared images, extracts the
tarballs they carry into the cache directory, garbage-collects what is no
longer declared, and reports per-node progress as node labels.

It is one half of [image-cache](../README.md); the other half, the
`containerd-image-preload` RPM, imports those tarballs into containerd. They
share a directory and nothing else. See [DESIGN.md](DESIGN.md) for this
module's model and [../DESIGN.md](../DESIGN.md) for the split between the two.

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
  cachePath: /var/lib/image-cache
```

- `source` is required: the image whose layers carry the `*.tar` exports to
  cache, as `registry[:port]/repository[:tag][@sha256:<digest>]`.
- `nodeSelector` matches node labels exactly, like a pod's own selector. Empty
  selects every node.
- `cachePath` defaults to `/var/lib/image-cache`. The agent extracts into
  `<cachePath>/<name>/` and deletes only what it owns: a directory carrying its
  sentinel, or one of its own interrupted extractions. It garbage-collects the
  default path on every pass even when no resource points at it, and it forgets
  a non-default path when the process restarts, so a resource deleted during a
  restart leaves its directory behind.

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
The one that changes behaviour is `--resync-period`, one hour by default: it
bounds how long a drift that raised no event at all can last. Resource changes
and tampering with the cache directory each trigger a pass of their own, and a
failed pass is retried with backoff, so the periodic pass is a safety net
rather than the main loop. Zero turns it off and leaves the agent purely event
driven, which is only safe while the filesystem watcher registers. When it
cannot, on a node that has hit its inotify limit or a cache path whose mount is
missing, the agent says so in its logs and leans on the periodic pass. With
zero there is no pass to lean on.

Leader election is deliberately absent. Every agent converges the node it runs
on, so there is nothing to elect a leader for.

## Development

```console
make test          # unit tests and the envtest suite
make test-e2e      # end-to-end tests on a kind cluster
make lint          # golangci-lint, custom build with the logcheck plugin
make run           # run the controller against your current kubeconfig
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
