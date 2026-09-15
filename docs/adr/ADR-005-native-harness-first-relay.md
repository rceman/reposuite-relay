# ADR-005: Native harness-first Relay

Status: Proposed (2026-09-15) — supersedes the terminal-first direction of
ADR-002's managed-generation plan. Awaiting Planner review.

## Context

RepoSuite Relay's product definition is a persistent structured
agent-session daemon with native harness adapters — not a terminal relay.
Research (`docs/research/NATIVE_HARNESS_ARCHITECTURE.md`) verified that
Codex app-server, OpenCode (serve + acp), and Devin acp all expose
structured protocols with durable native sessions, exact-ID resume across
runtime restart, streaming, cancel, model/mode control, metrics, and
agent-requested-input channels.

## Decision (proposed)

- **Native structured protocols are the canonical transport.** No
  terminal emulation in the core.
- **Runtime ≠ session.** `HarnessRuntime` (ephemeral process/server:
  COLD|STARTING|WARM|ACTIVE|STOPPING) hosts N `RelaySession`s (persistent
  logical identity + `nativeSessionId` + transcript cursor). A session
  may be idle with its runtime fully stopped (0 procs, 0 RAM).
- **Sleep policy is per-runtime and evidence-based:** a runtime sleeps
  only when every bound session is cold-resumable-safe — no active turn,
  no in-flight native request, no pending requested input.
- **`waiting_input` is a sleep blocker** — pending ask/permission
  requests are transport-bound and do not survive runtime restart.
- **UI attachment never wakes a runtime** — transcript/history is served
  from Relay/native durable state; only native ops (prompt, model/mode
  change, cancel) wake.
- **Minimal human UI:** transcript + prompt + requested-input
  (choice/free-text) + footer metrics + state; bypass permissions is the
  default posture; no approval-UX core requirement.
- **stdlib-first dependency policy:** hand-rolled NDJSON/JSON-RPC,
  HTTP+SSE; `golang.org/x/term` at most for the future TUI.
- **xterm-go / creack/pty: remove from Relay core** in a follow-up
  cleanup task; they remain available in git history but are not part of
  the canonical architecture.

## Consequences

- The daemon/session foundation (ADR-003) is retained; `ActiveGeneration`
  evolves into `HarnessRuntime` supervision.
- First adapter: Codex app-server (vertical slice), then Devin ACP /
  OpenCode ACP (shared ACP client), then OpenCode serve.
- Canonical events are seq-numbered for future reconnect/replay; durable
  event history remains deferred.
