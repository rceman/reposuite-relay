package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGatewayDocMatchesAPIVersion: docs/GATEWAY_API.md is the machine-
// client integration contract — its declared apiVersion must equal the
// production constant mechanically, never a duplicated magic number.
func TestGatewayDocMatchesAPIVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "GATEWAY_API.md"))
	if err != nil {
		t.Fatalf("read GATEWAY_API.md: %v", err)
	}
	doc := string(raw)
	want := fmt.Sprintf(`"apiVersion":%d`, Version)
	if !strings.Contains(doc, want) {
		t.Fatalf("GATEWAY_API.md does not document the production API version %d", Version)
	}
	for v := 1; v <= 16; v++ {
		if v == Version {
			continue
		}
		if strings.Contains(doc, fmt.Sprintf(`"apiVersion":%d`, v)) {
			t.Fatalf("GATEWAY_API.md documents a stale apiVersion %d (production is %d)", v, Version)
		}
	}
}
