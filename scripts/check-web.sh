#!/usr/bin/env bash
# Web Admin generated-drift gate: reinstall the locked frontend deps,
# re-run the static checks and the production build, then prove the
# committed web/build tree matches the regenerated output exactly.
#
# Usage: scripts/check-web.sh          (verify mode — fails on drift)
# Requires: node, npm (dev-only toolchain; never needed at runtime).
set -euo pipefail

cd "$(dirname "$0")/.."

cd web
npm ci --no-audit --no-fund
npm run check
npm run test
npm run build
cd ..

# The committed build must equal what the committed source + lockfile
# produce — no silent generated drift. Tracked modifications show up in
# git diff; NEW or deleted worktree files show up as `??` or a dirty
# second porcelain column. (A staged-but-uncommitted add is not drift.)
git diff --exit-code -- web/build
if git status --porcelain -- web/build | grep -qE '^\?\?|^.[^ ]'; then
	echo "check-web: web/build has drift" >&2
	git status --porcelain -- web/build >&2
	exit 1
fi
echo "check-web: OK"
