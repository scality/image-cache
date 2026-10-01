NAME    := containerd-image-preload
CTL     := imagecachectl
VERSION ?= 0.0.0
EL      ?= 9

# Rocky Linux base images per EL version, pinned by digest and managed manually.
# Kept per-major so a bump never crosses a major release.
ROCKY_8 := docker.io/rockylinux/rockylinux:8.10@sha256:e8a49c5403b687db05d4d67333fa45808fbe74f36e683cec7abb1f7d0f2338c6
ROCKY_9 := docker.io/rockylinux/rockylinux:9.8@sha256:8101994123cf3d0a8fee517bee7f39e555c7d92bd2d9eb3303cc988a0eeed00f
BUILD_IMAGE := $(ROCKY_$(EL))
IMAGE       := image-cache-rpmbuild:el$(EL)

# The imagecachectl spec is ExclusiveArch x86_64, so the toolchain container
# has to be that architecture too, whatever the developer's machine is.
PLATFORM   := linux/amd64
DOCKER_RUN := docker run --rm --platform $(PLATFORM) -v "$(CURDIR)":/work -w /work

.PHONY: image test rpm rpm-preload rpm-ctl ctl-binary lint clean

## Build the container image holding the RPM build toolchain (EL=8 or EL=9).
image:
	docker build --platform $(PLATFORM) -f build.Dockerfile --build-arg BUILD_IMAGE=$(BUILD_IMAGE) -t $(IMAGE) .

## Run the full package validation (bats: shellcheck, unit tests, rpmlint, assertions).
test: image ctl-binary
	$(DOCKER_RUN) -e VERSION=$(VERSION) $(IMAGE) bats test/

## Build every RPM and drop them under _build/ (owned by the host user).
rpm: rpm-preload rpm-ctl

rpm-preload: image
	$(DOCKER_RUN) -e VERSION=$(VERSION) \
		-e TARGET_UID=$$(id -u) -e TARGET_GID=$$(id -g) \
		$(IMAGE) bash build.sh /work/_build

## The command is compiled here and not in the RPM image: that image carries
## the packaging toolchain and no Go, and adding one would make every RPM
## build pull a compiler it does not need.
ctl-binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		$(MAKE) -C ../agent build-imagecachectl OUT=$(CURDIR)/sources/$(CTL)

rpm-ctl: image ctl-binary
	$(DOCKER_RUN) -e VERSION=$(VERSION) -e NAME=$(CTL) \
		-e TARGET_UID=$$(id -u) -e TARGET_GID=$$(id -g) \
		$(IMAGE) bash build.sh /work/_build

## Lint only (shellcheck + rpmlint on the spec).
lint: image
	$(DOCKER_RUN) $(IMAGE) bash -c \
		'shellcheck sources/$(NAME).sh build.sh && rpmlint -f rpmlintrc $(NAME).spec $(CTL).spec'

clean:
	rm -rf _build sources/$(CTL)
