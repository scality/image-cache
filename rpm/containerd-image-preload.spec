Name:           containerd-image-preload
Version:        _VERSION_
Release:        1%{?dist}
Summary:        Pre-load container images from a local cache into containerd

License:        ASL 2.0
URL:            https://github.com/scality/image-cache
Source0:        containerd-image-preload.sh
Source1:        containerd-image-preload.service
Source2:        containerd-image-preload.timer
Source3:        containerd-image-preload.sysconfig

BuildArch:      noarch
Requires:       containerd

%description
A systemd service and timer that periodically import container image tarballs
from a local cache directory into containerd's image store, so that nodes can
recover lost images without depending on a registry or Kubernetes being
available.

The package installs the units but does not enable them; enabling the timer is
left to the provisioning layer.

%install
install -D -m 0755 %{SOURCE0} %{buildroot}%{_bindir}/containerd-image-preload
install -D -m 0644 %{SOURCE1} %{buildroot}%{_unitdir}/containerd-image-preload.service
install -D -m 0644 %{SOURCE2} %{buildroot}%{_unitdir}/containerd-image-preload.timer
install -D -m 0644 %{SOURCE3} %{buildroot}%{_sysconfdir}/sysconfig/containerd-image-preload

%files
%{_bindir}/containerd-image-preload
%{_unitdir}/containerd-image-preload.service
%{_unitdir}/containerd-image-preload.timer
%config(noreplace) %{_sysconfdir}/sysconfig/containerd-image-preload

%changelog
* Wed Jul 08 2026 Alex Rodriguez <alex.rodriguez@scality.com> - 0.1.0-1
- Initial package
