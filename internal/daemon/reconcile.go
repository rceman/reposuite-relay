package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
)

// restartInputAbortReason is the stable durable reason recorded when a
// daemon restart orphans a requested-input request.
const restartInputAbortReason = "daemon restarted before requested input resolution committed"

// inputReconciler is the adapter-side half of restart reconciliation:
// durably abort each unresolved input and normalize the session state.
// Implemented by the Codex adapter — the only harness that produces
// input.requested today.
type inputReconciler interface {
	ReconcileInputs(m *session.Managed, inputIDs []string, reason string) error
}

// reconcileRestoredInputs closes the durability gap a restart creates
// for requested input: a durable input.requested with no matching
// input.resolved/input.aborted names a native JSON-RPC request that died
// with the previous daemon generation — it can never be answered again,
// so startup commits input.aborted and normalizes the restored
// waiting_input state before the descriptor is published. A failure
// fails startup closed: the daemon never serves an impossible
// pending-input authority.
//
// The transcript scan is O(n) but gated on the durable state signal
// (waiting_input); ordinary COLD/idle sessions cost nothing. No
// transcript handle survives the loop.
func (d *Daemon) reconcileRestoredInputs() error {
	for _, m := range d.registry.List() {
		snap := m.Snapshot()
		if snap.State != session.StateWaitingInput {
			continue
		}
		ids, err := d.unresolvedInputIDs(m)
		if err != nil {
			return fmt.Errorf("reconcile session %s: %w", snap.Key, err)
		}
		if len(ids) == 0 {
			// waiting_input with no unresolved request is equally
			// impossible after a restart — normalize it directly.
			idle := session.StateIdle
			if err := d.materialize(m, harness.SessionUpdate{State: &idle}); err != nil {
				return fmt.Errorf("reconcile session %s: %w", snap.Key, err)
			}
			continue
		}
		entry, ok := d.entryFor(snap.Harness)
		if !ok {
			return fmt.Errorf("reconcile session %s: harness %q unavailable", snap.Key, snap.Harness)
		}
		rec, ok := entry.adapter.(inputReconciler)
		if !ok {
			return fmt.Errorf("reconcile session %s: harness %q cannot resolve requested input", snap.Key, snap.Harness)
		}
		if err := rec.ReconcileInputs(m, ids, restartInputAbortReason); err != nil {
			return fmt.Errorf("reconcile session %s: %w", snap.Key, err)
		}
	}
	return nil
}

// unresolvedInputIDs scans the full durable transcript and returns the
// input IDs of every input.requested with no later matching
// input.resolved/input.aborted, in request order. Gated callers only:
// the scan is O(transcript length) and covers the transcript's exact
// current record count — no arbitrary ceiling.
//
// The three input.* records are the requested-input authority; a
// malformed payload or missing inputId in any of them inside a
// waiting_input session's transcript is corruption, not evidence — the
// scan fails closed rather than guess at unresolved state.
func (d *Daemon) unresolvedInputIDs(m *session.Managed) ([]string, error) {
	tr, err := d.store.Transcript(m.Session.ID)
	if err != nil {
		return nil, err
	}
	defer tr.Close()
	recs, _, err := tr.Tail(tr.Len())
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	var order []string
	for _, r := range recs {
		var p struct {
			InputID string `json:"inputId"`
		}
		switch r.Type {
		case api.EventInputRequested:
			if err := json.Unmarshal(r.Payload, &p); err != nil || p.InputID == "" {
				return nil, fmt.Errorf("malformed %s record (seq %d)", r.Type, r.Seq)
			}
			if !open[p.InputID] {
				open[p.InputID] = true
				order = append(order, p.InputID)
			}
		case api.EventInputResolved, api.EventInputAborted:
			if err := json.Unmarshal(r.Payload, &p); err != nil || p.InputID == "" {
				return nil, fmt.Errorf("malformed %s record (seq %d)", r.Type, r.Seq)
			}
			delete(open, p.InputID)
		}
	}
	out := order[:0]
	for _, id := range order {
		if open[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
