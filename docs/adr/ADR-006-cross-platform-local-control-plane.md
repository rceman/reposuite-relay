# ADR-006: Cross-platform local control plane

Status: **Accepted** (2026-09-15) — supersedes only ADR-003's
Unix-socket transport decision; ADR-003's singleton, lifecycle,
quiescence, and bounded-protocol semantics remain in force.

## Context

The finalized daemon/session foundation uses a private Unix socket and a
bespoke framing protocol. Modern Windows does support AF_UNIX, but a
socket-path transport still diverges per platform (path conventions,
socket-file lifecycle, permission semantics, discovery). The canonical
choice is **one uniform local Relay protocol and transport across Linux,
macOS, and Windows**: loopback TCP avoids every platform-specific IPC
difference.

## Decision

- **One daemon per RepoSuite state root** (unchanged invariant): exactly
  one relayd owns a state root — exclusive daemon ownership and
  fail-closed competing startup are architectural requirements. The
  *mechanism* is platform-specific: `flock` (or equivalent) on
  Unix-like systems, an appropriate exclusive-lock primitive on Windows —
  the exact Windows primitive is deliberately unfrozen here.
- **Transport: loopback TCP**, bound to `127.0.0.1:0` — an OS-selected
  ephemeral port; no global port allocation, no non-loopback exposure.
- **Commands: HTTP/JSON**; **events: streaming NDJSON over HTTP** —
  stdlib `net/http` + `encoding/json` only. **No WebSocket library in
  relayd core.**
- **Discovery:** a small descriptor
  `${REPOSUITE_HOME}/relay/run/daemon.json` carrying protocol version,
  endpoint, and daemon identity/PID — written by the lock owner;
  multiple isolated `REPOSUITE_HOME` roots coexist. The descriptor (and
  any future local bearer credential) must be **private to the current
  user**: mode `0600` on Unix, a user-private ACL or platform-equivalent
  on Windows — another ordinary local user must not be able to read it.
- **One local protocol for all clients** — CLI, TUI, and a future
  Gateway bridge share it.
- **Event cutover:** per-session monotonic `seq`; subscribe with
  `after=N` after hydrating `throughSeq=N`.
- **Auth-ready:** a local bearer token can be added to the descriptor +
  `Authorization` header without redesigning the protocol.
- **Remote boundary:** `relayd → RepoSuite Gateway → WSS → Web Admin`.
  Gateway owns WSS/TLS, remote auth, node routing, and Internet exposure;
  relayd never exposes the harness layer.

## Consequences

- Replaces the `relayd.sock` listener + custom bounded-JSON framing with
  stdlib HTTP; state root and run/ descriptor layout are unchanged; the
  exclusive-lock mechanism becomes platform-specific (invariant kept).
- Linux + macOS + Windows become supported targets; platform-specific
  socket code is removed with the terminal-first cleanup (Task A/B).
- Native harness servers remain private to relayd regardless of this
  public transport change (ADR-005 security invariants).
