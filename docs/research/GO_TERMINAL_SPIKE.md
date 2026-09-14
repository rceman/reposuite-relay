# RepoSuite Relay — Go Terminal/Runtime Feasibility Spike

PROMPT_ID: RSR-P-20260914-1205-GO01
Branch: `spike/go-terminal-runtime`
Status: research spike — NOT production code, NOT v0.1.0

## Question

Can RepoSuite Relay use Go as its implementation language while preserving
the terminal fidelity and PTY behavior proven in Airelay v0.1.150
(baseline `eef8d94d622ecde790562838652be0a0188ff634`)?

Answer: **yes**. Recommendation: **GO_ACCEPT**.

## Environment / Dependencies

| Item | Version |
|---|---|
| Go toolchain | go1.25 (module directive); toolchain auto-resolved to go1.26.8 on a go1.24.3 host |
| `github.com/gitpod-io/xterm-go` | `v0.0.0-20260907130418-dae5128cb6b3` (master @ 2026-09-07; **no tagged releases exist** — pseudo-version pinned by commit hash) |
| `github.com/creack/pty` | `v1.1.24` (tagged release) |
| Transitive | `github.com/google/go-cmp v0.7.0` — xterm-go's own test dep only |
| Node oracle (spike-only, `spike-oracle/`) | `@xterm/headless@6.0.0`, `@xterm/addon-serialize@0.14.0` — same versions Airelay runs |

`go mod graph` shows exactly two direct dependencies. No frameworks, no daemon
libs, no CLI libs (stdlib `flag` only).

## Layout

```
cmd/relay-spike/      — minimal CLI: run a child under PTY+model, or capture raw stream
cmd/pty-probe-child/  — fixture binary: emits Codex's probe batch, hex-dumps replies, reports winsize
internal/term/        — Model wrapper (xterm-go + host palette + reply plumbing) + fingerprint comparer
internal/ptyx/        — creack/pty runner: StartWithSize/Setsize/Pump/Close
internal/fixtures/    — deterministic byte streams (several mirror Airelay's oracle tests)
internal/spike/       — 10 MiB perf probe + benchmarks
spike-oracle/         — Node xterm.js fingerprint oracle (clearly spike-only; node_modules gitignored)
testdata/             — captured fixtures (real codex-startup.raw, 44,487 bytes)
docs/research/        — this report
```

## APIs used

- `xterm.New(WithCols, WithRows, WithScrollback, WithVtExtensions{KittyKeyboard:true})`
- `term.Write([]byte)` — synchronous parse; all state settled on return (no async write queue like xterm.js — a simplification vs Node)
- `term.OnData(fn)` — terminal-generated reply bytes (DA/DSR/CPR/DECRQM/kitty/window reports)
- `term.OnColor(fn)` — OSC 4/10/11/12 set/query/restore events; host supplies palette + replies
- `term.OnRequestWindowsOptionsReport / OnRequestColorSchemeQuery` — host-assisted reports
- `term.Resize(cols, rows)` — resize with reflow
- `term.Buffer()/NormalBuffer()/AltBuffer()/IsAltBufferActive()`, `buffer.Lines`, `line.LoadCell`, `CellData/AttributeData` — full cell inspection
- `term.Modes()/DecPrivateModes()/CurAttrData()/IsCursorHidden()/ScrollTop()/ScrollBottom()`
- `xterm.NewSerializeAddon(term).Serialize(&SerializeOptions{Scrollback})`
- `pty.StartWithSize / pty.Setsize / pty.Getsize`

## Terminal query results (zero-viewer)

Exact bytes observed, all replies generated synchronously inside `Write` and
deliverable straight back into the PTY master.

| Query | Classification | Observed reply |
|---|---|---|
| `CSI 6n` cursor pos | automatic | `ESC [ {r} ; {c} R` (1-based; `\x1b[1;1R` at home, `\x1b[3;1R` mid-stream) |
| `CSI 5n` status | automatic | `\x1b[0n` |
| `CSI ?6n` DECXCPR | automatic | `\x1b[?{r};{c}R` |
| `CSI c` primary DA | automatic | `\x1b[?1;2c` |
| `CSI >c` secondary DA | automatic | `\x1b[>0;276;0c` |
| `CSI ?u` kitty query | configuration | `VtExtensions.KittyKeyboard=true` → `\x1b[?{flags}u`; with ext off → silence (correct unsupported-terminal behavior) |
| `CSI >f u` / `CSI <u` / `CSI =f;m u` | configuration | kitty stack ops; verified `\x1b[>1u\x1b[?u`→`\x1b[?1u`, `\x1b[>15u`→`\x1b[?15u`, `\x1b[<u`→`\x1b[?1u` |
| `OSC 10 ; ?` fg | host-configured | `OnColor` report event → host replies `\x1b]10;rgb:e5e5/e5e5/e5e5\x1b\\` (xterm-style 16-bit channels) |
| `OSC 11 ; ?` bg | host-configured | same path → `\x1b]11;rgb:1a1a/1a1a/1a1a\x1b\\` |
| `OSC 12 ; ?` cursor color | host-configured | same path |
| `OSC 4 ; i ; ?` palette | host-configured | `\x1b]4;i;rgb:…\x1b\\`; set/restore tracked host-side (`OSC 10;rgb:12/34/56` set → queried back `1212/3434/5656`) |
| `CSI ? Ps $ p` DECRQM | automatic | e.g. `\x1b[?2004;2$y`, `\x1b[?2026;2$y` |
| `CSI >q` XTVERSION | automatic | `\x1bP>|xterm-go(0.1.0)\x1b\\` (note: reports "xterm-go" identity) |
| `CSI 18 t` win size | automatic | `\x1b[8;{rows};{cols}t` (superset: JS headless stays silent — harmless) |
| `CSI 14 t` / `16 t` | host-assisted | fires `OnRequestWindowsOptionsReport` (host must answer; matches upstream design) |
| `CSI ?996n` color scheme | host-assisted | fires `OnRequestColorSchemeQuery` only after app enables DECSET 2031; else silence (matches upstream) |
| `DCS $q …` DECRQSS | automatic | `\x1bP1$r0m\x1b\\` |
| `CSI =c` DA3 | unsupported | silence — identical to upstream |
| `DCS +q` XTGETTCAP | unsupported | silence — identical to upstream (verified on oracle) |

Nothing in Codex's required set is unsupported. The two host-configured
classes (OSC colors, window reports) are ~140 LOC of glue in
`internal/term/model.go` — the same responsibility xterm.js delegates to its
embedder. No query was faked.

## Central feasibility test (zero-viewer, real PTY)

`cmd/pty-probe-child` runs under `creack/pty`, sets its tty raw, emits the
exact Codex startup batch `ESC[6n ESC]10;? ST ESC]11;? ST ESC[?u ESC[c`,
reads replies from stdin, and hex-dumps them. Harness wires
`child output → term.Write` and `term reply → pty.Write`. Result — the child
received, through a real Linux PTY with no physical terminal anywhere:

```
\x1b[1;1R  \x1b]10;rgb:e5e5/e5e5/e5e5\x1b\\  \x1b]11;rgb:1a1a/1a1a/1a1a\x1b\\  \x1b[?0u  \x1b[?1;2c
```

Phase 2 (after `\x1b[>1u\x1b[?u`): `\x1b[?1u`. Resize mid-run was observed by
the child: `WINSIZE 50x100`. Exit 0. (`internal/ptyx/ptyx_test.go`)

## Real Codex fixture — PASS

Captured 44,487 bytes from the real `codex` binary (`/usr/local/bin/codex`,
@openai/codex@0.154.0) spawned under our PTY with `HOME=<tmpdir>`,
`CODEX_HOME=<tmpdir>/.codex`, `TERM=xterm-256color` — never touched
`~/.codex`, never attached to or prompted a user session
(`testdata/codex-startup.raw`; regenerable via `CAPTURE_CODEX=1`).

Sequences actually present (deduped, all classes):

- Probes: `ESC[6n` ×1, `ESC]10;? ST` ×1, `ESC]11;? ST` ×1, `ESC[?u` ×1, `ESC[c` ×1 — the documented batch, complete set confirmed
- `ESC[>7u` — kitty flags push (codex pushes before probing; query correctly replies `ESC[?7u`)
- `ESC[?2004h`, `ESC[?1004h`, `ESC[>4;0m`, `ESC[?25l`
- `ESC[?2026h` / `ESC[?2026l` ×68 — synchronized output per frame
- No DECSTBM, no alt buffer (Codex is inline TUI — confirmed)
- Indexed + RGB SGR, cursor addressing, EL/ED

Replaying it through xterm-go produced exactly 5 reply events
(`\x1b[3;1R`, OSC10, OSC11, `\x1b[?7u`, `\x1b[?1;2c`), rendered the auth
screen correctly, and the snapshot round-tripped with **0 cell diffs**.

## Snapshot serialization — PASS (gate D/E)

`SerializeAddon` is a faithful port of upstream master's `addon-serialize`
(including the xterm.js #3677 fix: the trailing SGR that restores the
cursor's *active* pen — the exact mechanism behind Airelay's continuation
regression).

Verified:

- viewport-only (`scrollback:0`) and full-scrollback snapshots restore cells + cursor + modes + scroll region into fresh terminals — fingerprint-identical
- **active SGR continuation**: `ESC[48;5;236m ESC[38;2;1;2;3m ESC[1m` at snapshot → post-snapshot `X` lands with bg pal:236, fg rgb:010203, bold on both sides — identical fingerprints (also italic+underline+inverse multi-attr)
- **future output order**: `[snapshot][post-boundary bytes]` replayed into a fresh terminal equals the authoritative source
- dirty destination: prefixing `\x1b[0m\x1b[H\x1b[2J\x1b[3J\x1b[H` (Airelay's `LIVE_PRESENTATION_RESET`) restores over stale content
- active alternate buffer serializes and restores (`?1049h` + content + `?1049l` round-trip keeps normal buffer intact)
- snapshots are restore-once into a clean target (repeated application scrolls — expected, matches upstream semantics)

### Serializer quirk (documented, bounded)

The serializer emits DECSTBM *after* the cursor-restore tail, and DECSTBM
homes the cursor → restored cursor lands at (0,0) instead of the true
position **when a custom scroll region is set at snapshot time**. This is
upstream-inherited behavior — upstream master's addon-serialize does the
same thing in the same order (verified in source). It differs from the older
`@xterm/addon-serialize@0.14.0` Airelay pins, which doesn't serialize the
scroll region at all (preserving cursor but losing the region). Severity:
low — Codex's real stream contains zero DECSTBM; workaround is a small
upstreamable patch (emit region before content, or re-emit cursor restore
after it) if Relay ever needs it for scroll-region TUIs.

Also serialized but absent from 0.14.0: `?25l` cursor-hidden (matches upstream master).

## Chunk-boundary robustness — PASS

Whole-write vs byte-at-a-time vs 8 seeded random chunkings over 6 stream
classes (style tour, codex probes, unicode, alt buffer, full-width bg,
mixed): fingerprints identical in all cases. Every probe split at every
byte boundary still produced exactly one correct reply (for OSC, the ESC of
`ESC \` legitimately completes the sequence — an early reply there is
correct parsing, not a bug).

## Style/cell fidelity — PASS

Per-cell assertions (chars, width, fg/bg mode+color, bold/dim/italic/
underline/inverse/blink/invisible/strikethrough/overline) for: standard and
bright colors (SGR 90–97→palette 8–15, 100–107→bg 8–15), 256-indexed, RGB,
fg+bg, all flags, full-row bg incl. trailing blank cells, BCE erase-fill
(`ESC[2K` fills with active bg — verified cell bg pal:236 at col 127),
inverse full-width, EL/ED, wide CJK (width-2 + width-0 continuation),
combining sequences (e+U+0301, a+U+0308 stored as combined cells).

## Alternate buffer — PASS

`?1049h` activates alt buffer; content written there is isolated; normal
buffer retains history; `?1049l` restores normal viewport.
`IsAltBufferActive()` is the authoritative tracker. Snapshot while alt is
active restores both buffers and the active selection.

## Resize — PASS with upstream quirk

120×30 → 108×71 → 60×71 → 108×20 → 140×40: dims, fingerprint integrity,
wide chars and content survive reflow. Narrowing a wrapped line re-wraps;
widening unwraps (43 W's → 20+20+3 → 43). Alt buffer stays active across
resize.

Upstream quirk verified against the oracle: when the cursor sits inside the
wrapped group being reflowed, the group is skipped (`reflowCursorLine`
defaults off) and trailing cells are truncated on shrink — identical in
`@xterm/headless` (both lose the same 20 cells on the same stream; oracle
parity case `wrap-cursor-line-quirk` passes with 0 diffs).

## Concurrency — PASS

8 independent terminals fed different streams concurrently (different
sizes, markers, probes): no shared cursor/reply/buffer state, all replies
per-instance. `-race` clean. One data race was found **in spike code**
(`ptyx.Process.Close` reading `Cmd.ProcessState` vs `cmd.Wait`) and fixed —
xterm-go itself showed no races.

## Lifecycle — PASS

200× (create → feed fixtures → snapshot → resize → dispose): goroutine
delta 0, fd delta 0. 50× PTY spawn/close: fd delta 0.

## Performance — PASS (comfortably sufficient)

i9-9900K, Linux amd64:

| Metric | Result |
|---|---|
| Parse+write 10 MiB ANSI stream (64 KiB chunks) | 415.6 ms ≈ **24 MiB/s**; formal bench 29.2 MB/s over 4 MiB |
| Snapshot serialize (viewport, populated term) | 460 µs – 750 µs |
| Resize 120×40→100×50 (10k scrollback reflow) | 34.7 ms |
| Terminal create+dispose | 129 µs |
| Process RSS after 10 MiB ingest + 10k scrollback | ~61 MiB (heap inuse 55.8 MiB incl. the stream copy) |
| `relay-spike` binary | 3.75 MiB |

A 1 MiB Codex hydration parses in ~40 ms and serializes in <1 ms. No
premature optimization needed.

## Node/xterm.js oracle parity — PASS

`spike-oracle/oracle.mjs` dumps a canonical JSON fingerprint
(chars/width/fg/bg/flags per cell + cursor + modes + alt + scrollback) from
`@xterm/headless`; Go emits the same shape from `term.TakeFingerprint`.
11 cases, **0 diffs**: style tour, full-width bg, unicode, alt buffer, BCE,
wrap reflow narrow+wide, cursor-line reflow quirk, and 4 snapshot
round-trips (style/bg/alt/unicode). Structural comparison was practical via
the shared cell encoding; fields the JS public API cannot expose (scroll
region, cursor visibility, current pen) are verified Go-side by the
non-oracle tests.

## Unsupported / mismatched features (full list)

| Item | Status | Severity for Relay |
|---|---|---|
| OSC 10/11/12, OSC 4 palette queries | host-configured (as upstream intends) | none — ~140 LOC palette glue, done |
| CSI 14t/16t window reports | host event; host answers | low — Codex doesn't send them |
| CSI ?996n color scheme | host event after DECSET 2031 | low — Codex doesn't send it |
| CSI =c (DA3), DCS +q XTGETTCAP | silent — matches upstream exactly | none observed in Codex |
| DECSTBM-at-end-of-snapshot homes cursor | upstream-inherited quirk | low — Codex emits no DECSTBM; small upstreamable fix exists |
| XTVERSION reply identity | reports `xterm-go(0.1.0)` | cosmetic; upstreamable to override |
| CSI 18t auto-reply | superset vs JS headless | harmless |
| Windows/ConPTY modes | present, unused | n/a — Linux only |

No hidden approximations: every supported reply above is byte-exact as
recorded; unsupported ones are silent exactly like upstream.

## Decision gates

| Gate | Result | Evidence |
|---|---|---|
| A — PTY | PASS | StartWithSize lands exact geometry (`stty size` → `41 133`), live `Setsize` → child reports `WINSIZE 50x100`, EIO/EOF pump handling, 50-cycle churn clean |
| B — Terminal model | PASS | cell-identical to `@xterm/headless` on all fidelity fixtures + real Codex stream |
| C — Terminal responses | PASS | all 5 Codex probes answered end-to-end through a real PTY with zero viewers |
| D — Snapshot | PASS | round-trips viewport/scrollback/alt/modes/cursor; 0 cell diffs incl. real Codex stream |
| E — Live continuation | PASS | active SGR (RGB fg + palette bg + bold + multi-attr) survives snapshot; post-snapshot bytes land identically |
| F — Chunking | PASS | byte-wise + 8 seeded random splits → identical fingerprints; split queries still answered |
| G — Multi-session | PASS | 8 concurrent instances, disjoint state, `-race` clean |
| H — Performance | PASS | ~24–29 MiB/s parse, sub-ms snapshot — far above hydration needs |
| I — Maintainability | PASS | zero custom parser/serializer; upstream port + official serializer only; ~140 LOC host glue |

## Recommendation

**GO_ACCEPT.**

xterm-go is a faithful port of upstream xterm.js (verified cell-for-cell
against the actual JS implementation including its quirks), its serializer
implements the SGR-continuation fix Airelay depends on, creack/pty covers
the Linux PTY lifecycle, and the zero-viewer query/reply path works
end-to-end against the real Codex binary. Nothing requires a custom
emulator or serializer. The only found divergences are upstream-inherited
or host-configurable, all bounded and documented.
