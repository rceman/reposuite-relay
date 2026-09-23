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

## CODEX_EXACT_RESUME — NOT_MEASURED_ZERO_MODEL_CONSTRAINT (WAKEBENCH01; superseded by the exact-reattach section below)

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

## DEVIN_SESSION_LOAD — NOT_MEASURED_ZERO_MODEL_CONSTRAINT (WAKEBENCH01; superseded by the exact-reattach section below)

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

## Exact Reattach Extension (WAKEBENCH02)

Two controlled setup turns (1 Codex + 1 Devin, real model quota,
single-shot no retries) created proven resumable identities via
Relay's normal production path; the 30 measured reattach iterations
then used ZERO model turns. Identity input via owner-private 0600
files (`--codex-resume-file`/`--devin-load-file`); raw IDs never
printed, serialized, or committed — fingerprints only.

### CODEX_EXACT_RESUME — spawn + initialize + exact thread/resume (N=30)

| metric | n | min | p50 | p90 | p95 | mean | max | stddev |
|---|---|---|---|---|---|---|---|---|
| processStartMs | 30 | 0.3 | 0.4 | 0.6 | 0.7 | 0.5 | 1.4 | 0.2 |
| initializeMs | 30 | 226.8 | 234.7 | 269.9 | 284.8 | 243.4 | 291.9 | 17.7 |
| resumeMs | 30 | 112.4 | 133.1 | 148.2 | 156.4 | 139.1 | 284.1 | 28.9 |
| totalResumeReadyMs | 30 | 349.5 | 371.8 | 423.2 | 434.1 | 383.0 | 553.1 | 38.1 |
| pssTreeMiB (attached) | 30 | 155.9 | 168.2 | 171.9 | 172.2 | 167.1 | 178.2 | 5.0 |
| rssTreeMiB | 30 | 235.8 | 251.0 | 254.1 | 254.8 | 249.6 | 260.9 | 5.3 |
| shutdownMs | 30 | 23.3 | 27.1 | 35.3 | 37.7 | 28.7 | 39.2 | 4.4 |

Fingerprint `1cf1591ee76f` (one proven thread, zero-turn resume was
rejected in WAKEBENCH01; this one completed a real turn first).
Every iteration reattached the exact requested identity — no fallback.

### DEVIN_EXACT_LOAD — spawn + initialize + session/load + mode (N=30)

| metric | n | min | p50 | p90 | p95 | mean | max | stddev |
|---|---|---|---|---|---|---|---|---|
| processStartMs | 30 | 0.3 | 0.4 | 0.5 | 0.7 | 0.4 | 0.9 | 0.1 |
| initializeMs | 30 | 209.7 | 215.5 | 220.8 | 223.5 | 216.0 | 227.0 | 4.1 |
| loadMs (session/load RPC) | 30 | 995.6 | 1066.2 | 1124.2 | 1128.0 | 1067.8 | 1159.3 | 38.3 |
| configureMs (bypass mode) | 30 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| totalLoadReadyMs | 30 | 1214.5 | 1282.8 | 1338.5 | 1343.2 | 1284.3 | 1374.4 | 38.1 |
| pssTreeMiB (attached) | 30 | 71.9 | 72.4 | 72.7 | 72.9 | 72.6 | 78.0 | 1.0 |
| rssTreeMiB | 30 | 104.2 | 104.6 | 105.0 | 105.1 | 104.8 | 110.4 | 1.1 |
| shutdownMs | 30 | 112.3 | 119.8 | 126.4 | 137.0 | 122.1 | 161.9 | 9.1 |

Fingerprint `697952ee364f` (one proven slug). `configureMs = 0`: the
loaded session persisted `bypass` as current mode, so Relay's attach
config is a no-op on resume. `session/load` omitted `sessionId` in
every response — the absent-echo contract held throughout.

### Init vs exact attach (descriptive p50/p95, not sampled distributions)

- Codex: resume overhead ≈ 372−249 = **+123 ms p50**, +137 ms p95 —
  the `thread/resume` RPC itself is ~133 ms p50.
- Devin: load overhead ≈ 1283−229 = **+1054 ms p50**, +1089 ms p95 —
  `session/load` is a ~1.1 s RPC, by far the dominant cold-attach cost.

### REALDOG01 cross-check

Codex: one prior real cold resume admission ≈420 ms end-to-end —
consistent with measured 372 ms p50 + turn admission. Devin: the 3.84 s
first admission decomposes as ~230 ms init + ~1.1 s session-load (this
measurement used session/load on an ESTABLISHED slug; first-ever used
session/new) + ~2.5 s remaining in first-session semantics/prompt
admission — process startup was never the story.

## Memory-time cost (attached PSS · grace seconds)

| grace | Codex (167 MiB) | Devin (72.6 MiB) |
|----:|------:|-----:|
|   0s | 0 | 0 |
|   5s | 836 | 363 |
|  10s | 1,671 | 726 |
|  30s | 5,013 | 2,178 |
|  60s | 10,026 | 4,356 |
| 120s | 20,052 | 8,712 |
| 300s | 50,130 | 21,780 |

## POLICY IMPLICATIONS (supersedes the WAKEBENCH01 Devin cell)

The WAKEBENCH01 Devin reasoning ("warm saves only ~0.23 s") was
INSUFFICIENT_DATA — it measured process init only. Exact `session/load`
costs ~1.07 s p50, so warm retention avoids ~1.28 s per established-
session wake, not ~0.23 s.

**Codex.** Exact cold reattach: **p50 372 ms, p95 434 ms** against
~168 MiB attached PSS (60 s grace ≈ 10 GiB·s saved latency ~0.4 s).
1) Immediate stop: plausible — ~0.4 s added per wake is minor.
2) 5 s grace: retains burst benefit cheaply (~836 MiB·s).
3) 15 s: reasonable burst window (~2.5 GiB·s).
4) 60 s: wasteful relative to measured latency.
Classification: **IMMEDIATE_COLD_CANDIDATE**; implementation should
experiment in **0–15 s**, favoring the low end.

**Devin.** Exact cold reattach: **p50 1283 ms, p95 1343 ms** against
~72.6 MiB attached PSS.
1) Immediate stop: creates a noticeable ~1.3 s penalty per wake —
   noticeable but tolerable.
2) 5 s grace: captures short follow-up bursts, avoids most reattach
   cost during rapid iteration.
3) 15 s: clearly useful — amortizes a ~1.3 s wake across prompt bursts
   at modest memory-time (~1.1 GiB·s).
4) 60 s: ~4.4 GiB·s to save ~1.3 s — defensible only if bursts are
   frequent; not obviously wasteful but not proven necessary.
Classification: **SHORT_GRACE_CANDIDATE**; implementation should
experiment in **5–30 s** (up to 60 s only if idle-gap telemetry later
shows frequent sub-minute returns).

No mathematically optimal timeout is claimed — next-prompt idle-gap
distribution is unknown. Provider-specific defaults are warranted.

## Data hygiene

Per-sample CSV + this doc contain no PIDs, absolute work paths, raw
native identities, tokens, or credentials. The disposable zero-turn
Codex thread exists in provider-native state (no verified safe
archive/delete was exercised) — documented, not guessed at.
