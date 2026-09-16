# Native Harness Architecture — Research Report

Status: research complete, accepted (ADR-005); §1/§7/§7c/§7d now have a
running implementation (see §7f and §11). Date: 2026-09-15, updated
2026-09-16. Base: main `633fa3f`; implementation base
`feature/core-domain-persistence-a2`.

Product direction: **RepoSuite Relay is a persistent structured
agent-session daemon with native harness adapters** — not a terminal
relay. PTY/terminal emulation is not part of the canonical architecture.

All findings below were measured against the actually installed binaries,
not documentation: `codex 0.154.0`, `opencode 1.17.20`,
`devin 3000.10.21`. Protocol traces were captured live.

---

## 1. Codex app-server (verified)

**Runtime command:** `codex app-server --listen stdio://` (default).
Also `unix://PATH`, `ws://IP:PORT`, `off`. A managed shared daemon exists:
`codex app-server daemon start|stop|restart`; `codex agents` is a TUI
browsing sessions on that daemon — **multi-session shared runtime is the
native topology**, confirmed live.

**Protocol:** newline-delimited JSON-RPC 2.0. `generate-json-schema`
produced the authoritative v2 surface: 99 client methods, 81 server
notifications, 10 server→client requests. Live probe:

```
> {"jsonrpc":"2.0","id":1,"method":"initialize",...}
< {"id":1,"result":{"userAgent":"rsr-probe/0.154.0",...}}
> {"id":2,"method":"thread/list"}
< result.data[] = persisted threads, stable UUID ids
```

**Native concepts:** `thread` (session; `thread/start`, `thread/resume`,
`thread/read`, `thread/list`, `thread/fork`, `thread/archive`),
`turn` (`turn/start` input, `turn/interrupt`, `turn/steer`),
`item` (userMessage, agentMessage, reasoning, commandExecution…;
`item/started`, `item/completed`, `item/agentMessage/delta`).

- **Streaming:** `item/agentMessage/delta` + `turn/completed`.
- **Exact session ID:** UUID `threadId`; `thread/resume {threadId}` —
  verified exact-ID cold resume after full process restart (see §5).
- **Requested input:** `ToolRequestUserInput` server→client request —
  `questions[]` with `header`, `id`, `question`, `options[]`
  (A/B/C-style) **or** `options:null` + `isOther`/`isSecret` (free text).
  ⚠ Environment-gated: a live turn reported
  `request_user_input is unavailable in Default` — the mechanism exists
  in the protocol but is not enabled in the default environment.
- **Approvals:** `approvalPolicy` (untrusted/on-request/never/granular) +
  `approvalsReviewer` (user/auto_review/guardian_subagent) at both
  `thread/start` and `turn/start` scope. `never` = bypass. Approval
  requests are typed server→client requests (exec/file-change/
  permissions/apply-patch/mcp-elicitation).
- **Model/mode:** `model`, `serviceTier`, `personality`, `effort`
  per-thread AND per-turn (`turn/start.model`, `serviceTierForTurn`);
  `model/list`; `model/rerouted` notification.
- **Metrics:** `thread/tokenUsage/updated` notifications;
  `account/rateLimits/read`, `account/usage/read`,
  `account/rateLimits/updated`.

## 2. OpenCode server (`opencode serve`) (verified)

**Runtime command:** `opencode serve --port <p>` — headless HTTP server
on 127.0.0.1, no auth by default. Authoritative OpenAPI 3.1 spec at `/doc`.

**Native concepts:** `session` (`ses_*` id — durable server-side store;
100+ historical sessions listed), `message`, `part`, SSE event stream.

- `POST /session`, `GET /session/{id}` (exact-ID resume verified across
  full restart), `session/prompt`, `session/prompt_async`,
  `session/abort`, `v2.session.interrupt`, `session.fork`.
- **Requested input:** `question.asked` / `question.replied` /
  `question.rejected` SSE events + `POST /question/{requestID}/reply|reject`,
  `GET /question` — `questions[]` = `{header, question,
  options[{label,description}], multiple, custom}`. Verified live: a turn
  produced `que_…` with 3 options. ⚠ **Pending questions do NOT survive
  server restart** — after kill+restart `GET /question` returns empty and
  the tool part is permanently stranded `status:"running"` →
  `waiting_input` is a hard sleep blocker.
- **Free model:** `opencode/muse-spark-1.3-contributor-free` runs turns
  without a paid key (used for the measurements below).
- **Permissions:** `permission.asked/replied` events +
  `permission/{requestID}/reply` (`once|always|reject`).
- **Model/mode:** `v2.session.switchModel`, `v2.session.switchAgent`
  (mode = agent), `v2.model.list`, `provider.list`.
- **Metrics:** `session.tokens {input,output,reasoning,cache}`,
  `session.cost`, `v2.session.context`.
- **History:** `session.history`, `session.messages` — transcript is
  server-side and durable.
- **Events:** `/event` + `/global/event` SSE; 94 event types incl.
  `session.idle`, `session.status`, `message.part.delta`.

## 3. OpenCode ACP (`opencode acp`) (verified)

JSON-RPC 2.0 over stdio per the Agent Client Protocol.

```
< initialize.result.agentCapabilities:
  loadSession:true, mcpCapabilities:{http,sse},
  promptCapabilities:{embeddedContext,image},
  sessionCapabilities:{close,fork,list,resume}
< session/new → sessionId "ses_..." (SAME durable ses_ store as serve)
```

- `session/list` works; **`session/load` with exact `ses_` id succeeds in
  ~0.55s after full process restart** — cold resume confirmed even for a
  zero-turn session.
- `configOptions` in `session/new` result expose `model` select with the
  full provider/model list; **`session/set_config_option` switches the
  model mid-session** (verified: `currentValue` updated to
  `opencode/muse-spark-1.3-contributor-free`).
- One process hosted 5 concurrent sessions (multi-session confirmed).
- Live turn on the free `muse-spark-1.3-contributor-free` model:
  prompt→first `agent_message_chunk` 2.07s, `stopReason:end_turn`,
  `usage{input 7476, output 12, thought 5}`.

## 4. Devin ACP (`devin acp`) (verified)

JSON-RPC 2.0 over stdio, ACP v1 + Cognition extensions.

```
< agentCapabilities: loadSession:true,
  sessionCapabilities:{list,delete,additionalDirectories},
  promptCapabilities:{image,embeddedContext},
  _meta.cognition.ai/{multiRootWorkspace,sessionRename,sessionShare,
    documentLifecycle,userEdits,terminalLifecycle,userConfig,
    userShellCommand,editableCommands,commandRevision,chains,megaplan,
    ruleMentions}
```

- `session/new` → `sessionId` = human slug (`petal-warlock`); **`bypass`
  appears as a `configOptions` mode** ("Bypass Permissions — auto-approve
  all tool calls") alongside `accept-edits`/`smart`/`ask`/`plan`.
- `session/prompt` → streamed `session/update` chunks
  (`agent_thought_chunk`, `agent_message_chunk`), result
  `stopReason:"end_turn"` + `usage{inputTokens,outputTokens,totalTokens}`.
- **`session/load` with exact slug id succeeded in ~0.13s after full
  restart** (zero-turn sessions return `session_not_found` — the session
  materializes on first turn); `session/list` enumerates durable sessions.
- Requested input: no generic elicitation capability advertised; ACP
  `session/request_permission` (options[]) is the in-band ask mechanism;
  Devin's `_meta` surface is rich but a free-form A/B/C question was not
  observed → PARTIAL/UNKNOWN.

## 4b. Capability matrix

| capability | Codex app-server | OpenCode serve | OpenCode ACP | Devin ACP |
|---|---|---|---|---|
| native structured protocol | YES (JSON-RPC 2.0) | YES (HTTP+SSE, OpenAPI) | YES (ACP JSON-RPC) | YES (ACP JSON-RPC) |
| transport | stdio/unix/ws | HTTP+SSE | stdio | stdio |
| multi-session runtime | YES (daemon + 5 threads live) | YES (server hosts all) | YES (5 verified) | YES (5 verified) |
| exact session ID | YES (UUID threadId) | YES (ses_*) | YES (ses_*) | YES (slug) |
| session create | YES `thread/start` | YES `POST /session` | YES `session/new` | YES `session/new` |
| session resume | YES `thread/resume` (≥1 turn) | YES `GET /session/{id}` | YES `session/load` | YES `session/load` |
| streaming output | YES (`item/agentMessage/delta`) | YES (SSE part.delta) | YES (`session/update`) | YES (`session/update` chunks) |
| cancel | YES `turn/interrupt` | YES `abort`/`interrupt` | YES `session/cancel` | YES `session/cancel` |
| requested user input | PARTIAL (`ToolRequestUserInput`, env-gated) | YES (`question.asked`+reply) | PARTIAL (ACP request_permission) | PARTIAL (request_permission; generic ask unobserved) |
| model selection | YES (thread+turn `model`) | YES (`switchModel`) | YES (configOptions) | YES (`--model`, configOptions) |
| mode/fast selection | YES (`effort`, `serviceTier`) | YES (`switchAgent`, `variant`) | YES (`session/set_config_option`, verified) | YES (mode: code/smart/ask/plan/bypass) |
| context metrics | YES (`tokenUsage/updated`) | YES (`session.context`) | PARTIAL | YES (usage in prompt result) |
| quota metrics | YES (`account/rateLimits/*`) | PARTIAL (`session.cost`) | PARTIAL | PARTIAL |
| bypass permissions | YES (`approvalPolicy:never`, reviewer) | YES (permission config/agent mode) | PARTIAL | YES (`bypass` mode) |
| client reconnect | YES (reconnect socket, thread/list) | YES (stateless HTTP) | YES (re-spawn + load) | YES (re-spawn + load) |
| runtime restart resume | YES (proven) | YES (proven) | YES (proven) | YES (proven) |
| schema discoverability | YES (`generate-json-schema`) | YES (`/doc` OpenAPI) | YES (ACP spec + initialize caps) | YES (ACP + `_meta` caps) |

## 5. Cold-resume / resource measurements (final, verified)

Measured via `/proc/<pid>/smaps_rollup` (PSS preferred) + process-tree
walk + fd count. Cold = runtime absent = 0 procs / 0 RAM (trivially true
for all — every runtime is just a child process we spawn).

Fresh-session prompt: `hey` (one turn per measurement). Models:
Codex `gpt-5.6-luna` @ `medium` (subscription); OpenCode serve + ACP
`muse-spark-1.3-contributor-free` (free); Devin `swe-1-7-medium`
(subscription, session default — confirmed `currentValue`).

| state | Codex app-server | OpenCode serve | OpenCode ACP | Devin ACP |
|---|---|---|---|---|
| model used | `gpt-5.6-luna` @ `medium` (sub) | `muse-spark-1.3-contributor-free` (free) | `muse-spark-1.3-contributor-free` (free) | `swe-1-7-medium` (sub; session default) |
| tree pids | 2 (codex+node) | 1 | 1 | 1 |
| STARTED PSS (no session) | 94.6 MB | 300.6 MB | UNKNOWN | ~27 MB |
| 1 idle session PSS | 124.6 | 327.6 | ~356 MB | ~46 MB (loaded) |
| 5 idle sessions PSS | 170.1 (+~15/thread) | 330.2 (+0.7/sess) | 485.8 | 89.8 |
| post-turn PSS | 225.8 | 650.8 | 572.5 | 51.2 |
| FDs | 56–73 | 24–34 | 36–41 | 28–29 |
| spawn→ready | 0.25s | 2.06s | 1.73s | 0.19s |
| exact cold resume | YES (`thread/resume`, ~0.3s; needs ≥1 turn materialized; ephemeral=opt-out) | YES (`GET /session/{ses_id}`, immediate) | YES (`session/load`, 0.55s) | YES (`session/load`, 0.13s) |
| fresh prompt→first agent event | 4.15s | 3.57s | 2.22s | 1.37s |
| transcript survives restart | YES | YES | YES | YES |
| model config survives | YES (incl. per-turn effort override) | YES | YES | YES |
| waiting_input survives restart | NO (transport-bound; tool env-gated) | **NO — proven** (pending `que_` lost; tool part stranded `running`) | NO (in-flight req dies) | NO (same) |
| verdict | COLD_RESUME_SUPPORTED | COLD_RESUME_SUPPORTED | COLD_RESUME_SUPPORTED | COLD_RESUME_SUPPORTED |

Note on OpenCode ACP: `~356 MB` was measured on a process that already
held one session — it is reported under "1 idle session", not as a true
zero-session STARTED value. Zero-session STARTED was not separately
measured → UNKNOWN.

**Latency terminology** — three distinct metrics, never conflated:

- **A: spawn → protocol ready** (process up, initialize/health answered)
- **B: ready → exact session resumed** (`thread/resume`/`session/load`/`GET /session/{id}`)
- **C: prompt dispatch → first agent event** (first delta/chunk/part)

Full cold-prompt latency (COLD → first agent event) was **not** measured
as one timer; A+B+C above are per-phase measurements — composing them is
inference, not a benchmark. The fresh-session C values (4.15 / 3.57 /
2.22 / 1.37s) supersede earlier mixed fresh/resumed numbers.

Real-quota turns consumed: Codex ×4, OpenCode serve ×4, OpenCode ACP ×2,
Devin ACP ×3 — minimal single-turn prompts only.

**Resource interpretation:** OpenCode runtimes are expensive residents
(post-turn PSS 650.8 / 572.5 MB vs Codex 225.8, Devin 51.2). Sleep/wake
is therefore a **core resource-management feature**, not cosmetic. Per
idle session cost is tiny for serve (+0.7 MB) and ACP; Devin is the
cheapest resident overall. There is **no universal idle timeout** —
`RuntimePolicy{warmGrace, coldResumeSupported, sleepBlockers}` is
per-adapter/per-runtime, with defaults later derived from resident PSS,
spawn latency, resume latency, and cold-prompt latency — not arbitrary
constants.

## 5b. Runtime transport security (verified)

Harness runtimes are **private implementation details of relayd** —
external clients (TUI/Web/Gateway/automation) never connect to them
directly. Verified transport/auth facts:

| fact | Codex app-server | OpenCode serve | OpenCode acp | Devin acp |
|---|---|---|---|---|
| preferred Relay transport | **stdio** (default) or `unix://` | HTTP loopback | **stdio only** | **stdio only** |
| network listener required | NO | YES (HTTP) | NO | NO |
| default bind | none (stdio) | 127.0.0.1 | n/a | n/a |
| loopback restriction | n/a | YES (`--hostname`, default is loopback; `--mdns` switches to 0.0.0.0 — never enable) | n/a | n/a |
| unix socket | YES — `unix://PATH` created **`srw-------` (0600)** verified | NO | n/a | n/a |
| authentication | stdio: n/a; ws: `--ws-auth capability-token\|signed-bearer-token` (for non-loopback); token via `--ws-token-file`/`--ws-shared-secret-file` (file, not argv) | YES — verified: `OPENCODE_SERVER_PASSWORD` env → HTTP Basic `opencode:<pw>`; all endpoints incl. `/event` SSE and `/session` return 401 without it | n/a (pipe fd) | n/a (pipe fd) |
| credential supply | env/`--ws-token-file` path | env `OPENCODE_SERVER_PASSWORD` (not argv) | — | — |
| argv/log exposure | token via file/sha — safe | env — safe | — | — |
| ephemeral credential per runtime | YES (random token file) | YES (random `OPENCODE_SERVER_PASSWORD` per spawn) | n/a | n/a |
| misconfiguration risk | ws on non-loopback w/o auth | `--mdns`/non-loopback bind or missing password = open server | none | none |

**Policy:** prefer `stdio` (Codex/ACP) → `unix://` socket 0600 (Codex
where a socket is needed) → loopback HTTP **with** generated Basic
password (OpenCode serve — Relay should *always* set a fresh random
`OPENCODE_SERVER_PASSWORD` per runtime spawn; never rely on the
unauthenticated default). Runtime credentials are **ephemeral per
generation** — generated with `crypto/rand` at wake, held only in the
runtime record, discarded at sleep; never part of `nativeSessionId`,
event history, logs, or status output.

**Fail-closed startup:** after spawning, the adapter must verify the
expected posture (e.g. `ss`/connect-check: listener is loopback-only;
a no-auth request returns 401). If the runtime comes up `0.0.0.0` or
unauthenticated, stop it and fail startup rather than accept the weaker
configuration.

## 6. Final canonical model (accepted direction)

- **RelayDaemon** — one daemon per `${REPOSUITE_HOME}/relay` state root
  (singleton/lifecycle semantics from ADR-003 retained; transport
  superseded by ADR-006).
- **HarnessRuntime** — replaces ActiveGeneration:
  `{runtimeID, harness, transport(stdio|unix|http+sse), processTree,
  state: COLD|STARTING|WARM|ACTIVE|STOPPING, capabilities,
  policy: RuntimePolicy}`. One runtime hosts N sessions (verified on all
  four paths).
- **HarnessAdapter** — per-harness protocol codec: Codex NDJSON/JSON-RPC,
  shared ACP client for Devin + OpenCode acp, OpenCode HTTP+SSE.
- **RelaySession** — durable logical identity:
  `{key, sessionId, nativeSessionId, runtimeID|-, state,
  config{model,mode}, transcriptCursor, createdAt}`. Survives TUI/Web
  disconnect, runtime sleep/death, and relayd restart (where the native
  store supports exact resume). Never resolved by last/newest/timestamp.
- **RuntimePolicy** — per-adapter/per-runtime:
  `{warmGrace, coldResumeSupported, sleepBlockers[]}`; defaults derived
  from measured PSS + latencies, not constants.
- **CanonicalEvent** — monotonic `seq` per session:
  `session.state`, `message.user`, `message.agent.delta`,
  `message.agent.completed`, `input.requested`, `input.resolved`,
  `session.metrics`, `session.error`.
- **RequestedInput** — `{requestId, questions[{id,header,question,
  options[]|null(free-text),multiple,custom,secret}], blocking}` —
  responder-agnostic (TUI/Web/Planner/lead agent/automation).
- **SessionMetrics** — all optional: `{model, mode, contextUsed,
  contextLimit, tokens{in,out,reasoning,cache}, quota{used,limit,
  resetAt}}`; UI omits absent fields.

## 7. RuntimeSupervisor (design only)

`internal/runtime` later: `EnsureAwake(runtimeID)` → spawn + ready-poll;
`ResumeExact(nativeSessionId)`; `Dispatch(prompt)`; session↔runtime
ownership tracking; `SafeToSleep`; graceful `Stop`; tree measurement via
`/proc`. **Sleep authority is per-runtime** — one shared runtime stays
awake while ANY bound session is unsafe (active turn, in-flight prompt
or cancel, unresolved requested input). Two-tier WARM-grace→COLD
evaluated later from measured data. UI attach must not wake; only
native ops (prompt, model/mode change, cancel, resume) wake.

## 7b. Relay-owned durable state (resolves the earlier contradiction)

UI attach must not wake a COLD runtime → Relay needs its own store.

- **Native harness store** — authoritative for: model context,
  native session, native resume state. Relay never duplicates model
  context.
- **Relay store** — authoritative for: Relay session key/identity,
  harness, exact `nativeSessionId` mapping, runtime reconstruction
  metadata, last-known model/mode, Relay state, compact transcript,
  event/reconnect cursor, relayd restart recovery.

**Filesystem-first layout** (stdlib only; no SQLite/Bolt/event-store):

```
${REPOSUITE_HOME}/relay/sessions/<id>/
    session.json       # identity, nativeSessionId, config, state
    transcript.jsonl   # durable records
    transcript.idx     # offset index -> bounded tail reads
```

Transcript tail access must be bounded — index/offset reads, never a
full-file scan. Schema details intentionally unfrozen.

## 7c. Live vs durable events

- **Transient (never persisted as deltas):** `message.agent.delta`,
  rapidly-changing metrics, streaming scratch state.
- **Durable (transcript records):** `message.user`,
  `message.agent.completed`, `input.requested`, `input.resolved`,
  important `session.state` transitions, last-known config/metrics.
- Goal: responsive streaming + compact durable transcript.

## 7d. Event cutover (exact reconnect)

Per-session monotonic `seq`. Attach = fetch transcript tail + current
state → `throughSeq=N` → subscribe `after=N` → receive `N+1…` — no gap
between hydration and live stream. Lost transient deltas converge via
the durable completed message.

## 7e. OpenCode adapter preference

**OpenCode ACP is the preferred Relay integration path** — verified:
stdio transport (no listener), exact `session/load` resume, multi-session
process, mid-session model mutation (`session/set_config_option`),
structured `session/update` streaming, lower spawn latency (1.73s) and
post-turn PSS (572.5 vs 650.8) than serve. `opencode serve` remains a
researched alternative — richer HTTP/OpenAPI/SSE surface, server-side
question records, multi-client attach — justified later if product needs
exceed ACP (e.g. multi-head attach, browser-direct access). Not claimed
permanently superior.

## 7f. Implemented adapter semantics (Task A2 vertical slice)

`internal/harness/codex` is the first real native adapter, built against the
verified §1 surface and gated by a deterministic fake app-server (no
model call, no network, no quota). Implementation notes that refine the
design above:

- **Transport:** `codex app-server` (the installed CLI, resolved through
  `exec.LookPath("codex")`) with newline-delimited JSON-RPC 2.0 over
  stdio. Frames are bounded (`MaxFrameBytes`); a malformed or oversized
  frame fails the connection closed and unblocks every pending call
  instead of being skipped.
- **Shared runtime:** one app-server process per daemon
  (`codex/default`) hosts every Codex session. Each Relay session owns
  exactly one native `thread`; Relay never reuses a thread across
  sessions.
- **Materialization:** the native `threadId` is persisted on
  `RelaySession.nativeSessionId` **only after a turn completes** — the
  research finding that zero-turn threads are not resumable makes an
  unproven thread in-memory-only state. A failed first turn therefore
  persists nothing, and the next prompt starts a fresh thread.
- **Exact resume:** every new runtime generation reattaches with
  `thread/resume {threadId: <durable nativeSessionId>}` and verifies the
  returned id matches. A mismatch or an error surfaces as
  `NATIVE_SESSION_LOST`; Relay never falls back to another thread.
- **Turn lifecycle:** `message.user` is published durably *before*
  `turn/start`, the logical state is written `active` before submission,
  and a turn stays in flight until its terminal durable record
  (`message.agent.completed`, `turn.interrupted`, `turn.failed`) is
  published — so a concurrent prompt can never interleave with the
  previous turn's completion.
- **Requested input:** `item/tool/requestUserInput` is answered through
  the relay input ID; the blocker is registered and the durable state
  becomes `waiting_input` **before** `input.requested` is published, and
  an unresolved request makes the runtime unsleepable
  (`RuntimeSupervisor.CanSleep`). `StopIfIdle` refuses with
  `ErrRuntimeBusy` while one is pending.
- **Runtime death:** the supervisor reports each runtime generation gone
  exactly once (deliberate stop or death). A death fails the in-flight
  turn durably, aborts unresolved input, records `runtime.exited`, and
  projects the session COLD; a deliberate stop has no in-flight work by
  construction and only releases local state.
- **No client access to the native server:** the app-server is spawned by
  relayd, speaks only to relayd, and is never exposed on any socket,
  descriptor, or API surface. Clients see Relay events only.
- **Not yet implemented in this slice:** approvals
  (`approvalPolicy=never` is pinned), `turn/steer`, `thread/fork`,
  rate-limit notifications, and the A/B/C input UI. The protocol types
  for approvals exist; the handler is a future adapter task.

## 8. xterm-go / creack/pty disposition

- **`internal/terminal` + `github.com/rceman/xterm-go` → REMOVE FROM
  RELAY CORE.** No harness needs terminal emulation; the canonical
  transport is structured protocol. (The fork stays published and
  history remains for reference.)
- **`internal/pty` + `github.com/creack/pty` → REMOVE FROM RELAY CORE.**
  No managed child needs a PTY — all researched harnesses have structured
  transports (stdio JSON-RPC or HTTP).
- Removal lands in Task A of the implementation train (§11); docs label
  both "legacy spike, superseded by ADR-005".

## 9. Dependency policy

stdlib-first: NDJSON/JSON-RPC over stdio+unix, HTTP+SSE via `net/http`,
`encoding/json` — all hand-rollable small. **No new third-party deps are
justified** for the adapter core or the local control plane. TUI:
stdlib first; `golang.org/x/term` may be considered later for raw-mode
handling only if justified. No Bubble Tea / Cobra / Viper / gRPC /
generic event-bus / vendor SDKs.

## 10. TUI — structured agent console (not a terminal emulator)

MVP: transcript viewport (prompts + agent output), prompt editor,
requested-input choice/free-text, model selector, mode selector where
supported, state indicator, footer `{harness | model | mode | context
used/limit | quota/plan}`. Keys (proposed): Enter send, Ctrl+C cancel
turn, Ctrl+M model, Ctrl+F mode, Ctrl+Q detach, PgUp/PgDn scroll.
No PTY/xterm UI.

**Bounded history policy (accepted):**

- Initial attach renders **~200 rows**; max retained scrollback
  **~1000 rendered rows** (rows, not messages — width-dependent).
- Attach fetches small recent record batches until ~200 rendered rows
  are satisfied; never loads the full transcript.
- Beyond 1000 rows: drop oldest presentation content (durable records
  are never deleted).
- Scrolling to top does NOT auto-fetch older history; widening does NOT
  auto-backfill; narrowing re-renders retained content and re-trims.
- Huge single messages get a bounded TUI representation.
- Full history lives behind a separate API/path (Web Admin, history
  command, search, export, audit) — not the interactive hot path.

**Performance invariant:** TUI attach latency + working set are bounded
by the presentation window, not transcript lifetime. A months-old 1M-
record session attaches like a 100-record one — verified later via
synthetic data (no model quota).

## 10b. Local control plane & remote boundary (ADR-006)

Local transport: loopback TCP `127.0.0.1:0` (ephemeral port), HTTP/JSON
commands + streaming NDJSON events, stdlib `net/http`. Discovery via
`${REPOSUITE_HOME}/relay/run/daemon.json` (protocol version, endpoint,
PID). One protocol shared by CLI/TUI/Gateway; no WebSocket in relayd.
Remote (Web Admin) is a separate layer: `relayd → Gateway → WSS` —
Gateway owns TLS/auth/routing; harness runtimes stay private.
Cross-platform target: Linux + macOS + Windows. See ADR-006.

## 11. Implementation train (updated)

A0. **Canonical event stream + COLD resource semantics** — done
   (commit `87383b9`): exact `after=N` replay with an explicit floor,
   cursor-ahead rejection, lazy replay ring, no retained transcript
   handles, bounded pages/frames.
A. **Core domain + persistence cleanup** — drop terminal-first
   assumptions; remove xterm-go/creack/pty from core; `RelaySession` +
   `HarnessRuntime` + `RuntimeSupervisor` boundaries; deterministic
   fake harness; filesystem SessionStore/TranscriptStore with indexed
   bounded tail; durable key→nativeSessionId map.
B. **Cross-platform local control plane** — loopback HTTP/JSON, NDJSON
   events, ephemeral port, `daemon.json`, client package; preserve
   singleton/quiescence.
C. **Codex app-server vertical slice** — **done** (commits `5866ef9`,
   `157474c`, `8e91757`): stdio JSON-RPC, shared runtime, exact
   thread start/resume, prompt, streaming deltas, interrupt,
   model/effort/tier, token-usage metrics, `approvalPolicy=never`,
   sleep/wake/resume, runtime death handling, structured HTTP/CLI
   control. Remaining for C2: approval requests, `turn/steer`,
   rate-limit surfaces.
D. **Canonical event cutover** — transient deltas vs durable records,
   seq, bounded subscribers, `throughSeq→after`.
E. **Minimal TUI** — ~200/~1000-row bounds, prompt, requested input,
   model/mode, footer.
F. **Devin ACP** adapter (shared ACP client).
G. **OpenCode ACP** adapter (preferred OpenCode path).
H. **OpenCode serve** — only if product requirements justify.
I. **Gateway/WSS/Web Admin.**
J. **Load/memory/long-lived-session hardening.**

No adapter-bundling: one vertical slice (Codex) first.

## 11b. Future load-test plan (synthetic, no quota)

- **relayd:** idle RSS/PSS; 1/10/100/1000 sessions; very large
  transcripts; restart with many sessions.
- **TUI:** attach vs 100/10k/100k/1M-record histories; attach latency +
  RSS; ≤~1000 rendered rows; hours of synthetic streaming without
  unbounded growth.
- **Event stream:** 1/10/100 subscribers; slow-subscriber handling;
  bounded per-client queues; reconnect-by-seq.
- **Persistence:** bounded tail latency independent of transcript size;
  index rebuild/recovery; partial-tail/crash handling.
- No brittle RSS thresholds before baselines exist.

## 12. Open questions / risks

- Codex `request_user_input` is environment-gated — which config enables
  it? (Blocking for the A/B/C UI on Codex.)
- ~~OpenCode question durability~~ — **answered: NO** — pending `que_`
  requests are lost across restart; the tool part stays `running`
  forever. `waiting_input` is a proven sleep blocker on all four paths.
- Devin generic (non-permission) agent questions — not observed;
  `_meta` surface suggests richer channels exist.
- Codex `thread/resume` needs ≥1 materialized turn — zero-turn sessions
  are not resumable. **Resolved in implementation:** Relay persists the
  native thread id only after a completed turn, so a resumable identity is
  never assumed for an empty thread.
- ~~ACP model setter~~ — **answered:** `session/set_config_option`
  (configId=`model`) switches mid-session on OpenCode ACP; Devin exposes
  mode via configOptions (`bypass` etc.) — exact Devin model setter
  verify at adapter build (`--model` covers creation-time).
