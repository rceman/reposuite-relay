#!/usr/bin/env bash
# Canonical structural-formatting gate for RepoSuite Relay (gofmt-struct).
#
# The rule: multi-field keyed struct literals are written one field per line,
# then the file is gofmt'd. Maps, arrays, slices, unkeyed literals, and
# single-field literals are untouched. Formatting is package-aware, so a file
# is classified with its sibling files and imported package export data.
#
# Usage:
#   scripts/check-go-format.sh [BASE]   # changed + untracked files (default BASE=HEAD)
#   scripts/check-go-format.sh --all    # authoritative full-tree check
#   scripts/check-go-format.sh --write [BASE|--all]
#                                       # rewrite in place (developer workflow)
#
# CI never rewrites: the check modes exit non-zero when a file needs work.

set -Eeuo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/go-tools.sh"

write=0
args=()
for arg in "$@"; do
	if [[ "$arg" == "--write" ]]; then
		write=1
	else
		args+=("$arg")
	fi
done
mode="${args[0]:-HEAD}"

root="$(rr_root)"
rr_collect_files "$root" "$mode"

if ((${#RR_FILES[@]} == 0)); then
	echo "check-go-format: no Go files in scope ($mode)"
	exit 0
fi

struct_bin="$(rr_tool gofmt-struct "$root")"
if ((write == 1)); then
	"$struct_bin" --write "${RR_FILES[@]}"
	echo "check-go-format: rewrote ${#RR_FILES[@]} file(s) in scope ($mode)"
	exit 0
fi

if ! "$struct_bin" --check "${RR_FILES[@]}"; then
	echo "check-go-format: FAILED (run scripts/check-go-format.sh --write $mode)"
	exit 1
fi
echo "check-go-format: OK (${#RR_FILES[@]} files, $mode)"
