# Integration test setup

## Configuration (relay.json)

```json
{
  "schemaVersion": 1,
  "listenHost": "127.0.0.1",
  "listenPort": <port>,
  "repodex": {
    "enabled": true,
    "stateDir": "<repodex state dir>",
    "batchMaxEvents": 64,
    "batchMaxBytes": 1048576,
    "batchMaxDelayMs": 250,
    "memoryQueueLimit": 4096,
    "spoolLimitBytes": 67108864
  }
}
```

All fields optional except `enabled`. `stateDir` defaults to
`~/reposuite/repodex`. Relay **reads** `service.runtime.json` +
`service.token` under that dir; it never writes there.

## Discovery contract

1. Read `<stateDir>/service.runtime.json` — host/port/pid/instance_id.
2. Reject non-loopback host (bearer would leave the machine).
3. Read `<stateDir>/service.token` (0600) → `Authorization: Bearer`.
4. `GET /v1/status` must return `reposuite.repodex.service.status.v1`
   before the first ingest.
5. On transport failure / HTTP non-2xx: bounded backoff, re-read the
   descriptor (dynamic port rediscovery), never reuse a stale port.

## Running the acceptance tests

```bash
# build RepoDex at the accepted revision in a scratch clone
git clone /path/to/reposuite-repodex /tmp/repodex-accept
cd /tmp/repodex-accept && git checkout c2bd73ab0f49c7db9cbd296c0d5c8558d41677c5
cargo build --release --locked

# run
cd /path/to/reposuite-relay
REPODEX_TEST_BIN=/tmp/repodex-accept/target/release/reposuite-repodex \
  go test -count=1 -run TestRepoDex ./internal/daemon -v
```

The tests spawn `reposuite-repodex serve --state-dir <t.TempDir()>` —
port 0 dynamic bind, token minted by RepoDex. Relay config is enabled via
relay.json between daemon generations. No real user state is touched.
