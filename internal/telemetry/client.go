package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RepoDex discovery/auth/transport — production IPC is direct loopback HTTP
// discovered from RepoDex-owned files. Relay never shells out to the
// repodex CLI for production telemetry.

const (
	runtimeFile   = "service.runtime.json"
	tokenFile     = "service.token"
	statusSchema  = "reposuite.repodex.service.status.v1"
	ingestSchema  = "reposuite.repodex.ingest.v1"
	runtimeSchema = "reposuite.repodex.service.runtime.v1"
	maxFile       = 64 << 10
	maxResp       = 4 << 20
)

// descriptor is RepoDex's service.runtime.json contract.
type descriptor struct {
	Schema          string `json:"schema"`
	PID             uint32 `json:"pid"`
	Host            string `json:"host"`
	Port            uint16 `json:"port"`
	InstanceID      string `json:"instance_id"`
	StartedAt       string `json:"started_at"`
	RepoDexVersion  string `json:"repodex_version"`
	ProtocolVersion uint32 `json:"protocol_version"`
}

// endpoint is a validated RepoDex target: descriptor + token.
type endpoint struct {
	desc  descriptor
	token string
}

func (e endpoint) url(p string) string {
	return "http://" + net.JoinHostPort(e.desc.Host,
		strconv.Itoa(int(e.desc.Port))) + p
}

// discover reads and validates the runtime descriptor + service token from
// the RepoDex state dir. Missing files are a clean "not running" — never an
// error that affects the agent.
func discover(stateDir string) (*endpoint, error) {
	desc, err := readDescriptor(filepath.Join(stateDir, runtimeFile))
	if err != nil {
		return nil, err
	}
	tok, err := readToken(filepath.Join(stateDir, tokenFile))
	if err != nil {
		return nil, err
	}
	return &endpoint{
		desc:  *desc,
		token: tok,
	}, nil
}

func readDescriptor(path string) (*descriptor, error) {
	b, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	var d descriptor
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("runtime descriptor: %w", err)
	}
	if d.Schema != runtimeSchema {
		return nil, fmt.Errorf("runtime descriptor schema %q", d.Schema)
	}
	if d.ProtocolVersion != 1 {
		return nil, fmt.Errorf("protocol_version %d unsupported", d.ProtocolVersion)
	}
	if !loopback(d.Host) {
		return nil, fmt.Errorf("refusing non-loopback host %q", d.Host)
	}
	if d.Port == 0 {
		return nil, fmt.Errorf("runtime descriptor port 0")
	}
	return &d, nil
}

func readToken(path string) (string, error) {
	b, err := readBounded(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 32 {
		return "", fmt.Errorf("service.token malformed")
	}
	return t, nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxFile))
}

func loopback(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Request/Reply are the Doer transport seam.
type Request struct {
	Method string
	URL    string
	Token  string
	Body   []byte
}

type Reply struct {
	Status int
	Body   []byte
}

// HTTPDoer is the production transport: bounded loopback HTTP client.
type HTTPDoer struct{ C *http.Client }

func (h HTTPDoer) Do(r *Request) (*Reply, error) {
	c := h.C
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequest(r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResp))
	if err != nil {
		return nil, err
	}
	return &Reply{
		Status: resp.StatusCode,
		Body:   body,
	}, nil
}

// statusOK validates GET /v1/status against the accepted status schema.
func statusOK(ep *endpoint, do Doer) error {
	rep, err := do.Do(&Request{
		Method: "GET",
		URL:    ep.url("/v1/status"),
		Token:  ep.token,
	})
	if err != nil {
		return err
	}
	if rep.Status != 200 {
		return fmt.Errorf("status http %d", rep.Status)
	}
	var v struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(rep.Body, &v); err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if v.Schema != statusSchema {
		return fmt.Errorf("status schema %q", v.Schema)
	}
	return nil
}

// ingestReply is the RepoDex ingest.v1 response contract.
type ingestReply struct {
	Schema     string   `json:"schema"`
	Accepted   int64    `json:"accepted"`
	Duplicates int64    `json:"duplicates"`
	Rejected   int64    `json:"rejected"`
	Errors     []string `json:"errors"`
}

// sendBatch POSTs a bare JSON array of canonical events — the exact
// RepoDex /v1/events/batch wire shape.
func sendBatch(ep *endpoint, do Doer, raws [][]byte) (*ingestReply, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, r := range raws {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(r)
	}
	buf.WriteByte(']')
	rep, err := do.Do(&Request{
		Method: "POST",
		URL:    ep.url("/v1/events/batch"),
		Token:  ep.token,
		Body:   buf.Bytes(),
	})
	if err != nil {
		return nil, err
	}
	if rep.Status == 401 || rep.Status == 403 {
		return nil, fmt.Errorf("auth rejected: %d", rep.Status)
	}
	if rep.Status != 200 {
		return nil, fmt.Errorf("ingest http %d", rep.Status)
	}
	var out ingestReply
	if err := json.Unmarshal(rep.Body, &out); err != nil {
		return nil, fmt.Errorf("ingest reply: %w", err)
	}
	if out.Schema != ingestSchema {
		return nil, fmt.Errorf("ingest schema %q", out.Schema)
	}
	return &out, nil
}

// rejectedIDs extracts permanently-rejected event_ids from ingest error
// strings ("{event_id}: reason", "conflicting event_id {id}"). Rejected
// events are permanent canonicalization bugs — retried forever they would
// wedge the spool, so they are pruned and counted as lost.
func rejectedIDs(rep *ingestReply) map[string]bool {
	out := map[string]bool{}
	for _, e := range rep.Errors {
		if i := strings.Index(e, ": "); i > 0 && !strings.Contains(e[:i], " ") {
			out[e[:i]] = true
		}
		if strings.HasPrefix(e, "conflicting event_id ") {
			out[strings.TrimPrefix(e, "conflicting event_id ")] = true
		}
	}
	return out
}
