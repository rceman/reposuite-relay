package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// The fake app-server is a deterministic, scripted stand-in for the real
// Codex app-server, reachable through the hidden `__fake-codex` mode of
// the Relay binary (the same pattern as the `__fixture` child). It makes
// no network access, performs no model call, and consumes no quota — it
// only speaks the verified JSON-RPC surface, so the whole adapter vertical
// slice (materialization, exact resume, requested input, cancellation,
// runtime death, process-tree teardown) can be exercised deterministically.
//
// Modes are selected with FAKE_CODEX_MODE:
//
//	""/"happy"      initialize, thread start/resume, one completed turn
//	"fail-turn"     the turn completes with status failed
//	"input"         the turn asks for user input before completing
//	"input-secret"  the turn asks one normal plus one isSecret question
//	"input-secrets" the turn asks one normal plus two isSecret questions
//	                (question IDs ordered so lexical ≠ a likely answer order)
//	"input-other"   the turn asks one question with options AND isOther
//	"input-pair"    the turn issues TWO concurrent requested-input
//	                requests and completes only after both are answered
//	"die-on-turn"   the process exits right after accepting a turn
//	"resume-error"  thread/resume always fails
//	"stubborn"      ignores stdin forever (forces the bounded kill path)
//	"child"         spawns a grandchild and reports its pid (tree teardown)
//
// FAKE_CODEX_INPUTRESP_FILE, when set, receives the running count of
// responses the fake receives for its requested-input request ID —
// written on every matching response so tests can prove at-most-once
// native delivery (including a duplicate arriving after the request was
// already answered).
func RunFakeAppServer(in io.Reader, out io.Writer) int {
	mode := os.Getenv("FAKE_CODEX_MODE")
	f := &fakeServer{
		mode:     mode,
		in:       bufio.NewScanner(in),
		out:      out,
		nextID:   100,
		awaiting: map[int64]bool{},
	}
	if mode == "stubborn" {
		select {} // never exits on stdin EOF
	}
	if mode == "child" {
		if err := f.spawnGrandchild(); err != nil {
			fmt.Fprintf(os.Stderr, "fake-codex: grandchild: %v\n", err)
			return 1
		}
	}
	f.loop()
	return 0
}

type fakeServer struct {
	mode   string
	in     *bufio.Scanner
	out    io.Writer
	nextID int64

	threads  int
	turns    int
	items    int
	pending  *pendingTurn
	threadID string
	// resumeCount counts successful resumes (test evidence).
	resumeCount int
	// awaiting holds the outstanding requested-input request IDs — a turn
	// completes only after every one has a response. lastReqID is the most
	// recent request ID; inputResponses counts every response frame sent
	// for a tracked request (double responses after finishTurn still
	// count — at-most-once evidence) and inputErrs classifies the RPC
	// error subset.
	awaiting       map[int64]bool
	lastReqID      int64
	inputResponses int
	inputErrs      int
}

type pendingTurn struct {
	threadID string
	turnID   string
	text     string
}

type frame struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

func (f *fakeServer) loop() {
	for f.in.Scan() {
		line := strings.TrimSpace(f.in.Text())
		if line == "" {
			continue
		}
		var msg frame
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			fmt.Fprintf(os.Stderr, "fake-codex: bad frame: %v\n", err)
			return
		}
		if msg.Method == "" && msg.ID != nil {
			// A response to the fake's own requested-input request.
			if f.awaiting[*msg.ID] {
				delete(f.awaiting, *msg.ID)
				f.noteInputResponse(msg)
				if len(f.awaiting) == 0 {
					f.finishTurn()
				}
			} else if *msg.ID == f.lastReqID && f.lastReqID != 0 {
				// A duplicate response to an already-answered request
				// still counts — at-most-once evidence.
				f.noteInputResponse(msg)
			}
			continue
		}
		f.dispatch(msg)
	}
}

func (f *fakeServer) dispatch(msg frame) {
	switch msg.Method {
	case "initialize":
		f.respond(msg, map[string]any{
			"userAgent":      "fake-codex/0.0.0",
			"codexHome":      "/fake/codex-home",
			"platformFamily": "unix",
			"platformOs":     "linux",
		})
	case "thread/start":
		f.threads++
		f.threadID = fmt.Sprintf("th_%016d", f.threads)
		f.respond(msg, map[string]any{
			"thread":         map[string]any{"id": f.threadID},
			"model":          f.modelFrom(msg, "fake-model"),
			"modelProvider":  "fake",
			"cwd":            "/fake",
			"approvalPolicy": ApprovalNever,
		})
		f.notify("thread/started", map[string]any{
			"thread": map[string]any{"id": f.threadID}})
	case "thread/resume":
		var p ThreadResumeParams
		_ = json.Unmarshal(msg.Params, &p)
		if f.mode == "resume-error" {
			f.respondErr(msg, -32000, "no such thread "+p.ThreadID)
			return
		}
		if p.ThreadID == "" {
			f.respondErr(msg, -32602, "threadId required")
			return
		}
		f.resumeCount++
		f.threadID = p.ThreadID
		f.respond(msg, map[string]any{
			"thread":         map[string]any{"id": p.ThreadID},
			"model":          f.modelFrom(msg, "fake-model"),
			"modelProvider":  "fake",
			"cwd":            "/fake",
			"approvalPolicy": ApprovalNever,
		})
	case "turn/start":
		f.startTurn(msg)
	case "turn/interrupt":
		var p TurnInterruptParams
		_ = json.Unmarshal(msg.Params, &p)
		f.respond(msg, map[string]any{"turn": map[string]any{"id": p.TurnID}})
		f.notify("turn/completed", map[string]any{
			"threadId": p.ThreadID,
			"turn":     map[string]any{"id": p.TurnID, "status": "interrupted"},
		})
	default:
		if msg.ID != nil {
			f.respondErr(msg, -32601, "unsupported method "+msg.Method)
		}
	}
}

func (f *fakeServer) modelFrom(msg frame, def string) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(msg.Params, &probe)
	if probe.Model == "" {
		return def
	}
	return probe.Model
}

func (f *fakeServer) startTurn(msg frame) {
	var p TurnStartParams
	_ = json.Unmarshal(msg.Params, &p)
	if p.ThreadID == "" || len(p.Input) == 0 {
		f.respondErr(msg, -32602, "threadId and input required")
		return
	}
	f.turns++
	turnID := fmt.Sprintf("tn_%016d", f.turns)
	f.respond(msg, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})

	if f.mode == "die-on-turn" {
		os.Exit(3) // simulates a runtime crash with a turn in flight
	}

	f.notify("turn/started", map[string]any{
		"threadId": p.ThreadID,
		"turn":     map[string]any{"id": turnID, "status": "inProgress"},
	})

	text := p.Input[0].Text
	f.pending = &pendingTurn{
		threadID: p.ThreadID,
		turnID:   turnID,
		text:     text,
	}

	if f.askInputs(p.ThreadID, turnID) {
		return // completion waits for the answers
	}
	f.finishTurn()
}

// finishTurn emits the deterministic completion sequence.
func (f *fakeServer) finishTurn() {
	pt := f.pending
	if pt == nil {
		return
	}
	f.pending = nil
	f.items++
	itemID := fmt.Sprintf("item_%016d", f.items)
	reply := "echo: " + pt.text
	// Deterministic tool-item fixtures: prompts prefixed "tool:<kind>"
	// emit a native tool item lifecycle before the agent message.
	if strings.HasPrefix(pt.text, "tool:") {
		f.emitToolItems(pt)
	}
	if f.mode == "fail-turn" {
		f.notify("turn/completed", map[string]any{
			"threadId": pt.threadID,
			"turn": map[string]any{"id": pt.turnID, "status": "failed",
				"error": map[string]any{"message": "scripted failure", "code": "fake"}},
		})
		return
	}
	half := len(reply) / 2
	for _, chunk := range []string{reply[:half], reply[half:]} {
		f.notify(NotifyAgentMessage, map[string]any{
			"threadId": pt.threadID, "turnId": pt.turnID,
			"itemId": itemID, "delta": chunk,
		})
	}
	f.notify("item/completed", map[string]any{
		"threadId": pt.threadID, "turnId": pt.turnID, "completedAtMs": 1,
		"item": map[string]any{"type": "agentMessage", "id": itemID, "text": reply},
	})
	f.notify("turn/completed", map[string]any{
		"threadId": pt.threadID,
		"turn": map[string]any{"id": pt.turnID, "status": "completed",
			"items": []map[string]any{
				{"type": "agentMessage", "id": itemID, "text": reply}}},
	})
	f.notify(NotifyTokenUsage, map[string]any{
		"threadId": pt.threadID, "turnId": pt.turnID,
		"tokenUsage": map[string]any{
			"last": map[string]any{"cachedInputTokens": 1, "inputTokens": 10,
				"outputTokens": 5, "reasoningOutputTokens": 0, "totalTokens": 15},
			"total": map[string]any{"cachedInputTokens": 1, "inputTokens": 10,
				"outputTokens": 5, "reasoningOutputTokens": 0, "totalTokens": 15},
			"modelContextWindow": 200000,
		},
	})
}

// spawnGrandchild models the runtime's own child process (the real
// app-server is a Node program): the whole tree must die with the runtime.
func (f *fakeServer) spawnGrandchild() error {
	path := os.Getenv("FAKE_CODEX_CHILDPID_FILE")
	if path == "" {
		return fmt.Errorf("FAKE_CODEX_CHILDPID_FILE unset")
	}
	child := exec.Command("sleep", "600")
	if err := child.Start(); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
}

func (f *fakeServer) respond(msg frame, result any) {
	id := int64(0)
	if msg.ID != nil {
		id = *msg.ID
	}
	f.write(frame{
		ID:     &id,
		Result: mustJSON(result),
	})
}

func (f *fakeServer) respondErr(msg frame, code int, text string) {
	id := int64(0)
	if msg.ID != nil {
		id = *msg.ID
	}
	f.write(frame{
		ID: &id,
		Error: &rpcError{
			Code:    code,
			Message: text,
		},
	})
}

func (f *fakeServer) notify(method string, params any) {
	f.write(frame{
		Method: method,
		Params: mustJSON(params),
	})
}

func (f *fakeServer) write(v frame) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	if _, err := f.out.Write(raw); err != nil {
		return
	}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
