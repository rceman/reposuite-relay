#!/usr/bin/env bash
# Shared helpers for the RepoSuite Relay Go hygiene gates.
#
# The tools live in the nested tools/ module (exact o200k_base counting needs
# tiktoken-go, which must never become a production dependency). They are
# built once into the user cache and reused until their sources change, so a
# gate invocation costs one tokenizer process — never one per file.

set -Eeuo pipefail

rr_root() {
	git rev-parse --show-toplevel
}

rr_cache_dir() {
	printf '%s' "${XDG_CACHE_HOME:-$HOME/.cache}/reposuite-relay/tools"
}

# rr_tool <name> — build (when stale) and print the tool binary path.
rr_tool() {
	local name="$1" root="$2" cache bin
	cache="$(rr_cache_dir)"
	bin="$cache/$name"
	if rr_tool_stale "$root" "$bin"; then
		mkdir -p "$cache"
		if ! (cd "$root/tools" && go build -o "$bin" "./cmd/$name"); then
			echo "hygiene gate: cannot build tools/$name" >&2
			return 1
		fi
	fi
	printf '%s' "$bin"
}

# rr_tool_stale <root> <bin> — true when the cached binary must be rebuilt.
# Invalidation inputs: every tools/**/*.go plus tools/go.mod and
# tools/go.sum, because a dependency-only change alters the built binary
# without touching a Go source file.
rr_tool_stale() {
	local root="$1" bin="$2"
	if [[ ! -x "$bin" ]]; then
		return 0
	fi
	[[ -n "$(find "$root/tools" \
		\( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) \
		-newer "$bin" -print -quit 2>/dev/null)" ]]
}

# rr_go_files <--all|BASE> — print the selected repository-relative Go files.
rr_go_files() {
	local root="$1" mode="$2" base
	if [[ "$mode" == "--all" ]]; then
		git -C "$root" ls-files -z --cached --others --exclude-standard -- '*.go'
	else
		if ! base="$(git -C "$root" rev-parse --verify "$mode^{commit}" 2>/dev/null)"; then
			echo "hygiene gate: unavailable comparison base: $mode" >&2
			return 1
		fi
		{
			git -C "$root" diff --name-only -z --diff-filter=ACMR "$base" -- '*.go'
			git -C "$root" ls-files -z --others --exclude-standard -- '*.go'
		}
	fi | tr '\0' '\n' | sort -u
}

# rr_collect_files <--all|BASE> — fill the global array RR_FILES.
rr_collect_files() {
	local root="$1" mode="$2"
	RR_FILES=()
	local path
	while IFS= read -r path; do
		if [[ -n "$path" ]]; then
			RR_FILES+=("$path")
		fi
	done < <(rr_go_files "$root" "$mode")
}
