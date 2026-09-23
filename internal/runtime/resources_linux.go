//go:build linux

package runtime

import (
	"os"
	"strconv"
	"strings"
)

// TreeResources is a best-effort aggregate process-tree measurement.
type TreeResources struct {
	PSSBytes     int64
	RSSBytes     int64
	ProcessCount int
}

// SampleTreeResources aggregates PSS/RSS over the process tree rooted at
// pid, discovered via /proc/<pid>/task/<pid>/children and measured via
// /proc/<pid>/smaps_rollup. procRoot is injectable for tests.
//
// Best-effort: returns ok=false only when the ROOT cannot be measured —
// a missing/unreadable/malformed root rollup is unavailable, never zero.
// Descendants that vanish or read malformed mid-traversal are skipped.
// ProcessCount counts successfully measured processes only — the PIDs
// actually represented in the PSS/RSS aggregate, not merely discovered.
// The PID-seen set prevents duplicate/cyclic accounting.
func SampleTreeResources(procRoot string, pid int) (TreeResources, bool) {
	var out TreeResources
	rootPSS, rootRSS, ok := smapsRollup(procRoot, pid)
	if !ok {
		return out, false
	}
	out.PSSBytes, out.RSSBytes = rootPSS*1024, rootRSS*1024
	out.ProcessCount = 1

	seen := map[int]bool{pid: true}
	queue := procChildren(procRoot, pid)
	for _, k := range queue {
		seen[k] = true
	}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if pss, rss, ok := smapsRollup(procRoot, p); ok {
			out.PSSBytes += pss * 1024
			out.RSSBytes += rss * 1024
			out.ProcessCount++
		}
		for _, k := range procChildren(procRoot, p) {
			if !seen[k] {
				seen[k] = true
				queue = append(queue, k)
			}
		}
	}
	return out, true
}

func procChildren(procRoot string, pid int) []int {
	b, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) +
		"/task/" + strconv.Itoa(pid) + "/children")
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if v, err := strconv.Atoi(f); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// smapsRollup reads PSS and RSS KiB from one process's rollup. Both
// fields are required with a valid non-negative integer KiB value —
// a missing, malformed, or negative field makes the measurement
// unavailable (ok=false), never a false zero. A genuine "0 kB" is a
// valid measured zero.
func smapsRollup(procRoot string, pid int) (pssKiB, rssKiB int64, ok bool) {
	b, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/smaps_rollup")
	if err != nil {
		return 0, 0, false
	}
	var gotPSS, gotRSS bool
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[2] != "kB" {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || v < 0 {
			continue
		}
		switch f[0] {
		case "Pss:":
			pssKiB, gotPSS = v, true
		case "Rss:":
			rssKiB, gotRSS = v, true
		}
	}
	return pssKiB, rssKiB, gotPSS && gotRSS
}
