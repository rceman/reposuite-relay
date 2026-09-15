# ADR-002: Managed session runtime

Status: Superseded in part (2026-09-15) — see ADR-005.

> **Supersession note.** The terminal-first mechanics of this record —
> `ActiveGeneration` holding a PTY + harness process + `VirtualTerminal`,
> terminal snapshot/attachment architecture — are superseded by ADR-005's
> `HarnessRuntime`/`RelaySession` model over native structured protocols.
> Still valid and carried forward: one daemon per state root, durable
> logical session identity, zero-runtime-resources-when-hibernated,
> exact native resume identity (never newest/`--last`), and the
> observer-never-wakes rule.

## Decision

RepoSuite Relay runs **one persistent daemon per user/state root**
(`reposuite-relayd`), owning every managed session.

- **One lightweight logical record per managed session** — the
  `LogicalSession`: stable identity + metadata, cheap to keep forever.
- **Zero PTY/harness/xterm resources for a hibernated session.**
- **N `ActiveGeneration`s only for currently awake sessions** — each holds
  the PTY, the harness process generation, the `VirtualTerminal`, and
  generation-scoped timers/listeners/resources.

### Hibernated invariant

    harness processes = 0
    PTY               = 0
    VirtualTerminal   = 0

### Wake

    spawn a NEW harness generation
    resume the exact native session identity
    increment the generation counter

## Decisions recorded

- **One controlling attachment max** per session at a time.
- **Observers do not hold the awake lease** — an observer must never wake a
  hibernated session.
- **Snapshot + ordered future output** for viewer hydration — never raw
  hydration replay.
- **Exact native resume identity** — always the authoritative harness-native
  session identity; never "newest"/`--last` guessing.
- **Daemon/socket/state live only under `${REPOSUITE_HOME}/relay`**
  (default `~/.reposuite/relay`). Nothing under `~/.airelay`; Airelay is a
  separate product and is not a migration source at runtime.

## Non-goals for this record

Implementation details not yet proven (socket protocol, store schema,
attach/delivery wire formats, process-tree ownership mechanics) are
deliberately unspecified here and decided by later tasks.
