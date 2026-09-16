# RepoSuite Relay — Domain vocabulary

This document defines the shared vocabulary for RepoSuite Relay. Terms
marked *target* are the accepted architecture (ADR-005/ADR-006); terms
marked *current* describe what the code does today.

## Product

**RepoSuite Relay** — a persistent structured agent-session daemon with
native harness adapters. It owns harness sessions on behalf of viewers
and keeps them alive independently of any single attachment. It is not a
terminal relay: no PTY, no terminal emulator, no xterm dependency in the
core.

- Standalone CLI: `reposuite-relay`
- Future RepoSuite umbrella invocation: `reposuite relay ...` — dispatches
  to the same Relay implementation.
- Daemon: one `relayd` per RepoSuite state root — currently realized as
  the hidden `reposuite-relay __daemon` mode of the same binary (the
  `reposuite-relayd` name remains the future packaged form).
  - *Current:* Unix socket `${REPOSUITE_HOME}/relay/run/relayd.sock`,
    singleton via `flock` on `run/relayd.lock` (Linux).
  - *Target (ADR-006):* loopback TCP `127.0.0.1:0`, HTTP/JSON commands,
    streaming NDJSON events, `run/daemon.json` descriptor; singleton via
    platform-appropriate exclusive lock on Linux/macOS/Windows.

## Sessions

**RelaySession** *(target)* — the durable logical Relay session identity:
key, harness, `NativeSessionID`, runtime binding or none, state,
model/mode config, transcript cursor, timestamps. It survives TUI/web
disconnect, harness runtime sleep/death, and relayd restart where the
native harness supports exact resume.

**Logical session** *(current)* — the in-memory registry entry today's
daemon owns; durable `RelaySession` persistence is the next core step
(Task A2).

**HarnessRuntime** *(target)* — the ephemeral owned runtime for one
harness: a process tree, transport, state
(`COLD|STARTING|WARM|ACTIVE|STOPPING`), capabilities, and
`RuntimePolicy`. One runtime may host multiple sessions; sleep authority
is per-runtime. A runtime may be completely absent while its
`RelaySession`s remain managed.

**HarnessAdapter** *(target)* — the protocol-specific bridge between a
native harness protocol (Codex app-server, OpenCode ACP/serve, Devin
ACP) and Relay's canonical session/event model. No adapter is
implemented yet.

**RuntimeSupervisor** *(target)* — owns runtime wake/sleep/lifecycle:
ensure-awake, protocol init, exact native resume, prompt dispatch,
per-runtime `SafeToSleep`, stop, and process-tree accounting.

**NativeSessionID** *(target)* — the exact harness-native resume
identity (e.g., a Codex thread UUID, an OpenCode ACP session ID). Resume
always uses the exact identity — never "newest", `--last`, timestamps,
or implicit current session.

**RuntimePolicy** *(target)* — per-adapter/runtime sleep policy:
conceptually `warmGrace`, `coldResumeSupported`, `sleepBlockers`. No
universal idle timeout exists; defaults derive from measured PSS and
spawn/resume/cold-prompt latency.

**COLD** *(target)* — a runtime (or bound session state) with **zero**
harness resources: no process, no sockets, ~0 RAM. Cold sleep is not
suspension; the runtime is gone and is re-created on wake.

## Requests

**RequestedInput** *(target)* — a blocking question/input request a
harness surfaces through the adapter (permission prompt, choice,
free-text). It is responder-agnostic: any attached client may answer.
An unresolved `RequestedInput` is a **sleep blocker** — a runtime must
not sleep while one is pending (native pending-request state is not
resumable).

## Events

**Canonical events** *(target)* — sequence-numbered per session:
`session.state`, `message.user`, `message.agent.delta`,
`message.agent.completed`, `input.requested`, `input.resolved`,
`session.metrics`, `session.error`. Durable records persist to the
session transcript; transient deltas (streaming, fast-changing metrics)
are live-only. Reconnect cutover is `throughSeq` (hydration) →
`afterSeq` (live subscription).

**Transcript** *(target)* — Relay-owned compact durable event history
per session, filesystem-first
(`sessions/<id>/{session.json,transcript.jsonl,transcript.idx}`) with
indexed bounded-tail reads. Relay never duplicates model context; the
native store stays authoritative for that.

## Attachments

**Attachment** *(target)* — a client consuming a session's state and
events through the local protocol. Attach/read is a pure Relay-store
operation: it must **not** wake a `COLD` runtime. A prompt/input action
is what wakes the runtime.

## Superseded terms

`ActiveGeneration`, `VirtualTerminal`, controlling-attachment PTY
ownership, and the headless-terminal model are superseded by ADR-005.
Historical ADRs (ADR-001/002) and `docs/research/GO_TERMINAL_SPIKE.md`
preserve that evidence unchanged.
