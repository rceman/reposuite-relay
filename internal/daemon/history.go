package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"net/http"
	"os"
	"strconv"
	"time"
)

// handleStop takes exclusive stop ownership (BeginStop), stops and
// detaches the runtime if one exists (ClearRuntime → COLD), durably
// deletes the session, then removes it (CommitStop) and closes its event
// subscribers. A failed runtime stop keeps the session as-is
// (AbortStop); a failed durable delete after a confirmed runtime stop
// keeps the session managed but COLD — the daemon never loses authority
// and never retains a dead PID.
func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.BeginStop(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	// Adapter cleanup first (still bound): an in-flight native turn is
	// interrupted, unresolved requested input is aborted durably, and the
	// session's native thread tracking is dropped.
	if a, ok := d.adapterFor(m.Snapshot().Harness); ok {
		a.StopSession(m)
	}
	// Runtime ownership: a dedicated runtime (fixture) is stopped and
	// reaped; a shared runtime (Codex) is only detached — deleting one
	// session never kills a runtime other sessions still use.
	if err := d.supervisor.StopSession(m.Session.ID); err != nil {
		d.registry.AbortStop(key)
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "stop runtime: "+err.Error())
		return
	}
	// Terminal RelaySession lifecycle: the only session_completed point —
	// runtime sleep/death and turn completion are not session completion.
	// Emitted before durable delete so the event still carries session
	// identity; delivery is async and never blocks the deletion.
	d.telemetry.SessionCompleted(m, "session_deleted")
	if err := d.store.Delete(m.Session.ID); err != nil {
		var ce *store.CleanupError
		var ue *store.UncertainError
		switch {
		case errors.As(err, &ce):
			// Deletion committed — apply it. The leftover tombstone is
			// recovered on next daemon start.
			fmt.Fprintf(os.Stderr, "relayd: %v\n", err)
			d.registry.CommitStop(key)
			d.broker.Remove(m.Session.ID)
			d.telemetry.ForgetSession(m.Session.ID)
			writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
			return
		case errors.As(err, &ue):
			// Namespace outcome committed but durability is ambiguous —
			// apply the outcome, then fail closed and halt.
			d.registry.CommitStop(key)
			d.broker.Remove(m.Session.ID)
			d.telemetry.ForgetSession(m.Session.ID)
			go d.initiate()
			writeErr(w, http.StatusInternalServerError, api.ErrInternal,
				"store commit uncertain; daemon halting: "+err.Error())
			return
		default:
			d.registry.AbortStop(key)
			writeErr(w, http.StatusInternalServerError, api.ErrInternal, "delete session: "+err.Error())
			return
		}
	}
	d.registry.CommitStop(key)
	d.broker.Remove(m.Session.ID)
	d.telemetry.ForgetSession(m.Session.ID)
	writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
}

// handleTranscript serves bounded durable transcript tail records through
// the derived index — durable records, not rendered rows.
func (d *Daemon) handleTranscript(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	limit := api.TranscriptDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid limit")
			return
		}
		limit = n
	}
	if limit > api.TranscriptMaxLimit {
		limit = api.TranscriptMaxLimit
	}
	evs, err := d.broker.Ensure(m)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	page, err := evs.HistorySnapshot(limit, api.HistoryPageMaxBytes)
	if err != nil {
		if errors.Is(err, store.ErrRecordTooLarge) {
			writeErr(w, http.StatusInternalServerError, api.ErrHistoryRecordTooLarge,
				"canonical record exceeds the bounded history page")
			return
		}
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "read transcript: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.TranscriptPage{
		ThroughSeq:    page.ThroughSeq,
		Records:       page.Records,
		HasMoreBefore: page.HasMoreBefore,
	})
}

// handleEvents streams canonical events as NDJSON: the atomic replay
// prefix (events after the requested cursor) followed by live events. The
// cursor-too-old case is a normal JSON error before any streaming begins.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	var after uint64
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid after cursor")
			return
		}
		after = n
	}
	evs, err := d.broker.Ensure(m)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	sub, err := evs.Subscribe(after)
	if err != nil {
		switch {
		case errors.Is(err, events.ErrCursorTooOld):
			writeErr(w, http.StatusConflict, api.ErrCursorTooOld,
				"requested cursor is older than the replay window; rehydrate durable history")
			return
		case errors.Is(err, events.ErrCursorAhead):
			writeErr(w, http.StatusConflict, api.ErrCursorAhead,
				"requested cursor is ahead of the current event cursor")
			return
		}
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	defer evs.Unsubscribe(sub)

	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // streams are intentionally long-lived
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flush := func() { _ = rc.Flush() }
	for _, ev := range sub.Replay {
		_ = enc.Encode(ev)
	}
	flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				if reason := sub.Err(); reason != nil {
					code := "STREAM_CLOSED"
					if errors.Is(reason, events.ErrSubscriberEvicted) {
						code = "SUBSCRIBER_EVICTED"
					}
					_ = enc.Encode(api.ErrorBody{Error: api.ErrorDetail{
						Code: code, Message: reason.Error(),
					}})
					flush()
				}
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			flush()
		}
	}
}
