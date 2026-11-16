# kubectl is the only tool agent/Makefile does not download. Same image as CI.
# renovate: datasource=docker depName=registry.k8s.io/kubectl
ARG KUBECTL_VERSION=v1.37.1@sha256:b7cab618e281b1ee7484e7b706a96e2135fbb6e072c2a573a7dab4e87d7f2385
FROM registry.k8s.io/kubectl:${KUBECTL_VERSION} AS kubectl

FROM mcr.microsoft.com/devcontainers/go:1.27-trixie@sha256:c83c320b68e27dc36903cd5ee07394870c5330dfd77b0897e78c7e1aa00ddfd0
# The devcontainers image tag has no Go patch version, and the image sets
# GOTOOLCHAIN=local. Take Go from the Dockerfile's golang image, so the
# patch is pinned and matches go.mod.
COPY --from=golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 /usr/local/go /usr/local/go

COPY --from=kubectl /bin/kubectl /usr/local/bin/kubectl

USER vscode
