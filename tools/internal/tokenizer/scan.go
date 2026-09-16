package tokenizer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/rceman/reposuite-relay/tools/internal/gofiles"
)

// FileCount is one counted hand-written source file.
type FileCount struct {
	Path   string
	Tokens int
	Bytes  int
}

// Report is a complete token audit of a file set.
type Report struct {
	// Files is every counted file, sorted by path.
	Files []FileCount
	// Offending is the violating files, worst first (tokens desc, path asc).
	Offending []FileCount
	// Max is the largest counted file.
	Max FileCount
	// Skipped lists generated files that were excluded from the budget.
	Skipped []FileCount
	// MaxTokens is the budget the report was produced against.
	MaxTokens int
	// CountAboveMax is len(Offending), kept explicit for report consumers.
	CountAboveMax int

	TotalTokens int
	TotalBytes  int64
}

// ScanOptions selects what a scan counts.
type ScanOptions struct {
	// Root is the repository root used to resolve relative paths.
	Root string
	// Paths are repository-relative Go files to count.
	Paths []string
	// Max is the per-file token budget (0 means MaxTokens).
	Max int
	// Workers bounds concurrent reads and counting (0 means DefaultWorkers).
	Workers int
}

// DefaultWorkers is the measured worker policy for repository scans.
//
// tiktoken-go's encoder is a read-only shared structure whose regexp
// runner is pooled per match, so counting is genuinely parallel. The
// repository scan is dominated by BPE merges (CPU), not by the payload
// load, so a bounded worker pool is used; it is capped well below the
// number of files to keep memory and scheduler pressure predictable on
// large trees.
func DefaultWorkers() int {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// Scan counts every path and returns a deterministic report. Generated
// files are excluded from the budget but reported separately, and unreadable
// or non-UTF-8 files fail the scan closed.
func (c *Counter) Scan(ctx context.Context, opts ScanOptions) (Report, error) {
	max := opts.Max
	if max <= 0 {
		max = MaxTokens
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = DefaultWorkers()
	}
	if workers > len(opts.Paths) {
		workers = len(opts.Paths)
	}
	report := Report{MaxTokens: max}
	if len(opts.Paths) == 0 {
		return report, nil
	}

	counted := make([]FileCount, len(opts.Paths))
	skipped := make([]FileCount, len(opts.Paths))
	indexes := make([]int, len(opts.Paths))
	for i := range indexes {
		indexes[i] = i
	}

	var (
		mu       sync.Mutex
		failures []error
	)
	work := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				path := opts.Paths[index]
				full := filepath.Join(opts.Root, filepath.FromSlash(path))
				data, err := os.ReadFile(full)
				if err != nil {
					mu.Lock()
					failures = append(failures, fmt.Errorf("read %s: %w", path, err))
					mu.Unlock()
					continue
				}
				if gofiles.Generated(path, data) {
					skipped[index] = FileCount{
						Path:  path,
						Bytes: len(data),
					}
					continue
				}
				tokens, err := c.CountText(data)
				if err != nil {
					mu.Lock()
					failures = append(failures, fmt.Errorf("count %s: %w", path, err))
					mu.Unlock()
					continue
				}
				counted[index] = FileCount{
					Path:   path,
					Tokens: tokens,
					Bytes:  len(data),
				}
			}
		}()
	}
	for _, index := range indexes {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return Report{}, ctx.Err()
		case work <- index:
		}
	}
	close(work)
	wg.Wait()
	if len(failures) > 0 {
		sort.Slice(failures, func(i, j int) bool { return failures[i].Error() < failures[j].Error() })
		return Report{}, errors.Join(failures...)
	}

	for i, file := range counted {
		if file.Path == "" {
			if skipped[i].Path != "" {
				report.Skipped = append(report.Skipped, skipped[i])
			}
			continue
		}
		report.Files = append(report.Files, file)
		report.TotalTokens += file.Tokens
		report.TotalBytes += int64(file.Bytes)
		if file.Tokens > report.Max.Tokens {
			report.Max = file
		}
		if file.Tokens > max {
			report.Offending = append(report.Offending, file)
		}
	}
	sort.Slice(report.Files, func(i, j int) bool { return report.Files[i].Path < report.Files[j].Path })
	sort.Slice(report.Offending, func(i, j int) bool {
		if report.Offending[i].Tokens != report.Offending[j].Tokens {
			return report.Offending[i].Tokens > report.Offending[j].Tokens
		}
		return report.Offending[i].Path < report.Offending[j].Path
	})
	sort.Slice(report.Skipped, func(i, j int) bool { return report.Skipped[i].Path < report.Skipped[j].Path })
	report.CountAboveMax = len(report.Offending)
	return report, nil
}
