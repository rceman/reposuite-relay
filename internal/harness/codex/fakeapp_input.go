package codex

import (
	"fmt"
	"os"
	"strconv"
)

// askInputs issues the mode's requested-input server requests (zero, one,
// or — "input-pair" — two concurrent ones) and reports whether the turn
// is waiting on answers. Every issued request ID is tracked in
// f.awaiting: the turn completes only after ALL of them are answered.
func (f *fakeServer) askInputs(threadID, turnID string) bool {
	nReq := 0
	switch f.mode {
	case "input", "input-secret", "input-secrets", "input-other":
		nReq = 1
	case "input-pair":
		nReq = 2
	}
	for i := 0; i < nReq; i++ {
		f.nextID++
		reqID := f.nextID
		f.awaiting[reqID] = true
		f.lastReqID = reqID
		f.write(frame{
			ID:     &reqID,
			Method: MethodRequestUserInput,
			Params: mustJSON(map[string]any{
				"threadId":   threadID,
				"turnId":     turnID,
				"itemId":     fmt.Sprintf("item_input_%d", i+1),
				"isBlocking": true,
				"questions":  f.inputQuestions(),
			}),
		})
	}
	return nReq > 0
}

// inputQuestions builds the scripted requested-input question set for
// the current fake mode.
func (f *fakeServer) inputQuestions() []map[string]any {
	questions := []map[string]any{{
		"id":       "q1",
		"header":   "Choose",
		"question": "Pick one",
		"options": []map[string]any{
			{"label": "alpha", "description": "first"},
			{"label": "beta", "description": "second"},
		},
	}}
	if f.mode == "input-other" {
		// A question can offer structured options AND a free-form path.
		questions[0]["isOther"] = true
	}
	if f.mode == "input-secret" {
		questions = append(questions, map[string]any{
			"id":       "q2",
			"header":   "Credential",
			"question": "Enter the token",
			"isOther":  true,
			"isSecret": true,
		})
	}
	if f.mode == "input-secrets" {
		questions = append(questions,
			map[string]any{
				"id":       "zz",
				"header":   "Credential Z",
				"question": "Enter the Z token",
				"isOther":  true,
				"isSecret": true,
			},
			map[string]any{
				"id":       "aa",
				"header":   "Credential A",
				"question": "Enter the A token",
				"isOther":  true,
				"isSecret": true,
			},
		)
	}
	return questions
}

// reportInputResponses persists the running native-response count for
// the current requested-input request — the at-most-once evidence seam.
// Atomic rewrite: readers never see a torn count.
func (f *fakeServer) reportInputResponses() {
	path := os.Getenv("FAKE_CODEX_INPUTRESP_FILE")
	if path == "" {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(f.inputResponses)), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
