# image-cache-agent design

The image-cache-agent is a Kubernetes DaemonSet that keeps a local, per-node
cache of container image tarballs in sync with the desired state declared by
`ImageCache` custom resources. The cache lets nodes restore critical images
into containerd (via the `containerd-image-preload` service shipped in this
repository) without depending on a registry or on Kubernetes being healthy.

The agent pulls cache images from an OCI registry and extracts their content
to a directory on the host. It never talks to containerd: importing the
tarballs is the preload service's job.

## ImageCache CRD

Cluster-scoped, no status subresource.

```yaml
apiVersion: image-cache.scality.com/v1alpha1
kind: ImageCache
metadata:
  name: worker-134-0-0
spec:
  # Exact key/value match against node labels, same semantics as a pod's
  # .spec.nodeSelector. Empty or absent selects every node.
  nodeSelector:
    kubernetes.io/os: linux
  # Required. The image whose layers contain the tarballs to cache.
  source: registry.example.com/boot-cache-worker:134.0.0
  # Optional. Defaults to /var/lib/image-cache, matching the
  # containerd-image-preload default.
  cachePath: /var/lib/image-cache
```

Validations:

- `source` is required and non-empty.
- `cachePath` must be an absolute path.
- `metadata.name` is capped at 63 characters: the name becomes a node label
  name (see below), and Kubernetes label names cannot exceed 63 characters.

Several ImageCache resources can select the same node: during an upgrade the
old and new versions coexist until the old resource is deleted.

Per-node sync status is intentionally not stored on the resource. A single
cluster-scoped object cannot represent divergent per-node states; node labels
can (Node Problem Detector pattern).

## Node labels contract

For every ImageCache selecting a node, the agent maintains a label on that
node:

```yaml
labels:
  image-cache.scality.com/worker-134-0-0: synced   # or: pending
```

- `pending`: the cache content for this resource is not (yet) on disk.
- `synced`: the tarballs are fully extracted under the resource's cache
  directory.

If a resource stays `pending` longer than expected, look at the events the
agent records on it — sync failures and missing cache-path mounts are
reported there without changing the label's two-value vocabulary:

```console
kubectl describe imagecache worker-134-0-0
```

When an ImageCache is deleted or stops selecting the node, the agent removes
the label. Orchestration tooling can gate on the labels, e.g.:

```console
kubectl get nodes -l image-cache.scality.com/worker-134-0-0=synced
```

Absence of the label means either the node is not selected by this
ImageCache, or the agent has not completed a pass since the resource
appeared — both look the same to a gate; the startup and watch triggers make
the second case transient.

## Cache layout

Each ImageCache owns a subdirectory of its `cachePath`, named after the
resource:

```
/var/lib/image-cache/
├── some-bootstrap-image.tar        # flat files are never touched by the agent
├── worker-133-0-0/
│   ├── .image-cache-agent.json     # sentinel, written last
│   └── *.tar
└── worker-134-0-0/
    ├── .image-cache-agent.json
    └── *.tar
```

Per-resource subdirectories make name collisions between versions impossible
(two versions of the same cache image ship tarballs with the same file names)
and make garbage collection atomic: removing a resource's cache is removing
one directory.

The sentinel file is written after everything else and marks the directory as
complete and agent-owned:

- **Ownership**: garbage collection only ever considers directories containing
  a sentinel. Flat tarballs (e.g. placed by provisioning at bootstrap) and
  foreign directories in a shared cache path are never touched.
- **Completeness**: a directory without a sentinel is a partial extraction and
  is redone. The sentinel lists the expected file names, so a manually deleted
  tarball is detected and repaired.
- **Traceability**: the sentinel records the resolved image digest.

Extraction is atomic for a first extraction: layers are extracted to a
temporary directory next to the target, the sentinel is written, then the
directory is renamed into place. A re-extraction swaps remove-then-rename (a
rename cannot replace an existing directory), so a crash mid-swap can
transiently leave the resource absent; the next pass redoes the extraction.
A crash before the rename leaves a hidden `.<name>.tmp-*` directory next to
the target; garbage collection removes it on a later pass, the same as an
orphaned resource directory.

A resource whose directory is complete is never re-pulled: `spec.source` is
effectively immutable once synced. Publishing new content means creating a
new ImageCache (the name carries the version), not editing an existing one.

## Reconciliation model

The agent reconciles the node as a whole rather than one ImageCache at a
time. Every trigger enqueues the same single key, and each pass rebuilds the
full desired state and converges the disk and the node labels to it.

Triggers:

- Any ImageCache event (create, update, delete), mapped to the single key.
- Filesystem events (fsnotify) on the cache paths in use, so a manual
  deletion of tarballs is repaired quickly. Watchers are adjusted after each
  pass as cache paths come and go.
- A periodic resync as a safety net for missed events.
- A startup trigger guarantees one pass when the agent boots, so stale
  labels and directories from a previous life are cleaned up even if no
  ImageCache exists anymore.

One pass:

1. Compute `desired`: the ImageCache resources whose `nodeSelector` matches
   the labels of the node named by `NODE_NAME` (downward API).
2. For each desired resource: if its directory is complete, done. Otherwise
   set the label to `pending`, pull `spec.source` (linux/amd64), extract
   atomically, then set the label to `synced`.
3. Garbage-collect: delete sentinel-bearing directories that no desired
   resource claims.
4. Remove `image-cache.scality.com/*` node labels that no desired resource
   claims.

Failures are handled per resource inside the pass: a resource whose image
cannot be pulled keeps its `pending` label and gets a Kubernetes event
recorded against it, while the other resources still converge. Errors are
aggregated and the pass is requeued with backoff.

This level-triggered model needs no finalizers. Deletion is not a special
case — the resource simply disappears from the desired state — so a deletion
that happens while the agent is down is repaired by the next full pass. (With
per-resource reconciliation, cleanup after deletion would require finalizers;
one finalizer per node on a shared cluster-scoped resource is fragile: a
decommissioned node would leave a finalizer behind and block the deletion
forever.)

## Image pulling

Pulling and extraction use
[go-containerregistry](https://github.com/google/go-containerregistry):

- Pulling an image and walking its layers is its core use case; the flattened
  filesystem comes out of `mutate.Extract`.
- Multi-arch indexes are resolved client-side (`remote.WithPlatform`), which
  is exactly what a static, spec-compliant registry expects from its clients.
- Its in-memory registry (`pkg/registry`) lets tests exercise the real pull
  path without infrastructure.

The cache images are regular container images (they must remain importable
and mountable with `ctr` by provisioning tooling), so an artifact-oriented
client such as oras-go would bring no benefit here. The puller sits behind a
small interface in its own package, so the implementation can change without
touching the reconciler.

## Container image and deployment

The agent ships as a distroless, rootless, amd64-only image:
`gcr.io/distroless/static:nonroot` base, static binary, numeric
`USER 65532:65532` (a named user with `runAsNonRoot` fails container
creation).

It deploys as a DaemonSet (sample under `config/`): `NODE_NAME` from the
downward API, control-plane tolerations, read-only root filesystem.

Two deployment constraints follow from `cachePath` living on the host:

- **Mounts must cover cache paths.** The pod mounts host paths at identical
  container paths (the sample mounts the default `/var/lib/image-cache`).
  A resource whose `cachePath` is not covered by a mount would silently write
  to the container filesystem; the agent therefore refuses to process a
  resource whose `cachePath` does not exist, records an event, and leaves the
  label `pending`. Integrators must mount every cache path their resources
  declare.
- **The host directory must be writable by UID 65532.** `fsGroup` does not
  apply to hostPath volumes. The sample manifest uses a root init container
  that chowns the cache directory; integrators managing permissions at
  provisioning time can drop it.
- **The namespace must enforce the `privileged` Pod Security Standard.**
  hostPath volumes are already disallowed at the `baseline` level, and the
  chown init container runs as root, so the agent's namespace needs
  `pod-security.kubernetes.io/enforce: privileged` (see `test/e2e` for a
  working example).

## RBAC

- `imagecaches`: get, list, watch
- `nodes`: get, list, watch, patch (labels)
- `events`: create, patch

The agent never writes ImageCache resources (no status, no finalizers).

## Testing

- Unit tests: cache store (atomic extraction, sentinel handling, garbage
  collection ownership rules), node-selector matching, label diffing.
- Puller tests against go-containerregistry's in-memory registry, pulling a
  forged image whose layers contain tarballs.
- envtest: the full reconciler with a fake puller — resource lifecycle to
  node labels and on-disk state, including failure paths.
- A minimal kind-based e2e smoke test: CRD installed, agent running, node
  labelled `pending` for a resource with an unreachable source, label
  cleared on deletion — no registry infrastructure required.
