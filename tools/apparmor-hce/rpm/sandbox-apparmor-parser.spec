Name:           sandbox-apparmor-parser
Version:        4.1.7
Release:        1%{?dist}
Summary:        AppArmor policy parser for sandbox workloads
License:        GPL-2.0-or-later
URL:            https://apparmor.net/
Source0:        apparmor-%{version}.tar.gz
ExclusiveArch:  x86_64 aarch64
BuildRequires:  rpm-build
BuildRequires:  gcc
BuildRequires:  make
BuildRequires:  bison
BuildRequires:  flex
BuildRequires:  pcre2-devel

%description
The AppArmor parser used to compile and validate sandbox policy supplied by
the workload runtime.  This package contains no host policy and does not
configure a service or runtime daemon.

%prep
%autosetup -n apparmor-%{version}

%build
make -C libraries/libapparmor
make -C libraries/libapparmor check
make -C parser

%install
rm -rf %{buildroot}
install -D -m 0755 parser/apparmor_parser %{buildroot}%{_sbindir}/apparmor_parser

%files
%license LICENSE
%{_sbindir}/apparmor_parser

%changelog
* Thu Jan 01 1970 HCE Build Service <noreply@example.invalid> - 4.1.7-1
- Package the AppArmor parser for sandbox policy compilation.
