package codex

import (
	"os"
	"strconv"
)

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
