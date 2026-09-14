# RepoSuite Relay — Domain vocabulary

This document defines the shared vocabulary for RepoSuite Relay. Terms here
are architectural decisions already made; later tasks must use one model.

## Product

**RepoSuite Relay** — a standalone managed agent runtime/session relay
module. It owns harness processes on behalf of viewers and keeps sessions
alive independently of any single attachment.

- Standalone CLI: `reposuite-relay`
- Future RepoSuite umbrella invocation: `reposuite relay ...` — dispatches to
  the same Relay implementation.
- Daemon: one persistent daemon per user/state root, currently realized as
  the hidden `reposuite-relay __daemon` mode of the same binary (the
  `reposuite-relayd` name remains the future packaged form). Socket:
  `${REPOSUITE_HOME}/relay/run/relayd.sock`; singleton via advisory lock on
  `run/relayd.lock`.

**Current implementation status:** the daemon, control protocol, logical
session registry, and a **fixture** ActiveGeneration (deterministic
same-binary child, no PTY/terminal) are implemented. PTY-backed harness
generations, hibernation, attach, and durable persistence are not yet.

## Sessions

**Logical session** — a stable session identity that survives harness
process generations. Clients refer to the logical session, never to a PID.
Owned by the daemon; persists as a lightweight record even while the session
is hibernated.

**Active generation** — the ephemeral owned runtime resources for one awake
harness generation: eventually the PTY, the harness process, the
VirtualTerminal, and generation-scoped timers/listeners. A generation is
created on wake and destroyed completely on hibernate/exit. Session
generation counters increment per generation.

_Currently:_ a generation owns exactly one fixture child process
(`reposuite-relay __fixture` with a daemon-held stdin pipe); no PTY or
VirtualTerminal is attached yet.

**Hibernated session** — a logical session with **zero** active-generation
resources: no harness process, no PTY, no xterm object. Hibernation is not
suspension; the process is gone.

**Native session identity** — the authoritative harness-native resume
identity (e.g., a Codex thread/session UUID). Resume always uses the exact
native identity — never "newest" or "--last" guessing.

## Attachments

**Controlling attachment** — a future read/write/resize client. At most one
controls a session at a time.

**Observer attachment** — a future read-only client. Observers do not hold
the awake lease and must not wake a hibernated session.

## Terminal

**VirtualTerminal** — the authoritative headless terminal state between the
PTY and any viewers (implemented on `rceman/xterm-go` via
`internal/terminal`). It is the terminal peer of the harness process: it
parses all PTY output, answers terminal queries itself (zero-viewer
capable), and serializes snapshots for viewer hydration.

## Presentation

Viewer attach delivers a **snapshot + ordered future output** — a serialized
terminal snapshot followed by the exact post-snapshot byte stream — never a
raw hydration replay of the session's history.
