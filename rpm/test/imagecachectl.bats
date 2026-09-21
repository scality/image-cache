#!/usr/bin/env bats
# Assertions on the built (and installed) imagecachectl RPM.

NAME=imagecachectl

# The binary is compiled on the host before the container runs, because this
# image carries the packaging toolchain and no Go.
setup_file() {
    # A failure and not a skip: a skipped file still exits 0, which would let
    # the whole package go unasserted the day the Go step stops running.
    if [ ! -f "${BATS_TEST_DIRNAME}/../sources/${NAME}" ]; then
        echo "sources/${NAME} is missing: 'make test' builds it, and 'make ctl-binary'" >&2
        echo "builds it on its own when bats is called directly." >&2
        return 1
    fi
    RPM="$(NAME="$NAME" VERSION="${VERSION:-0.0.0}" bash "${BATS_TEST_DIRNAME}/../build.sh")"
    rpm -i --nodeps "$RPM"
    export RPM
}

@test "package name is imagecachectl" {
    [ "$(rpm -qp --qf '%{NAME}' "$RPM")" = "$NAME" ]
}

# A compiled binary, unlike the preload package next to it.
@test "package architecture is x86_64" {
    [ "$(rpm -qp --qf '%{ARCH}' "$RPM")" = "x86_64" ]
}

@test "license is ASL 2.0" {
    [ "$(rpm -qp --qf '%{LICENSE}' "$RPM")" = "ASL 2.0" ]
}

# Statically linked, so nothing is pulled in behind it. Left unchecked, a
# build that lost CGO_ENABLED=0 would ship a package that installs on the
# build host and fails on a node.
@test "package requires nothing" {
    run rpm -qpR "$RPM"
    [ "$status" -eq 0 ]
    [ -z "$(echo "$output" | grep -v '^rpmlib(' | grep -v '^$' || true)" ]
}

@test "package ships only the command" {
    run rpm -qlp "$RPM"
    [ "$status" -eq 0 ]
    [ "$output" = "/usr/bin/imagecachectl" ]
}

@test "the installed command answers with its usage" {
    run /usr/bin/imagecachectl
    [ "$status" -eq 2 ]
    echo "$output" | grep -q "imagecachectl import"
}

# The usage ends on an Options: heading the flag list fills in, and the help
# request succeeds rather than reporting a mistake.
@test "the installed command prints its flags under help" {
    run /usr/bin/imagecachectl --help
    [ "$status" -eq 0 ]
    echo "$output" | grep -q -- "-cache-path"
    echo "$output" | grep -q -- "-name"
    [ "$(echo "$output" | grep -c "Usage: imagecachectl")" -eq 1 ]
}

@test "the installed command refuses a source without a name" {
    run /usr/bin/imagecachectl import /tmp/whatever.tar
    [ "$status" -eq 2 ]
    echo "$output" | grep -q -- "--name"
}

# CONTRIBUTING says every spec passes rpmlint, and nothing was running it on
# this one. The exit status alone is not the gate: rpmlint returns 0 with
# warnings, so the summary line is what the name of this test refers to.
@test "rpmlint reports no errors or warnings" {
    run rpmlint -f "${BATS_TEST_DIRNAME}/../rpmlintrc" \
        "${BATS_TEST_DIRNAME}/../imagecachectl.spec" "$RPM"
    echo "$output"
    [ "$status" -eq 0 ]
    echo "$output" | grep -qE "0 errors, 0 warnings"
}
