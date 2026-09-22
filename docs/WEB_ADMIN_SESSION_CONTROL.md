# Web Admin Session Control

Canonical semantics for the embedded Web Admin's session control surface.
This document is current authority — `docs/WEB_ADMIN_SECURITY.md` covers
the security model; this file covers the session lifecycle UX contract.

## Routes

| Route | Purpose |
|---|---|
| `/sessions` | Registry snapshot — explicit Refresh only, no per-session streams |
| `/sessions/new` | Create session (provider, key, cwd, optional model/mode) |
| `/sessions/<key>` | Session detail: transcript + live events + controls |

All extensionless routes resolve through the canonical SPA fallback —
exactly one HTML document (`index.html`) is ever served, with the
document CSP. `SessionInfo.key` is the only route identity (never
`sessionId`, `nativeSessionId`, or `runtimeId`).

## Auth

Everything goes through the admin browser cookie domain: `GET` is
cookie-authenticated; unsafe methods additionally require the exact
`Origin` and `X-Relay-CSRF` from `/auth/session`. The SPA never touches
descriptor or machine bearer tokens. CSRF attaches centrally in
`lib/api/client.ts` — never per-component.

## History → live cutover

The session page bootstraps exactly once:

1. `GET /v1/sessions/<key>/transcript?limit=200` → durable tail +
   `throughSeq` (the canonical event cursor — **not** the last record
   seq; transient events and reserved gaps advance past it).
2. Hydrate the timeline from durable records.
3. `GET /v1/sessions/<key>/events?after=<throughSeq>` → NDJSON stream:
   atomic replay prefix, then live events.

No event published between the snapshot and the subscribe is missed —
the replay prefix covers it. Sequence values are uint64 on the wire; the
frontend requires `Number.isSafeInteger` and fails closed (protocol
error) rather than rounding.

## NDJSON stream

`lib/api/stream.ts` parses `Response.body` as raw bytes — arbitrary
chunk boundaries, multi-line chunks, and UTF-8 splits are handled by
accumulating `Uint8Array` chunks, and a final unterminated line still
parses. One frame is bounded by the canonical `MaxEventFrameBytes`
(4 MiB) measured on the frame's **UTF-8 bytes** — never JS string
length, which undercounts multibyte text; an over-bound frame aborts the
stream as a protocol error before it is decoded. Terminal
`{error:{code,message}}` frames (`STREAM_CLOSED`, `SUBSCRIBER_EVICTED`)
are recognized separately from events and trigger reconnect, not decode
failure.

## Reconnect and cursor recovery

- Stream end/error → reconnect `events?after=<lastAppliedSeq>` with
  bounded backoff (250 ms → 500 → 1 s → 2 s cap). The backoff resets only
  after useful progress — the first applied event — so an
  open→immediate-close stream escalates instead of hot-looping at
  250 ms. Replay duplicates are deduplicated by seq; events are never
  reordered.
- `409 CURSOR_TOO_OLD` — the replay window no longer proves the cursor:
  discard transient state, re-fetch the transcript, subscribe after the
  **new** `throughSeq`.
- `409 CURSOR_AHEAD` — same recovery; after three consecutive failures
  the stream is declared offline with a protocol error.
- `401` — the browser session expired (daemon restart): refresh
  `/auth/session`, go offline; the auth gate returns to login. No
  protected retry loop.
- Route change / key change / unmount / logout → `AbortController`
  aborts the stream and any pending reconnect timer.
- One SessionLive owns at most one stream and one pending timer at any
  instant: `connect()` cancels a pending reconnect and aborts the old
  stream, and "Load more history" rehydrates the durable snapshot BEFORE
  the old stream is replaced — no transient gap, no second subscription.
- Detail reads are guarded by a per-route `DetailLoader`: a new key (or
  teardown) aborts the in-flight `getSession` and a superseded response
  resolves stale — it can never overwrite a newer route's session.

## Timeline

`lib/session/timeline.ts` is a pure reducer (no DOM/Svelte):

- `message.user` → user row; `message.agent.completed` → agent row.
- `message.agent.delta` (transient) accumulates into a live draft keyed
  by the collision-proof `(turnId, itemId)` pair; the durable completion
  replaces the draft in place — durable truth is always authoritative.
- `turn.failed`, `turn.interrupted`, `harness.started`, `runtime.exited`,
  `session.native`, `session.config`, `harness.error`, `input.*` render
  as quiet system rows; `metrics.updated` updates the metrics panel.
- Unknown event types render a diagnostic row (`Event: <type>`) — never
  a crash.
- All text renders as plain text nodes (`whitespace-pre-wrap`) — no
  `{@html}`, no Markdown renderer yet.

Pending requested input is reconstructed from **durable** records
(`input.requested` adds; `input.resolved`/`input.aborted` removes by
exact `inputId`), so a browser reload restores the pending state. A
session may carry **multiple** unresolved inputs at once — the pending
projection is the full unresolved set, never "the last request".

## Requested-input establishment

A native `item/tool/requestUserInput` is itself a transaction. The
pending entry is installed as `announcing` *before* any durable work —
a terminal path racing the handler can mark `wantAbort`, and the sweep
never publishes `input.aborted` for a request whose `input.requested`
record does not yet exist (`input.aborted` never precedes
`input.requested`). Then, in order:

1. durable `Session.State = waiting_input` commits — checked, not
   ignored: restart reconciliation scans on exactly this signal, so no
   unresolved `input.requested` may exist without it;
2. durable `input.requested` commits — checked;
3. `announcing → pending` — only now is the input answerable.

Either durable step failing is **fail-closed**: the entry is removed
(the request was never durably announced, so no terminal record is
owed), the canonical projection reconciles activity/state, and the
native request receives an RPC error. If the announcement committed
while a terminal path visited, the owner then publishes `input.aborted`
after `input.requested`. If the announce-time state rollback itself
fails, a stale durable `waiting_input` with zero unresolved requests is
normalized to `idle` on restart.

The `ready` barrier that releases `AnswerInput` callers parked on an
`announcing` input means only *"the announcement left its initial
phase"* — never *"the input is answerable"*. Terminal intent dominates:
an input marked `wantAbort` is not claimable for `Respond` even while
`pending` (a failed `input.aborted` append leaves exactly that retained,
non-answerable state until a retry commits). One native request
therefore receives at most **one** response frame — the deferred answer
or the handler's RPC error, never both.

## Session activity/state projection

One canonical rule derives activity/state from actual adapter state —
used by announcement cleanup, resolution commit, abort, terminal sweeps,
and runtime-exit reconciliation:

```
len(inputs) > 0   → waiting_input   (every retained phase counts:
                                     announcing … aborting)
current != nil    → active
else              → idle
```

Resolving one of several inputs leaves `waiting_input` while any input
remains; a `Respond` failure after a terminal visit converges to the
durable `input.aborted` plus the projected state — no restart needed.

## Requested-input resolution durability

`POST .../input` is a two-commit-point transaction in the Codex adapter.
Per input, the in-memory lifecycle is:

```
announcing → pending → responding → answered → committing → (removed)
                ↑________|              ↑________|
                Respond failed          durable append failed (retryable,
                (retryable)             never re-Responds)
```

`announcing` is internal only — it is never a durable event type.

- **Native commit point:** `Conn.Respond` is the irreversible external
  side effect and is sent **at most once** per input. The caller that
  claims `pending → responding` owns it; concurrent answers converge on
  `SESSION_BUSY`, and a post-commit retry hits `answered` which retries
  only the durable commit — it can never re-send the native response or
  replace the native-accepted payload with different answers.
- **Durable commit point:** the sanitized `input.resolved` payload
  (non-secret answers + sorted `redacted` IDs, zero secret plaintext) is
  built *before* `Respond` and retained while `answered`, so a failed
  durable append is retryable without the original answers.
- **Terminal sweep:** turn completion, runtime exit, and session stop
  publish `input.aborted` for `pending` inputs — but commit the retained
  `input.resolved` for `answered` ones, never the contradictory opposite.
  Inputs owned by an in-flight side effect are marked and reconciled by
  their owner. Exactly one terminal outcome (`resolved` XOR `aborted`)
  exists per input, and a pending entry is discarded only *after* its
  durable terminal record commits — a failed append retains it.
- **Non-clean visibility:** while a terminal record is uncommitted, the
  session stays `waiting_input` (a hard sleep blocker — the retained
  reconciliation state cannot be slept away). It settles `idle`/`active`
  when the commit converges.
- **Restart reconciliation:** a native request dies with its daemon
  generation. On startup, sessions restored as `waiting_input` get a
  full transcript scan (exact record count, no cap); every unresolved
  `input.requested` is durably `input.aborted` (`"daemon restarted
  before requested input resolution committed"`) and the state
  normalizes — startup **fails closed** if the compensating record
  cannot be committed, and a malformed `input.*` authority record (bad
  payload or missing `inputId`) fails closed too rather than being read
  as "nothing unresolved".

A commit that crosses a daemon generation is indistinguishable from an
unanswered request, so the canonical restart outcome is `input.aborted`
even when the native side already received the answer.

## History window

The transcript endpoint returns a bounded tail (≤ `TranscriptMaxLimit`
= 1000). "Load more history" grows the requested limit 200 → 400 → 800 →
1000; each growth rehydrates and re-subscribes from the new `throughSeq`.
At the maximum with `hasMoreBefore`, the UI says older history exists
beyond the API window — it never claims completeness.

## Controls

| Control | Route | Notes |
|---|---|---|
| Prompt | `POST /v1/sessions/<key>/prompt` | `{text}` only — harness-neutral (ACP rejects per-turn overrides). Accepted prompt on a COLD session wakes it intentionally. Empty text rejected client-side. |
| Cancel | `POST /v1/sessions/<key>/cancel` | Enabled on `active`/`waiting_input`. A raced `NO_ACTIVE_TURN` is a quiet reconcile, not an error. |
| Input | `POST /v1/sessions/<key>/input` | `{inputId, answers:[{questionId, answers:[…]}]}`; options are multi-select-safe (`[]string`); `isOther` offers free text **in addition to** any structured options (never discards them); `isSecret` uses a password field. Free text is forwarded **verbatim** — no trim; a secret may legally contain surrounding whitespace. The pending card reconciles only through `input.resolved`/`input.aborted`. |
| Config | `PATCH /v1/sessions/<key>/config` | Sends only changed fields; empty patch never sent. `SessionInfo.model/mode` = **desired**; `SessionMetrics.model/mode` = **effective** — labeled separately, never conflated. |
| Delete | `DELETE /v1/sessions/<key>` | Durable deletion, AlertDialog-confirmed with the key named; success aborts the stream and returns to `/sessions`. |
| Create | `POST /v1/sessions/<provider>` | `codex`/`devin`/`opencode`; durable COLD (generation 0, pid 0) — no harness process until first prompt. |

## Secret requested input

A native `isSecret` question is preserved in the durable
`input.requested` payload (`"isSecret": true`). Its answer is forwarded
to the harness **exactly as entered** — leading/trailing whitespace is
significant — but **never persisted**: the durable `input.resolved`
record withholds secret answers and lists the question IDs under
`"redacted"` in canonical (sorted) order, so the durable record is
deterministic regardless of answer-submission order. Non-secret answers
record normally — `answers[questionId] = []string`.

Browser-side, the request body is built first and the secret draft is
cleared from component memory **before the send is awaited** — the
native side may already hold the answer while the durable commit fails,
so an ambiguous rejection must never leave a credential in page state.
The request body alone carries the exact secret for the send; nothing
secret ever touches browser storage.

## Resource guarantees

- Detail bootstrap (`GET session`, `GET transcript`, `GET events`) is a
  pure observer — a COLD session stays `runtimeState=cold, pid=0,
  generation=0`.
- Only `/sessions/<key>` holds an event stream; the list page is a cheap
  snapshot with explicit Refresh.
- Session-projection refresh after state-affecting events is coalesced
  (250 ms) — never one fetch per token delta.
