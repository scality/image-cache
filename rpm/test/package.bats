#!/usr/bin/env bats
# Assertions on the built (and installed) containerd-image-preload RPM.

NAME=containerd-image-preload

# Build the RPM once and install it into the container for the whole file.
setup_file() {
    RPM="$(VERSION="${VERSION:-0.0.0}" bash "${BATS_TEST_DIRNAME}/../build.sh")"
    rpm -i --nodeps "$RPM"
    export RPM
}

# A linter's whole value is its diagnostic, and `run` captures it instead of
# letting CI show it. Let the message out before asserting on the status.
assert_linter_clean() {
    if [ "$status" -ne 0 ]; then
        echo "$output" >&2
    fi
    [ "$status" -eq 0 ]
}

@test "package name is containerd-image-preload" {
    [ "$(rpm -qp --qf '%{NAME}' "$RPM")" = "$NAME" ]
}

@test "package architecture is noarch" {
    [ "$(rpm -qp --qf '%{ARCH}' "$RPM")" = "noarch" ]
}

@test "license is ASL 2.0" {
    [ "$(rpm -qp --qf '%{LICENSE}' "$RPM")" = "ASL 2.0" ]
}

@test "package requires containerd" {
    rpm -qpR "$RPM" | grep -qx "containerd"
}

@test "package ships the expected files" {
    run rpm -qlp "$RPM"
    [ "$status" -eq 0 ]
    grep -qxF "/usr/bin/$NAME" <<<"$output"
    grep -qxF "/usr/lib/systemd/system/$NAME.service" <<<"$output"
    grep -qxF "/usr/lib/systemd/system/$NAME.timer" <<<"$output"
    grep -qxF "/etc/sysconfig/$NAME" <<<"$output"
}

@test "sysconfig is marked config(noreplace)" {
    flags=$(rpm -qp --qf '[%{filenames} %{fileflags:fflags}\n]' "$RPM" \
        | awk -v f="/etc/sysconfig/$NAME" '$1==f{print $2}')
    [[ "$flags" == *c* ]]
    [[ "$flags" == *n* ]]
}

@test "rpmlint reports no errors or warnings" {
    cd "${BATS_TEST_DIRNAME}/.."
    run rpmlint -f rpmlintrc "$RPM"
    assert_linter_clean
}

@test "installed script is executable" {
    test -x "/usr/bin/$NAME"
}

@test "systemd units are valid apart from the absent containerd.service" {
    output=$(systemd-analyze verify \
        "/usr/lib/systemd/system/$NAME.service" \
        "/usr/lib/systemd/system/$NAME.timer" 2>&1 || true)
    # The only tolerated message is the missing containerd.service dependency.
    remaining=$(printf '%s\n' "$output" | grep -v 'containerd.service' || true)
    [ -z "$remaining" ]
}

@test "package does not enable the timer" {
    [ ! -L "/etc/systemd/system/timers.target.wants/$NAME.timer" ]
}

@test "shell scripts pass shellcheck" {
    cd "${BATS_TEST_DIRNAME}/.."
    run shellcheck "sources/$NAME.sh" build.sh
    assert_linter_clean
}

@test "bats files pass shellcheck" {
    # ShellCheck learned to parse bats in 0.9.0, and Rocky 8 ships 0.6.0, which
    # stops at the first @test. The gate holds on the EL9 leg of the matrix.
    local version
    version="$(shellcheck --version | awk '/^version:/ { print $2 }')"
    # An empty version means no shellcheck at all, which must fail rather than
    # turn this gate into a silent skip.
    [ -n "$version" ]
    if [ "$(printf '%s\n0.9.0\n' "$version" | sort -V | head -n 1)" != "0.9.0" ]; then
        skip "shellcheck $version predates bats support"
    fi
    cd "${BATS_TEST_DIRNAME}/.."
    run shellcheck test/*.bats
    assert_linter_clean
}

@test "a release tag is normalized into a valid RPM version" {
    # Release tags look like v1.2.3 or v1.2.3-alpha.4; RPM forbids '-' in Version
    # and drops the leading 'v'. The hyphen becomes '~' so pre-releases sort
    # before the final version.
    rpm_path="$(VERSION=v9.9.9-alpha.7 bash "${BATS_TEST_DIRNAME}/../build.sh")"
    [ "$(rpm -qp --qf '%{VERSION}' "$rpm_path")" = "9.9.9~alpha.7" ]
}
