package daemon

import (
	"net/http"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	rt "github.com/rceman/reposuite-relay/internal/runtime"
)

// GET /v1/runtimes — the global live harness-runtime inventory. Purely
// observational: it never wakes a COLD session, spawns a harness, or
// mutates durable state. The Supervisor snapshot is taken under the
// supervisor lock; all /proc sampling happens AFTER the lock is
// released and each sample is revalidated against the generation it was
// measured on — a runtime that changed generation mid-sample reports
// resources unavailable rather than inheriting stale numbers.
func (d *Daemon) handleRuntimes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	// Immutable snapshot — released before any filesystem access.
	snaps := d.supervisor.Snapshot()

	// RelaySession ID → key for human-facing binding labels.
	keyOf := map[string]string{}
	for _, m := range d.registry.List() {
		if s := m.Snapshot(); s.ID != "" {
			keyOf[s.ID] = s.Key
		}
	}

	resp := api.RuntimeList{
		Daemon:    d.info(),
		SampledAt: time.Now().UTC().Format(time.RFC3339Nano),
		Runtimes:  []api.RuntimeInfo{},
	}
	for _, snap := range snaps {
		info := api.RuntimeInfo{
			RuntimeID: snap.ID,
			Harness:   snap.Kind,
			Shared:    snap.Shared,
			PID:       snap.PID,
			StartedAt: snap.StartedAt.UTC().Format(time.RFC3339Nano),
			State:     snap.State,
			Sessions:  []api.RuntimeBinding{},
			Resources: api.RuntimeResources{Available: false},
		}
		if !snap.StartedAt.IsZero() {
			info.UptimeSeconds = int64(time.Since(snap.StartedAt).Seconds())
		}
		for _, b := range snap.Sessions {
			info.Sessions = append(info.Sessions, api.RuntimeBinding{
				Key:       keyOf[b.SessionID],
				SessionID: b.SessionID,
				Activity:  b.Activity,
				Mutating:  b.Mutating,
			})
			info.SessionCount++
			switch b.Activity {
			case rt.ActivityActive:
				info.ActiveSessionCount++
			case rt.ActivityWaitingInput:
				info.WaitingInputCount++
			}
			if b.Mutating {
				info.MutationCount++
			}
		}

		// Sample the owned process tree outside the supervisor lock,
		// then revalidate: if the generation was replaced mid-sample the
		// numbers belong to a dead PID, not this runtime.
		if snap.PID > 0 {
			if res, ok := rt.SampleTreeResources("/proc", snap.PID); ok &&
				d.supervisor.ValidateGeneration(snap.Key, snap.ID, snap.PID) {
				info.Resources = api.RuntimeResources{
					Available:    true,
					PSSBytes:     res.PSSBytes,
					RSSBytes:     res.RSSBytes,
					ProcessCount: res.ProcessCount,
				}
			}
		}
		resp.Runtimes = append(resp.Runtimes, info)

		resp.Totals.RuntimeCount++
		resp.Totals.SessionCount += info.SessionCount
		resp.Totals.ActiveSessionCount += info.ActiveSessionCount
		resp.Totals.WaitingInputCount += info.WaitingInputCount
		if info.Resources.Available {
			resp.Totals.MeasuredRuntimeCount++
			resp.Totals.PSSBytes += info.Resources.PSSBytes
			resp.Totals.RSSBytes += info.Resources.RSSBytes
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
