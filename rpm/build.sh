#!/usr/bin/env bash
# Build the containerd-image-preload RPM. Prints the path of the built .rpm.
# VERSION stamps the package; an optional argument sets an output directory.
set -euo pipefail

cd "$(dirname "$0")"
NAME=containerd-image-preload
VERSION="${VERSION:-0.0.0}"
# Map a release tag to a valid RPM version: drop the leading 'v', and turn the
# pre-release hyphen (v1.2.3-alpha.4) into '~' since RPM forbids '-' in Version.
# The '~' also sorts before the final release, as pre-releases should.
VERSION="${VERSION#v}"
VERSION="${VERSION//-/\~}"
dest="${1:-}"

rpmdev-setuptree
cp -- sources/* "$HOME/rpmbuild/SOURCES/"
sed "s/_VERSION_/${VERSION}/g" "${NAME}.spec" > "$HOME/rpmbuild/SPECS/${NAME}.spec"
rpmbuild -bb "$HOME/rpmbuild/SPECS/${NAME}.spec" >&2

built=$(find "$HOME/rpmbuild/RPMS" -name "${NAME}-*.rpm" | head -n1)

if [ -n "$dest" ]; then
    mkdir -p "$dest"
    cp -- "$built" "$dest/"
    built="$dest/$(basename "$built")"
    # Hand the artifact back to the host user when building as root in a container.
    if [ -n "${TARGET_UID:-}" ] && [ -n "${TARGET_GID:-}" ]; then
        chown "${TARGET_UID}:${TARGET_GID}" "$dest" "$built"
    fi
fi

echo "$built"
