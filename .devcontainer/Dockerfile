# kubectl is the only tool agent/Makefile does not download. Same image as CI.
# renovate: datasource=docker depName=registry.k8s.io/kubectl
ARG KUBECTL_VERSION=v1.37.1@sha256:b7cab618e281b1ee7484e7b706a96e2135fbb6e072c2a573a7dab4e87d7f2385
FROM registry.k8s.io/kubectl:${KUBECTL_VERSION} AS kubectl

FROM mcr.microsoft.com/devcontainers/go:1.27-trixie@sha256:32aad7e9eefed4fe99987cc4efef2f9723bd57c556f171143ecf65e0ba855ff3

COPY --from=kubectl /bin/kubectl /usr/local/bin/kubectl

USER vscode
