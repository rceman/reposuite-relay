#!/usr/bin/env bash
# Canonical Go hygiene gate for RepoSuite Relay.
#
# Checks, for every hand-written Go file in scope:
#   1. normal gofmt cleanliness
#   2. structural formatting (gofmt-struct: keyed struct literals vertically)
#   3. the hard o200k_base token budget (default 3000 tokens per file)
#
# Usage:
#   scripts/check-go-files.sh [BASE]   # fast mode: changed + untracked files (default BASE=HEAD)
#   scripts/check-go-files.sh --all    # authoritative full-tree mode (CI and final gates)
#
# Generated files (standard "// Code generated ... DO NOT EDIT." header) are
# excluded from the token budget. Nothing is rewritten: this gate only reports.

set -Eeuo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/go-tools.sh"

mode="${1:-HEAD}"
root="$(rr_root)"
rr_collect_files "$root" "$mode"

if ((${#RR_FILES[@]} == 0)); then
	echo "check-go-files: no Go files in scope ($mode)"
	exit 0
fi

status=0

unformatted="$(gofmt -l "${RR_FILES[@]}" 2>&1 || true)"
if [[ -n "$unformatted" ]]; then
	echo "check-go-files: gofmt required for:"
	printf '%s\n' "$unformatted" | sed 's/^/  /'
	status=1
fi

struct_bin="$(rr_tool gofmt-struct "$root")"
if ! "$struct_bin" --check "${RR_FILES[@]}"; then
	status=1
fi

token_bin="$(rr_tool go-file-tokens "$root")"
if ! printf '%s\n' "${RR_FILES[@]}" | "$token_bin" --root "$root" --files-from -; then
	status=1
fi

if ((status != 0)); then
	echo "check-go-files: FAILED"
	exit 1
fi
echo "check-go-files: OK (${#RR_FILES[@]} files, $mode)"
