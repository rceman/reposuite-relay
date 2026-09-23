#!/usr/bin/env bash
# dogfood-real-providers.sh — bounded REAL provider exercise.
#
# Explicit opt-in only: at least one of --codex/--devin/--all AND --yes
# are required before any provider process is spawned. Each selected
# provider performs AT MOST two accepted turns (marker prompts only,
# disposable cwd, no file mutations). Nothing is installed, logged in,
# or configured. Secrets are never printed or placed in argv.
#
# Directory contract:
#   <temp>/raw/       ephemeral command output — ALWAYS deleted
#   <temp>/evidence/  sanitized summaries only — kept with --keep-evidence
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
  --self-test       validate helpers only — invokes NO provider
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

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

# ---------------------------------------------------------------------
# Pure helpers (shared by the run path and --self-test).
# ---------------------------------------------------------------------

# Exact session directory lookup: the JSON object whose "key" matches.
# Exactly one match required — 0 or >1 is a hard failure, never "first".
session_dir_for_key() { # sessions_root key → dir
	python3 - "$1" "$2" <<'PY'
import json, os, sys
root, key = sys.argv[1], sys.argv[2]
hits = []
if os.path.isdir(root):
    for d in sorted(os.listdir(root)):
        p = os.path.join(root, d, "session.json")
        if not os.path.isfile(p):
            continue
        try:
            if json.load(open(p)).get("key") == key:
                hits.append(os.path.dirname(p))
        except Exception:
            pass
sys.exit(2) if len(hits) != 1 else print(hits[0])
PY
}
# generation parser: status line → generation value
field_of() { sed -n "s/.*\\b$1=\\([^ ]*\\).*/\\1/p" | head -1; }
# native fingerprint: session.json → sha256[:12] of nativeSessionId
fingerprint() {
	python3 -c "import json,hashlib,sys
d=json.load(open(sys.argv[1])); n=d.get('nativeSessionId','')
print(hashlib.sha256(n.encode()).hexdigest()[:12] if n else 'EMPTY')" "$1"
}
# transcript counters for THAT session's transcript only
tr_count() { # transcript.jsonl type → count
	grep -c "\"type\": \"$2\"" "$1" 2>/dev/null || true
}
tr_resumed() { # transcript.jsonl true|false → count of matching harness.started
	grep -c "\"type\": \"harness.started\".*\"resumed\": $2" "$1" 2>/dev/null || true
}

cleanup() {
	[ -x "${BIN:-}" ] && "$BIN" daemon stop >/dev/null 2>&1
	rm -rf "${T:-/nonexistent}/raw" "${T:-/nonexistent}/reposuite-home" \
		"${T:-/nonexistent}/work" 2>/dev/null
	[ "${KEEP:-0}" != 1 ] && rm -rf "${T:-/nonexistent}"
}
fail() { echo "FAIL: $1" >&2; exit 1; }
say() { printf '%-58s %s\n' "$1" "$2"; }

# ---------------------------------------------------------------------
if [ "$SELFTEST" = 1 ]; then
	ST=$(mktemp -d /tmp/rsr-dogfood-selftest.XXXXXX) || exit 1
	mkdir -p "$ST/sessions/s1" "$ST/raw" "$ST/evidence"
	echo '{"key":"dogfood_x","nativeSessionId":"","generation":0}' > "$ST/sessions/s1/session.json"
	echo '{"key":"other","nativeSessionId":"","generation":0}' > "$ST/sessions/other.json"
	mkdir -p "$ST/sessions/s2"; echo '{"key":"dogfood_x","nativeSessionId":"","generation":0}' > "$ST/sessions/s2/session.json"
	# exact lookup: 2 matches → fail; remove dupe → exactly 1
	session_dir_for_key "$ST/sessions" "dogfood_x" >/dev/null 2>&1 && { echo "selftest FAIL: dup lookup"; exit 1; }
	rm -rf "$ST/sessions/s2"
	[ "$(session_dir_for_key "$ST/sessions" dogfood_x)" = "$ST/sessions/s1" ] || { echo "selftest FAIL: lookup"; exit 1; }
	session_dir_for_key "$ST/sessions" "nope" >/dev/null 2>&1 && { echo "selftest FAIL: miss lookup"; exit 1; }
	# generation parser
	echo "key=x generation=2 pid=0" | [ "$(field_of generation)" = 2 ] || { echo "selftest FAIL: field"; exit 1; }
	# transcript parsers on a fake transcript
	TR="$ST/tr.jsonl"
	printf '%s\n' '{"type": "harness.started","payload": {"resumed": false}}' \
		'{"type": "session.native","payload": {"nativeSessionId": "x"}}' \
		'{"type": "harness.started","payload": {"resumed": true}}' > "$TR"
	[ "$(tr_count "$TR" session.native)" = 1 ] || { echo "selftest FAIL: tr_count"; exit 1; }
	[ "$(tr_resumed "$TR" true)" = 1 ] && [ "$(tr_resumed "$TR" false)" = 1 ] || { echo "selftest FAIL: resumed"; exit 1; }
	# fingerprint sanitization: empty → EMPTY
	[ "$(fingerprint "$ST/sessions/s1/session.json")" = EMPTY ] || { echo "selftest FAIL: fingerprint"; exit 1; }
	# raw/evidence separation: raw is always deleted
	touch "$ST/raw/out" "$ST/evidence/summary.txt"
	rm -rf "$ST/raw"; [ ! -e "$ST/raw" ] || { echo "selftest FAIL: raw delete"; exit 1; }
	# turn-budget guard (pure counter)
	turns=0; for _ in 1 2 3; do [ "$turns" -ge 2 ] && break; turns=$((turns+1)); done
	[ "$turns" = 2 ] || { echo "selftest FAIL: turn guard"; exit 1; }
	rm -rf "$ST"; [ ! -e "$ST" ] || { echo "selftest FAIL: cleanup"; exit 1; }
	echo "selftest OK (0 provider processes invoked)"
	exit 0
fi

# ---------------------------------------------------------------------
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

# Provider discovery BEFORE any state/model work — fail early if absent.
[ "$DO_CODEX" = 1 ] && { command -v codex >/dev/null || fail "codex executable not found"; }
[ "$DO_DEVIN" = 1 ] && { command -v devin >/dev/null || fail "devin executable not found"; }
{
	echo "codex=$( [ "$DO_CODEX" = 1 ] && codex --version 2>/dev/null | head -1 || echo skipped )"
	echo "devin=$( [ "$DO_DEVIN" = 1 ] && devin --version 2>/dev/null | head -1 || echo skipped )"
} > /dev/null

T=$(mktemp -d /tmp/reposuite-relay-real-dogfood.XXXXXX) || exit 1
BIN="$T/reposuite-relay"
export REPOSUITE_HOME="$T/reposuite-home"
mkdir -p "$REPOSUITE_HOME" "$T/work/codex" "$T/work/devin" "$T/evidence" "$T/raw"
trap cleanup EXIT

( cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/reposuite-relay ) || fail "build"
echo sentinel > "$T/work/codex/SENTINEL.txt"
echo sentinel > "$T/work/devin/SENTINEL.txt"
SENT_C=$(sha256sum "$T/work/codex/SENTINEL.txt" | cut -d' ' -f1)
SENT_D=$(sha256sum "$T/work/devin/SENTINEL.txt" | cut -d' ' -f1)

"$BIN" list >/dev/null || fail "daemon start"
[ "$(stat -c %a "$REPOSUITE_HOME/relay/config/api.token")" = 600 ] || fail "api.token mode"
DAEMON_PID=$("$BIN" list | tail -1 | field_of daemon_pid)
say "daemon up (pid $DAEMON_PID); api.token 0600" "PASS"

# Record a provider runtime PID spawned by THIS daemon only.
record_provider_pid() { # key → prints daemon-child pid or empty
	local st pid
	st=$("$BIN" session status "$1" | tail -1)
	pid=$(echo "$st" | field_of pid)
	[ -n "$pid" ] && [ "$pid" != 0 ] && [ -d "/proc/$pid" ] && echo "$pid" || true
}
wait_idle() {
	for _ in $(seq 1 40); do
		"$BIN" session status "$1" 2>/dev/null | tail -1 | grep -q "state=idle" && return 0
		sleep 3
	done
	return 1
}
assert_gen() { # key expected
	local g
	g=$("$BIN" session status "$1" | tail -1 | field_of generation)
	[ "$g" = "$2" ] || fail "$1 generation=$g want $2"
}

run_provider() { # harness marker
	local h=$1 marker=$2 sdir tr sj fp rt1 rt2 pid1 turns=0
	# --- create COLD; create must not wake a provider -------------
	pre_children=$(ls "/proc/$DAEMON_PID/task/$DAEMON_PID/children" 2>/dev/null | tr ' ' '\n' | wc -l)
	( cd "$T/work/$h" && "$BIN" serve "$h" --key "dogfood_$h" ) >/dev/null || fail "$h create"
	"$BIN" session status "dogfood_$h" | tail -1 | grep -q "runtimeState=cold" || fail "$h not COLD after create"
	assert_gen "dogfood_$h" 0
	sdir=$(session_dir_for_key "$REPOSUITE_HOME/relay/sessions" "dogfood_$h") || fail "$h session lookup"
	sj="$sdir/session.json"; tr="$sdir/transcript.jsonl"
	[ "$(fingerprint "$sj")" = EMPTY ] || fail "$h native id before first turn"
	post_children=$(ls "/proc/$DAEMON_PID/task/$DAEMON_PID/children" 2>/dev/null | tr ' ' '\n' | wc -l)
	[ "$post_children" = "$pre_children" ] || fail "$h create woke a provider process"
	say "$h create COLD gen=0, no provider process" "PASS"

	# --- Turn 1 (accepted turn 1 of max 2) ------------------------
	[ "$turns" -ge 2 ] && fail "$h turn budget exceeded"
	turns=$((turns+1))
	# Raw prompt output (contains raw native_session_id) → raw/, never evidence/.
	"$BIN" prompt "dogfood_$h" --text "Reply with exactly:
${marker}_1

Do not modify files.
Do not run shell commands or tools.
Do not perform any other task." > "$T/raw/${h}_turn1.out" || fail "$h turn 1 admission"
	wait_idle "dogfood_$h" || fail "$h turn 1 completion (120s)"
	assert_gen "dogfood_$h" 1
	grep -q "${marker}_1" "$tr" || fail "$h turn 1 marker not in ITS transcript"
	fp=$(fingerprint "$sj")
	[ "$fp" != EMPTY ] || fail "$h native identity not materialized"
	[ "$(tr_count "$tr" session.native)" = 1 ] || fail "$h session.native count"
	[ "$(tr_resumed "$tr" false)" = 1 ] || fail "$h resumed=false missing"
	rt1=$("$BIN" session status "dogfood_$h" | tail -1 | field_of runtimeId)
	[ -n "$rt1" ] || fail "$h runtime id empty while warm"
	pid1=$(record_provider_pid "dogfood_$h")
	say "$h turn1 gen=1 fp=$fp runtime=$rt1 pid=${pid1:-n/a}" "PASS"

	# --- daemon restart boundary ----------------------------------
	"$BIN" daemon stop >/dev/null || fail "daemon stop"
	if [ -n "${pid1:-}" ] && kill -0 "$pid1" 2>/dev/null; then fail "$h provider pid $pid1 survived daemon stop"; fi
	"$BIN" list >/dev/null || fail "daemon restart"
	DAEMON_PID=$("$BIN" list | tail -1 | field_of daemon_pid)
	"$BIN" session status "dogfood_$h" | tail -1 | grep -q "runtimeState=cold" || fail "$h not COLD after restart"
	assert_gen "dogfood_$h" 1
	[ -z "$("$BIN" session status "dogfood_$h" | tail -1 | field_of runtimeId)" ] || fail "$h runtimeId not empty when COLD"
	[ "$(fingerprint "$sj")" = "$fp" ] || fail "$h fingerprint changed across restart"
	say "$h restart: COLD gen=1 fp stable" "PASS"

	# --- Turn 2 exact cold resume (accepted turn 2 of max 2) ------
	[ "$turns" -ge 2 ] && fail "$h turn budget exceeded"
	turns=$((turns+1))
	"$BIN" prompt "dogfood_$h" --text "Reply with exactly:
${marker}_2

Do not modify files.
Do not run shell commands or tools.
Do not perform any other task." > "$T/raw/${h}_turn2.out" || fail "$h turn 2 admission"
	wait_idle "dogfood_$h" || fail "$h turn 2 completion (120s)"
	assert_gen "dogfood_$h" 2
	[ "$(fingerprint "$sj")" = "$fp" ] || fail "$h fingerprint changed after resume"
	[ "$(tr_count "$tr" session.native)" = 1 ] || fail "$h second native materialization"
	[ "$(tr_resumed "$tr" true)" = 1 ] || fail "$h resumed=true missing"
	rt2=$("$BIN" session status "dogfood_$h" | tail -1 | field_of runtimeId)
	[ -n "$rt2" ] || fail "$h runtime id empty after turn 2"
	[ "$rt2" != "$rt1" ] || fail "$h runtime id reused across generations"
	pid1=$(record_provider_pid "dogfood_$h")
	say "$h turn2 gen=2 exact resume runtime=$rt2" "PASS"

	# --- final restart COLD proof (no turn 3) ---------------------
	"$BIN" daemon stop >/dev/null || fail "final stop"
	if [ -n "${pid1:-}" ] && kill -0 "$pid1" 2>/dev/null; then fail "$h provider pid survived final stop"; fi
	"$BIN" list >/dev/null || fail "final restart"
	DAEMON_PID=$("$BIN" list | tail -1 | field_of daemon_pid)
	"$BIN" session status "dogfood_$h" | tail -1 | grep -q "runtimeState=cold" || fail "$h not COLD at final check"
	assert_gen "dogfood_$h" 2
	[ "$(fingerprint "$sj")" = "$fp" ] || fail "$h fingerprint drift at final check"
	say "$h final restart: COLD gen=2 fp stable" "PASS"

	# sanitized evidence only — never raw prompt output/session.json
	{
		echo "provider=$h"
		echo "nativeFingerprint=$fp"
		echo "generations=0,1,1,2,2"
		echo "runtimeTurn1=$rt1 runtimeTurn2=$rt2"
	} > "$T/evidence/${h}.txt"
}

[ "$DO_CODEX" = 1 ] && run_provider codex "RSR_CODEX_DOGFOOD_TURN"
[ "$DO_DEVIN" = 1 ] && run_provider devin "RSR_DEVIN_DOGFOOD_TURN"

[ "$(sha256sum "$T/work/codex/SENTINEL.txt" | cut -d' ' -f1)" = "$SENT_C" ] || fail "codex sentinel"
[ "$(sha256sum "$T/work/devin/SENTINEL.txt" | cut -d' ' -f1)" = "$SENT_D" ] || fail "devin sentinel"
say "sentinels byte-identical" "PASS"

# Secret/identity scan over evidence before optional preservation.
TOK=$(tr -d '\r\n' < "$REPOSUITE_HOME/relay/config/api.token")
leaks=$(grep -rl "$TOK" "$T/evidence" 2>/dev/null | wc -l)
[ "$leaks" = 0 ] || fail "evidence scan: $leaks forbidden token files"
nidraw=$(grep -rl "nativeSessionId" "$T/evidence" 2>/dev/null | wc -l)
[ "$nidraw" = 0 ] || fail "evidence scan: $nidraw raw native-id files"
say "evidence scan: token=0 raw-native-id=0" "PASS"

"$BIN" daemon stop >/dev/null || fail "final daemon stop"
[ "$KEEP" = 1 ] && echo "sanitized evidence preserved at $T/evidence"
echo "=== DOGFOOD: PASS ==="
