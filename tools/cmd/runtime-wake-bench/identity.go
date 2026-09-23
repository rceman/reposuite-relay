// Secure input of a proven native identity. The raw ID is accepted only
// from an owner-private regular file — never from argv — and is never
// printed or serialized into benchmark output (fingerprint only).
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// IdentityFile carries a proven native identity plus the disposable cwd
// it was created in.
type IdentityFile struct {
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
}

// readIdentityFile loads a proven identity from a 0600-or-stricter
// regular file. Anything else (symlink to elsewhere, world/group-
// readable, empty identity) is a hard refusal.
func readIdentityFile(path string) (IdentityFile, error) {
	var id IdentityFile
	fi, err := os.Lstat(path)
	if err != nil {
		return id, err
	}
	if !fi.Mode().IsRegular() {
		return id, fmt.Errorf("identity file %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return id, fmt.Errorf("identity file %s must be owner-private (mode %o)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return id, err
	}
	if err := json.Unmarshal(b, &id); err != nil {
		return id, fmt.Errorf("identity file %s: %w", path, err)
	}
	if id.ID == "" {
		return id, fmt.Errorf("identity file %s has empty id", path)
	}
	if id.Cwd == "" {
		return id, fmt.Errorf("identity file %s has empty cwd", path)
	}
	return id, nil
}
