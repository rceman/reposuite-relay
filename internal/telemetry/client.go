package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
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

// errContract marks a RepoDex contract violation (descriptor/status/
// ingest schema mismatch, malformed reply) — reported "incompatible" and
// never retried as a transport fault. errPermanent marks a definitively
// rejected request (4xx other than auth): the batch is dropped and
// counted lost rather than wedging the pipeline on infinite retry.
var (
	errContract  = errors.New("repodex contract violation")
	errPermanent = errors.New("repodex permanent rejection")
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
		return nil, fmt.Errorf("%w: runtime descriptor schema %q", errContract, d.Schema)
	}
	if d.ProtocolVersion != 1 {
		return nil, fmt.Errorf("%w: protocol_version %d unsupported", errContract, d.ProtocolVersion)
	}
	if !loopback(d.Host) {
		return nil, fmt.Errorf("%w: refusing non-loopback host %q", errContract, d.Host)
	}
	if d.Port == 0 {
		return nil, fmt.Errorf("%w: runtime descriptor port 0", errContract)
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
		c = &http.Client{
			Timeout: 10 * time.Second,
			// Never follow redirects: the bearer must stay on the exact
			// loopback endpoint the descriptor published.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
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
		return fmt.Errorf("%w: status: %v", errContract, err)
	}
	if v.Schema != statusSchema {
		return fmt.Errorf("%w: status schema %q", errContract, v.Schema)
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
		// Auth failure is retryable: the sender re-discovers the
		// descriptor/token (RepoDex restart rotates it).
		return nil, fmt.Errorf("auth rejected: %d", rep.Status)
	}
	if rep.Status >= 400 && rep.Status < 500 {
		// Any other 4xx is a definitive rejection of this request —
		// retrying it verbatim can never succeed.
		return nil, fmt.Errorf("%w: ingest http %d", errPermanent, rep.Status)
	}
	if rep.Status != 200 {
		return nil, fmt.Errorf("ingest http %d", rep.Status)
	}
	var out ingestReply
	if err := json.Unmarshal(rep.Body, &out); err != nil {
		return nil, fmt.Errorf("%w: ingest reply: %v", errContract, err)
	}
	if out.Schema != ingestSchema {
		return nil, fmt.Errorf("%w: ingest schema %q", errContract, out.Schema)
	}
	return &out, nil
}
