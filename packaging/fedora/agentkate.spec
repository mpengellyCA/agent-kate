Name:           agentkate
Version:        0.1.0
Release:        1%{?dist}
Summary:        Native KDE multi-agent coding arena built on Kate

License:        LGPL-2.0-or-later
URL:            https://github.com/mpengelly/AgentKate
# Produce with: packaging/make-dist.sh (vendors Go deps for offline build).
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  cmake >= 3.20
BuildRequires:  ninja-build
BuildRequires:  gcc-c++
BuildRequires:  golang >= 1.22
BuildRequires:  pkgconfig
BuildRequires:  extra-cmake-modules >= 6.0
BuildRequires:  qt6-qtbase-devel >= 6.6
BuildRequires:  cmake(KF6TextEditor)
BuildRequires:  cmake(KF6SyntaxHighlighting)
BuildRequires:  cmake(KF6XmlGui)
BuildRequires:  cmake(KF6ConfigWidgets)
BuildRequires:  cmake(KF6Config)
BuildRequires:  cmake(KF6CoreAddons)
BuildRequires:  cmake(KF6I18n)
BuildRequires:  cmake(KF6Parts)
BuildRequires:  cmake(KF6WidgetsAddons)
BuildRequires:  desktop-file-utils
BuildRequires:  libappstream-glib

Requires:       konsole
Recommends:     git

%description
AgentKate combines the Kate text editor with an orchestration core for
running multiple coding agents in parallel, with integrated git, LSP,
terminal, and worktree management.

%prep
%autosetup -n %{name}-%{version}

%build
export GOFLAGS="-mod=vendor -trimpath"
export GOPROXY=off
export GOCACHE=%{_builddir}/.gocache
export GOMODCACHE=%{_builddir}/.gomodcache
%cmake -GNinja -DCMAKE_BUILD_TYPE=RelWithDebInfo
%cmake_build

%install
%cmake_install

%check
desktop-file-validate %{buildroot}%{_datadir}/applications/org.kde.agentkate.desktop
appstream-util validate-relax --nonet %{buildroot}%{_datadir}/metainfo/org.kde.agentkate.metainfo.xml

%files
%license LICENSES/LGPL-2.0-or-later.txt
%doc README.md ARCHITECTURE.md
%{_bindir}/agentkate
%{_bindir}/akcore
%{_datadir}/applications/org.kde.agentkate.desktop
%{_datadir}/metainfo/org.kde.agentkate.metainfo.xml
%{_datadir}/icons/hicolor/*/apps/agentkate.*

%changelog
* Sun May 24 2026 Mike Pengelly <mike@leadrix.io> - 0.1.0-1
- Initial package.
