# Performance & concurrency

## Observed (local, go1.27.1, fake harnesses + real RepoDex release build)

```text
TestRepoDexRealIngest         0.44s   session→turn→durable-ack roundtrip
TestRepoDexRestartRediscovery 1.60s   outage→rediscovery→replay
TestRepoDexTenSessions        0.99s   10 concurrent sessions, 60+ events
```

## Overhead properties

- Emission is O(1): marshal + bounded queue push. The agent path never
  blocks on RepoDex I/O — ACK is asynchronous.
- Durable cost is one session.json write per 256-event sequence block —
  the same amortization shape as the canonical transcript allocator.
- Batching bounds: 64 events / 1 MiB / 250 ms, whichever hits first.
- Sender is a single goroutine per daemon; queue and spool are bounded.
- Memory per event ≈ encoded frame size (KBs); queue limit 4096.

## Concurrency

Telemetry `emit` serializes per-service via one mutex; per-session state
is created atomically. Adapter calls happen off the session MetaMu.
10-way concurrent session test: zero lost, zero rejected, zero
cross-session contamination.
