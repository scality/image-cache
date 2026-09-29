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

- `source` is required and must read as a container image reference:
  `registry[:port]/repository[:tag][@sha256:<digest>]`, with a lowercase
  repository, at most 512 characters. The reference is not resolved at
  admission; only its shape is checked.
- `cachePath` must be an absolute path and must not contain `..` anywhere,
  checked as a plain substring rather than by parsing path segments.
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

The sentinel file is written after everything else. It marks the directory as
complete and records who wrote it:

- **Ownership**: garbage collection only ever considers directories containing
  a sentinel, plus the agent's own interrupted extractions (hidden, and holding
  `.tmp-`, see below). Flat tarballs (e.g. placed by provisioning at bootstrap)
  and foreign directories in a shared cache path are never touched. The
  sentinel records its writer, and a directory `imagecachectl` wrote is left
  alone until a resource adopts it (see the one-shot command below). A
  sentinel written before writers were recorded, or one that does not parse,
  counts as the agent's.
- **Completeness**: a directory without a sentinel is a partial extraction and
  is redone. The sentinel lists the expected file names, so a manually deleted
  tarball is detected and repaired.
- **Traceability**: the sentinel records who wrote the directory, the source
  it was read from, the manifest digest and the diff IDs of the image's
  layers.

The name is the agent's own: an entry carrying it inside a cache image is
skipped, so the sentinel always describes what the store extracted. Entries
are extracted by base name, so nothing in an image can write outside its
directory. Directory entries are skipped, since everything lands flat, and any
other kind of entry refuses the image whole. A symbolic link or a device is
not written out, and skipping it would publish a resource short of an archive
while the sentinel calls it complete. `docker build` copies a symbolic link as
it finds it, dangling if it pointed outside the build context, which is how
that would happen. A hard link could be recreated from its target, but a boot
cache image has no use for one.

Extraction is atomic for a first extraction: layers are extracted to a
temporary directory next to the target, the sentinel is written, then the
directory is renamed into place. A re-extraction swaps remove-then-rename (a
rename cannot replace an existing directory), so a crash mid-swap can
transiently leave the resource absent; the next pass redoes the extraction.
A crash before the rename leaves a hidden `.<name>.tmp-*` directory next to
the target; garbage collection removes it on a later pass, the same as an
orphaned resource directory.

A resource whose directory is complete and written by the agent is never
re-pulled: `spec.source` is effectively immutable once synced. A directory
`imagecachectl` seeded is checked once against `spec.source` before the agent
takes it over, see the one-shot command below. Publishing new content means creating a
new ImageCache (the name carries the version), not editing an existing one.

## Reconciliation model

The agent reconciles the node as a whole rather than one ImageCache at a
time. Every trigger enqueues the same single key, and each pass rebuilds the
full desired state and converges the disk and the node labels to it. That key
is the node's name: nothing reads it back, but it is what the manager logs as
the object being reconciled, which is what a log aggregator groups on.

Triggers:

- Any ImageCache event (create, update, delete), mapped to the single key.
- Filesystem events (fsnotify) on the cache paths in use, so a manual
  deletion of tarballs is repaired quickly. Each cache path is watched
  together with the resource directories under it: fsnotify is not
  recursive, and a watch on the cache path alone would report a whole
  resource directory disappearing but not a single tarball deleted inside
  one. Watchers are adjusted after each pass as cache paths and resource
  directories come and go.
- A periodic resync as a safety net, every hour by default
  (`--resync-period`). It is not what retries a failed pass, which is
  requeued with backoff, nor what repairs tampering, which raises a
  filesystem event. It only bounds how long a drift that raised no event at
  all can last, such as one spanning an agent restart.
- A startup trigger guarantees one pass when the agent boots, so stale
  labels and directories from a previous life are cleaned up even if no
  ImageCache exists anymore.

One pass:

1. Compute `desired`: the ImageCache resources whose `nodeSelector` matches
   the labels of the node named by `NODE_NAME` (downward API).
2. For each desired resource: if its directory is complete and the agent
   wrote it, done. If `imagecachectl` wrote it, set the label to `pending` and
   check that it holds the resource's image before adopting it (see the
   one-shot command below). Otherwise set the label to `pending`, pull
   `spec.source` (linux/amd64), extract atomically, then set the label to
   `synced`.
3. Garbage-collect: in every scanned cache path, delete the directories whose
   sentinel names the agent and that no desired resource claims. The scan set is the default
   cache path plus every cache path an ImageCache references now or referenced
   earlier in this agent process's lifetime — a custom path stays in the set
   after its last resource is deleted, so its orphaned directory is still
   collected. This set lives only in memory.
4. Remove `image-cache.scality.com/*` node labels that no desired resource
   claims.

Failures are handled per resource inside the pass: a resource whose image
cannot be pulled keeps its `pending` label and gets a Kubernetes event
recorded against it, while the other resources still converge. Errors are
aggregated and the pass is requeued with backoff. Each one is stamped with a
sentinel from `scality/go-errors` where it enters the agent, so an API
problem, a registry problem and a filesystem problem stay distinguishable
with `errors.Is` once they have been aggregated.

This level-triggered model needs no finalizers. Deletion is not a special
case — the resource simply disappears from the desired state — so a deletion
that happens while the agent is running is repaired by the next full pass.
One bounded exception survives a restart: because the scan set is in-memory,
an orphan directory on a non-default cache path whose last resource was
deleted while the agent was down is collected only once that path is
referenced again (the default path is always scanned, so it is never
affected).

Finalizers were considered and rejected: with per-resource reconciliation,
cleanup after deletion would require one finalizer per node on a shared
cluster-scoped resource, which is fragile — a decommissioned node would
leave a finalizer behind and block the deletion forever.

## Image pulling

Pulling and extraction use
[go-containerregistry](https://github.com/google/go-containerregistry):

- Pulling an image and walking its layers is its core use case. The layers
  are read one after the other and their entries passed on as they are, not
  through `mutate.Extract`: that one flattens the image, and on the way it
  drops every relative link whose target leaves the image root, which is the
  one link the store most needs to refuse. A boot cache image is built from
  scratch and only adds files, so the flattening buys nothing, and a layer
  that deletes a file of an earlier one is refused instead.
- Multi-arch indexes are resolved client-side (`remote.WithPlatform`), which
  is exactly what a static, spec-compliant registry expects from its clients.
- Its in-memory registry (`pkg/registry`) lets tests exercise the real pull
  path without infrastructure.

The cache images are regular container images (they must remain importable
and mountable with `ctr` by provisioning tooling), so an artifact-oriented
client such as oras-go would bring no benefit here. The puller sits behind a
small interface in its own package, so the implementation can change without
touching the reconciler.

## The one-shot command

A node being installed has no Kubernetes to run the agent in, and its kubelet
needs images before it starts. `imagecachectl` does one resource's worth of
that work from a command, and ships in its own RPM.

It is the same two pieces behind a different entry point: the puller above,
with a second implementation reading a docker archive on disk (the form the
installation media carries, where there is no registry to reach), and the same
store, so the layout and the sentinel are the agent's, not a second format.

Three consequences worth stating:

- **The name is the caller's.** It becomes the resource directory, and the
  agent recognises a resource by it. Given the name the `ImageCache` will
  carry, the agent finds the directory complete and adopts it once it has
  checked the content (below). Given any other name, the directory stays:
  garbage collection leaves what the command wrote, and no resource claims
  it, so it is the caller's to remove. The command validates the name as a
  DNS-1123 subdomain, the rule the API server applies to the resource, so the
  two cannot disagree on what a name is.
- **It replaces only what the store wrote.** The swap that publishes a
  resource removes whatever is at the destination first, so it refuses a
  directory that does not bear the sentinel. The cache path is shared, and a
  name is not a claim on what happens to sit under it: without the check, a
  resource named after a neighbour of the cache path, or a cache path one
  level too high, is an `rm -rf` of somebody else's data, run as root.
- **It fills once.** The state of the directory decides, not a comparison
  against the registry: once a resource is complete the command reaches no
  registry, whatever the source now points at. Keeping a node up to date is
  the agent's job, and a command that ran again on every convergence would be
  a second, weaker one.

### Adopting a seeded directory

The sentinel names its writer, so the agent tells what the command seeded from
what it wrote itself. Garbage collection leaves the command's directories
alone: at install the agent can land before the resources that name them, and
collecting a seeded cache would pull the same gigabyte again.

A resource that claims a seeded directory keeps its node `pending` until the
agent has checked the content. The agent resolves `spec.source`, which reads
the manifest and the configuration and never a layer. It compares the diff
IDs of the layers, in order, with the ones the command recorded:

- the same image: the agent rewrites the owner in the sentinel, through a
  temporary file renamed over it, and labels the node `synced` without
  pulling. From then on the directory is the agent's, to refresh and to
  collect.
- another image: the directory is replaced like any other that does not hold
  what its resource asks for.
- the source cannot be resolved: the directory is left as it is, and the next
  pass tries again.

The diff IDs identify the content because the cache holds nothing else: the
files come from the layers. They are the digests of the uncompressed layers,
so they do not depend on how the image is stored. The other identities do:

- the source string: the first node is seeded from an archive path and its
  resource names a registry reference. The string also changes with the
  registry endpoint.
- the manifest digest: it changes when an image is saved or pushed.
- the configuration digest: a format conversion, Docker to OCI for example,
  writes the configuration out again in another key order. The content is
  the same, the digest is not.

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
- `events` (`events.k8s.io`): create, patch

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
