package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"io"
	"net/http"
	"strconv"
)

// EventStream is an open NDJSON event stream.
type EventStream struct {
	rc io.ReadCloser
	sc *bufio.Scanner
}

// Next returns the next canonical event, or an error: io.EOF at stream
// end, *APIError for a deterministic stream termination
// (SUBSCRIBER_EVICTED / STREAM_CLOSED), or a transport error.
func (s *EventStream) Next() (events.Event, error) {
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			Error *api.ErrorDetail `json:"error"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Error != nil {
			return events.Event{}, &APIError{
				Code:    probe.Error.Code,
				Message: probe.Error.Message,
			}
		}
		var ev events.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return events.Event{}, fmt.Errorf("malformed event line: %w", err)
		}
		return ev, nil
	}
	if err := s.sc.Err(); err != nil {
		return events.Event{}, err
	}
	return events.Event{}, io.EOF
}

// Close ends the stream (cancels the subscription server-side).
func (s *EventStream) Close() error { return s.rc.Close() }

// Events opens the canonical event stream for a session after the given
// cursor. A too-old cursor is returned as *APIError CURSOR_TOO_OLD before
// any streaming begins.
func (c *Client) Events(ctx context.Context, key string, after uint64) (*EventStream, error) {
	path := sessionPath(key) + "/events?after=" + strconv.FormatUint(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.desc.Endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.desc.BearerToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var eb api.ErrorBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Code != "" {
			return nil, &APIError{
				Status:  resp.StatusCode,
				Code:    eb.Error.Code,
				Message: eb.Error.Message,
			}
		}
		return nil, &APIError{
			Status: resp.StatusCode,
			Code:   fmt.Sprintf("HTTP_%d", resp.StatusCode),
		}
	}
	sc := bufio.NewScanner(resp.Body)
	// The scanner accepts exactly the canonical frame bound — the same
	// contract enforced at publication and by the NDJSON server.
	sc.Buffer(make([]byte, 0, 64*1024), api.MaxEventFrameBytes)
	return &EventStream{
		rc: resp.Body,
		sc: sc,
	}, nil
}
