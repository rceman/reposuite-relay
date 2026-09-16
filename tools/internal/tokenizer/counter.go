// Package tokenizer counts hand-written source in exact o200k_base tokens.
//
// One Counter owns one lazily initialized encoding for its lifetime: a scan
// of a whole repository initializes the tokenizer once and never spawns a
// process per file. The encoding payload is read from a stable cache
// location (see loader.go) and is fetched from the public OpenAI blob only
// on a cold cache — never once per file and never once per scan.
package tokenizer

import (
	"bytes"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/pkoukk/tiktoken-go"
)

const (
	// EncodingName is the canonical encoding for the file budget rule.
	EncodingName = "o200k_base"
	// MaxTokens is the hard per-file budget for hand-written Go source.
	MaxTokens = 3000
)

// Counter owns one lazily initialized encoding for its lifetime.
type Counter struct {
	once sync.Once
	enc  *tiktoken.Tiktoken
	err  error
}

// NewCounter returns an uninitialized counter. Initialization happens on
// the first count, so callers that find no files never pay for it.
func NewCounter() *Counter { return &Counter{} }

func (c *Counter) encoding() (*tiktoken.Tiktoken, error) {
	c.once.Do(func() {
		tiktoken.SetBpeLoader(cachedLoader{})
		c.enc, c.err = tiktoken.GetEncoding(EncodingName)
	})
	if c.err != nil {
		return nil, fmt.Errorf("initialize %s tokenizer: %w", EncodingName, c.err)
	}
	return c.enc, nil
}

// CountText returns the exact o200k_base token count of a complete file.
// The whole file is counted — code, comments, strings, and tests — because
// the budget exists for agent/context readability, not compiler complexity.
// Input that is not valid UTF-8 (or that contains a NUL byte) fails closed:
// a Go source file in that state is a broken tree, not a budget question.
func (c *Counter) CountText(data []byte) (int, error) {
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, fmt.Errorf("source contains a NUL byte")
	}
	if !utf8.Valid(data) {
		return 0, fmt.Errorf("source is not valid UTF-8")
	}
	enc, err := c.encoding()
	if err != nil {
		return 0, err
	}
	return len(enc.Encode(string(data), nil, nil)), nil
}
