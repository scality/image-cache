# Design

This document covers the project as a whole: the problem, the split into two
components, and the on-disk contract between them. The agent's own design
(its custom resource, its reconciliation model, its failure handling) lives in
[agent/DESIGN.md](agent/DESIGN.md).

## The problem

A Kubernetes cluster that hosts its own image registry has a circular
dependency: the registry needs its images to run, and the images come from the
registry. Any event that empties containerd's image store on a node leaves it
unable to start the very workloads that would let it pull again. A prune, a
disk failure, a reinstall, or a cold start with no external registry reachable
all do it.

The fix is to keep a copy of the critical images on the node, outside
containerd, in a form that can be restored without the network and without a
working control plane.

## Two components, one directory

Restoring the cache and filling it are separate problems with separate
lifetimes, so they are separate components:

- **`containerd-image-preload`** (an RPM) answers *restore*. A systemd timer
  runs `ctr images import` over every tarball of the cache directory, on boot
  and periodically. It is deliberately dumb: a shell script, no daemon, no
  Kubernetes client, no registry access. It works on a node whose cluster is
  down, and it is idempotent: re-importing what containerd already has
  replaces nothing and breaks nothing.

- **`image-cache-agent`** (a Kubernetes DaemonSet) answers *fill*. It watches
  `ImageCache` custom resources, pulls the images they point at, extracts the
  tarballs they carry into the cache directory, and removes what is no longer
  declared. It never talks to containerd: importing is the preload service's
  job, and keeping the two apart means the agent needs no privileged socket
  access.

They communicate through the filesystem only, at `/var/lib/image-cache` by
default. Neither knows the other exists, so either can be deployed alone:
tarballs written by provisioning tooling get imported with no agent running,
and a cluster that imports its cache some other way can still use the agent to
maintain it.

## The cache directory

```
/var/lib/image-cache/
├── some-image.tar              # flat file, e.g. written at provisioning time
└── worker-1-0-0/               # owned by an ImageCache named worker-1-0-0
    ├── .image-cache-agent.json # sentinel, written last
    └── *.tar
```

The preload service imports the flat tarballs and those one level down, which
covers both writers. The agent works exclusively inside its own
subdirectories, one per resource. It deletes a directory only when that
directory carries its sentinel, or when the name marks it as one of its own
interrupted extractions (hidden, and holding `.tmp-`), so a cache shared with
provisioning tooling is safe from it.
A subdirectory per resource also lets two versions of the same content coexist
during an upgrade, where flat tarballs would collide on identical file names.

## Why an image as the transport

The cache content ships as an ordinary container image whose layers contain
the tarballs. That choice keeps the whole distribution problem on
infrastructure that already exists: the registry stores it, ordinary tooling
pushes it, `ctr` can import and mount it directly during provisioning, and the
agent resolves it client-side by digest. Publishing new content is pushing an
image and declaring a new resource, with no templating, no configuration
management and no side channel.

## Scope

image-cache is generic: it caches whatever images it is told to cache, for
whatever consumer. It makes no assumption about the distribution running on
the node beyond systemd, containerd, and Kubernetes for the agent.

The architecture is the one exception. Both halves target `linux/amd64`: the
preload service imports with that platform by default, the agent image is
built for it alone, and the DaemonSet carries a matching `nodeSelector` so it
stays off nodes it could not run on. A node selected by an `ImageCache` but not
by the agent never reports a label, so a mixed cluster needs a selector that
says so.

Out of scope, deliberately:

- **Building the cache images.** Their content is the caller's decision; any
  image whose layers contain `*.tar` exports works.
- **Deciding when to publish a new version.** The agent converges on what
  exists; orchestration (creating resources, gating on the node labels,
  deleting the old ones) belongs to whatever drives the upgrade.
- **Talking to containerd from the agent**, as described above.
