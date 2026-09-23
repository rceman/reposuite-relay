# Real Provider Dogfood — Codex app-server + Devin ACP

First intentional exercise of REAL provider runtimes against a disposable
Relay state root. Date 2026-09-23 · Relay commit `b8c4f16` (+ dogfood
branch fixes) · Linux x86_64 · go1.27.1.

Native session identities are reported as SHA-256 fingerprints
(first 12 hex). Runtime IDs are ephemeral Relay IDs and safe to show.
No credentials, raw native IDs, or user paths appear here.

## Environment

| Provider | Executable | Version | Notes |
|----------|-----------|---------|-------|
| Codex | `/usr/local/bin/codex` | codex-cli 0.155.1 | real `codex app-server` |
| Devin | `devin` (user-local install) | devin 3000.11.1 | real `devin acp` |

No provider install, upgrade, login, or config change was performed.
Dogfood verified with the versions above — no claim about other versions.

## Real turn budget

| Provider | Accepted turns used | Max | Result |
|----------|--------------------|-----|--------|
| Codex | 2 | 2 | Turn 1 OK; Turn 2 ENVIRONMENT-blocked (provider usage limit) |
| Devin | 2 | 2 | Turn 1 OK; Turn 2 OK after Relay fix |
| OpenCode | 0 | — | not exercised this pass |

## Codex — `dogfood_codex`

- **Create COLD**: `runtimeState=cold, pid=0, generation=0`, empty
  `nativeSessionId`, no `codex app-server` spawned by create/read.
- **Turn 1** (`RSR_CODEX_DOGFOOD_TURN_1`): cold prompt admission
  **0.65 s** (spawn + initialize + thread + turn admission); completed
  with the exact marker. `harness.started resumed=false`,
  `model=gpt-6-luna`, exactly one `session.native` materialization,
  `message.user` → `message.agent.completed`, no failure events.
  Runtime warm/idle, `generation=1`, pid>0.
- **Warm footprint** (`/proc/<pid>/smaps_rollup`): node wrapper
  PSS ≈ 9.2 MiB / RSS 43 MiB; native child PSS ≈ 148 MiB /
  RSS 236 MiB. Aggregate ≈ 157 MiB PSS.
- `nativeFingerprint=88c5ff4236c7`.
- **Daemon stop**: descriptor removed; daemon + both Codex PIDs reaped;
  `status` reports stopped without auto-start.
- **Restart**: session COLD, `pid=0`, `generation=1`, fingerprint
  unchanged, zero provider processes.
- **Turn 2** (`RSR_CODEX_DOGFOOD_TURN_2`): cold-resume admission
  **0.42 s**; `harness.started resumed=true` on a NEW runtime
  (`f53473aa…` ≠ `05567018…`), same `nativeSessionId` (fingerprint
  unchanged), `session.native` count stayed 1 — **exact resume proven**.
  The model turn then failed at the PROVIDER level: `turn.failed`
  recorded the usage-limit error verbatim — classified ENVIRONMENT
  (provider quota), not a Relay defect. Session landed `generation=2`,
  COLD-capable, identity intact.

## Devin — `dogfood_devin`

- **Create COLD**: `runtimeState=cold, pid=0, generation=0`, empty
  `nativeSessionId`, no `devin acp` spawned by create/read.
- **Turn 1** (`RSR_DEVIN_DOGFOOD_TURN_1`): cold prompt admission
  **3.84 s**; completed with the exact marker.
  `harness.started resumed=false` with EMPTY `nativeSessionId`; the slug
  was materialized by `session.native` only AFTER the completed turn —
  the proven-slug rule holds end-to-end. `nativeFingerprint=373686c63ff1`.
- **Warm footprint**: `devin acp` single process PSS ≈ 41 MiB /
  RSS ≈ 105 MiB (turn-2 generation measured PSS ≈ 44 MiB).
- **Turn 2 first attempt**: `session/load` returned a result WITHOUT
  `sessionId` (`{_meta, modes, configOptions}`) → Relay's strict echo
  check misclassified it `NATIVE_SESSION_LOST`. AND the freshly spawned
  `devin acp` was left running as an orphan holding the provider-side
  `session_locked` claim — poisoning later loads. Two real defects; both
  fixed (below).
- **Turn 2 retry** (after fix): `session/load` exact slug succeeded;
  `harness.started resumed=true` on new runtime; marker completed;
  `generation=2`; fingerprint unchanged; `session.native` count = 1.
- **Restart**: both sessions COLD, `generation=2`, fingerprints
  unchanged, provider PIDs reaped.

## Confirmed Relay defects (fixed on this branch)

1. **ACP `session/load` echo requirement too strict.**
   `acp.Client.SessionLoad` required `result.sessionId == requested`.
   The real `devin acp` (3000.11.1) does not echo the id — the request
   already names it; ACP does not require the echo. Every Devin exact
   resume therefore failed `NATIVE_SESSION_LOST`. Fix: an ABSENT echo is
   normalized to the requested id; a non-empty DIFFERENT id remains a
   hard mismatch. Fake mode `load-no-echo` reproduces the drift;
   `TestLoadWithoutIdentityEcho` covers the resume path.

2. **Orphaned runtime generation on bind failure.** When `session/new`,
   `session/load`, or permission-mode application failed on a freshly
   spawned generation, the runtime was never stopped — leaving a
   `devin acp` process holding the provider session lock, which returned
   `session_locked` to every later attempt. Fix: `stopOrphanRuntime` in
   both ACP adapters reaps a zero-bound-session generation on any attach
   failure (shared generations are never touched). Regression covered by
   the extended `TestFailedExactLoadNeverSubstitutes`.

No Codex adapter defect found; the Turn-2 failure is a provider-side
usage limit (ENVIRONMENT).

## Transcript correctness

Per provider: `message.user` → `harness.started` (gen 1 `resumed=false`,
gen 2 `resumed=true`) → `session.native` exactly once (first proven
materialization) → `message.agent.completed`; `runtime.exited` on
daemon stop. No duplicate terminal events, no contradictory completion,
no credential material in any record. Devin's failed first Turn-2
attempt is durably recorded as `turn.failed` — correct history.

## Process ownership

Relay spawned only `codex app-server` and `devin acp` — as children of
the daemon, fully reaped on `daemon stop` (and on the new orphan-reap
path). No orphan trees at cleanup.

## Workspace safety

`SENTINEL.txt` in both `<temp>/work/{codex,devin}` byte-identical before
and after; zero new files in either workspace.

## Current product gap — no automatic idle sleep

`Supervisor.CanSleep`/`StopIfIdle` exist with correct blockers (active
turn, waiting_input, mutation) but NO scheduler invokes them — a warm
idle runtime stays warm indefinitely. Dogfood COLD boundaries were
proven via graceful daemon restart. Warm measurements above are the
input for the next milestone `RUNTIME_POLICY_IDLE_SLEEP` — Codex ≈ 157
MiB PSS warm, Devin ≈ 44 MiB PSS warm.

## Dogfood script

`scripts/dogfood-real-providers.sh` — explicit `--codex/--devin/--all`
+ `--yes` opt-in, fresh temp `REPOSUITE_HOME`, disposable work dirs,
≤2 turns per provider, bounded waits, trap cleanup, no secrets in argv
or output.
