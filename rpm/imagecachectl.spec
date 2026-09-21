Name:           imagecachectl
Version:        _VERSION_
Release:        1%{?dist}
Summary:        Fill a node's image cache from a registry or a local archive

License:        ASL 2.0
URL:            https://github.com/scality/image-cache
Source0:        imagecachectl

# A compiled binary, unlike the preload package next to it, which ships a
# script and stays noarch.
ExclusiveArch:  x86_64

# One static binary and nothing else. Without this, rpmbuild adds the
# /usr/lib/.build-id links it generates for any ELF file, which serve
# debuginfo this package does not ship.
%global _build_id_links none

%description
A one-shot command that fills a node's local image cache with the archives a
boot cache image carries, pulled from a registry or read from a docker archive.

It does what the image-cache agent does, once, for a node being installed:
there is no Kubernetes yet to run the agent in, and the kubelet needs its
images before it starts.

%install
install -D -m 0755 %{SOURCE0} %{buildroot}%{_bindir}/imagecachectl

%files
%{_bindir}/imagecachectl

%changelog
* Mon Sep 21 2026 Alex Rodriguez <alex.rodriguez@scality.com> - 0.1.0-1
- Initial package
