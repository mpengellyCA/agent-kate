#!/usr/bin/env bash
# Shared helpers for the Agent Kate build/package/install scripts.
# Source this from another script: `source "$(dirname "$0")/lib.sh"`.
#
# Provides: coloured logging (info/ok/warn/err/die/step), command checks,
# the project root, the project version, and simple distro detection.

# --- locate the project root (the dir containing this scripts/ folder) ------
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${LIB_DIR}/.." && pwd)"
export ROOT

# --- colours (disabled when not writing to a terminal or NO_COLOR is set) ---
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'
    C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
    C_BLUE=$'\033[34m'; C_CYAN=$'\033[36m'
else
    C_RESET=''; C_BOLD=''; C_DIM=''
    C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_CYAN=''
fi

step() { printf '\n%s==>%s %s%s%s\n' "$C_BLUE$C_BOLD" "$C_RESET" "$C_BOLD" "$*" "$C_RESET"; }
info() { printf '%s•%s %s\n' "$C_CYAN" "$C_RESET" "$*"; }
ok()   { printf '%s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf '%s✗%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
die()  { err "$@"; exit 1; }

# require_cmd <cmd> [hint] — abort with a helpful message if a tool is missing.
require_cmd() {
    local cmd="$1" hint="${2:-}"
    if ! command -v "$cmd" >/dev/null 2>&1; then
        err "required tool not found: ${C_BOLD}${cmd}${C_RESET}"
        [[ -n "$hint" ]] && info "  $hint"
        exit 1
    fi
}

# Project version: MAJOR.MINOR comes from CMakeLists.txt; the patch component is
# the number of first-parent commits on the current branch, so every commit that
# lands on main bumps the version automatically (e.g. 0.1.84). Override with
# VERSION=...; falls back to the literal CMakeLists version outside a git tree
# (e.g. when building from an extracted source tarball).
project_version() {
    if [[ -n "${VERSION:-}" ]]; then
        printf '%s' "$VERSION"; return
    fi
    local full major minor patch
    full="$(awk -F'[ )]' '/^project\(AgentKate VERSION/ {print $3; exit}' "${ROOT}/CMakeLists.txt")"
    IFS=. read -r major minor _ <<<"$full"
    if patch="$(git -C "$ROOT" rev-list --count --first-parent HEAD 2>/dev/null)" \
        && [[ -n "$patch" ]]; then
        printf '%s.%s.%s' "$major" "$minor" "$patch"
    else
        printf '%s' "$full"
    fi
}

# distro_family — echoes one of: arch | debian | fedora | unknown
distro_family() {
    local id="" like=""
    if [[ -r /etc/os-release ]]; then
        # shellcheck disable=SC1091
        id="$(. /etc/os-release; printf '%s' "${ID:-}")"
        like="$(. /etc/os-release; printf '%s' "${ID_LIKE:-}")"
    fi
    case " $id $like " in
        *" arch "*)            printf 'arch' ;;
        *" debian "*|*" ubuntu "*) printf 'debian' ;;
        *" fedora "*|*" rhel "*)   printf 'fedora' ;;
        *) printf 'unknown' ;;
    esac
}
