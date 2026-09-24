# RSR-P-20260924-1134-REPODEXEVENT01 — Evidence

Mission: RepoSuite Relay → RepoDex canonical AgentEvent v1 telemetry.

## Fixed points

```text
REPODEX_ACCEPTED_REVISION = c2bd73ab0f49c7db9cbd296c0d5c8558d41677c5
RELAY_BASE                = 743021ba7d225d2517d3b3bdea2d5488a8a24e0c
BRANCH                    = feature/repodex-agent-event-v1
```

RepoDex acceptance binary was built in a scratch clone at the accepted
revision (`/tmp/repodex-accept`, `cargo build --release --locked`); the
working RepoDex checkout was never modified (`REPODEX_MODIFIED=false`).

## Invariant results

```text
REPODEX_INTEGRATION_OPTIONAL   = true   # repodex{} section absent → disabled
RELAY_WORKS_WITHOUT_REPODEX    = true   # disabled/no-service tests pass
RELAY_AUTOSTARTS_REPODEX       = false  # Relay never spawns RepoDex
RELAY_AUTO_INSTALLS_REPODEX    = false

DEFAULT_REPODEX_STATE_DIR      = ~/reposuite/repodex
REPODEX_DYNAMIC_PORT_SUPPORTED = true   # descriptor discovery, port never fixed
FIXED_REPODEX_PORT_ASSUMED     = false
RUNTIME_DESCRIPTOR_DISCOVERY   = true   # service.runtime.json + PID liveness
REPODEX_STATUS_HTTP_VALIDATION = true   # GET /v1/status schema-checked
REPODEX_CLI_STATUS_USED_FOR_PRODUCTION_IPC = false
REPODEX_BEARER_AUTH_SUPPORTED  = true   # service.token → Authorization header

RELAY_CANONICAL_AGENT_EVENT_V1 = true
RELAY_SEPARATE_TELEMETRY_SEQUENCE = true   # TelemetrySeqWatermark ≠ SeqHighWatermark
TELEMETRY_SEQUENCE_IS_PER_SESSION = true
PROJECT_GLOBAL_SEQUENCE = false

SOURCE_OBSERVED_MEANS_CONTENT_DELIVERED = true
FILENAME_ONLY_COUNTS_AS_SOURCE_OBSERVED = false
GENERIC_SHELL_OUTPUT_IMPLIES_SOURCE_OBSERVED = false
TELEMETRY_EXPANDS_SOURCE_ACCESS = false  # only digests/bytes, already-delivered content

AUTHORITATIVE_INPUT_TOKENS_CAPTURED  = true (provider-exposed)
AUTHORITATIVE_OUTPUT_TOKENS_CAPTURED = true (provider-exposed)
REASONING_TOKENS_ESTIMATED  = false
HIDDEN_REASONING_CAPTURED   = false  # ACP thought chunks never carried

MODEL_CALL_BOUNDARIES_FABRICATED = false
# A Relay turn is not a proven single model call. Turn-scoped usage is
# emitted as model_call_completed with model_call_id "turn:<id>" and
# usage_estimated=true — honest aggregation, never a fabricated boundary.
TASK_OUTCOME_EMITTED_WITHOUT_AUTHORITY = false  # task_outcome never emitted

ASYNC_TELEMETRY_DELIVERY                = true
AGENT_WAITS_FOR_REPODEX_HTTP_ACK        = false
AGENT_WAITS_FOR_REPODEX_DERIVED_PROCESSING = false
NORMAL_PATH_DOUBLE_DURABLE_WRITE        = false  # queue→batch; spool only on failure
FALLBACK_SPOOL_SUPPORTED                = true   # durable spool chunks, replayed

REPODEX_BATCH_WIRE_IS_BARE_ARRAY    = true   # verified vs real ingest
REPODEX_DURABLE_ACK_USED            = true   # accepted/duplicates/rejected counted
REPODEX_EVENT_ID_IDEMPOTENCY_USED   = true   # restart re-emission dedupes server-side
REPODEX_DYNAMIC_PORT_REDISCOVERY    = true   # restart test rebinds new port

MULTI_AGENT_10_SUPPORTED        = true
TELEMETRY_EVENTS_LOST           = 0 (controlled scenarios)
CROSS_SESSION_CONTAMINATION     = 0 (controlled scenarios)

SQLITE_ADDED = false
REDIS_ADDED  = false

RELAY_REPODEX_QUERY_SUPPORTED            = false
RELAY_AGENT_FACING_REPODEX_HELPER        = false
RELAY_RESOLVES_QUERY_REPOSITORY_VIEW     = false
RELAY_RESOLVES_TASK_QUERY_AUTHORITY      = false
GATEWAY_OWNS_REPODEX_QUERY_AUTHORITY     = true
RELAY_REPODEX_RESPONSIBILITY             = TELEMETRY_ONLY

AUTOMATIC_RUNTIME_SLEEP = DISABLED   # unchanged — no eviction added

REPODEX_MODIFIED = false
GATEWAY_MODIFIED = false
MAIN_MERGED    = false
```

## Acceptance evidence

Isolated real-RepoDex tests (`internal/daemon/repodex_integration_test.go`,
env-gated on `REPODEX_TEST_BIN`, all state under `t.TempDir()`):

```text
TestRepoDexRealIngest          PASS — 6+ canonical events persisted by real
                                   RepoDex; health=connected; lost=0
TestRepoDexRestartRediscovery  PASS — RepoDex killed mid-stream, second turn
                                   spooled; restart rebinds a NEW dynamic port;
                                   Relay rediscovers descriptor and replays;
                                   persisted>=10; lost=0; rediscoveries>0
TestRepoDexTenSessions         PASS — 10 concurrent sessions; persisted>=60;
                                   lost=0; rejected=0 (cross-contamination
                                   symptom absent)
```

Unit/deterministic coverage:

```text
internal/telemetry        envelope validation, event_id={session}:{seq},
                          per-session sequence domains, restart re-emission
                          of identical session_started at seq 0, sequence
                          resume after durable watermark, queue bound →
                          counted loss + degraded, disabled no-op, nil-safe
                          calls, discovery contract (non-loopback rejected),
                          bare-array batch, reject handling, outage spool,
                          shutdown spool + restart replay
internal/harness/codex    tool lifecycle start/complete pairing by native
                          item id; positive source_observed (structured read
                          action + delivered output; fileChange diff);
                          negative (listFiles, search, multi-file read,
                          mcpToolCall, webSearch, unknown item type);
                          failed tool → ok=false; usage copied exactly with
                          usage_estimated=true; session_started at seq 0
internal/harness/devin    ACP tool_call/tool_call_update mapping incl.
internal/harness/opencode kind/location tracking across updates; listing
                          never observes; usage fields exact; session_started
internal/daemon           GET /v1/telemetry/repodex auth matrix
                          (anon 401 / descriptor 200 / machine 200), disabled
                          default shape, repodex.enabled via relay.json
```

## Known limitations

- `model_call_started` is never emitted: no harness exposes an objective
  per-model-call boundary; usage rides `model_call_completed` marked
  `usage_estimated` when scoped to a turn.
- `task_outcome` is never emitted: no authoritative outcome source.
- `context_artifact_presented` is not yet emitted.
- ACP `agent_message` is not emitted (chunked deltas are not message
  boundaries); `final_answer` covers the completed output.
- Permanent RepoDex rejects are counted `rejected`/`lost` and the pipeline
  reports `degraded` — they are not retried (the durable log's word is
  final), and the session sequence stays monotone.
- The outage spool is bounded (`spoolLimitBytes`, default 64 MiB); a
  prolonged outage beyond the bound reports `lost` and `degraded`.

## Authority boundary

Relay emits TELEMETRY ONLY. No `/v1/query` client exists; no agent-facing
RepoDex helper is exposed; no RepositoryView or task resolution is done
anywhere in Relay. RepoDex lifecycle (start/install/stop) is never
touched — discovery is descriptor-read + status-validate only.
