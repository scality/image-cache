# Build the manager binary
# Override BASE_IMAGE to build from another registry. Keep a digest and a Go
# version no older than the go directive of go.mod, e.g.
# <mirror>/library/golang:<go.mod version>-alpine@sha256:<digest>.
ARG BASE_IMAGE=golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS=linux
# The agent only ships for amd64 (see DESIGN.md).
ARG TARGETARCH=amd64

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager ./cmd/manager

# The agent runs as UID 65532 and writes in a root-owned cache path, so it
# needs cap_dac_override as a file capability. See "Container image and
# deployment" in DESIGN.md. BuildKit keeps the xattr across the COPY below.
RUN apk add --no-cache libcap \
    && setcap cap_dac_override+ep manager \
    && getcap manager

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
