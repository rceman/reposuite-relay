// Package gofiles enumerates the hand-written Go files a repository gate
// must cover: tracked files plus new untracked, non-ignored files.
package gofiles

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// outputLimit bounds git command output so a hostile or broken repository
// cannot make a gate allocate without limit.
const outputLimit = 64 << 20

// Options selects the file set.
type Options struct {
	// Root is the repository root (required).
	Root string
	// Base is a revision to diff against. Empty means the full tree.
	Base string
}

// List returns repository-relative Go file paths, sorted and deduplicated.
//
// Full mode (no Base) uses one git enumeration of tracked and untracked
// non-ignored files. Changed mode (Base set) uses one git diff plus the
// untracked files, so a fast gate sees both edited and brand-new files.
func List(ctx context.Context, opts Options) ([]string, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("repository root is required")
	}
	var paths []string
	if opts.Base == "" {
		out, err := git(ctx, opts.Root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go")
		if err != nil {
			return nil, err
		}
		paths = splitNUL(out)
	} else {
		base, err := git(ctx, opts.Root, "rev-parse", "--verify", opts.Base+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("resolve base %q: %w", opts.Base, err)
		}
		_ = base
		changed, err := git(ctx, opts.Root, "diff", "--name-only", "-z", "--diff-filter=ACMR", opts.Base, "--", "*.go")
		if err != nil {
			return nil, err
		}
		untracked, err := git(ctx, opts.Root, "ls-files", "-z", "--others", "--exclude-standard", "--", "*.go")
		if err != nil {
			return nil, err
		}
		paths = append(splitNUL(changed), splitNUL(untracked)...)
	}
	return Normalize(paths)
}

// Normalize validates, deduplicates, and sorts repository-relative paths.
func Normalize(paths []string) ([]string, error) {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := filepath.ToSlash(filepath.Clean(strings.TrimSpace(raw)))
		if path == "" || path == "." {
			continue
		}
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
			return nil, fmt.Errorf("unsafe repository path %q", raw)
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// generatedRule is the Go convention: a "// Code generated ... DO NOT EDIT."
// comment before the package clause.
var generatedRule = ast.IsGenerated

// Generated reports whether a file carries the standard generated-code
// header and is therefore excluded from the hand-written budget.
func Generated(path string, source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), path, source,
		parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		// A file that does not parse is hand-written enough to be counted:
		// the budget gate must fail on it rather than skip it.
		return false
	}
	return generatedRule(file)
}

func git(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	var stdout, stderr boundedOutput
	stdout.limit = outputLimit
	stderr.limit = 4096
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stdout.exceeded {
			return nil, fmt.Errorf("git %s: output exceeds %d bytes", args[0], outputLimit)
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(stderr.data)))
	}
	if stdout.exceeded {
		return nil, fmt.Errorf("git %s: output exceeds %d bytes", args[0], outputLimit)
	}
	return stdout.data, nil
}

func splitNUL(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			out = append(out, string(part))
		}
	}
	return out
}

type boundedOutput struct {
	data     []byte
	limit    int
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if n > remaining {
			p = p[:remaining]
			b.exceeded = true
		}
		b.data = append(b.data, p...)
	} else if n > 0 {
		b.exceeded = true
	}
	return n, nil
}
