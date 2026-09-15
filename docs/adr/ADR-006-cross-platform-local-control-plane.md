# ADR-006: Cross-platform local control plane

Status: **Accepted** (2026-09-15) — supersedes only ADR-003's
Unix-socket transport decision; ADR-003's singleton, lifecycle,
quiescence, and bounded-protocol semantics remain in force.

## Context

The finalized daemon/session foundation uses a private Unix socket,
which cannot serve a cross-platform product (Windows has no Unix domain
sockets for this purpose) and forces a bespoke framing protocol.

## Decision

- **One daemon per RepoSuite state root** (unchanged; flock singleton
  semantics from ADR-003 preserved).
- **Transport: loopback TCP**, bound to `127.0.0.1:0` — an OS-selected
  ephemeral port; no global port allocation, no non-loopback exposure.
- **Commands: HTTP/JSON**; **events: streaming NDJSON over HTTP** —
  stdlib `net/http` + `encoding/json` only. **No WebSocket library in
  relayd core.**
- **Discovery:** a small protected descriptor
  `${REPOSUITE_HOME}/relay/run/daemon.json` (0600) carrying protocol
  version, endpoint, and daemon identity/PID — written by the lock owner;
  multiple isolated `REPOSUITE_HOME` roots coexist.
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
  stdlib HTTP; state root, lock file, and run/ descriptor layout are
  unchanged.
- Linux + macOS + Windows become supported targets; platform-specific
  socket code is removed with the terminal-first cleanup (Task A/B).
- Native harness servers remain private to relayd regardless of this
  public transport change (ADR-005 security invariants).
