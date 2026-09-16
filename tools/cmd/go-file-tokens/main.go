// Command go-file-tokens enforces the hand-written Go file budget.
//
// Every tracked or untracked, non-ignored hand-written *.go file must stay
// at or below the o200k_base token budget (default 3000), counting the
// complete file: code, comments, strings, and tests. Generated files
// carrying the standard "// Code generated ... DO NOT EDIT." header are
// excluded.
//
// Usage:
//
//	go-file-tokens [--all | --base REV | --files-from -] [--root DIR]
//	               [--max N] [--verbose] [--json]
//
// Exit status is non-zero when any file violates the budget, when a file
// cannot be counted, or when the file set cannot be enumerated.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/rceman/reposuite-relay/tools/internal/gofiles"
	"github.com/rceman/reposuite-relay/tools/internal/tokenizer"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "go-file-tokens: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	all       bool
	base      string
	filesFrom string
	root      string
	max       int
	verbose   bool
	json      bool
	workers   int
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("go-file-tokens", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.BoolVar(&opts.all, "all", false, "scan every tracked and untracked non-ignored Go file")
	flags.StringVar(&opts.base, "base", "", "scan Go files changed against this revision, plus untracked files")
	flags.StringVar(&opts.filesFrom, "files-from", "", "read the Go file list from this file ('-' for stdin)")
	flags.StringVar(&opts.root, "root", "", "repository root (default: the git toplevel of the working directory)")
	flags.IntVar(&opts.max, "max", tokenizer.MaxTokens, "per-file token budget")
	flags.BoolVar(&opts.verbose, "verbose", false, "print every counted file, not just violations")
	flags.BoolVar(&opts.json, "json", false, "print a machine-readable report")
	flags.IntVar(&opts.workers, "workers", 0, "concurrent counting workers (0 = measured default)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	selected := 0
	for _, set := range []bool{opts.all, opts.base != "", opts.filesFrom != ""} {
		if set {
			selected++
		}
	}
	if selected > 1 {
		return errors.New("choose one of --all, --base, or --files-from")
	}

	ctx := context.Background()
	root, err := resolveRoot(ctx, opts.root)
	if err != nil {
		return err
	}
	paths, err := selectPaths(ctx, root, opts, stdin)
	if err != nil {
		return err
	}

	counter := tokenizer.NewCounter()
	report, err := counter.Scan(ctx, tokenizer.ScanOptions{
		Root:    root,
		Paths:   paths,
		Max:     opts.max,
		Workers: opts.workers,
	})
	if err != nil {
		return err
	}
	if opts.json {
		return writeJSON(stdout, report)
	}
	writeText(stdout, stderr, report, opts)
	if len(report.Offending) > 0 {
		return fmt.Errorf("%d Go file(s) exceed %d %s tokens",
			len(report.Offending), report.MaxTokens, tokenizer.EncodingName)
	}
	return nil
}

func selectPaths(ctx context.Context, root string, opts options, stdin io.Reader) ([]string, error) {
	switch {
	case opts.filesFrom != "":
		return readPathList(opts.filesFrom, stdin)
	case opts.all || opts.base == "":
		return gofiles.List(ctx, gofiles.Options{Root: root})
	default:
		return gofiles.List(ctx, gofiles.Options{Root: root, Base: opts.base})
	}
}

func readPathList(source string, stdin io.Reader) ([]string, error) {
	reader := stdin
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return nil, fmt.Errorf("open file list: %w", err)
		}
		defer file.Close()
		reader = file
	}
	var paths []string
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		paths = append(paths, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read file list: %w", err)
	}
	return gofiles.Normalize(paths)
}

func resolveRoot(ctx context.Context, given string) (string, error) {
	if strings.TrimSpace(given) != "" {
		return given, nil
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func writeText(stdout, stderr io.Writer, report tokenizer.Report, opts options) {
	if opts.verbose {
		files := make([]tokenizer.FileCount, len(report.Files))
		copy(files, report.Files)
		sortByTokens(files)
		for _, file := range files {
			fmt.Fprintf(stdout, "%d\t%s\n", file.Tokens, file.Path)
		}
	}
	for _, file := range report.Offending {
		fmt.Fprintf(stdout, "%d\t%s\t(+%d)\n", file.Tokens, file.Path, file.Tokens-report.MaxTokens)
	}
	scope := "full tree"
	if opts.base != "" {
		scope = "changed against " + opts.base
	} else if opts.filesFrom != "" {
		scope = "selected files"
	}
	fmt.Fprintf(stderr, "go-file-tokens: %s: %d files, %d tokens, %d bytes, max %d/%d, %d violation(s)\n",
		scope, len(report.Files), report.TotalTokens, report.TotalBytes,
		report.Max.Tokens, report.MaxTokens, len(report.Offending))
	if report.Max.Path != "" {
		fmt.Fprintf(stderr, "go-file-tokens: largest %s (%d tokens)\n", report.Max.Path, report.Max.Tokens)
	}
	if len(report.Skipped) > 0 {
		fmt.Fprintf(stderr, "go-file-tokens: %d generated file(s) excluded\n", len(report.Skipped))
	}
}

func writeJSON(stdout io.Writer, report tokenizer.Report) error {
	payload := struct {
		Encoding    string                `json:"encoding"`
		MaxTokens   int                   `json:"maxTokens"`
		Files       []tokenizer.FileCount `json:"files"`
		Offending   []tokenizer.FileCount `json:"offending"`
		Skipped     []tokenizer.FileCount `json:"skipped"`
		TotalTokens int                   `json:"totalTokens"`
		TotalBytes  int64                 `json:"totalBytes"`
	}{
		Encoding:    tokenizer.EncodingName,
		MaxTokens:   report.MaxTokens,
		Files:       report.Files,
		Offending:   report.Offending,
		Skipped:     report.Skipped,
		TotalTokens: report.TotalTokens,
		TotalBytes:  report.TotalBytes,
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func sortByTokens(files []tokenizer.FileCount) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Tokens != files[j].Tokens {
			return files[i].Tokens > files[j].Tokens
		}
		return files[i].Path < files[j].Path
	})
}
