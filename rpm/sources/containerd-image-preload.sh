#!/usr/bin/env bash
set -euo pipefail

IMAGE_CACHE_DIR="${IMAGE_CACHE_DIR:-/var/lib/image-cache}"
IMAGE_PLATFORM="${IMAGE_PLATFORM:-linux/amd64}"

# No match -> empty loop instead of the literal glob.
shopt -s nullglob

# journald takes a leading "<N>" as the line's syslog priority and strips it.
# Without it, stderr lands at info like stdout. The prefix is only written on
# a stream that is the journal: JOURNAL_STREAM alone is inherited by shells
# started from a logged session, so the descriptor is compared to it.
on_journal() {
    [ -n "${JOURNAL_STREAM:-}" ] &&
        [ "$(stat -L -c '%d:%i' "/proc/$$/fd/$1" 2> /dev/null)" = "${JOURNAL_STREAM}" ]
}

log() {
    local priority="$1" fd="$2"
    shift 2
    if on_journal "${fd}"; then
        printf '<%s>%s\n' "${priority}" "$*" >&"${fd}"
    else
        printf '%s\n' "$*" >&"${fd}"
    fi
}

info() { log 6 1 "$@"; }
warn() { log 4 2 "$@"; }
error() { log 3 2 "$@"; }

# Largest index.json read out of an archive. A real one is a few hundred bytes
# per image; anything past this is not trusted to describe the archive.
INDEX_MAX_BYTES=1048576

# The images containerd holds complete and unpacked, one "name digest" per
# line. `ctr images check` prints REF TYPE DIGEST STATUS SIZE UNPACKED, and
# STATUS and SIZE hold spaces of their own, so UNPACKED is read as the last
# field. Completeness is judged for containerd's default platform, which is
# IMAGE_PLATFORM on any node that keeps the default.
ready_images() {
    ctr -n k8s.io images check |
        awk 'NR > 1 && $4 == "complete" && $NF == "true" { print $1, $3 }'
}

# The "name digest" pairs an archive would import, read from its OCI
# index.json without reading the blobs: GNU tar seeks over the members it does
# not extract. Fails when the archive has no index, an image in it has no
# name, or the index is too large to trust, and the caller then imports.
archive_images() {
    tar --seek --occurrence=1 -xOf "$1" index.json 2> /dev/null |
        head -c "${INDEX_MAX_BYTES}" |
        jq -r '
            .manifests
            | if length > 0
                 and all(.[]; (.annotations["io.containerd.image.name"] // "") != "")
              then .[] | "\(.annotations["io.containerd.image.name"]) \(.digest)"
              else error("not every image is named") end
        ' 2> /dev/null
}

# True when containerd already holds every image of the archive, under the
# same name and digest, complete and unpacked. Importing it again would change
# nothing and still read and write about twice its size.
already_imported() {
    local images line
    [ -n "${ready}" ] || return 1
    images="$(archive_images "$1")" || return 1
    [ -n "${images}" ] || return 1
    while IFS= read -r line; do
        grep -qxF -- "${line}" <<< "${ready}" || return 1
    done <<< "${images}"
}

archives=("${IMAGE_CACHE_DIR}"/*.tar "${IMAGE_CACHE_DIR}"/*/*.tar)
scanned=${#archives[@]}
skipped=0
current=0
failures=0

# Asked once per run, and only when there is something to compare it with. A
# containerd that cannot answer leaves the set empty, so every archive is
# imported, as it was before the check existed, and fails the way it would.
ready=""
if [ "${scanned}" -ne 0 ] && ! ready="$(ready_images)"; then
    warn "Could not list the images containerd has, importing every archive"
    ready=""
fi

# Tars live flat (provisioning) or in per-resource subdirectories (agent).
# An archive containerd cannot read must not keep the others out, so every
# archive is tried and each failure is named as it happens.
for tar in "${archives[@]}"; do
    # The glob expanded before the first import, and the agent takes a
    # resource's directory away to collect it and again to swap a fresh
    # extraction in. An archive that is already gone when its turn comes is
    # therefore not a failure to report: either the node is not meant to hold
    # those images any more, or the next run will find them back.
    if [ ! -e "${tar}" ]; then
        info "Skipping ${tar}, gone since the cache was scanned"
        skipped=$((skipped + 1))
        continue
    fi
    # The archive's own digest says nothing here: it does not change when
    # containerd loses the image, which is the case this service exists for.
    # What containerd holds is compared with what the archive names instead.
    if already_imported "${tar}"; then
        info "Skipping ${tar}, already in containerd"
        current=$((current + 1))
        continue
    fi
    info "Importing ${tar}"
    if ! ctr -n k8s.io images import --platform "${IMAGE_PLATFORM}" "${tar}"; then
        error "Failed to import ${tar}"
        failures=$((failures + 1))
    fi
done

imported=$((scanned - skipped - current - failures))

if [ "${failures}" -ne 0 ]; then
    # Say what became of all of them: one torn archive out of forty reads
    # nothing like a containerd that is down, and neither reads like a cache
    # the agent emptied while the run was walking it.
    error "Failed to import ${failures} of the ${scanned} cached archives" \
          "(${imported} imported, ${current} already in containerd," \
          "${skipped} no longer there)"
    exit 1
fi

if [ "${scanned}" -eq 0 ]; then
    info "No cached archives to import in ${IMAGE_CACHE_DIR}"
elif [ "${skipped}" -eq "${scanned}" ]; then
    # Nothing failed, yet nothing was there either. A whole cache going away
    # under the run is legitimate (every resource collected at once) and is not
    # this script's call to make, but it must not pass for a quiet success.
    warn "None of the ${scanned} cached archives were still there to import"
else
    info "Imported ${imported} of the ${scanned} cached archives" \
         "(${current} already in containerd, ${skipped} no longer there)"
fi
