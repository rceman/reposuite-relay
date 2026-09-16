// Command gofmt-struct applies RepoSuite Relay's structural Go formatting
// rule: multi-field keyed struct literals are written one field per line,
// then the file is gofmt'd. Maps, arrays, slices, unkeyed literals, and
// single-field literals are left alone.
//
// Usage:
//
//	gofmt-struct --check <path...>   # list files needing formatting; non-zero exit
//	gofmt-struct --write <path...>   # rewrite files in place
//
// Paths may be files or directories (walked recursively, skipping .git and
// vendor). Formatting is package-aware: a file is classified using its
// sibling files and imported package export data.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/rceman/reposuite-relay/tools/internal/gofmtstruct"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("gofmt-struct", flag.ContinueOnError)
	flags.SetOutput(errOut)
	check := flags.Bool("check", false, "check canonical structural formatting without changing files")
	write := flags.Bool("write", false, "rewrite files using canonical structural formatting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *check == *write {
		return errors.New("exactly one of --check or --write is required")
	}
	paths := flags.Args()
	if len(paths) == 0 {
		return errors.New("at least one Go file or directory is required")
	}
	files, err := goFiles(paths)
	if err != nil {
		return err
	}
	changed := make([]string, 0)
	byDirectory := make(map[string][]string)
	for _, path := range files {
		path = filepath.Clean(path)
		byDirectory[filepath.Dir(path)] = append(byDirectory[filepath.Dir(path)], path)
	}
	directories := make([]string, 0, len(byDirectory))
	for directory := range byDirectory {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	for _, directory := range directories {
		packageSources, err := directoryGoSources(directory)
		if err != nil {
			return err
		}
		formatted, err := gofmtstruct.FormatPackage(packageSources)
		if err != nil {
			return fmt.Errorf("format package %s: %w", directory, err)
		}
		for _, path := range byDirectory[directory] {
			source, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			canonical, ok := formatted[path]
			if !ok {
				return fmt.Errorf("format package %s did not return %s", directory, path)
			}
			if bytes.Equal(source, canonical) {
				continue
			}
			changed = append(changed, path)
			if *write {
				if err := os.WriteFile(path, canonical, 0o644); err != nil {
					return fmt.Errorf("write %s: %w", path, err)
				}
			}
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 && *check {
		for _, path := range changed {
			fmt.Fprintln(out, path)
		}
		return fmt.Errorf("%d file(s) need canonical keyed-struct formatting", len(changed))
	}
	return nil
}

func directoryGoSources(directory string) (map[string][]byte, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	sources := make(map[string][]byte)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		sources[filepath.Clean(path)] = source
	}
	return sources, nil
}

func goFiles(paths []string) ([]string, error) {
	files := make([]string, 0)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if filepath.Ext(path) == ".go" {
				files = append(files, filepath.Clean(path))
			}
			continue
		}
		err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			if !entry.IsDir() && filepath.Ext(current) == ".go" {
				files = append(files, filepath.Clean(current))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}
