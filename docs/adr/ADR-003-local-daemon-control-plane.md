# ADR-003: Local daemon control plane

Status: Accepted (2026-09-14) — implemented in this milestone.

## Decision

RepoSuite Relay runs **one persistent daemon per RepoSuite state root**,
reachable only on a private Unix socket:

    ${REPOSUITE_HOME}/relay/run/relayd.sock   (default ~/.reposuite/relay/run/relayd.sock)

## Mechanics

- **Same binary, hidden modes.** `reposuite-relay __daemon` is the daemon
  entry point; `reposuite-relay __fixture` is the fixture child. Neither is
  a public interface.
- **Singleton via advisory lock.** The daemon holds `flock(LOCK_EX|NB)` on
  `run/relayd.lock` (0600). Racing client-spawned contenders lose the flock
  and exit; exactly one persists. No PID files — the protocol is the
  liveness authority.
- **Stale socket recovery.** Only the lock owner inspects the socket path:
  a stale Unix socket inode is removed and rebound; a non-socket object
  (regular file, directory, symlink) is never deleted — startup fails
  closed instead.
- **Permissions.** State dirs 0700, socket 0600, lock 0600. No TCP, no
  localhost HTTP, no WSS, no network exposure.
- **Client flow.** Managed commands (`serve`/`list`/`status`/`stop`) ping
  the socket; only when nothing answers (`ErrNotRunning`) do they spawn
  `__daemon` detached (new session, absolute `REPOSUITE_HOME`,
  CWD-independent) and poll readiness with a bounded 4s deadline. A live
  but incompatible peer (`ErrProtocolMismatch`) or malformed peer
  (`ErrBadPeer`) fails immediately — never misreported as "not running",
  never a reason to spawn. `daemon status|stop` never auto-start.
- **Protocol.** Versioned (`v=1`) JSON request/response; one connection =
  one request = one response = close. Max request 64 KiB; phase-specific
  deadlines — the read phase gets 5s (SetReadDeadline before reading the
  request / before reading the response on the client), the write phase
  gets 5s — not a combined 10s. Structured error codes; no client-supplied
  command fields — the only harness is `fixture`.
- **Atomic creation.** `serve_fixture` reserves the key under the registry
  lock *before* spawning (`Reserve`/`Commit`/`Cancel`): a duplicate or
  racing request loses the reservation, returns `SESSION_EXISTS`, and
  never spawns a process. A failed runtime-id generation or spawn cancels
  the reservation and leaves nothing.
- **Atomic removal.** `stop_session` takes exclusive stop ownership
  (`BeginStop`/`CommitStop`/`AbortStop`): the session is removed only
  after its generation is confirmed stopped. A failed stop aborts the
  ownership and retains the session — the daemon never loses authority
  over a still-running generation. Concurrent stops resolve to exactly
  one owner; losers get `SESSION_NOT_FOUND`.
- **Registry.** In-memory `mutex + map` is authoritative for logical
  sessions in this milestone. A daemon restart loses all sessions; durable
  metadata and crash recovery are deferred (ADR-002).
- **Fixture ownership.** Each `serve fixture` spawns one `__fixture` child
  whose stdin is a daemon-held pipe. Stop closes the pipe (bounded wait,
  SIGKILL fallback); daemon death closes it from the kernel, so fixture
  children cannot be orphaned.
- **Graceful shutdown.** `daemon stop` (or SIGTERM/SIGINT — one daemon-level
  signal owner) drains the registry, stops every generation, closes the
  listener, removes the socket, and releases the lock.

## Consequences

- Relative `REPOSUITE_HOME` is rejected at path resolution — daemon
  identity must not depend on client CWD.
- A pre-existing `~/.airelay` overlap remains rejected before any write.
- The daemon log is `<relay>/logs/relayd.log` (0600), written only on
  notable events; no rotation yet.
