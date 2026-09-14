# AGENTS.md — reposuite-relay

## Scope

- Linux-only. Do not add macOS/Windows abstractions or stubs.
- Go implementation, stdlib-first. No CLI/DI/logging/config frameworks.
- Production Go toolchain: **exactly go1.27.1** (`go 1.27.1`, no floating
  toolchain). CI fails unless `go env GOVERSION == go1.27.1`.

## Gates

Before finalizing any task, run the canonical gates in `GATES.md` and
report each as PASS/FAIL/N/A with evidence.

## Runtime boundaries

- Relay state lives only under `${REPOSUITE_HOME}/relay`
  (default `~/.reposuite/relay`). See `internal/paths`.
- **Never** touch `~/.airelay`, `AIRELAY_*`, Airelay sockets/PIDs/state, or
  live Airelay/Codex sessions. Airelay is reference material, not a runtime
  dependency.
- Tests must use isolated temp roots — never the developer's real
  `~/.reposuite` or `~/.airelay`.
- Private state directories use `0700`.

## Dependencies

- Terminal engine is `github.com/rceman/xterm-go` at an exact pinned
  commit (see `docs/dependencies/XTERM_GO.md`). Do not float it, do not
  `replace` it, and never import `github.com/gitpod-io/xterm-go` in code.
- Bumping the pin follows the update policy in that document.

## Architecture invariants

- "Hibernated" means **zero** active runtime resources (no process, no PTY,
  no xterm object).
- Session resume requires the **exact native session identity** — never
  newest/`--last` guessing.
- Terminal primitives live in `internal/terminal` (generic) and
  `internal/pty`; session/daemon concepts do not belong there.
- The headless terminal is the child's terminal peer: PTY output →
  `terminal.Write`; terminal replies → PTY input. Zero viewers required.
- Do not modify spike evidence branches casually
  (`spike/*` are immutable research references).

## Style

Small, explicit packages. No speculative interfaces, service containers,
event buses, or persistence abstractions. Optimize for correctness,
readability, low overhead, and auditability.
