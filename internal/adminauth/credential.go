package adminauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// SchemaVersion is the only supported admin.json schema.
const SchemaVersion = 1

// maxFile bounds a credential read — the schema is tiny, so anything
// larger is malformed by definition.
const maxFile = 8 << 10

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Credentials is the durable Web Admin credential: a username and an
// Argon2id PHC password hash. It never contains a plaintext password.
type Credentials struct {
	SchemaVersion int    `json:"schemaVersion"`
	Username      string `json:"username"`
	PasswordHash  string `json:"passwordHash"`
}

// ValidateUsername enforces the username contract: 1..64 chars from
// [A-Za-z0-9._-] — no whitespace, no control characters.
func ValidateUsername(username string) error {
	if !usernameRE.MatchString(username) {
		return fmt.Errorf("username must match [A-Za-z0-9._-]{1,64}")
	}
	return nil
}

// Validate fails closed on anything that is not exactly the supported
// credential schema.
func (c Credentials) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d (want %d)", c.SchemaVersion, SchemaVersion)
	}
	if err := ValidateUsername(c.Username); err != nil {
		return fmt.Errorf("username: %w", err)
	}
	if _, err := parsePHC(c.PasswordHash); err != nil {
		return fmt.Errorf("passwordHash: %w", err)
	}
	return nil
}

// Load reads and strictly validates the admin credential. Missing file →
// os.ErrNotExist (unconfigured is a state, not an error). Malformed,
// oversized, trailing-content, non-regular, or owner-exposed content is
// a hard error — the caller must fail closed.
func Load(path string) (Credentials, error) {
	raw, err := readRegular(path)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Credentials{}, fmt.Errorf("admin credentials %s: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Credentials{}, fmt.Errorf("admin credentials %s: trailing content after the JSON object", path)
	}
	if err := c.Validate(); err != nil {
		return Credentials{}, fmt.Errorf("admin credentials %s: %w", path, err)
	}
	return c, nil
}

// PostCommitError reports a failure AFTER the canonical commit point:
// admin.json was already linked into the config namespace, so the
// credential IS committed — callers must never treat this as a clean
// rollback or allow a second setup to overwrite authority.
type PostCommitError struct {
	Op  string // "cleanup" | "dirsync"
	Err error
}

func (e *PostCommitError) Error() string {
	return fmt.Sprintf("admin credentials committed; post-commit %s failed: %v", e.Op, e.Err)
}
func (e *PostCommitError) Unwrap() error { return e.Err }

// Hooks is the narrow deterministic fault seam for commit-path failure
// injection — tests only; production passes nil.
type Hooks struct {
	Link    func(oldname, newname string) error
	Remove  func(name string) error
	SyncDir func(dir string) error
}

func (h *Hooks) link(oldname, newname string) error {
	if h != nil && h.Link != nil {
		return h.Link(oldname, newname)
	}
	return os.Link(oldname, newname)
}
func (h *Hooks) remove(name string) error {
	if h != nil && h.Remove != nil {
		return h.Remove(name)
	}
	return os.Remove(name)
}
func (h *Hooks) syncDir(dir string) error {
	if h != nil && h.SyncDir != nil {
		return h.SyncDir(dir)
	}
	return syncDir(dir)
}

// CommitOnce persists the credential atomically and exactly once:
// same-directory temp (0600), fsync, atomic hard-link create, temp
// removal, directory fsync.
//
// COMMIT POINT: the successful os.Link is the namespace commit — after
// it, admin.json EXISTS and is canonical. Outcomes:
//
//	pre-commit failure:   created=false, err!=nil — nothing committed
//	already exists:       created=false, err=nil  — a winner is committed
//	full durable commit:  created=true,  err=nil
//	post-commit failure:  created=true,  err=*PostCommitError — the
//	                      credential IS committed; only cleanup or the
//	                      directory fsync is uncertain
func CommitOnce(path string, c Credentials) (bool, error) {
	return CommitOnceWith(path, c, nil)
}

// CommitOnceWith is CommitOnce with an explicit fault-injection seam —
// daemon tests only; production callers pass nil via CommitOnce.
func CommitOnceWith(path string, c Credentials, hooks *Hooks) (created bool, err error) {
	if err := c.Validate(); err != nil {
		return false, fmt.Errorf("admin credentials: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".admin.json.tmp-*")
	if err != nil {
		return false, fmt.Errorf("admin credentials temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) (bool, error) {
		tmp.Close()
		hooks.remove(tmpPath)
		return false, fmt.Errorf("admin credentials: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	// Atomic create-once: link() names the staged content at path only if
	// path does not already exist — a loser of a setup race keeps its own
	// unlinked temp and reports "not created", never clobbering the winner.
	// This IS the commit point: anything after it is post-commit.
	if err := hooks.link(tmpPath, path); err != nil {
		hooks.remove(tmpPath)
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("admin credentials create: %w", err)
	}
	// Post-commit: canonical admin.json exists. A leftover staged temp
	// file or an uncertain directory fsync must surface as committed-with-
	// uncertainty, never as a clean failure.
	if err := hooks.remove(tmpPath); err != nil {
		return true, &PostCommitError{
			Op:  "cleanup",
			Err: err,
		}
	}
	if err := hooks.syncDir(dir); err != nil {
		return true, &PostCommitError{
			Op:  "dirsync",
			Err: err,
		}
	}
	return true, nil
}

// readRegular reads a bounded, owner-private regular file: a symlink or
// special file, any mode other than exactly 0600, or content exceeding
// the bound all fail closed — a truncated prefix is never parsed as a
// valid credential, and an already-exposed credential is never silently
// repaired.
func readRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("admin credentials %s: mode %o, want 0600 owner-private",
			path, fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFile {
		return nil, fmt.Errorf("admin credentials %s: exceeds %d-byte bound", path, maxFile)
	}
	return raw, nil
}

// syncDir fsyncs a directory so entry changes are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
