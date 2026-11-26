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

# Archive metadata larger than this is not trusted: the archive is imported.
MEMBER_MAX_BYTES=1048576

# "name digest" of each image containerd holds complete and unpacked.
# ctr exits non-zero when any image fails its check but still prints every
# row, so only a missing header means containerd did not answer. With no
# image at all, it prints nothing, not even the header, and succeeds.
ready_images() {
    local table status=0
    table="$(ctr -n k8s.io images check 2> /dev/null)" || status=$?
    if [ -z "${table}" ] && [ "${status}" -eq 0 ]; then
        return 0
    fi
    [[ "${table}" == REF* ]] || return 1
    awk 'NR > 1 && $4 == "complete" && $NF == "true" { print $1, $3 }' <<< "${table}"
}

# One file of an archive, cut at MEMBER_MAX_BYTES (a cut file fails to parse
# or to hash). The name can come from the archive: `--` stops GNU tar from
# reading it as an option, --no-wildcards from matching `*` or `[` as a glob.
archive_member() {
    tar --seek --occurrence=1 --no-wildcards -xOf "$1" -- "$2" 2> /dev/null |
        head -c "${MEMBER_MAX_BYTES}"
}

# "name manifest-digest" of each image in an OCI archive. Fails when an image
# is unnamed.
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

# The name containerd gives a docker-archive tag on import, as Docker does.
normalize_name() {
    local name="$1" domain path
    domain="${name%%/*}"
    path="${name#*/}"
    # The first component is a registry only if it looks like a host.
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

# "name config-digest" of each tag in a docker-archive. containerd writes its
# own manifest on import, so the configuration is what both share; its digest
# is the hash of the file. Fails on an untagged image, or on a tag or path
# that is empty, holds white space, or (for a path) starts with a dash.
docker_archive_images() {
    local entries name config sum
    local -A config_digests=()
    entries="$(archive_member "$1" manifest.json |
        jq -r '
            if type == "array" and length > 0
               and all(.[]; (.Config | type) == "string"
                            and (.Config | test("^[^-\\s][^\\s]*$"))
                            and ((.RepoTags // []) | type) == "array"
                            and ((.RepoTags // []) | length) > 0
                            and all(.RepoTags[]; type == "string" and test("^[^\\s]+$")))
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

# Every name an import of the archive can set, read leniently: an archive
# rejected for a skip still moves the names it does have.
oci_archive_names() {
    archive_member "$1" index.json |
        jq -r '
            .manifests | arrays | .[] | objects
            | .annotations | objects | .["io.containerd.image.name"]
            | strings | select(test("^[^\\s]+$"))
        ' 2> /dev/null
}

docker_archive_names() {
    local tag
    archive_member "$1" manifest.json |
        jq -r '
            arrays | .[] | objects | .RepoTags | arrays | .[]
            | strings | select(test("^[^\\s]+$"))
        ' 2> /dev/null |
        while IFS= read -r tag; do
            normalize_name "${tag}"
        done
}

# Configuration digest per manifest, cached for the run (empty if unreadable).
declare -A ready_configs=()

# Sets config to the configuration digest of the ready image named $1. Fails
# when there is none, or when the name points at an index.
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

# Names of the archive last given to already_imported.
archive_names=""

# True when containerd already holds every image of the archive. Like
# containerd, it prefers index.json when manifest.json is there too.
already_imported() {
    local members images line name digest config
    archive_names=""
    [ -n "${ready}" ] || return 1
    members="$(tar --seek -tf "$1" 2> /dev/null)" || return 1
    if grep -qxF index.json <<< "${members}"; then
        archive_names="$(oci_archive_names "$1")" || true
        images="$(oci_archive_images "$1")" || return 1
        [ -n "${images}" ] || return 1
        while IFS= read -r line; do
            grep -qxF -- "${line}" <<< "${ready}" || return 1
        done <<< "${images}"
    elif grep -qxF manifest.json <<< "${members}"; then
        archive_names="$(docker_archive_names "$1")" || true
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

# Drops the names of the archive just imported (or failed: it may have moved
# some) from ready, so a later archive holding the same name with another
# image is imported too and the last one wins, instead of the name switching
# at every run.
forget_archive_names() {
    [ -n "${archive_names}" ] || return 0
    ready="$(awk 'NR == FNR { names[$1]; next } !($1 in names)' \
        <(printf '%s\n' "${archive_names}") <(printf '%s\n' "${ready}"))"
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
    forget_archive_names
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
