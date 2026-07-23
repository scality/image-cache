#!/usr/bin/env bash
set -euo pipefail

IMAGE_CACHE_DIR="${IMAGE_CACHE_DIR:-/var/lib/image-cache}"
IMAGE_PLATFORM="${IMAGE_PLATFORM:-linux/amd64}"

# No match -> empty loop instead of the literal glob.
shopt -s nullglob

# Tars live flat (provisioning) or in per-resource subdirectories (agent).
for tar in "${IMAGE_CACHE_DIR}"/*.tar "${IMAGE_CACHE_DIR}"/*/*.tar; do
    echo "Importing ${tar}"
    ctr -n k8s.io images import --platform "${IMAGE_PLATFORM}" "${tar}"
done
