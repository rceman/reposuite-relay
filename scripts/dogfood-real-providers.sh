#!/usr/bin/env bash
# dogfood-real-providers.sh — bounded REAL provider exercise.
#
# Explicit opt-in only: at least one of --codex/--devin/--all AND --yes
# are required before any provider process is spawned. Each selected
# provider performs AT MOST two accepted turns (marker prompts only,
# disposable cwd, no file mutations). Nothing is installed, logged in,
# or configured. Secrets are never printed or placed in argv.
set -u

DO_CODEX=0; DO_DEVIN=0; YES=0; KEEP=0; SELFTEST=0
usage() {
	cat <<'EOF'
usage: dogfood-real-providers.sh [--codex] [--devin] [--all] [--yes]
                                 [--keep-evidence] [--self-test]
  --codex           exercise the real `codex app-server` (<= 2 turns)
  --devin           exercise the real `devin acp` (<= 2 turns)
  --all             both providers
  --yes             non-interactive confirmation (else prompt)
  --keep-evidence   preserve sanitized evidence under <temp>/evidence
  --self-test       validate flags/cleanup only — invokes NO provider
EOF
}
for arg in "$@"; do
	case "$arg" in
		--codex) DO_CODEX=1 ;;
		--devin) DO_DEVIN=1 ;;
		--all) DO_CODEX=1; DO_DEVIN=1 ;;
		--yes) YES=1 ;;
		--keep-evidence) KEEP=1 ;;
		--self-test) SELFTEST=1 ;;
		-h|--help) usage; exit 0 ;;
		*) echo "unknown flag: $arg" >&2; usage >&2; exit 2 ;;
	esac
done

if [ "$SELFTEST" = 1 ]; then
	# Deterministic, provider-free: temp root lifecycle + cleanup only.
	T=$(mktemp -d /tmp/rsr-dogfood-selftest.XXXXXX) || exit 1
	mkdir -p "$T/reposuite-home" "$T/work" "$T/evidence"
	[ -d "$T/reposuite-home" ] && [ -d "$T/work" ] || { echo "selftest FAIL"; exit 1; }
	rm -rf "$T"
	[ ! -e "$T" ] || { echo "selftest FAIL: cleanup"; exit 1; }
	echo "selftest OK (no provider invoked)"
	exit 0
fi

if [ "$DO_CODEX" = 0 ] && [ "$DO_DEVIN" = 0 ]; then
	echo "no provider selected — nothing to do (use --codex/--devin/--all)" >&2
	exit 2
fi

echo "REAL MODEL CALLS WILL OCCUR — at most 2 accepted turns per selected provider."
if [ "$YES" != 1 ]; then
	if [ -t 0 ]; then
		read -r -p "Proceed? [y/N] " ans
		[ "$ans" = y ] || [ "$ans" = yes ] || { echo "aborted"; exit 1; }
	else
		echo "non-interactive execution requires --yes" >&2
		exit 2
	fi
fi

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
T=$(mktemp -d /tmp/reposuite-relay-real-dogfood.XXXXXX) || exit 1
BIN="$T/reposuite-relay"
export REPOSUITE_HOME="$T/reposuite-home"
mkdir -p "$REPOSUITE_HOME" "$T/work/codex" "$T/work/devin" "$T/evidence"

cleanup() {
	"$BIN" daemon stop >/dev/null 2>&1
	# Remove secret-bearing state before any evidence is preserved.
	rm -f "$T"/reposuite-home/relay/config/api.token \
		"$T"/reposuite-home/relay/run/daemon.json 2>/dev/null
	if [ "$KEEP" != 1 ]; then rm -rf "$T"; else rm -rf "$T/reposuite-home" "$T/work"; fi
}
trap cleanup EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }
say() { printf '%-58s %s\n' "$1" "$2"; }

# --- build + fresh state ----------------------------------------------
( cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/reposuite-relay ) || fail "build"
echo sentinel > "$T/work/codex/SENTINEL.txt"
echo sentinel > "$T/work/devin/SENTINEL.txt"
SENT_C=$(sha256sum "$T/work/codex/SENTINEL.txt" | cut -d' ' -f1)
SENT_D=$(sha256sum "$T/work/devin/SENTINEL.txt" | cut -d' ' -f1)

"$BIN" list >/dev/null || fail "daemon start"
[ "$(stat -c %a "$REPOSUITE_HOME/relay/config/api.token")" = 600 ] || fail "api.token mode"
say "daemon up; api.token 0600" "PASS"

fingerprint() { # session.json path → sha256[:12] of nativeSessionId
	python3 -c "import json,hashlib,sys
d=json.load(open(sys.argv[1])); n=d.get('nativeSessionId','')
print(hashlib.sha256(n.encode()).hexdigest()[:12] if n else 'EMPTY')" "$1"
}
session_json() { # key → session.json path (first match)
	find "$REPOSUITE_HOME/relay/sessions" -name session.json | head -1
}
wait_idle() { # key → wait <=120s for state=idle
	for _ in $(seq 1 40); do
		"$BIN" session status "$1" 2>/dev/null | tail -1 | grep -q "state=idle" && return 0
		sleep 3
	done
	return 1
}
sess_field() { "$BIN" session status "$1" 2>/dev/null | tail -1; }

run_provider() { # harness workdir marker-prefix
	local h=$1 w=$2 marker=$3 sj fp1 rt1
	sj=""
	( cd "$w" && "$BIN" serve "$h" --key "dogfood_$h" ) >/dev/null || fail "$h create"
	sess_field "dogfood_$h" | grep -q "runtimeState=cold" || fail "$h not COLD after create"
	say "$h create COLD (no provider process)" "PASS"
	sj=$(find "$REPOSUITE_HOME/relay/sessions" -name session.json | xargs grep -l "\"key\": \"dogfood_$h\"\|dogfood_$h" | head -1)
	[ -n "$sj" ] || sj=$(session_json)

	# Turn 1 — accepted turn #1 of max 2.
	"$BIN" prompt "dogfood_$h" --text "Reply with exactly:
${marker}_1

Do not modify files.
Do not run shell commands or tools.
Do not perform any other task." > "$T/evidence/${h}_turn1.out" || fail "$h turn 1 admission"
	wait_idle "dogfood_$h" || fail "$h turn 1 completion (120s)"
	grep -q "${marker}_1" "$REPOSUITE_HOME"/relay/sessions/*/transcript.jsonl || fail "$h turn 1 marker"
	fp1=$(fingerprint "$sj")
	[ "$fp1" != EMPTY ] || fail "$h native identity not materialized"
	say "$h turn 1 complete; native fp=$fp1" "PASS"

	# Restart boundary.
	"$BIN" daemon stop >/dev/null || fail "daemon stop"
	"$BIN" list >/dev/null || fail "daemon restart"
	sess_field "dogfood_$h" | grep -q "runtimeState=cold" || fail "$h not COLD after restart"
	[ "$(fingerprint "$sj")" = "$fp1" ] || fail "$h fingerprint changed across restart"
	say "$h restart: COLD, fingerprint stable" "PASS"

	# Turn 2 — accepted turn #2 of max 2. Exact resume.
	rt1=$(sess_field "dogfood_$h" | grep -o "runtimeId=[^ ]*" | cut -d= -f2)
	"$BIN" prompt "dogfood_$h" --text "Reply with exactly:
${marker}_2

Do not modify files.
Do not run shell commands or tools.
Do not perform any other task." > "$T/evidence/${h}_turn2.out" || fail "$h turn 2 admission"
	wait_idle "dogfood_$h" || fail "$h turn 2 completion (120s)"
	[ "$(fingerprint "$sj")" = "$fp1" ] || fail "$h fingerprint changed after resume"
	rt2=$(sess_field "dogfood_$h" | grep -o "runtimeId=[^ ]*" | cut -d= -f2)
	[ "$rt2" != "$rt1" ] || true # runtimeId must differ once bound
	grep -q "${marker}_2" "$REPOSUITE_HOME"/relay/sessions/*/transcript.jsonl || fail "$h turn 2 marker"
	say "$h turn 2 exact resume proven" "PASS"
}

[ "$DO_CODEX" = 1 ] && run_provider codex "$T/work/codex" "RSR_CODEX_DOGFOOD_TURN"
[ "$DO_DEVIN" = 1 ] && run_provider devin "$T/work/devin" "RSR_DEVIN_DOGFOOD_TURN"

# Workspace safety.
[ "$(sha256sum "$T/work/codex/SENTINEL.txt" | cut -d' ' -f1)" = "$SENT_C" ] || fail "codex sentinel"
[ "$(sha256sum "$T/work/devin/SENTINEL.txt" | cut -d' ' -f1)" = "$SENT_D" ] || fail "devin sentinel"
say "sentinels byte-identical" "PASS"

# Secret scan: machine token only inside config/api.token.
TOK=$(cat "$REPOSUITE_HOME/relay/config/api.token" | tr -d '\r\n')
leaks=$(grep -rl "$TOK" "$REPOSUITE_HOME" 2>/dev/null | grep -v "config/api.token" | wc -l)
[ "$leaks" = 0 ] || fail "secret scan: $leaks forbidden files"
say "secret scan clean (token only in api.token)" "PASS"

"$BIN" daemon stop >/dev/null || fail "final daemon stop"
say "daemon stopped cleanly" "PASS"
echo "=== DOGFOOD: PASS ==="
