#!/usr/bin/env bash
set -euo pipefail

IMAGE_CACHE_DIR="${IMAGE_CACHE_DIR:-/var/lib/image-cache}"
IMAGE_PLATFORM="${IMAGE_PLATFORM:-linux/amd64}"

# No match -> empty loop instead of the literal glob.
shopt -s nullglob

scanned=0
skipped=0
failures=0

# Tars live flat (provisioning) or in per-resource subdirectories (agent).
# An archive containerd cannot read must not keep the others out, so every
# archive is tried and each failure is named as it happens.
for tar in "${IMAGE_CACHE_DIR}"/*.tar "${IMAGE_CACHE_DIR}"/*/*.tar; do
    scanned=$((scanned + 1))
    # The glob expanded before the first import, and the agent takes a
    # resource's directory away to collect it and again to swap a fresh
    # extraction in. An archive that is already gone when its turn comes is
    # therefore not a failure to report: either the node is not meant to hold
    # those images any more, or the next run will find them back.
    if [ ! -e "${tar}" ]; then
        echo "Skipping ${tar}, gone since the cache was scanned"
        skipped=$((skipped + 1))
        continue
    fi
    echo "Importing ${tar}"
    if ! ctr -n k8s.io images import --platform "${IMAGE_PLATFORM}" "${tar}"; then
        echo "Failed to import ${tar}" >&2
        failures=$((failures + 1))
    fi
done

imported=$((scanned - skipped - failures))

if [ "${failures}" -ne 0 ]; then
    # Say what became of all of them: one torn archive out of forty reads
    # nothing like a containerd that is down, and neither reads like a cache
    # the agent emptied while the run was walking it.
    echo "Failed to import ${failures} of the ${scanned} cached archives" \
         "(${imported} imported, ${skipped} no longer there)" >&2
    exit 1
fi

if [ "${scanned}" -ne 0 ] && [ "${imported}" -eq 0 ]; then
    # Nothing failed, yet nothing made it in either. A whole cache going away
    # under the run is legitimate (every resource collected at once) and is not
    # this script's call to make, but it must not pass for a quiet success.
    echo "None of the ${scanned} cached archives were still there to import" >&2
fi
