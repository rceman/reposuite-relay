# RepoDex telemetry (AgentEvent v1)

Relay emits canonical `reposuite.agent-event.v1` telemetry into a running
RepoDex service. It is OPTIONAL and disabled by default: absent or
disabled `repodex` config means zero RepoDex dependency anywhere in the
agent path.

## Boundary

```text
GPT Tunnel Gateway → RepoDex = QUERY      (Gateway owns query authority)
RepoSuite Relay    → RepoDex = TELEMETRY  (this document)
```

Relay never exposes `/v1/query` to clients, never offers agent-facing
RepoDex helpers, never resolves repository views or task authority.
Relay never starts, installs, or stops RepoDex — it discovers a running
service via `<stateDir>/service.runtime.json`, authenticates with
`service.token` (0600), and validates `GET /v1/status` before ingest.

## Configuration (relay.json, strict schema)

```json
"repodex": {
  "enabled": true,
  "stateDir": "~/reposuite/repodex",   // default
  "batchMaxEvents": 64, "batchMaxBytes": 1048576, "batchMaxDelayMs": 250,
  "memoryQueueLimit": 4096, "spoolLimitBytes": 67108864
}
```

## Delivery

Emission is O(1) enqueue onto a bounded per-daemon queue — the agent path
never waits for RepoDex. A single sender goroutine batches by
count/bytes/delay and POSTs a **bare JSON array** to
`/v1/events/batch`. RepoDex's ingest response (accepted/duplicates/
rejected) is a durable ack: permanent rejects are counted, transport
failures retry with bounded backoff and descriptor rediscovery. Events
unacknowledged across an outage or shutdown are persisted to
`telemetry-spool/` (durable chunk files) and replayed on recovery —
identical `event_id`s dedupe server-side.

## Sequence

Each session owns an independent telemetry sequence starting at 0
(`session_started`). `TelemetrySeqWatermark` on the durable session
reserves blocks of 256; a crash may leave gaps but never reuses values.
This domain is fully separate from the canonical transcript sequence
(`SeqHighWatermark`).

## Health

`GET /v1/telemetry/repodex` (observer-only, all three credential domains)
returns `{enabled, state, queueDepth, spoolBytes, observed, canonicalized,
acknowledged, duplicates, rejected, lost, retries, rediscoveries,
instanceId, endpoint, lastAckAt}`. No credentials ever appear. The Web
Admin footer shows a RepoDex segment when telemetry is enabled.

States: `disabled` · `disconnected` (no live RepoDex — events queue/spool)
· `connected` · `degraded` (queue overflow or permanent rejects — loss is
visible, never silent) · `incompatible` (status schema mismatch).
