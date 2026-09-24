package telemetry

// Shared test seams: RepoDex-faithful fake transport + session helpers.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
)

func testManaged(id, harness, cwd string, watermark uint64) *session.Managed {
	return &session.Managed{Session: &session.RelaySession{
		ID: id, Key: "k-" + id, Harness: harness, Cwd: cwd,
		State: "idle", TelemetrySeqWatermark: watermark,
	}}
}

// fakeDoer records requests and replays scripted replies.
type fakeDoer struct {
	t       *testing.T
	reqs    atomic.Int64
	mu      sync.Mutex
	bodies  [][]byte
	fail    atomic.Bool
	status  int
	lastErr error
	reply   *ingestReply
	replyFn func() *ingestReply // per-call override; nil → dedup accounting
	// seen reproduces RepoDex durable dedup: event_id → semantic digest
	// (sha256 of the raw event line). Identical resubmission = Duplicate;
	// same id + different bytes = Conflict (rejected, permanent).
	seen map[string]string
}

func (f *fakeDoer) Do(r *Request) (*Reply, error) {
	f.reqs.Add(1)
	f.mu.Lock()
	f.bodies = append(f.bodies, r.Body)
	f.mu.Unlock()
	if f.fail.Load() {
		return nil, fmt.Errorf("connection refused")
	}
	if f.lastErr != nil {
		return nil, f.lastErr
	}
	if strings.HasSuffix(r.URL, "/v1/status") {
		// Discovery always answers 200 — f.status scripts the ingest
		// endpoint only.
		return &Reply{
			Status: 200,
			Body:   []byte(`{"schema":"` + statusSchema + `"}`),
		}, nil
	}
	if f.status != 0 && f.status != 200 {
		return &Reply{
			Status: f.status,
			Body:   []byte(`{}`),
		}, nil
	}
	rep := f.reply
	if f.replyFn != nil {
		rep = f.replyFn()
	}
	if rep == nil {
		// RepoDex-faithful verdicts: dedup on event_id + content digest.
		var evs []json.RawMessage
		_ = json.Unmarshal(r.Body, &evs)
		rep = &ingestReply{Schema: ingestSchema}
		f.mu.Lock()
		if f.seen == nil {
			f.seen = map[string]string{}
		}
		for _, raw := range evs {
			id := eventIDOf(raw)
			dig := Digest(raw)
			if prev, ok := f.seen[id]; ok {
				if prev == dig {
					rep.Duplicates++
				} else {
					rep.Rejected++
					rep.Errors = append(rep.Errors, "conflicting event_id "+id)
				}
				continue
			}
			f.seen[id] = dig
			rep.Accepted++
		}
		f.mu.Unlock()
	}
	rep.Schema = ingestSchema
	b, _ := json.Marshal(rep)
	return &Reply{
		Status: 200,
		Body:   b,
	}, nil
}

// writeDescriptor publishes a descriptor+token into a temp state dir.
func writeDescriptor(t *testing.T, dir, host string, port uint16) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := descriptor{
		Schema:          runtimeSchema,
		PID:             1,
		Host:            host,
		Port:            port,
		InstanceID:      "repodex-1",
		StartedAt:       "2026-01-01T00:00:00Z",
		RepoDexVersion:  "0.1.0",
		ProtocolVersion: 1,
	}
	b, _ := json.Marshal(d)
	if err := os.WriteFile(filepath.Join(dir, runtimeFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	tok := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(dir, tokenFile), []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testDeps(t *testing.T, doer Doer, spoolDir string) Deps {
	t.Helper()
	marks := map[string]uint64{}
	return Deps{
		SpoolDir:       spoolDir,
		AdapterVersion: "test",
		ReserveSeq: func(m *session.Managed, mark uint64) error {
			m.MetaMu.Lock()
			defer m.MetaMu.Unlock()
			if mark > marks[m.Session.ID] {
				marks[m.Session.ID] = mark
				m.Session.TelemetrySeqWatermark = mark
			}
			return nil
		},
		FreezeStart: func(m *session.Managed, mark uint64, raw []byte) error {
			m.MetaMu.Lock()
			defer m.MetaMu.Unlock()
			if mark > marks[m.Session.ID] {
				marks[m.Session.ID] = mark
				m.Session.TelemetrySeqWatermark = mark
			}
			if m.Session.TelemetryStartedEvent == "" {
				m.Session.TelemetryStartedEvent = string(raw)
			}
			return nil
		},
		HTTPClient: doer,
		Now:        func() time.Time { return time.Now().UTC() },
		Sleep:      func(time.Duration) {},
	}
}

func enabledCfg(t *testing.T, stateDir string) Config {
	return Config{
		Enabled:  true,
		StateDir: stateDir,
	}.withDefaults()
}
