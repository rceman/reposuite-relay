// Package presentation owns Relay-local display and grouping
// configuration: durable human-facing metadata that is never session
// lifecycle authority. Schema v1 carries only Projects — a stable
// Relay-local ID, a display name, and an absolute filesystem root used
// purely to group sessions by their cwd. It is unrelated to GTW
// project/task/workflow authority and never touches a harness.
package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SchemaVersion is the only supported presentation.json schema.
const SchemaVersion = 1

// MaxFile bounds the durable file — the catalog is small by contract.
const MaxFile = 256 << 10 // 256 KiB

// MaxProjects bounds the catalog; beyond it the file cannot be valid.
const MaxProjects = 256

// MaxNameBytes bounds a display name after whitespace normalization.
const MaxNameBytes = 128

var idRE = regexp.MustCompile(`^prj_[0-9a-f]{32}$`)

// Project is Relay-local grouping metadata: a stable opaque ID, a
// human display name, and an absolute filesystem root. Membership is
// derived from session cwd — the project never owns the session.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

// Catalog is the durable presentation configuration.
type Catalog struct {
	SchemaVersion int       `json:"schemaVersion"`
	Projects      []Project `json:"projects"`
}

// ValidID reports whether s is a canonical project ID.
func ValidID(s string) bool { return idRE.MatchString(s) }

// ValidateName normalizes surrounding whitespace and enforces the
// display-name contract: 1..128 UTF-8 bytes, no NUL or control chars.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("project name is required")
	}
	if len(name) > MaxNameBytes {
		return "", fmt.Errorf("project name exceeds %d bytes", MaxNameBytes)
	}
	if !utf8.ValidString(name) {
		return "", errors.New("project name is not valid UTF-8")
	}
	for _, r := range name {
		// unicode.IsControl covers C0, DEL, AND the C1 range (U+0080–9F
		// incl. NEL U+0085) — the documented "no control characters"
		// contract, not just ASCII. Ordinary non-ASCII text stays legal.
		if unicode.IsControl(r) {
			return "", errors.New("project name contains control characters")
		}
	}
	return name, nil
}

// ValidateRoot enforces the root contract: a required absolute path,
// reduced to canonical filepath.Clean form. The path is never stat'ed —
// a configured project directory may be temporarily offline.
func ValidateRoot(root string) (string, error) {
	if strings.ContainsRune(root, 0) {
		return "", errors.New("project root contains NUL")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("project root %q is not an absolute path", root)
	}
	return filepath.Clean(root), nil
}

// Validate fails closed on anything that is not exactly the supported
// schema: valid IDs, valid names/roots, no duplicate IDs, no duplicate
// canonical roots (nested roots are legal — longest-root matching makes
// them deterministic), and the project-count bound.
func (c Catalog) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d (want %d)", c.SchemaVersion, SchemaVersion)
	}
	if len(c.Projects) > MaxProjects {
		return fmt.Errorf("project count %d exceeds bound %d", len(c.Projects), MaxProjects)
	}
	seenID := map[string]bool{}
	seenRoot := map[string]bool{}
	for i, p := range c.Projects {
		if !ValidID(p.ID) {
			return fmt.Errorf("project %d: id %q is not prj_<32 lowercase hex>", i, p.ID)
		}
		name, err := ValidateName(p.Name)
		if err != nil {
			return fmt.Errorf("project %d: %w", i, err)
		}
		if name != p.Name {
			return fmt.Errorf("project %d: name %q is not normalized (%q)", i, p.Name, name)
		}
		root, err := ValidateRoot(p.Root)
		if err != nil {
			return fmt.Errorf("project %d: %w", i, err)
		}
		if root != p.Root {
			return fmt.Errorf("project %d: root %q is not canonical (%q)", i, p.Root, root)
		}
		if seenID[p.ID] {
			return fmt.Errorf("project %d: duplicate id %s", i, p.ID)
		}
		seenID[p.ID] = true
		if seenRoot[p.Root] {
			return fmt.Errorf("project %d: duplicate root %s", i, p.Root)
		}
		seenRoot[p.Root] = true
	}
	return nil
}

// Match returns the project whose canonical root most specifically
// contains cwd — longest matching root wins. Matching is pure path
// semantics (filepath.Rel is component-aware): /work/foobar does NOT
// match root /work/foo. It touches no filesystem and no session state.
func Match(c Catalog, cwd string) (Project, bool) {
	cwd = filepath.Clean(cwd)
	var best Project
	bestLen := -1
	for _, p := range c.Projects {
		rel, err := filepath.Rel(p.Root, cwd)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // cwd escapes the root — /work/foobar vs /work/foo
		}
		if len(p.Root) > bestLen {
			best, bestLen = p, len(p.Root)
		}
	}
	return best, bestLen >= 0
}

// Load reads and strictly validates presentation.json. A missing file is
// a legitimate empty catalog (schema 1, no projects) — reads never create
// the file. Malformed, oversized, trailing-content, non-regular,
// symlinked, or owner-exposed content is a hard error: the caller must
// fail closed and the file is never silently rewritten or repaired.
func Load(path string) (Catalog, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Catalog{
				SchemaVersion: SchemaVersion,
				Projects:      []Project{},
			}, nil
		}
		return Catalog{}, err
	}
	if !fi.Mode().IsRegular() {
		return Catalog{}, fmt.Errorf("presentation config %s is not a regular file", path)
	}
	if fi.Mode().Perm() != 0o600 {
		return Catalog{}, fmt.Errorf("presentation config %s: mode %o, want 0600 owner-private",
			path, fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return Catalog{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return Catalog{}, err
	}
	if len(raw) > MaxFile {
		return Catalog{}, fmt.Errorf("presentation config %s: exceeds %d-byte bound", path, MaxFile)
	}
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Catalog{}, fmt.Errorf("presentation config %s: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Catalog{}, fmt.Errorf("presentation config %s: trailing content after the JSON object", path)
	}
	if err := c.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("presentation config %s: %w", path, err)
	}
	return c, nil
}

// PostCommitError reports a failure AFTER the canonical commit point:
// presentation.json was already renamed into place, so the catalog IS
// committed — callers must converge in-memory state to the new content
// and never treat this as a clean rollback.
type PostCommitError struct {
	Op  string // "dirsync"
	Err error
}

func (e *PostCommitError) Error() string {
	return fmt.Sprintf("presentation config committed; post-commit %s failed: %v", e.Op, e.Err)
}
func (e *PostCommitError) Unwrap() error { return e.Err }

// Hooks is the narrow deterministic fault seam for commit-path failure
// injection — tests only; production passes nil.
type Hooks struct {
	WriteTemp func(f *os.File, b []byte) (int, error)
	SyncTemp  func(f *os.File) error
	Rename    func(oldname, newname string) error
	SyncDir   func(dir string) error
}

func (h *Hooks) writeTemp(f *os.File, b []byte) (int, error) {
	if h != nil && h.WriteTemp != nil {
		return h.WriteTemp(f, b)
	}
	return f.Write(b)
}
func (h *Hooks) syncTemp(f *os.File) error {
	if h != nil && h.SyncTemp != nil {
		return h.SyncTemp(f)
	}
	return f.Sync()
}
func (h *Hooks) rename(oldname, newname string) error {
	if h != nil && h.Rename != nil {
		return h.Rename(oldname, newname)
	}
	return os.Rename(oldname, newname)
}
func (h *Hooks) syncDir(dir string) error {
	if h != nil && h.SyncDir != nil {
		return h.SyncDir(dir)
	}
	return syncDir(dir)
}

// Commit persists the catalog atomically: same-directory temp (0600),
// full JSON write, temp fsync, close, atomic rename over the canonical
// path, directory fsync.
//
// COMMIT POINT: the successful rename. Anything before it is pre-commit
// — the canonical catalog is unchanged. Anything after it is post-commit
// — the new catalog IS durable content on the filesystem; a directory
// fsync failure surfaces as *PostCommitError, never as a rollback.
func Commit(path string, c Catalog) error {
	return CommitWith(path, c, nil)
}

// CommitWith is Commit with an explicit fault-injection seam — tests
// only; production callers use Commit.
func CommitWith(path string, c Catalog, hooks *Hooks) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("presentation config: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".presentation.json.tmp-*")
	if err != nil {
		return fmt.Errorf("presentation config temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("presentation config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fail(err)
	}
	if _, err := hooks.writeTemp(tmp, append(raw, '\n')); err != nil {
		return fail(err)
	}
	if err := hooks.syncTemp(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	// COMMIT POINT: rename names the staged content canonically. After
	// this returns nil the new catalog is committed regardless of what
	// follows.
	if err := hooks.rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("presentation config commit: %w", err)
	}
	if err := hooks.syncDir(dir); err != nil {
		return &PostCommitError{
			Op:  "dirsync",
			Err: err,
		}
	}
	return nil
}

// syncDir fsyncs a directory so the renamed entry is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
