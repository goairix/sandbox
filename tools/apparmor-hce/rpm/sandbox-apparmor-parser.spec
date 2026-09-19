%global debug_package %{nil}
%global _build_id_links none
%global _licensedir /usr/share/licenses
Name:           sandbox-apparmor-parser
Version:        4.1.7
Release:        1
Summary:        AppArmor policy parser for sandbox workloads
License:        GPL-2.0-only AND LGPL-2.1-or-later
URL:            https://apparmor.net/
Source0:        apparmor-v%{version}.tar.gz
Source1:        ax_check_compile_flag.m4
ExclusiveArch:  x86_64 aarch64
BuildRequires:  rpm-build
BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  libstdc++-static
BuildRequires:  make
BuildRequires:  bison
BuildRequires:  flex
BuildRequires:  autoconf
BuildRequires:  automake
BuildRequires:  libtool
BuildRequires:  pkgconfig
BuildRequires:  dejagnu
BuildRequires:  perl
BuildRequires:  perl-Test-Simple
BuildRequires:  perl-Pod-Checker
BuildRequires:  perl-podlators
BuildRequires:  python3
BuildRequires:  binutils-extra

%description
The AppArmor parser used to compile sandbox policy in userspace. This package
contains no policy, service configuration, or lifecycle scriptlets. Local build
artifacts are unsigned testing artifacts and require a production signing gate.

%prep
%autosetup -n apparmor-v%{version}
install -D -m 0644 %{SOURCE1} libraries/libapparmor/m4/ax_check_compile_flag.m4

%build
cd libraries/libapparmor
ACLOCAL_PATH="$PWD/m4${ACLOCAL_PATH:+:$ACLOCAL_PATH}" sh ./autogen.sh
%configure --disable-man-pages --without-python --without-perl --without-ruby
make %{?_smp_mflags}
cd ../..
# Upstream links libapparmor and libstdc++ statically into the parser.
make -C parser %{?_smp_mflags} apparmor_parser

%check
make -C libraries/libapparmor check
make -C parser check

%install
install -D -m 0755 parser/apparmor_parser %{buildroot}%{_sbindir}/apparmor_parser

%files
%defattr(-,root,root,-)
%license parser/COPYING.GPL libraries/libapparmor/COPYING.LGPL
%{_sbindir}/apparmor_parser

%changelog
* Sat Sep 19 2026 HCE Build Service <noreply@example.invalid> - 4.1.7-1
- Package only the parser and its licenses, with library/parser checks.
