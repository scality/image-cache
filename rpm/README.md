# containerd-image-preload

A systemd service and timer that import container image tarballs from a local
directory into containerd, on boot and every ten minutes. No Kubernetes client,
no registry, no network: it works on a node whose cluster is down.

It is one half of [image-cache](../README.md). The other half, the
[image-cache-agent](../agent/README.md), fills that directory from a cluster.
They share the directory and nothing else, so this package is useful on its own
wherever the tarballs come from. See [../DESIGN.md](../DESIGN.md) for the split
between the two.

All the commands below run from this directory.

## Installing

The package targets Enterprise Linux 8 and 9, and requires `containerd`.
Releases carry a build for both as assets.

```console
dnf install containerd-image-preload-<version>-1.el9.noarch.rpm
systemctl enable --now containerd-image-preload.timer
```

Installing does not enable anything: the package ships the units and stops
there, so the provisioning layer decides when they start. The timer fires once
at boot (`OnBootSec=0`) and every ten minutes after each run
(`OnUnitActiveSec=10min`). To import right away:

```console
systemctl start containerd-image-preload.service
```

The service is a `oneshot` ordered after `containerd.service` and requiring it,
so it never runs against a containerd that is not up.

## Configuration

`/etc/sysconfig/containerd-image-preload` holds both knobs, and is
`%config(noreplace)`, so an upgrade of the package leaves your edits alone.

```sh
# Directory holding the *.tar image exports to import into containerd.
IMAGE_CACHE_DIR=/var/lib/image-cache

# Platform passed to `ctr images import`.
IMAGE_PLATFORM=linux/amd64
```

The package does not create `IMAGE_CACHE_DIR`. Create it yourself, or let the
agent do it.

## What it imports

Every `*.tar` directly in the cache directory, and every `*.tar` one level
below it:

```
/var/lib/image-cache/
├── some-image.tar        # imported, typically written at provisioning time
└── worker-1-0-0/
    └── *.tar             # imported, written by the agent
```

Nothing deeper, and nothing under a hidden directory. Each file goes in with
`ctr -n k8s.io images import --platform "$IMAGE_PLATFORM"`, into the `k8s.io`
namespace, which is where the kubelet looks. Re-importing an image containerd
already has replaces nothing and breaks nothing, which is what makes running
the timer on a short period harmless.

The script runs under `set -euo pipefail`. A tarball that fails to import
aborts that run, leaving the ones after it for the next tick. An empty or
missing cache directory imports nothing and succeeds.

## Development

Docker is the only requirement. Everything builds and runs inside a Rocky Linux
container, pinned by digest per major version, so the result does not depend on
your host.

```console
make test EL=9       # shellcheck, the bats suites and rpmlint
make rpm EL=9        # build into _build/, owned by you rather than by root
make lint            # shellcheck and rpmlint alone
make image EL=8      # just the toolchain image
make clean           # remove _build/
```

`EL` selects the target, 8 or 9, and both are supported: keep out of any bash,
systemd or `ctr` feature that only one of them has. Any change to behaviour
comes with a bats case in [`test/`](test), exercised on both.

`VERSION` stamps the package, defaulting to `0.0.0`. Release tags are
normalized on the way in: the leading `v` goes, and the hyphen of a pre-release
becomes `~`, which RPM allows in a version and which sorts before the final
release.

See [../CONTRIBUTING.md](../CONTRIBUTING.md) for the conventions and the pull
request workflow.

## License

Apache 2.0, see [LICENSE](../LICENSE).
