package presentation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNotFound names an unknown project ID; ErrRootConflict names a
// canonical root already owned by another project. The daemon maps both
// to stable machine errors.
var (
	ErrNotFound     = errors.New("project not found")
	ErrRootConflict = errors.New("project root already belongs to another project")
)

// Manager serializes catalog mutations over an immutable in-memory
// snapshot: every create/update/delete takes the mutex, copy-on-write
// clones the catalog, validates, commits atomically, then swaps the
// snapshot. A post-commit error still swaps — a successful rename means
// the new catalog IS canonical and memory must converge to it.
type Manager struct {
	mu    sync.Mutex
	path  string
	hooks *Hooks
	cat   Catalog
}

// NewManager loads the catalog at path (missing file → empty) and
// returns a manager whose mutations commit atomically to that path.
// hooks is a test-only fault seam — production passes nil.
func NewManager(path string, hooks *Hooks) (*Manager, error) {
	cat, err := Load(path)
	if err != nil {
		return nil, err
	}
	return &Manager{
		path:  path,
		hooks: hooks,
		cat:   cat,
	}, nil
}

// Snapshot returns the current catalog. The slice is treated as
// immutable — mutations copy it — so readers can iterate it without
// the lock.
func (m *Manager) Snapshot() Catalog {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cat
}

// Match resolves the most-specific project root containing cwd using the
// current snapshot — pure, observer-only.
func (m *Manager) Match(cwd string) (Project, bool) {
	return Match(m.Snapshot(), cwd)
}

// commit validates, sorts, and atomically persists the catalog, then
// converges the snapshot — including after a post-commit error, since a
// successful rename means the new catalog IS canonical.
func (m *Manager) commit(cat Catalog) error {
	if err := cat.Validate(); err != nil {
		return err
	}
	// Canonical API/output order: name, then root, then ID — never map
	// iteration order.
	sort.Slice(cat.Projects, func(i, j int) bool {
		a, b := cat.Projects[i], cat.Projects[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Root != b.Root {
			return a.Root < b.Root
		}
		return a.ID < b.ID
	})
	err := CommitWith(m.path, cat, m.hooks)
	// Converge whenever the rename commit point may have passed: nil
	// error or *PostCommitError both mean the new catalog is canonical.
	if err == nil || postCommit(err) {
		m.cat = cat
	}
	return err
}

func postCommit(err error) bool { return IsPostCommit(err) }

// IsPostCommit reports whether err is a post-commit failure — the
// rename already committed, so the catalog IS durable and the caller
// must converge to it rather than treat the mutation as rolled back.
func IsPostCommit(err error) bool {
	var pc *PostCommitError
	return errors.As(err, &pc)
}

// NewID mints a Relay-local opaque project ID: prj_ + 128 bits of
// crypto/rand hex — never derived from name, path, or foreign identity.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "prj_" + hex.EncodeToString(b[:]), nil
}

// Create adds a project; name is normalized, root is canonicalized.
// A canonical root owned by an existing project is ErrRootConflict.
func (m *Manager) Create(name, root string) (Project, error) {
	name, err := ValidateName(name)
	if err != nil {
		return Project{}, err
	}
	root, err = ValidateRoot(root)
	if err != nil {
		return Project{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.cat.Projects {
		if p.Root == root {
			return Project{}, fmt.Errorf("%w: %s", ErrRootConflict, root)
		}
	}
	id, err := NewID()
	if err != nil {
		return Project{}, fmt.Errorf("project id: %w", err)
	}
	prj := Project{
		ID:   id,
		Name: name,
		Root: root,
	}
	next := Catalog{
		SchemaVersion: SchemaVersion,
		Projects:      append(append([]Project{}, m.cat.Projects...), prj),
	}
	if err := m.commit(next); err != nil {
		// Post-commit: the catalog IS committed — return the project so
		// callers can report what actually exists alongside the error.
		if postCommit(err) {
			return prj, err
		}
		return Project{}, err
	}
	return prj, nil
}

// Update applies a partial edit — nil fields are unchanged. The project
// ID is immutable. A root move to another project's canonical root is
// ErrRootConflict.
func (m *Manager) Update(id, name, root string, setName, setRoot bool) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := Catalog{
		SchemaVersion: SchemaVersion,
		Projects:      append([]Project{}, m.cat.Projects...),
	}
	for i := range next.Projects {
		p := &next.Projects[i]
		if p.ID != id {
			continue
		}
		if setName {
			n, err := ValidateName(name)
			if err != nil {
				return Project{}, err
			}
			p.Name = n
		}
		if setRoot {
			r, err := ValidateRoot(root)
			if err != nil {
				return Project{}, err
			}
			for _, o := range m.cat.Projects {
				if o.ID != id && o.Root == r {
					return Project{}, fmt.Errorf("%w: %s", ErrRootConflict, r)
				}
			}
			p.Root = r
		}
		out := *p
		if err := m.commit(next); err != nil {
			return Project{}, err
		}
		return out, nil
	}
	return Project{}, ErrNotFound
}

// Delete removes presentation metadata only — it touches no session,
// runtime, transcript, or filesystem path. Matched sessions simply fall
// through to a less-specific project or Ungrouped on the next read.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := Catalog{SchemaVersion: SchemaVersion}
	found := false
	for _, p := range m.cat.Projects {
		if p.ID == id {
			found = true
			continue
		}
		next.Projects = append(next.Projects, p)
	}
	if !found {
		return ErrNotFound
	}
	return m.commit(next)
}
