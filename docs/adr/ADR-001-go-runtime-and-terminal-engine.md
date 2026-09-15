# ADR-001: Go runtime and terminal engine

Status: Accepted (2026-09-14) — partially superseded (2026-09-15)

> **Supersession note.** The **Go runtime/toolchain decision stands**.
> The following portions are superseded by ADR-005/ADR-006: the
> Linux-only product target (cross-platform target is now Linux + macOS +
> Windows), PTY as canonical runtime transport, and `xterm-go` as the
> canonical terminal engine (no terminal emulation in Relay core; both
> dependencies are scheduled for removal). The spike evidence below is
> preserved as research history.

## Decision

RepoSuite Relay is implemented in **Go**.

- Canonical toolchain: exactly **go1.27.1** (`go 1.27.1`; CI hard-fails
  unless `go env GOVERSION == go1.27.1`).
- Platform scope: **Linux only**.
- PTY: `github.com/creack/pty v1.1.24`.
- Terminal engine: `github.com/rceman/xterm-go` — our maintained downstream
  fork of `github.com/gitpod-io/xterm-go` — pinned at commit
  `02f8c312eaafac9069c571f6a26982238a001360`
  (`v0.0.0-20260914121151-02f8c312eaaf`; accepted baseline `f690bbc` +
  first downstream fix: OSC 104 restore-all event disambiguation), no
  replace directive.
- Node/xterm.js is **not** a production dependency. The Node oracle used in
  the feasibility spike remains research tooling only.
- No Rust spike: unnecessary — Go passed all required fidelity gates.

## Evidence

`docs/research/GO_TERMINAL_SPIKE.md` records the accepted feasibility spike:
zero-viewer terminal query/reply over a real PTY, snapshot round-trip and
active-SGR continuation, alternate buffer, resize/reflow, arbitrary chunk
boundaries, multi-instance race-freedom, real Codex startup fixture replay,
and 11/11 cell-identical parity cases vs `@xterm/headless`.

## Known limitation (non-blocking technical debt)

The upstream-inherited serializer emits a custom scroll region (DECSTBM)
after the cursor-restore tail; DECSTBM homes the cursor, so a restored
terminal loses exact cursor position **when a custom scroll region was
active at snapshot time**. This matches upstream xterm.js master's addon
behavior; Codex-class streams do not use DECSTBM. A focused
`rceman/xterm-go` fix is planned later — see `docs/research/GO_TERMINAL_SPIKE.md`
and `docs/dependencies/XTERM_GO.md`.

## Consequences

- Fork policy: upstream changes adopted selectively; local fixes may land in
  `rceman/xterm-go` immediately when Relay needs them (with tests).
- xterm-go remains a generic terminal library — no RepoSuite logic upstream.
- A pre-stable security audit of `rceman/xterm-go` is required before public
  release (see `docs/dependencies/XTERM_GO.md`).
