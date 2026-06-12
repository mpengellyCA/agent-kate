# Packaging Agent Kate

Native package recipes for the major Linux distributions. All recipes build
both binaries (`agentkate` UI + `akcore` Go orchestration core) and install
the desktop entry, hicolor icons, and AppStream metainfo.

## Source tarball

For RPM and Debian builds, first generate a release tarball with vendored
Go dependencies (so the package build can run with no network access):

```sh
./packaging/make-dist.sh
# -> dist/agentkate-<version>.tar.gz
```

Arch's `PKGBUILD` pulls the tarball from GitHub and vendors during `prepare()`,
so `make-dist.sh` is not required there (though the `prepare()` step needs
network access for `go mod vendor`).

## Common build dependencies

| Component                 | Notes                                  |
| ------------------------- | -------------------------------------- |
| CMake ≥ 3.20, Ninja       | Build system                           |
| GCC/Clang with C++20      | UI is C++20                            |
| Go ≥ 1.22                 | Builds the `akcore` orchestration core |
| Qt 6 ≥ 6.6                | Core, Gui, Widgets, Network            |
| KDE Frameworks 6 ≥ 6.0    | TextEditor, SyntaxHighlighting, XmlGui, ConfigWidgets, Config, CoreAddons, I18n, Parts, WidgetsAddons |
| Konsole                   | Runtime — embedded terminal panel      |

---

## Fedora

The quickest path is the helper script, which vendors Go deps, builds the RPM
with `rpmbuild`, and installs/upgrades it via `dnf` in one step:

```sh
sudo dnf install rpm-build cmake ninja-build golang \
    extra-cmake-modules qt6-qtbase-devel \
    kf6-ktexteditor-devel kf6-syntax-highlighting-devel kf6-kxmlgui-devel \
    kf6-kconfigwidgets-devel kf6-kconfig-devel kf6-kcoreaddons-devel \
    kf6-ki18n-devel kf6-kparts-devel kf6-kwidgetsaddons-devel \
    desktop-file-utils libappstream-glib
scripts/ak install            # build the RPM and install/upgrade in place
```

To build the RPM by hand instead:

```sh
# Install build deps
sudo dnf install rpm-build rpmdevtools cmake ninja-build golang \
    extra-cmake-modules qt6-qtbase-devel \
    kf6-ktexteditor-devel kf6-syntax-highlighting-devel kf6-kxmlgui-devel \
    kf6-kconfigwidgets-devel kf6-kconfig-devel kf6-kcoreaddons-devel \
    kf6-ki18n-devel kf6-kparts-devel kf6-kwidgetsaddons-devel \
    desktop-file-utils libappstream-glib

# Build
rpmdev-setuptree
./packaging/make-dist.sh
cp dist/agentkate-0.1.0.tar.gz ~/rpmbuild/SOURCES/
rpmbuild -ba packaging/fedora/agentkate.spec
```

The resulting RPM lands in `~/rpmbuild/RPMS/<arch>/`.

## Debian / Ubuntu

```sh
sudo apt install build-essential debhelper dh-make cmake ninja-build \
    golang-go pkg-config extra-cmake-modules qt6-base-dev qt6-tools-dev \
    libkf6texteditor-dev libkf6syntaxhighlighting-dev libkf6xmlgui-dev \
    libkf6configwidgets-dev libkf6config-dev libkf6coreaddons-dev \
    libkf6i18n-dev libkf6parts-dev libkf6widgetsaddons-dev

./packaging/make-dist.sh
mkdir -p build-deb && cd build-deb
tar xf ../dist/agentkate-0.1.0.tar.gz
cd agentkate-0.1.0
cp -r ../../packaging/debian .
dpkg-buildpackage -us -uc -b
# .deb appears in build-deb/
```

The same recipe works on Ubuntu 24.04+ (which ships KF6).

## Arch Linux / CachyOS

CachyOS uses Arch's package format and repos, so the same `PKGBUILD` works.

```sh
cp packaging/arch/PKGBUILD .
# (Optional) update the source URL to a local tarball during development:
#   source=("agentkate-$pkgver.tar.gz::file://$PWD/../dist/agentkate-$pkgver.tar.gz")
makepkg -si
```

For an AUR submission, regenerate `sha256sums` with `updpkgsums` and drop the
`SKIP` placeholder.

---

## Smoke-test an installed package

```sh
which agentkate akcore        # both should be in /usr/bin
agentkate &                   # KDE app menu also shows Agent Kate
```

The UI spawns `akcore` from its own directory, so as long as both binaries
land in the same `bindir` (the install rules guarantee this), the runtime
handshake succeeds.
