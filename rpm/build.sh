#!/usr/bin/env bash
# Build one of the repository's RPMs. Prints the path of the built .rpm.
# NAME picks the package, VERSION stamps it, and an optional argument sets an
# output directory.
set -euo pipefail

cd "$(dirname "$0")"
NAME="${NAME:-containerd-image-preload}"
VERSION="${VERSION:-0.0.0}"
# Map a release tag to a valid RPM version: drop the leading 'v', and turn the
# pre-release hyphen (v1.2.3-alpha.4) into '~' since RPM forbids '-' in Version.
# The '~' also sorts before the final release, as pre-releases should.
VERSION="${VERSION#v}"
VERSION="${VERSION//-/\~}"
dest="${1:-}"

rpmdev-setuptree
# Only what this spec declares. Staging the whole directory would put the
# compiled binary of the other package into the sources of a noarch one that
# never uses it, and make each build's inputs depend on what was built before.
sed -n 's/^Source[0-9]*:[[:space:]]*//p' "${NAME}.spec" | while read -r src; do
    cp -- "sources/${src}" "$HOME/rpmbuild/SOURCES/"
done
sed "s/_VERSION_/${VERSION}/g" "${NAME}.spec" > "$HOME/rpmbuild/SPECS/${NAME}.spec"
rpmbuild -bb "$HOME/rpmbuild/SPECS/${NAME}.spec" >&2

# Match this build's version so a leftover RPM from another build in the same
# tree (e.g. the test harness building twice) can never be picked instead.
built=$(find "$HOME/rpmbuild/RPMS" -name "${NAME}-${VERSION}-*.rpm" | head -n1)

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
