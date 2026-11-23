#!/usr/bin/env bats
# Unit tests for the containerd-image-preload import script (with a stubbed `ctr`).
#
# `run --separate-stderr` and `run !` need bats 1.5.0, which is what Rocky 8
# ships. Declaring that floor is what silences the BW02 warnings, but the
# declaration itself only arrived in 1.7.0, so it is made when it exists.
if command -v bats_require_minimum_version > /dev/null 2>&1; then
    bats_require_minimum_version 1.5.0
elif [ "$(printf '%s\n1.5.0\n' "${BATS_VERSION:-0}" | sort -V | head -n 1)" != "1.5.0" ]; then
    echo "these tests need bats 1.5.0 or newer, this is ${BATS_VERSION:-unknown}" >&2
    exit 1
fi

setup() {
    SCRIPT="${BATS_TEST_DIRNAME}/../sources/containerd-image-preload.sh"
    TMP="$(mktemp -d)"
    CACHE="$TMP/cache"
    BIN="$TMP/bin"
    CTR_LOG="$TMP/ctr.log"
    CTR_FAIL="$TMP/ctr.fail"
    CTR_RM="$TMP/ctr.rm"
    CTR_IMAGES="$TMP/ctr.images"
    CTR_CHECK_LOG="$TMP/ctr.check.log"
    CTR_CHECK_FAIL="$TMP/ctr.check.fail"
    CTR_CHECK_PARTIAL="$TMP/ctr.check.partial"
    CTR_CONTENT="$TMP/ctr.content"
    CTR_CONTENT_LOG="$TMP/ctr.content.log"
    mkdir -p "$CACHE" "$BIN" "$CTR_CONTENT"
    : > "$CTR_CONTENT_LOG"
    : > "$CTR_LOG"
    : > "$CTR_FAIL"
    : > "$CTR_RM"
    : > "$CTR_IMAGES"
    : > "$CTR_CHECK_LOG"
    # The stub logs every import, and exits non-zero on the archives listed in
    # $CTR_FAIL, the way containerd reports one it cannot read. The archive is
    # the last argument, and is matched by its path inside the cache: the same
    # file name legitimately lives flat and in a subdirectory. `images check`
    # prints $CTR_IMAGES and logs to $CTR_CHECK_LOG. `content get` prints the
    # blob stored under its digest in $CTR_CONTENT, fails on any other, and
    # logs to $CTR_CONTENT_LOG.
    cat > "$BIN/ctr" <<EOF
#!/usr/bin/env bash
if [ "\$3 \$4" = "content get" ]; then
    echo "\$5" >> "$CTR_CONTENT_LOG"
    if [ -f "$CTR_CONTENT/\$5" ]; then
        cat "$CTR_CONTENT/\$5"
        exit 0
    fi
    echo "ctr: \$5: not found" >&2
    exit 1
fi
if [ "\$3 \$4" = "images check" ]; then
    echo "\$*" >> "$CTR_CHECK_LOG"
    if [ -e "$CTR_CHECK_FAIL" ]; then
        echo "ctr: connection refused" >&2
        exit 1
    fi
    printf 'REF\tTYPE\tDIGEST\tSTATUS\tSIZE\tUNPACKED\n'
    cat "$CTR_IMAGES"
    # Like the real ctr: every row printed, then a non-zero exit when one
    # image could not be checked.
    if [ -e "$CTR_CHECK_PARTIAL" ]; then
        echo "ctr: unable to check registry.example/broken:1" >&2
        exit 1
    fi
    exit 0
fi
echo "\$*" >> "$CTR_LOG"
# Stand in for an agent garbage collecting a resource mid-run, so the archives
# the loop has not reached yet are gone when their turn comes.
if [ -s "$CTR_RM" ]; then
    while read -r path; do
        rm -rf "\$path"
    done < "$CTR_RM"
    : > "$CTR_RM"
fi
archive="\${*: -1}"
if grep -qxF -- "\${archive#"$CACHE"/}" "$CTR_FAIL"; then
    echo "ctr: unexpected EOF" >&2
    exit 1
fi
EOF
    chmod +x "$BIN/ctr"
}

teardown() {
    rm -rf "$TMP"
}

# Make the stubbed `ctr` fail on these archives, named by their path inside
# the cache directory.
fail_on() {
    printf '%s\n' "$@" > "$CTR_FAIL"
}

# Have the stubbed `ctr` delete these paths on its first call, the way the
# agent removes a resource's directory when its ImageCache goes away.
vanish_on_first_import() {
    printf '%s\n' "$@" > "$CTR_RM"
}

# A digest made of one repeated hex digit, enough to tell images apart.
digest() {
    printf 'sha256:%064d' 0 | tr 0 "$1"
}

# Write an OCI archive at $1 holding the given "name digest" pairs.
make_archive() {
    local archive="$1" dir manifests="" sep=""
    shift
    dir="$(mktemp -d "$TMP/archive.XXXXXX")"
    while [ "$#" -ge 2 ]; do
        manifests+="${sep}{\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"digest\":\"$2\",\"size\":1,\"annotations\":{\"io.containerd.image.name\":\"$1\",\"org.opencontainers.image.ref.name\":\"1\"}}"
        sep=","
        shift 2
    done
    write_index "$dir" "{\"schemaVersion\":2,\"manifests\":[${manifests}]}"
    tar_archive "$archive" "$dir"
}

# Lay out an OCI directory around an index.json given verbatim.
write_index() {
    mkdir -p "$1/blobs/sha256"
    head -c 1048576 /dev/zero > "$1/blobs/sha256/$(digest a | cut -d: -f2)"
    printf '%s' '{"imageLayoutVersion":"1.0.0"}' > "$1/oci-layout"
    printf '%s' "$2" > "$1/index.json"
}

tar_archive() {
    mkdir -p "$(dirname "$1")"
    tar -cf "$1" -C "$2" blobs oci-layout index.json
}

# Write a docker-archive at $1, the way skopeo and Docker before 25 write
# one: a manifest.json and no index.json. Each image is a "tag[,tag...]" and
# a configuration given verbatim, stored under the file name of its hash.
# Prints nothing; the digest of a configuration is $(config_digest CONTENT).
make_docker_archive() {
    local archive="$1" dir entries="" sep="" tags config file
    shift
    dir="$(mktemp -d "$TMP/docker-archive.XXXXXX")"
    while [ "$#" -ge 2 ]; do
        tags="$(jq -cn --arg t "$1" '$t | split(",")')"
        config="$2"
        file="$(config_digest "${config}" | cut -d: -f2).json"
        printf '%s' "${config}" > "$dir/$file"
        entries+="${sep}{\"Config\":\"$file\",\"RepoTags\":${tags},\"Layers\":[\"layer.tar\"]}"
        sep=","
        shift 2
    done
    head -c 1048576 /dev/zero > "$dir/layer.tar"
    printf '[%s]' "${entries}" > "$dir/manifest.json"
    mkdir -p "$(dirname "$archive")"
    (cd "$dir" && tar -cf "$archive" -- *)
}

# The digest of an image configuration, given verbatim.
config_digest() {
    printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"
}

# Have containerd hold an image imported from a docker-archive: its row in
# `ctr images check`, under the manifest digest $2 that containerd wrote, and
# that manifest in the content store, naming the configuration $3 verbatim.
containerd_has_config() {
    containerd_has "$1" "$2" "${4:-complete (3/3)}" "${5:-true}"
    printf '{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":"%s","size":1},"layers":[]}' \
        "$(config_digest "$3")" > "$CTR_CONTENT/$2"
}

# Add an image to the `ctr images check` output (healthy by default).
containerd_has() {
    printf '%s\tapplication/vnd.oci.image.index.v1+json\t%s\t%s\t%s\t%s\n' \
        "$1" "$2" "${3:-complete (3/3)}" "2.1 MiB/2.1 MiB" "${4:-true}" >> "$CTR_IMAGES"
}

# Run the script against the stubbed `ctr`, keeping stdout and stderr apart so
# a test can tell what the unit reports from what it merely logs. Extra
# `NAME=value` arguments override the environment, the last one winning.
run_preload() {
    run --separate-stderr env "PATH=$BIN:$PATH" "IMAGE_CACHE_DIR=$CACHE" "$@" bash "$SCRIPT"
}

# Run the script with both streams in one file, and JOURNAL_STREAM naming that
# file's identity, so the script takes it for the journal socket.
run_preload_on_journal() {
    : > "$TMP/journal"
    # shellcheck disable=SC2016 # expanded by the inner bash, not here
    run env "PATH=$BIN:$PATH" "IMAGE_CACHE_DIR=$CACHE" \
        "JOURNAL_STREAM=$(stat -L -c '%d:%i' "$TMP/journal")" \
        bash -c 'bash "$1" >> "$2" 2>&1' _ "$SCRIPT" "$TMP/journal"
}

@test "imports .tar files and ignores the rest" {
    : > "$CACHE/a.tar"
    : > "$CACHE/b.tar"
    : > "$CACHE/notes.txt"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "a.tar" "$CTR_LOG"
    grep -q "b.tar" "$CTR_LOG"
    # A healthy run says nothing on stderr, summary line included. A plain
    # `run` leaves $stderr at whatever the last --separate-stderr one set, so
    # assert it while it still belongs to the script.
    [ -z "$stderr" ]
    run ! grep -q "notes.txt" "$CTR_LOG"
}

@test "empty cache directory imports nothing and succeeds" {
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
}

@test "missing cache directory imports nothing and succeeds" {
    run_preload "IMAGE_CACHE_DIR=$TMP/absent"
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
}

@test "uses linux/amd64 platform by default" {
    : > "$CACHE/a.tar"
    run_preload
    [ "$status" -eq 0 ]
    grep -q -- "--platform linux/amd64" "$CTR_LOG"
}

@test "IMAGE_PLATFORM overrides the platform" {
    : > "$CACHE/a.tar"
    run_preload "IMAGE_PLATFORM=linux/arm64"
    [ "$status" -eq 0 ]
    grep -q -- "--platform linux/arm64" "$CTR_LOG"
}

@test "imports tars from cache subdirectories" {
    # a per-resource subdirectory as laid out by the cache agent
    mkdir -p "$CACHE/worker-134-0-0"
    : > "$CACHE/worker-134-0-0/etcd.tar"
    : > "$CACHE/worker-134-0-0/.image-cache-agent.json"
    run_preload
    [ "$status" -eq 0 ]
    [ "$(grep -c "worker-134-0-0/etcd.tar" "$CTR_LOG")" -eq 1 ]
    run ! grep -q "image-cache-agent.json" "$CTR_LOG"
}

@test "an unreadable archive does not stop the ones after it" {
    fail_on bad.tar
    : > "$CACHE/alpha.tar"
    : > "$CACHE/bad.tar"
    : > "$CACHE/zulu.tar"
    run_preload
    [ "$status" -eq 1 ]
    grep -q "alpha.tar" "$CTR_LOG"
    grep -q "zulu.tar" "$CTR_LOG"
    [[ "$stderr" == *"Failed to import $CACHE/bad.tar"* ]]
    [[ "$stderr" != *"Failed to import $CACHE/alpha.tar"* ]]
    # containerd's own diagnostic is what says why the archive was refused.
    [[ "$stderr" == *"ctr: unexpected EOF"* ]]
}

@test "names every archive that failed, flat ones and subdirectory ones" {
    fail_on flat-bad.tar worker-134-0-0/nested-bad.tar
    : > "$CACHE/flat-bad.tar"
    : > "$CACHE/flat-good.tar"
    mkdir -p "$CACHE/worker-134-0-0"
    : > "$CACHE/worker-134-0-0/nested-bad.tar"
    : > "$CACHE/worker-134-0-0/nested-good.tar"
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import $CACHE/flat-bad.tar"* ]]
    [[ "$stderr" == *"Failed to import $CACHE/worker-134-0-0/nested-bad.tar"* ]]
    [[ "$stderr" == *"Failed to import 2 of the 4 cached archives (2 imported, 0 already in containerd, 0 no longer there)"* ]]
    grep -q "flat-good.tar" "$CTR_LOG"
    grep -q "worker-134-0-0/nested-good.tar" "$CTR_LOG"
}

@test "a cache where every archive fails reports as many as it found" {
    fail_on a.tar b.tar
    : > "$CACHE/a.tar"
    : > "$CACHE/b.tar"
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import 2 of the 2 cached archives (0 imported, 0 already in containerd, 0 no longer there)"* ]]
}

@test "a cache with nothing left to import says so instead of passing quietly" {
    # Dangling symlinks stand in for archives the agent collected between the
    # scan and their turn: `[ -e ]` is false for both, so every one is skipped.
    ln -s "$TMP/never-existed" "$CACHE/a.tar"
    ln -s "$TMP/never-existed" "$CACHE/b.tar"
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
    [[ "$stderr" == *"None of the 2 cached archives were still there to import"* ]]
}

@test "the summary counts every archive the cache held, skipped ones included" {
    : > "$CACHE/bad.tar"
    : > "$CACHE/good.tar"
    mkdir -p "$CACHE/worker-134-0-0"
    : > "$CACHE/worker-134-0-0/gone.tar"
    # The first import collects the resource; bad.tar itself is only torn.
    vanish_on_first_import "$CACHE/worker-134-0-0"
    fail_on bad.tar
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import 1 of the 3 cached archives (1 imported, 0 already in containerd, 1 no longer there)"* ]]
}

@test "an archive that fails and then disappears is still a failure" {
    # Deliberate: once `ctr` has refused it, a later stat saying the file is
    # gone proves it is absent now, not that absence is why the import failed.
    # A containerd outage that coincides with a cleanup must not read as success.
    : > "$CACHE/bad.tar"
    : > "$CACHE/good.tar"
    vanish_on_first_import "$CACHE/bad.tar"
    fail_on bad.tar
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import $CACHE/bad.tar"* ]]
    [[ "$stderr" == *"Failed to import 1 of the 2 cached archives (1 imported, 0 already in containerd, 0 no longer there)"* ]]
}

@test "a run that fails and skips does not also claim it found nothing" {
    : > "$CACHE/bad-one.tar"
    : > "$CACHE/bad-two.tar"
    mkdir -p "$CACHE/worker-134-0-0"
    : > "$CACHE/worker-134-0-0/gone.tar"
    vanish_on_first_import "$CACHE/worker-134-0-0"
    fail_on bad-one.tar bad-two.tar
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import 2 of the 3 cached archives (0 imported, 0 already in containerd, 1 no longer there)"* ]]
    [[ "$stderr" != *"None of the"* ]]
}

@test "an archive gone since the scan is skipped, not counted as a failure" {
    : > "$CACHE/alpha.tar"
    mkdir -p "$CACHE/worker-134-0-0"
    : > "$CACHE/worker-134-0-0/etcd.tar"
    : > "$CACHE/worker-134-0-0/pause.tar"
    # The agent drops the resource while the run is busy with the flat archive.
    vanish_on_first_import "$CACHE/worker-134-0-0"
    run_preload
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
    [ "$(grep -c . "$CTR_LOG")" -eq 1 ]
    [[ "$output" == *"Skipping $CACHE/worker-134-0-0/etcd.tar"* ]]
}

@test "a successful run ends with what it imported" {
    : > "$CACHE/a.tar"
    : > "$CACHE/b.tar"
    run_preload
    [ "$status" -eq 0 ]
    [[ "$output" == *"Imported 2 of the 2 cached archives (0 already in containerd, 0 no longer there)"* ]]
}

@test "an empty cache says there was nothing to import" {
    run_preload
    [ "$status" -eq 0 ]
    [[ "$output" == *"No cached archives to import in $CACHE"* ]]
}

@test "on the journal, failures are errors and the rest is info" {
    fail_on bad.tar
    : > "$CACHE/bad.tar"
    : > "$CACHE/good.tar"
    run_preload_on_journal
    [ "$status" -eq 1 ]
    grep -qxF "<6>Importing $CACHE/good.tar" "$TMP/journal"
    grep -qxF "<3>Failed to import $CACHE/bad.tar" "$TMP/journal"
    grep -qxF "<3>Failed to import 1 of the 2 cached archives (1 imported, 0 already in containerd, 0 no longer there)" "$TMP/journal"
}

@test "on the journal, a cache with nothing left is a warning" {
    ln -s "$TMP/never-existed" "$CACHE/a.tar"
    run_preload_on_journal
    [ "$status" -eq 0 ]
    grep -qxF "<4>None of the 1 cached archives were still there to import" "$TMP/journal"
}

@test "an inherited JOURNAL_STREAM does not prefix a terminal's output" {
    : > "$CACHE/a.tar"
    run_preload "JOURNAL_STREAM=0:0"
    [ "$status" -eq 0 ]
    [[ "$output" == "Importing $CACHE/a.tar"* ]]
    [[ "$output" != *"<"* ]]
}

@test "does not import tars from hidden or deeper directories" {
    mkdir -p "$CACHE/.worker.tmp-1" "$CACHE/deep/nested"
    : > "$CACHE/.worker.tmp-1/etcd.tar"
    : > "$CACHE/deep/nested/etcd.tar"
    run_preload
    [ "$status" -eq 0 ]
    # Neither is reachable, so `ctr` is never called at all.
    [ ! -s "$CTR_LOG" ]
    [ ! -s "$CTR_CHECK_LOG" ]
}

@test "an archive containerd already holds is not imported again" {
    make_archive "$CACHE/worker-134-0-0/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
    [ ! -s "$CTR_LOG" ]
    [[ "$output" == *"Skipping $CACHE/worker-134-0-0/etcd.tar, already in containerd"* ]]
    [[ "$output" == *"Imported 0 of the 1 cached archives (1 already in containerd, 0 no longer there)"* ]]
}

@test "containerd is asked once per run, in the kubelet's namespace" {
    make_archive "$CACHE/a.tar" registry.example/a:1 "$(digest 1)"
    make_archive "$CACHE/b.tar" registry.example/b:1 "$(digest 2)"
    run_preload
    [ "$status" -eq 0 ]
    [ "$(grep -c . "$CTR_CHECK_LOG")" -eq 1 ]
    grep -qx -- "-n k8s.io images check" "$CTR_CHECK_LOG"
}

@test "an empty cache does not ask containerd anything" {
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_CHECK_LOG" ]
}

@test "an image containerd lost is imported again" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/other:1 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
    [[ "$output" == *"Imported 1 of the 1 cached archives (0 already in containerd, 0 no longer there)"* ]]
}

@test "an archive replaced with another digest is imported" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 2)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
}

@test "an image containerd holds incomplete is imported" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)" "incomplete (2/3)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
}

@test "an image containerd holds but has not unpacked is imported" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)" "complete (3/3)" false
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
}

@test "an archive is imported when containerd lacks one of its images" {
    make_archive "$CACHE/control-plane.tar" \
        registry.example/apiserver:1 "$(digest 1)" \
        registry.example/scheduler:1 "$(digest 2)"
    containerd_has registry.example/apiserver:1 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "control-plane.tar" "$CTR_LOG"
}

@test "an archive with an unnamed image is always imported" {
    local dir="$TMP/unnamed"
    write_index "$dir" "{\"schemaVersion\":2,\"manifests\":[{\"digest\":\"$(digest 1)\",\"annotations\":{\"org.opencontainers.image.ref.name\":\"3.5\"}}]}"
    tar_archive "$CACHE/etcd.tar" "$dir"
    containerd_has 3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
}

@test "an archive with neither an index.json nor a manifest.json is always imported" {
    mkdir -p "$TMP/neither"
    : > "$TMP/neither/layer.tar"
    tar -cf "$CACHE/neither.tar" -C "$TMP/neither" layer.tar
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "neither.tar" "$CTR_LOG"
}

@test "an oversized index is not trusted to skip the import" {
    local dir="$TMP/huge" padding
    padding="$(head -c 1100000 /dev/zero | tr '\0' x)"
    write_index "$dir" "{\"schemaVersion\":2,\"pad\":\"${padding}\",\"manifests\":[{\"digest\":\"$(digest 1)\",\"annotations\":{\"io.containerd.image.name\":\"registry.example/etcd:3.5\"}}]}"
    tar_archive "$CACHE/etcd.tar" "$dir"
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
}

@test "a containerd that cannot list its images gets every archive imported" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    : > "$CTR_CHECK_FAIL"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "etcd.tar" "$CTR_LOG"
    [[ "$stderr" == *"Could not list the images containerd has, importing every archive"* ]]
}

@test "one image containerd cannot check does not stop the others from being skipped" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    make_archive "$CACHE/broken.tar" registry.example/broken:1 "$(digest 2)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/broken:1 "$(digest 2)" "unavailable (1/?)" false
    : > "$CTR_CHECK_PARTIAL"
    run_preload
    [ "$status" -eq 0 ]
    [[ "$output" == *"Skipping $CACHE/etcd.tar, already in containerd"* ]]
    [[ "$stderr" != *"Could not list the images containerd has"* ]]
    # The image that failed its check is not taken as present.
    grep -q "broken.tar" "$CTR_LOG"
    run ! grep -q "etcd.tar" "$CTR_LOG"
}

@test "a run where everything is already in containerd is no warning" {
    make_archive "$CACHE/a.tar" registry.example/a:1 "$(digest 1)"
    containerd_has registry.example/a:1 "$(digest 1)"
    run_preload_on_journal
    [ "$status" -eq 0 ]
    grep -qxF "<6>Skipping $CACHE/a.tar, already in containerd" "$TMP/journal"
    run ! grep -q "^<[0-4]>" "$TMP/journal"
}

@test "a failure is counted apart from the archives already in containerd" {
    make_archive "$CACHE/current.tar" registry.example/a:1 "$(digest 1)"
    containerd_has registry.example/a:1 "$(digest 1)"
    : > "$CACHE/bad.tar"
    : > "$CACHE/good.tar"
    fail_on bad.tar
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import 1 of the 3 cached archives (1 imported, 1 already in containerd, 0 no longer there)"* ]]
}

@test "a docker-archive whose configuration containerd holds under its tag is not imported again" {
    make_docker_archive "$CACHE/worker-134-0-0/pause.tar" registry.example/pause:3.10 '{"pause":1}'
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
    [ ! -s "$CTR_LOG" ]
    grep -qxF "$(digest 1)" "$CTR_CONTENT_LOG"
    [[ "$output" == *"Skipping $CACHE/worker-134-0-0/pause.tar, already in containerd"* ]]
    [[ "$output" == *"Imported 0 of the 1 cached archives (1 already in containerd, 0 no longer there)"* ]]
}

@test "a docker-archive with another configuration under the same tag is imported" {
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10 '{"pause":2}'
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive whose configuration containerd holds under another tag only is imported" {
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10 '{"pause":1}'
    containerd_has_config registry.example/other:1 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive image containerd holds incomplete or not unpacked is imported" {
    make_docker_archive "$CACHE/incomplete.tar" registry.example/a:1 '{"a":1}'
    make_docker_archive "$CACHE/packed.tar" registry.example/b:1 '{"b":1}'
    containerd_has_config registry.example/a:1 "$(digest 1)" '{"a":1}' "incomplete (2/3)"
    containerd_has_config registry.example/b:1 "$(digest 2)" '{"b":1}' "complete (3/3)" false
    run_preload
    [ "$status" -eq 0 ]
    grep -q "incomplete.tar" "$CTR_LOG"
    grep -q "packed.tar" "$CTR_LOG"
}

@test "a docker-archive is imported when containerd lacks one of its images" {
    make_docker_archive "$CACHE/control-plane.tar" \
        registry.example/apiserver:1 '{"apiserver":1}' \
        registry.example/scheduler:1 '{"scheduler":1}'
    containerd_has_config registry.example/apiserver:1 "$(digest 1)" '{"apiserver":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "control-plane.tar" "$CTR_LOG"
}

@test "a docker-archive is skipped only when containerd holds every tag of an image" {
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10,registry.example/pause:stable '{"pause":1}'
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
    containerd_has_config registry.example/pause:stable "$(digest 1)" '{"pause":1}'
    : > "$CTR_LOG"
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
}

@test "docker-archive tags are matched under the names containerd gives them" {
    # Each name on the right is what containerd 1.6, 1.7 and 2.4 list after
    # importing a docker-archive tagged with the name on the left.
    local pair tag name n=0
    for pair in \
        "pause:short docker.io/library/pause:short" \
        "myorg/pause:1 docker.io/myorg/pause:1" \
        "myorg/pause docker.io/myorg/pause:latest" \
        "pause docker.io/library/pause:latest" \
        "docker.io/pause:6 docker.io/library/pause:6" \
        "index.docker.io/library/pause:4 docker.io/library/pause:4" \
        "localhost/pause:2 localhost/pause:2" \
        "reg:5000/pause:3 reg:5000/pause:3" \
        "reg:5000/pause reg:5000/pause:latest" \
        "Upper/pause:5 Upper/pause:5" \
        "a.example/deep/path/p:1 a.example/deep/path/p:1"; do
        tag="${pair% *}"
        name="${pair#* }"
        n=$((n + 1))
        make_docker_archive "$CACHE/image-$n.tar" "$tag" "{\"n\":$n}"
        containerd_has_config "$name" "$(printf 'sha256:%064d' "$n")" "{\"n\":$n}"
    done
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
    [[ "$output" == *"Imported 0 of the 11 cached archives (11 already in containerd, 0 no longer there)"* ]]
}

@test "a docker-archive tag is not matched under a name containerd would not give it" {
    make_docker_archive "$CACHE/pause.tar" pause:3.10 '{"pause":1}'
    containerd_has_config pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive configuration is identified by its hash, not by its file name" {
    # The file is named after the configuration containerd holds, but holds
    # another one: containerd would import this other one.
    local dir="$TMP/misnamed" held='{"pause":1}'
    mkdir -p "$dir"
    printf '%s' '{"pause":2}' > "$dir/$(config_digest "$held" | cut -d: -f2).json"
    printf '[{"Config":"%s.json","RepoTags":["registry.example/pause:3.10"],"Layers":[]}]' \
        "$(config_digest "$held" | cut -d: -f2)" > "$dir/manifest.json"
    (cd "$dir" && tar -cf "$CACHE/pause.tar" -- *)
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" "$held"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive whose configuration file is missing is imported" {
    local dir="$TMP/noconfig"
    mkdir -p "$dir"
    printf '[{"Config":"%s.json","RepoTags":["registry.example/pause:3.10"],"Layers":[]}]' \
        "$(config_digest '{"pause":1}' | cut -d: -f2)" > "$dir/manifest.json"
    tar -cf "$CACHE/pause.tar" -C "$dir" manifest.json
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive with an untagged image is always imported" {
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10 '{"pause":1}' "" '{"other":1}'
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive with an empty or broken manifest.json is always imported" {
    mkdir -p "$TMP/empty" "$TMP/broken"
    printf '[]' > "$TMP/empty/manifest.json"
    printf '[{"Config":' > "$TMP/broken/manifest.json"
    tar -cf "$CACHE/empty.tar" -C "$TMP/empty" manifest.json
    tar -cf "$CACHE/broken.tar" -C "$TMP/broken" manifest.json
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "empty.tar" "$CTR_LOG"
    grep -q "broken.tar" "$CTR_LOG"
}

@test "an oversized manifest.json is not trusted to skip the import" {
    local dir="$TMP/huge" padding file
    padding="$(head -c 1100000 /dev/zero | tr '\0' x)"
    file="$(config_digest '{"pause":1}' | cut -d: -f2).json"
    mkdir -p "$dir"
    printf '%s' '{"pause":1}' > "$dir/$file"
    printf '[{"Config":"%s","RepoTags":["registry.example/pause:3.10"],"Pad":"%s"}]' \
        "$file" "$padding" > "$dir/manifest.json"
    (cd "$dir" && tar -cf "$CACHE/pause.tar" -- *)
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive tag that points at an index in containerd is imported" {
    # Pulled from a registry rather than imported: the name points at an
    # index, which has no configuration of its own.
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10 '{"pause":1}'
    containerd_has registry.example/pause:3.10 "$(digest 1)"
    printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}' \
        > "$CTR_CONTENT/$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
}

@test "a docker-archive is imported when containerd cannot give the manifest" {
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10 '{"pause":1}'
    containerd_has registry.example/pause:3.10 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
    grep -qxF "$(digest 1)" "$CTR_CONTENT_LOG"
}

@test "containerd is asked for each manifest once per run" {
    make_docker_archive "$CACHE/a.tar" registry.example/pause:3.10 '{"pause":1}'
    mkdir -p "$CACHE/worker-134-0-0"
    make_docker_archive "$CACHE/worker-134-0-0/b.tar" registry.example/pause:3.10 '{"pause":1}'
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
    [ "$(grep -c . "$CTR_CONTENT_LOG")" -eq 1 ]
}

@test "an archive with an index.json is read as OCI even when it has a manifest.json" {
    # Docker 25 and later write both, and containerd imports from index.json.
    local dir="$TMP/both" config='{"pause":1}'
    write_index "$dir" "{\"schemaVersion\":2,\"manifests\":[{\"digest\":\"$(digest 2)\",\"annotations\":{\"io.containerd.image.name\":\"registry.example/pause:3.10\"}}]}"
    printf '%s' "$config" > "$dir/config.json"
    printf '[{"Config":"config.json","RepoTags":["registry.example/pause:3.10"],"Layers":[]}]' > "$dir/manifest.json"
    tar -cf "$CACHE/pause.tar" -C "$dir" blobs oci-layout index.json manifest.json config.json
    # The manifest.json would match, but not the index.json.
    containerd_has_config registry.example/pause:3.10 "$(digest 1)" "$config"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "pause.tar" "$CTR_LOG"
    [ ! -s "$CTR_CONTENT_LOG" ]
}

@test "an OCI archive does not ask containerd for manifests" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
    [ ! -s "$CTR_CONTENT_LOG" ]
}

@test "OCI archives and docker-archives are skipped side by side" {
    make_archive "$CACHE/etcd.tar" registry.example/etcd:3.5 "$(digest 1)"
    make_docker_archive "$CACHE/pause.tar" registry.example/pause:3.10 '{"pause":1}'
    make_docker_archive "$CACHE/nginx.tar" registry.example/nginx:1 '{"nginx":1}'
    containerd_has registry.example/etcd:3.5 "$(digest 1)"
    containerd_has_config registry.example/pause:3.10 "$(digest 2)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    [ "$(grep -c . "$CTR_LOG")" -eq 1 ]
    grep -q "nginx.tar" "$CTR_LOG"
    [[ "$output" == *"Imported 1 of the 3 cached archives (2 already in containerd, 0 no longer there)"* ]]
}

@test "a later OCI archive holding a name the run just imported is imported too" {
    # containerd holds the name at b's image, as the last run left it. a moves
    # it, so b must go in again, or the name would switch at every run.
    make_archive "$CACHE/a.tar" registry.example/postgres:17 "$(digest 1)"
    make_archive "$CACHE/b.tar" registry.example/postgres:17 "$(digest 2)"
    containerd_has registry.example/postgres:17 "$(digest 2)"
    run_preload
    [ "$status" -eq 0 ]
    [ "$(grep -c . "$CTR_LOG")" -eq 2 ]
    [ "$(sed -n 1p "$CTR_LOG" | grep -c "/a.tar$")" -eq 1 ]
    [ "$(sed -n 2p "$CTR_LOG" | grep -c "/b.tar$")" -eq 1 ]
}

@test "a later docker-archive holding a name the run just imported is imported too" {
    make_docker_archive "$CACHE/a.tar" registry.example/postgres:17 '{"postgres":1}'
    make_docker_archive "$CACHE/b.tar" registry.example/postgres:17 '{"postgres":2}'
    containerd_has_config registry.example/postgres:17 "$(digest 2)" '{"postgres":2}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "/a.tar$" "$CTR_LOG"
    grep -q "/b.tar$" "$CTR_LOG"
}

@test "a name imported from an OCI archive is not taken as present for a later docker-archive" {
    # Short tag: the docker-archive is matched under its normalized name.
    make_archive "$CACHE/a.tar" docker.io/library/postgres:17 "$(digest 1)"
    make_docker_archive "$CACHE/b.tar" postgres:17 '{"postgres":2}'
    containerd_has_config docker.io/library/postgres:17 "$(digest 2)" '{"postgres":2}'
    run_preload
    [ "$status" -eq 0 ]
    grep -q "/a.tar$" "$CTR_LOG"
    grep -q "/b.tar$" "$CTR_LOG"
}

@test "a name imported from a docker-archive is not taken as present for a later OCI archive" {
    make_docker_archive "$CACHE/a.tar" registry.example/postgres:17 '{"postgres":1}'
    make_archive "$CACHE/b.tar" registry.example/postgres:17 "$(digest 2)"
    containerd_has registry.example/postgres:17 "$(digest 2)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "/a.tar$" "$CTR_LOG"
    grep -q "/b.tar$" "$CTR_LOG"
}

@test "an import only forgets the names of the archive it imported" {
    make_archive "$CACHE/a.tar" registry.example/postgres:17 "$(digest 1)"
    make_archive "$CACHE/b.tar" registry.example/redis:7 "$(digest 2)"
    make_docker_archive "$CACHE/c.tar" registry.example/pause:3.10 '{"pause":1}'
    containerd_has registry.example/redis:7 "$(digest 2)"
    containerd_has_config registry.example/pause:3.10 "$(digest 3)" '{"pause":1}'
    run_preload
    [ "$status" -eq 0 ]
    [ "$(grep -c . "$CTR_LOG")" -eq 1 ]
    grep -q "/a.tar$" "$CTR_LOG"
    [[ "$output" == *"Imported 1 of the 3 cached archives (2 already in containerd, 0 no longer there)"* ]]
}

@test "a skipped archive does not make a later one with the same name imported" {
    # Same name, same image in both: nothing moved it, both stay skipped.
    make_archive "$CACHE/a.tar" registry.example/postgres:17 "$(digest 1)"
    make_archive "$CACHE/b.tar" registry.example/postgres:17 "$(digest 1)"
    containerd_has registry.example/postgres:17 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
}

@test "a failed import forgets its names too" {
    # A failed import may still have moved a name before it gave up.
    make_archive "$CACHE/a.tar" registry.example/postgres:17 "$(digest 1)"
    make_archive "$CACHE/b.tar" registry.example/postgres:17 "$(digest 2)"
    containerd_has registry.example/postgres:17 "$(digest 2)"
    fail_on a.tar
    run_preload
    [ "$status" -eq 1 ]
    grep -q "/b.tar$" "$CTR_LOG"
}

@test "an archive whose names cannot be read does not keep the others from being skipped" {
    mkdir -p "$TMP/neither"
    : > "$TMP/neither/layer.tar"
    tar -cf "$CACHE/a.tar" -C "$TMP/neither" layer.tar
    make_archive "$CACHE/b.tar" registry.example/redis:7 "$(digest 2)"
    containerd_has registry.example/redis:7 "$(digest 2)"
    run_preload
    [ "$status" -eq 0 ]
    grep -q "/a.tar$" "$CTR_LOG"
    run ! grep -q "/b.tar$" "$CTR_LOG"
}

@test "an archive whose names cannot be read forgets none of the names read before it" {
    # a is skipped; importing b, whose names are unknown, must not drop a's
    # names, so c, which holds the same image as a, is still skipped.
    make_archive "$CACHE/a.tar" registry.example/postgres:17 "$(digest 1)"
    mkdir -p "$TMP/neither"
    : > "$TMP/neither/layer.tar"
    tar -cf "$CACHE/b.tar" -C "$TMP/neither" layer.tar
    make_archive "$CACHE/c.tar" registry.example/postgres:17 "$(digest 1)"
    containerd_has registry.example/postgres:17 "$(digest 1)"
    run_preload
    [ "$status" -eq 0 ]
    [ "$(grep -c . "$CTR_LOG")" -eq 1 ]
    grep -q "/b.tar$" "$CTR_LOG"
}
