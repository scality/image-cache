#!/usr/bin/env bats
# Unit tests for the containerd-image-preload import script (with a stubbed `ctr`).

setup() {
    SCRIPT="${BATS_TEST_DIRNAME}/../sources/containerd-image-preload.sh"
    TMP="$(mktemp -d)"
    CACHE="$TMP/cache"
    BIN="$TMP/bin"
    CTR_LOG="$TMP/ctr.log"
    mkdir -p "$CACHE" "$BIN"
    : > "$CTR_LOG"
    cat > "$BIN/ctr" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$CTR_LOG"
EOF
    chmod +x "$BIN/ctr"
}

teardown() {
    rm -rf "$TMP"
}

@test "imports .tar files and ignores the rest" {
    : > "$CACHE/a.tar"
    : > "$CACHE/b.tar"
    : > "$CACHE/notes.txt"
    run env "PATH=$BIN:$PATH" "IMAGE_CACHE_DIR=$CACHE" bash "$SCRIPT"
    [ "$status" -eq 0 ]
    grep -q "a.tar" "$CTR_LOG"
    grep -q "b.tar" "$CTR_LOG"
    ! grep -q "notes.txt" "$CTR_LOG"
}

@test "empty cache directory imports nothing and succeeds" {
    run env "PATH=$BIN:$PATH" "IMAGE_CACHE_DIR=$CACHE" bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [ ! -s "$CTR_LOG" ]
}

@test "uses linux/amd64 platform by default" {
    : > "$CACHE/a.tar"
    run env "PATH=$BIN:$PATH" "IMAGE_CACHE_DIR=$CACHE" bash "$SCRIPT"
    [ "$status" -eq 0 ]
    grep -q -- "--platform linux/amd64" "$CTR_LOG"
}

@test "IMAGE_PLATFORM overrides the platform" {
    : > "$CACHE/a.tar"
    run env "PATH=$BIN:$PATH" "IMAGE_CACHE_DIR=$CACHE" "IMAGE_PLATFORM=linux/arm64" bash "$SCRIPT"
    [ "$status" -eq 0 ]
    grep -q -- "--platform linux/arm64" "$CTR_LOG"
}
