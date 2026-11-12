# Build image for the containerd-image-preload RPM.
# Provides the RHEL-family RPM build toolchain (rpm-build + rpmlint) plus
# shellcheck and systemd to validate the shipped script and unit files, and
# jq and tar, which the script runs and its unit tests run for real.
# The base image (Rocky Linux 8 or 9) is provided by the caller; see the Makefile.
ARG BUILD_IMAGE
FROM ${BUILD_IMAGE}

RUN dnf install -y --setopt=skip_missing_names_on_install=False \
        rpm-build rpmlint rpmdevtools systemd jq tar && \
    dnf install -y epel-release && \
    dnf install -y ShellCheck bats && \
    dnf clean all

WORKDIR /work
