# Runtime Wake Benchmark — 2026-09-23

Model-free measurement of provider runtime cold-start and reattach
cost. **Zero model turns were sent** — no `turn/start` (Codex), no
`session/prompt` (Devin), no OpenCode prompt. Tool:
`tools/cmd/runtime-wake-bench` (stdlib + repo-internal protocol
packages; per-sample data in `runtime-wake-2026-09-23.csv`).

## Environment

- Linux amd64 · Intel i9-9900K · 8 logical CPUs · ~16 GiB RAM
- codex-cli 0.155.1 · devin 3000.11.1 — no install/update/login changes
- loadavg before `2.79 4.78 4.70`, after `3.19 4.32 4.54` — ordinary
  workstation load; steady local (page-cache-warm) behavior, not cold
  disk boot

## Methodology

- 3 warm-up + **30 measured** iterations per scenario, sequential,
  never concurrent across providers.
- Timers: Go monotonic `time.Now()` deltas, milliseconds.
- Memory: `/proc/<pid>/smaps_rollup` aggregated over the
  benchmark-owned process tree at READY+1s (benchmark stabilization
  delay, not correctness sync). Tree discovered via
  `/proc/<pid>/task/<pid>/children` recursively; only
  benchmark-owned PIDs are touched — pre-existing provider processes
  are never classified or stopped.
- Shutdown: `srv.Stop()` → whole process-group reaped before next
  sample; surviving children would mark the iteration failed (none
  did).
- Percentiles: linear interpolation between closest ranks
  (numpy "linear"): `r = p/100·(n−1)`, value between `s[⌊r⌋]` and
  `s[⌈r⌉]`.
- Failed iterations are retained as degraded samples (none occurred —
  all 60 measured samples clean).

## CODEX_INIT — spawn + initialize (N=30)

| metric | n | min | p50 | p90 | p95 | mean | max | stddev |
|---|---|---|---|---|---|---|---|---|
| processStartMs | 30 | 0.4 | 0.5 | 0.5 | 0.6 | 0.5 | 0.6 | 0.1 |
| initializeMs | 30 | 228.8 | 248.1 | 281.8 | 296.8 | 256.1 | 346.6 | 25.0 |
| totalReadyMs | 30 | 229.3 | 248.6 | 282.3 | 297.2 | 256.5 | 347.2 | 25.0 |
| pssTreeMiB | 30 | 114.0 | 119.7 | 123.3 | 123.6 | 119.5 | 125.0 | 3.3 |
| rssTreeMiB | 30 | 167.7 | 173.5 | 177.2 | 177.4 | 173.3 | 178.8 | 3.2 |
| shutdownMs | 30 | 15.5 | 17.6 | 21.1 | 22.4 | 18.4 | 25.1 | 2.1 |

## CODEX_EXACT_RESUME — NOT_MEASURED_ZERO_MODEL_CONSTRAINT

REAL OBSERVATION: `codex-cli 0.155.1` rejects `thread/resume` on a
zero-turn thread — `no rollout found for thread id` — because the
rollout (native thread state) is only persisted after the first
completed turn. A disposable resumable thread therefore cannot be
created without a model turn, which this benchmark forbids.

Consequence: exact-resume latency can only be bounded, not measured —
it is `CODEX_INIT` (~250 ms) plus one `thread/resume` RPC whose cost is
expected to be comparable to `thread/start` (tens of ms), so a
plausible bound is ≲400 ms. The raw disposable thread ID was never
stored; fingerprint `4a0949dddb94` only.

## DEVIN_INIT — spawn + initialize (N=30)

| metric | n | min | p50 | p90 | p95 | mean | max | stddev |
|---|---|---|---|---|---|---|---|---|
| processStartMs | 30 | 0.4 | 0.4 | 0.5 | 0.7 | 0.6 | 4.0 | 0.6 |
| initializeMs | 30 | 213.8 | 228.3 | 248.7 | 253.4 | 231.0 | 263.4 | 12.8 |
| totalReadyMs | 30 | 214.3 | 228.7 | 250.5 | 253.8 | 231.5 | 263.8 | 12.9 |
| pssTreeMiB | 30 | 57.8 | 59.5 | 59.7 | 59.7 | 59.3 | 59.9 | 0.5 |
| rssTreeMiB | 30 | 76.9 | 78.6 | 78.8 | 78.8 | 78.4 | 79.0 | 0.5 |
| shutdownMs | 30 | 114.0 | 123.6 | 149.3 | 165.2 | 129.6 | 198.6 | 17.7 |

## DEVIN_SESSION_LOAD — NOT_MEASURED_ZERO_MODEL_CONSTRAINT

Devin zero-turn slugs are not resumable identities (REALDOG01), and no
already-known disposable proven slug was supplied. `session/load`
latency was therefore not measured. `session/new` one-off was skipped
(uncertain semantics; process+initialize is the primary metric).

## Comparison to REALDOG01

| | REALDOG01 (Relay path, incl. model admission) | WAKEBENCH (runtime only, no model) |
|---|---|---|
| Codex cold first admission | ~650 ms | ~250 ms ready (init) |
| Codex cold resume admission | ~420 ms | init ~250 ms + resume RPC (bounded ≲400 ms) |
| Codex warm PSS | ~157 MiB (after completed turn) | ~120 MiB (idle, ready-only) |
| Devin first admission | ~3,840 ms | ~230 ms ready (init) |
| Devin warm PSS | ~41–44 MiB | ~59.5 MiB (idle, ready-only) |

Differences: REALDOG01 numbers include Relay attach + native session
setup + turn admission (and for Devin, `session/new`+`session/prompt`
server-side work) — WAKEBENCH isolates process+protocol only.

## Memory-time cost (aggregate PSS MiB · grace seconds)

| grace | Codex (119.5 MiB) | Devin (59.3 MiB) |
|----:|-----:|-----:|
|   0s | 0 | 0 |
|   5s | 597 | 297 |
|  10s | 1,195 | 593 |
|  30s | 3,585 | 1,779 |
|  60s | 7,170 | 3,559 |
| 120s | 14,340 | 7,119 |
| 300s | 35,850 | 17,797 |

## Latency saved by staying warm

| provider | p50 | p95 |
|---|---:|---:|
| Codex (init; resume bounded ≲400 ms) | ~250 ms | ~300 ms |
| Devin (init only; full wake unmeasured, REALDOG01 saw 3.84 s) | ~230 ms | ~255 ms |

## POLICY IMPLICATIONS

**Codex.** (A) Cold wake is cheap: ~250 ms p50, ~300 ms p95 to ready;
exact resume is bounded ≲400 ms. (B) Warm retention is expensive:
~120–157 MiB. (C) Stop-immediately is technically plausible — a
sub-second reattach against ~150 MiB resident is a poor trade per
prompt. (D) A 60 s grace costs ~7.2 GiB·s per idle runtime to save
≤0.4 s — **not obviously justified**; it looks excessive for Codex.
(E) Next experiment range: **0–5 s** (test whether a same-prompt
follow-up window even exists in practice), possibly 5–15 s as the
upper bound. Classification: **IMMEDIATE_COLD_CANDIDATE**.

Direct answer to §32: keeping ~150 MiB resident for 60 s is **not**
justified by a ~0.3–0.4 s reattach — 0–5 s (or 5–15 s as an outer
bound) is the sensible next experiment range.

**Devin.** (A) Process+init is ~230 ms p50 — the REALDOG01 ~3.84 s cold
admission is **NOT** dominated by process startup; the residual lives
in `session/new`/`session/load`+prompt admission server-side (unmeasured
here by the zero-model rule). (B) Warm retention ~59 MiB is cheaper
than Codex but non-trivial. (C) Stop-immediately is plausible — warm
retention saves only ~0.23 s of a ~3.8 s observed path. (D) A 60 s
grace saves ~6% of observed admission cost for 3.6 GiB·s — weakly
justified at best; the dominant latency is elsewhere. (E) Next range:
**0–15 s**, pending a session/load measurement in the design milestone.
Classification: **IMMEDIATE_COLD_CANDIDATE** (for the process component;
`session/load` contribution is INSUFFICIENT_DATA until measured).

Both providers point the same way: provider process startup is NOT the
expensive part. Aggressive COLD is cheap; the design milestone should
focus on exact-attach latency (thread/resume with a proven rollout,
session/load with a proven slug) rather than process warm-keeping.

## Data hygiene

Per-sample CSV + this doc contain no PIDs, absolute work paths, raw
native identities, tokens, or credentials. The disposable zero-turn
Codex thread exists in provider-native state (no verified safe
archive/delete was exercised) — documented, not guessed at.
