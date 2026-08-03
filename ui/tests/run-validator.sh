#!/bin/sh
# ctest wrapper for external validators (plan 27 §4): runs the named tool with
# the given arguments, and exits 77 — ctest's SKIP_RETURN_CODE — when the tool
# is not installed, so a build box without appstreamcli/desktop-file-validate
# reports the test as SKIPPED rather than failing or silently not existing.
tool="$1"
shift
command -v "$tool" >/dev/null 2>&1 || exit 77
exec "$tool" "$@"
