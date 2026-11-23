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

# Larger index.json, manifest.json or image configurations are not trusted;
# the archive is imported.
MEMBER_MAX_BYTES=1048576

# "name digest" of each image containerd holds complete and unpacked.
# STATUS and SIZE contain spaces, so UNPACKED is read as the last field.
# ctr exits non-zero as soon as one image of the namespace cannot be checked,
# any image, not only cached ones, yet still prints every row. So its exit
# code is ignored: only a table with no header means containerd did not answer.
ready_images() {
    local table
    table="$(ctr -n k8s.io images check 2> /dev/null)" || true
    [[ "${table}" == REF* ]] || return 1
    awk 'NR > 1 && $4 == "complete" && $NF == "true" { print $1, $3 }' <<< "${table}"
}

# One file at the root of an archive, cut at MEMBER_MAX_BYTES. Fails when
# the archive does not hold it. A file cut short fails whatever reads it: it
# is no longer valid JSON, and it no longer hashes to its digest.
archive_member() {
    tar --seek --occurrence=1 -xOf "$1" "$2" 2> /dev/null |
        head -c "${MEMBER_MAX_BYTES}"
}

# "name digest" of each image in an OCI archive's index.json, the manifest
# digest that containerd keeps as is. Fails when an image is unnamed.
oci_archive_images() {
    archive_member "$1" index.json |
        jq -r '
            .manifests
            | if length > 0
                 and all(.[]; (.annotations["io.containerd.image.name"] // "") != "")
              then .[] | "\(.annotations["io.containerd.image.name"]) \(.digest)"
              else error("not every image is named") end
        ' 2> /dev/null
}

# The name containerd gives a docker-archive tag on import: docker.io and
# library/ added to a short name, latest to an untagged one.
normalize_name() {
    local name="$1" domain path
    domain="${name%%/*}"
    path="${name#*/}"
    # As in Docker, a first component is a registry only when it looks like
    # a host: a dot, a port, localhost, or an upper case letter, which a
    # repository cannot have.
    if [[ "${name}" != */* ]] ||
        [[ "${domain}" != *[.:[:upper:]]* && "${domain}" != localhost ]]; then
        domain=docker.io
        path="${name}"
    fi
    if [ "${domain}" = index.docker.io ]; then
        domain=docker.io
    fi
    if [ "${domain}" = docker.io ] && [[ "${path}" != */* ]]; then
        path="library/${path}"
    fi
    if [[ "${path##*/}" != *:* ]]; then
        path="${path}:latest"
    fi
    printf '%s/%s\n' "${domain}" "${path}"
}

# "name digest" of each tag in a docker-archive's manifest.json, with the
# digest of the image configuration. containerd writes a manifest of its own
# on import, so the configuration, copied byte for byte, is what the archive
# and containerd have in common. Its digest is the hash of the file, not its
# name. Fails when an image has no tag.
docker_archive_images() {
    local entries name config sum
    local -A config_digests=()
    entries="$(archive_member "$1" manifest.json |
        jq -r '
            if type == "array" and length > 0
               and all(.[]; (.Config | type) == "string"
                            and ((.RepoTags // []) | length) > 0)
            then .[] | .Config as $config | .RepoTags[] | "\(.) \($config)"
            else error("not every image is tagged") end
        ' 2> /dev/null)" || return 1
    while read -r name config; do
        if [ -z "${config_digests[${config}]+set}" ]; then
            sum="$(archive_member "$1" "${config}" | sha256sum)" || return 1
            config_digests[${config}]="sha256:${sum%% *}"
        fi
        printf '%s %s\n' "$(normalize_name "${name}")" "${config_digests[${config}]}"
    done <<< "${entries}"
}

# Configuration digest of each manifest looked up so far, empty when it could
# not be read. Kept across archives, so an image is read once per run.
declare -A ready_configs=()

# Sets config to the configuration digest of the image containerd holds
# ready under the name $1. Fails when it holds none, or when the name points
# at an index rather than at a single manifest.
ready_config() {
    local manifest
    manifest="$(name="$1" awk '$1 == ENVIRON["name"] { print $2; exit }' <<< "${ready}")"
    [ -n "${manifest}" ] || return 1
    if [ -z "${ready_configs[${manifest}]+set}" ]; then
        ready_configs[${manifest}]="$(ctr -n k8s.io content get "${manifest}" 2> /dev/null |
            head -c "${MEMBER_MAX_BYTES}" |
            jq -er '.config.digest | strings' 2> /dev/null)" ||
            ready_configs[${manifest}]=""
    fi
    config="${ready_configs[${manifest}]}"
    [ -n "${config}" ]
}

# True when containerd already holds every image of the archive. Like
# containerd, it reads an archive with an index.json as OCI, even when it also
# has the manifest.json that Docker 25 and later write next to it.
already_imported() {
    local members images line name digest config
    [ -n "${ready}" ] || return 1
    members="$(tar --seek -tf "$1" 2> /dev/null)" || return 1
    if grep -qxF index.json <<< "${members}"; then
        images="$(oci_archive_images "$1")" || return 1
        [ -n "${images}" ] || return 1
        while IFS= read -r line; do
            grep -qxF -- "${line}" <<< "${ready}" || return 1
        done <<< "${images}"
    elif grep -qxF manifest.json <<< "${members}"; then
        images="$(docker_archive_images "$1")" || return 1
        [ -n "${images}" ] || return 1
        while read -r name digest; do
            ready_config "${name}" || return 1
            [ "${config}" = "${digest}" ] || return 1
        done <<< "${images}"
    else
        return 1
    fi
}

archives=("${IMAGE_CACHE_DIR}"/*.tar "${IMAGE_CACHE_DIR}"/*/*.tar)
scanned=${#archives[@]}
skipped=0
current=0
failures=0

# If containerd does not answer at all, every archive is imported.
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
