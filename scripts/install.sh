#!/usr/bin/env bash
# Install or upgrade Agent Kate system-wide. On Arch and Fedora this builds a
# native package (if needed) and installs it with the system package manager,
# so it is cleanly tracked and upgraded in place — running this again upgrades.
#
#   scripts/install.sh             # install, or upgrade if already installed
#   scripts/install.sh --rebuild   # force a fresh package build first
#
# On other distros it falls back to `cmake --install` from ./build into
# a prefix (default /usr/local; override with PREFIX=...).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

REBUILD=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --rebuild) REBUILD=1 ;;
        -h|--help) grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; 1d'; exit 0 ;;
        *) die "unknown option: $1 (try --help)" ;;
    esac
    shift
done

cd "$ROOT"
VERSION="$(project_version)"
FAMILY="$(distro_family)"

# Pick the right privilege-escalation helper.
SUDO=""
if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null 2>&1; then SUDO="sudo"
    elif command -v doas >/dev/null 2>&1; then SUDO="doas"
    else die "need root (or sudo/doas) to install system-wide"; fi
fi

# Locate a freshly-built package, building one if missing or --rebuild was given.
# Sets PKG.
ensure_package() {
    PKG="$(find_built_package "$FAMILY" "$VERSION")"
    if [[ "$REBUILD" == 1 || -z "$PKG" ]]; then
        step "Building package"
        "${ROOT}/scripts/package.sh"
        PKG="$(find_built_package "$FAMILY" "$VERSION")"
    else
        info "Using existing package: ${C_BOLD}$(basename "$PKG")${C_RESET}"
        info "(pass ${C_BOLD}--rebuild${C_RESET} to rebuild from current source)"
    fi
    [[ -n "$PKG" ]] || die "no package found to install"
}

if [[ "$FAMILY" == "arch" ]]; then
    require_cmd pacman
    ensure_package

    if pacman -Q agentkate >/dev/null 2>&1; then
        local_ver="$(pacman -Q agentkate | awk '{print $2}')"
        step "Upgrading Agent Kate (installed: ${local_ver})"
    else
        step "Installing Agent Kate ${VERSION}"
    fi
    # pacman -U installs or upgrades to the given package; same pkgname = upgrade.
    $SUDO pacman -U --noconfirm "$PKG"

    step "Done"
    ok "Installed: ${C_BOLD}$(pacman -Q agentkate)${C_RESET}"
    info "Launch from your app menu, run ${C_BOLD}agentkate${C_RESET}, or ${C_BOLD}scripts/ak run${C_RESET}."
    exit 0
fi

if [[ "$FAMILY" == "fedora" ]]; then
    require_cmd dnf
    ensure_package

    # Compare the installed version-release against the package we built so we
    # can pick the right dnf verb: install/upgrade for a different version, but
    # reinstall for an identical one (dnf install treats that as a no-op).
    pkg_nvr="$(rpm -qp --qf '%{VERSION}-%{RELEASE}' "$PKG" 2>/dev/null)"
    if installed_nvr="$(rpm -q --qf '%{VERSION}-%{RELEASE}' agentkate 2>/dev/null)" \
        && [[ "$installed_nvr" == "$pkg_nvr" ]]; then
        step "Reinstalling Agent Kate ${installed_nvr} (already installed)"
        # `dnf install` is a no-op for the same version, so force a reinstall.
        $SUDO dnf reinstall -y "$PKG"
    else
        if [[ -n "$installed_nvr" ]]; then
            step "Upgrading Agent Kate (installed: ${installed_nvr})"
        else
            step "Installing Agent Kate ${VERSION}"
        fi
        # `dnf install` on a local .rpm installs or upgrades in place and pulls
        # in any missing runtime dependencies from the repos.
        $SUDO dnf install -y "$PKG"
    fi

    step "Done"
    ok "Installed: ${C_BOLD}$(rpm -q agentkate)${C_RESET}"
    info "Launch from your app menu, run ${C_BOLD}agentkate${C_RESET}, or ${C_BOLD}scripts/ak run${C_RESET}."
    exit 0
fi

# --- other distros: cmake --install from ./build ----------------------------
PREFIX="${PREFIX:-/usr/local}"
BUILD_DIR="${BUILD_DIR:-build}"
warn "No native-package installer for distro family '${FAMILY}'."
info "Falling back to a direct ${C_BOLD}cmake --install${C_RESET} into ${C_BOLD}${PREFIX}${C_RESET}."

if [[ ! -d "$BUILD_DIR" || "$REBUILD" == 1 ]]; then
    step "Building (RelWithDebInfo)"
    BUILD_TYPE="RelWithDebInfo" "${ROOT}/scripts/build.sh"
fi

step "Installing into ${PREFIX}"
$SUDO cmake --install "$BUILD_DIR" --prefix "$PREFIX"

step "Done"
ok "Installed to ${C_BOLD}${PREFIX}${C_RESET}"
info "Ensure ${C_BOLD}${PREFIX}/bin${C_RESET} is on your PATH, then run ${C_BOLD}agentkate${C_RESET}."
info "Uninstall later with ${C_BOLD}scripts/uninstall.sh${C_RESET}."
