// Process-tree discovery and smaps_rollup memory aggregation.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// children reads /proc/<pid>/task/<pid>/children. procRoot makes the path
// injectable for tests.
func children(procRoot string, pid int) ([]int, error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/task/%d/children", procRoot, pid, pid))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if v, err := strconv.Atoi(f); err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// treePIDs returns pid plus all discoverable descendants (DFS).
func treePIDs(procRoot string, pid int) []int {
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	var out []int
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		out = append(out, p)
		kids, err := children(procRoot, p)
		if err != nil {
			continue // dead or unreadable — treat as leaf
		}
		for _, k := range kids {
			if !seen[k] {
				seen[k] = true
				queue = append(queue, k)
			}
		}
	}
	return out
}

// MemSample aggregates smaps_rollup PSS/RSS for a process tree (KiB).
type MemSample struct {
	RootPSSKiB, RootRSSKiB int64
	TreePSSKiB, TreeRSSKiB int64
	Available              bool
}

func smapsRollup(procRoot string, pid int) (pss, rss int64, err error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/smaps_rollup", procRoot, pid))
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "Pss:":
			pss = v
		case "Rss:":
			rss = v
		}
	}
	return pss, rss, nil
}

func sampleTreeMemory(procRoot string, rootPID int) MemSample {
	var m MemSample
	pids := treePIDs(procRoot, rootPID)
	for i, p := range pids {
		pss, rss, err := smapsRollup(procRoot, p)
		if err != nil {
			continue
		}
		m.Available = true
		m.TreePSSKiB += pss
		m.TreeRSSKiB += rss
		if i == 0 {
			m.RootPSSKiB, m.RootRSSKiB = pss, rss
		}
	}
	return m
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
