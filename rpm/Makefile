NAME    := containerd-image-preload
VERSION ?= 0.0.0
EL      ?= 9

# Rocky Linux base images per EL version, pinned by digest and managed manually.
# Kept per-major so a bump never crosses a major release.
ROCKY_8 := docker.io/rockylinux/rockylinux:8.10@sha256:e8a49c5403b687db05d4d67333fa45808fbe74f36e683cec7abb1f7d0f2338c6
ROCKY_9 := docker.io/rockylinux/rockylinux:9.8@sha256:8101994123cf3d0a8fee517bee7f39e555c7d92bd2d9eb3303cc988a0eeed00f
BUILD_IMAGE := $(ROCKY_$(EL))
IMAGE       := image-cache-rpmbuild:el$(EL)

DOCKER_RUN := docker run --rm -v "$(CURDIR)":/work -w /work

.PHONY: image test rpm lint clean

## Build the container image holding the RPM build toolchain (EL=8 or EL=9).
image:
	docker build -f build.Dockerfile --build-arg BUILD_IMAGE=$(BUILD_IMAGE) -t $(IMAGE) .

## Run the full package validation (bats: shellcheck, unit tests, rpmlint, assertions).
test: image
	$(DOCKER_RUN) -e VERSION=$(VERSION) $(IMAGE) bats test/

## Build the RPM and drop it under _build/ (owned by the host user).
rpm: image
	$(DOCKER_RUN) -e VERSION=$(VERSION) \
		-e TARGET_UID=$$(id -u) -e TARGET_GID=$$(id -g) \
		$(IMAGE) bash build.sh /work/_build

## Lint only (shellcheck + rpmlint on the spec).
lint: image
	$(DOCKER_RUN) $(IMAGE) bash -c \
		'shellcheck sources/$(NAME).sh build.sh && rpmlint -f rpmlintrc $(NAME).spec'

clean:
	rm -rf _build
