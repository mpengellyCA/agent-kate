#!/usr/bin/env bash
# Remove an installed Agent Kate. On Arch this uses pacman and on Fedora dnf;
# otherwise it removes the files recorded in build/install_manifest.txt.
#
#   scripts/uninstall.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

cd "$ROOT"
FAMILY="$(distro_family)"

SUDO=""
if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null 2>&1; then SUDO="sudo"
    elif command -v doas >/dev/null 2>&1; then SUDO="doas"
    else die "need root (or sudo/doas) to uninstall"; fi
fi

if [[ "$FAMILY" == "arch" ]] && command -v pacman >/dev/null 2>&1; then
    if ! pacman -Q agentkate >/dev/null 2>&1; then
        warn "agentkate is not installed via pacman — nothing to do."
        exit 0
    fi
    step "Removing Agent Kate (pacman)"
    $SUDO pacman -R --noconfirm agentkate
    ok "Removed."
    exit 0
fi

if [[ "$FAMILY" == "fedora" ]] && command -v dnf >/dev/null 2>&1; then
    if ! rpm -q agentkate >/dev/null 2>&1; then
        warn "agentkate is not installed via dnf/rpm — nothing to do."
        exit 0
    fi
    step "Removing Agent Kate (dnf)"
    $SUDO dnf remove -y agentkate
    ok "Removed."
    exit 0
fi

# --- other distros: use the install manifest --------------------------------
BUILD_DIR="${BUILD_DIR:-build}"
MANIFEST="${BUILD_DIR}/install_manifest.txt"
[[ -f "$MANIFEST" ]] || die "no install manifest at ${MANIFEST}; cannot uninstall a cmake --install build."

step "Removing files from ${MANIFEST}"
count=0
while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    if [[ -e "$f" || -L "$f" ]]; then
        $SUDO rm -f "$f" && count=$((count+1))
    fi
done < "$MANIFEST"
ok "Removed ${count} files."
