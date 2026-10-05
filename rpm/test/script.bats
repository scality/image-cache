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
    mkdir -p "$CACHE" "$BIN"
    : > "$CTR_LOG"
    : > "$CTR_FAIL"
    : > "$CTR_RM"
    # The stub logs every call, and exits non-zero on the archives listed in
    # $CTR_FAIL, the way containerd reports one it cannot read. The archive is
    # the last argument, and is matched by its path inside the cache: the same
    # file name legitimately lives flat and in a subdirectory.
    cat > "$BIN/ctr" <<EOF
#!/usr/bin/env bash
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
    [[ "$stderr" == *"Failed to import 2 of the 4 cached archives (2 imported, 0 no longer there)"* ]]
    grep -q "flat-good.tar" "$CTR_LOG"
    grep -q "worker-134-0-0/nested-good.tar" "$CTR_LOG"
}

@test "a cache where every archive fails reports as many as it found" {
    fail_on a.tar b.tar
    : > "$CACHE/a.tar"
    : > "$CACHE/b.tar"
    run_preload
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"Failed to import 2 of the 2 cached archives (0 imported, 0 no longer there)"* ]]
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
    [[ "$stderr" == *"Failed to import 1 of the 3 cached archives (1 imported, 1 no longer there)"* ]]
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
    [[ "$stderr" == *"Failed to import 1 of the 2 cached archives (1 imported, 0 no longer there)"* ]]
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
    [[ "$stderr" == *"Failed to import 2 of the 3 cached archives (0 imported, 1 no longer there)"* ]]
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
    [[ "$output" == *"Imported 2 of the 2 cached archives (0 no longer there)"* ]]
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
    grep -qxF "<3>Failed to import 1 of the 2 cached archives (1 imported, 0 no longer there)" "$TMP/journal"
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
}
