# ADR-005: Native harness-first Relay

Status: **Accepted** (2026-09-15) — supersedes the terminal-first
direction of ADR-002's managed-generation plan.

## Context

RepoSuite Relay's product definition is a persistent structured
agent-session daemon with native harness adapters — not a terminal relay.
Research (`docs/research/NATIVE_HARNESS_ARCHITECTURE.md`) verified that
Codex app-server, OpenCode (serve + acp), and Devin acp all expose
structured protocols with durable native sessions, exact-ID resume across
runtime restart, streaming, cancel, model/mode control, metrics, and
agent-requested-input channels.

## Decision

- **Native structured protocols are the canonical transport.** No
  terminal emulation, no PTY, no xterm-go in Relay core.
- **Runtime ≠ session.** `HarnessRuntime` (ephemeral process/server:
  COLD|STARTING|WARM|ACTIVE|STOPPING) hosts N `RelaySession`s — a
  durable logical identity + exact `nativeSessionId` + transcript cursor.
  A RelaySession may be managed while its runtime is fully absent
  (0 procs, ~0 RAM).
- **Exact native resume only** — never last/newest/timestamp/implicit.
- **Per-runtime sleep authority.** A shared runtime stays awake while any
  bound session is unsafe: active/streaming turn, in-flight prompt or
  cancel, unresolved requested input.
- **`waiting_input` is a proven sleep blocker** — pending ask/permission
  requests are transport-bound (OpenCode: `que_` lost across restart,
  tool part stranded `running`).
- **Idle cold sleep** — unused runtimes stop completely; a prompt wakes:
  spawn → protocol ready → `ResumeExact(nativeSessionId)` → dispatch.
- **Adapter-specific `RuntimePolicy`** (`warmGrace`, `coldResumeSupported`,
  `sleepBlockers`) — no universal idle timeout; defaults derived from
  measured PSS/latency.
- **Relay-owned durable state** — Relay store is authoritative for
  identity/key, harness, nativeSessionId mapping, runtime reconstruction,
  last-known config, Relay state, compact transcript, event cursor, and
  restart recovery. Native stores stay authoritative for model context
  and native resume state. Filesystem-first:
  `sessions/<id>/{session.json, transcript.jsonl, transcript.idx}` with
  indexed bounded tail reads; no SQLite/Bolt/event-store.
- **Transient vs durable events** — deltas and rapidly-changing metrics
  are live-only; user messages, completed agent messages, input
  request/resolution, and state transitions are durable records.
- **Seq-numbered canonical events** with exact cutover:
  `throughSeq=N` → subscribe `after=N` → `N+1…`.
- **UI attach/read never wakes a runtime** — only native ops wake.
- **Harness runtimes are private to relayd.** Transport order: stdio →
  unix socket (0600) → loopback HTTP + auth. OpenCode serve always gets a
  fresh random `OPENCODE_SERVER_PASSWORD` per spawn (Basic
  `opencode:<pw>`, env-supplied — verified). Runtime credentials are
  ephemeral per generation, never part of `nativeSessionId`/logs/events;
  startup fails closed on weaker-than-expected exposure.
- **Minimal human UI** — structured agent console: transcript, prompt
  editor, requested input (choice/free-text), model/mode selectors,
  footer metrics. ~200 rendered rows on attach, ~1000 max; bounded tail,
  never full-transcript loads. Bypass permissions is the default posture.
- **stdlib-first dependencies** — hand-rolled JSON-RPC/NDJSON/HTTP/SSE;
  `x/term` at most, later, for the TUI.
- **Cross-platform target** — Linux + macOS + Windows (local transport
  in ADR-006; this supersedes ADR-001's Linux-only product scope).

## Consequences

- The daemon/session foundation (ADR-003 lifecycle/quiescence) is
  retained; `ActiveGeneration` becomes `HarnessRuntime` supervision and
  the registry evolves to durable `RelaySession` records.
- Implementation train: core domain+persistence cleanup → cross-platform
  control plane → Codex vertical slice → canonical event cutover →
  minimal TUI → Devin ACP → OpenCode ACP (preferred) → OpenCode serve
  (if needed) → Gateway/WSS → load hardening.
