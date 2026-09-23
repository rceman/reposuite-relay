// Deterministic tests — zero provider processes.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSummarizeDistribution(t *testing.T) {
	// Sorted: 1..10 — p50 linear-interpolates between ranks.
	st := summarize([]float64{10, 5, 1, 9, 3, 7, 2, 8, 4, 6})
	if st.N != 10 || st.Min != 1 || st.Max != 10 {
		t.Fatalf("bounds: %+v", st)
	}
	if st.P50 != 5.5 {
		t.Fatalf("p50 = %v, want 5.5", st.P50)
	}
	if st.Mean != 5.5 {
		t.Fatalf("mean = %v", st.Mean)
	}
	if st.P90 <= st.P50 || st.P95 <= st.P90 {
		t.Fatalf("percentile ordering broken: %+v", st)
	}
	if st.StdDev <= 0 {
		t.Fatalf("stddev = %v", st.StdDev)
	}
	// Single sample: all percentiles equal the value.
	one := summarize([]float64{42})
	if one.P50 != 42 || one.P95 != 42 || one.StdDev != 0 {
		t.Fatalf("single: %+v", one)
	}
	if (summarize(nil)).N != 0 {
		t.Fatal("empty input must produce N=0")
	}
}

// fakeProc builds a /proc-like tree: pid/children + smaps_rollup.
func fakeProc(t *testing.T, root string, tree map[int][]int, mem map[int][2]int64) {
	t.Helper()
	for pid, kids := range tree {
		dir := filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		for _, k := range kids {
			sb.WriteString(strconv.Itoa(k) + " ")
		}
		if err := os.WriteFile(filepath.Join(dir, "children"), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, m := range mem {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("Rss:               %d kB\nPss:               %d kB\n", m[1], m[0])
		if err := os.WriteFile(filepath.Join(dir, "smaps_rollup"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTreePIDsAndMemory(t *testing.T) {
	root := t.TempDir()
	fakeProc(t, root,
		map[int][]int{100: {200, 300}, 200: {400}},
		map[int][2]int64{100: {1024, 4096}, 200: {2048, 8192}, 300: {512, 2048}, 400: {256, 1024}})
	pids := treePIDs(root, 100)
	if len(pids) != 4 {
		t.Fatalf("tree = %v", pids)
	}
	m := sampleTreeMemory(root, 100)
	if !m.Available {
		t.Fatal("memory unavailable")
	}
	if m.RootPSSKiB != 1024 || m.RootRSSKiB != 4096 {
		t.Fatalf("root mem: %+v", m)
	}
	if m.TreePSSKiB != 1024+2048+512+256 {
		t.Fatalf("tree PSS = %d", m.TreePSSKiB)
	}
	// A missing leaf's children file must not break traversal.
	fakeProc(t, root, map[int][]int{7: {8}}, nil)
	pids = treePIDs(root, 7)
	if len(pids) != 2 {
		t.Fatalf("partial tree = %v", pids)
	}
}

func TestCSVOutputHasNoRawIdentity(t *testing.T) {
	res := Result{Scenarios: map[string][]Sample{
		"TEST": {{Scenario: "TEST", Iteration: 1, Total: 12.3, PSSTreeMiB: 4.5}},
	}}
	path := filepath.Join(t.TempDir(), "out.csv")
	if err := writeCSV(path, res); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "scenario,iteration") || !strings.Contains(s, "12.30") {
		t.Fatalf("csv content: %q", s)
	}
	for _, banned := range []string{"nativeSessionId", "thread-", "api.token", "Bearer"} {
		if strings.Contains(s, banned) {
			t.Fatalf("csv leaked %q", banned)
		}
	}
}

func TestFailedSampleIsRetained(t *testing.T) {
	// Failure policy: a failed measured iteration appears in output
	// with Err set — never silently dropped.
	s := Sample{
		Scenario:  "X",
		Iteration: 3,
		Err:       "spawn: nope",
	}
	if s.Err == "" {
		t.Fatal("failed sample lost its error")
	}
	vals := fieldsOf([]Sample{s, {Iteration: 4, Total: 5}}, func(x Sample) float64 { return x.Total })
	if len(vals) != 1 || vals[0] != 5 {
		t.Fatalf("errored sample must be excluded from stats, kept in rows: %v", vals)
	}
}

func TestFingerprintSanitization(t *testing.T) {
	// The committed identity representation is sha256[:12] hex of the
	// raw native id — the raw value itself is never emitted.
	fp := fingerprintOf("native-secret-id")
	if len(fp) != 12 {
		t.Fatalf("fingerprint len = %d", len(fp))
	}
	if strings.Contains(fp, "native-secret-id") {
		t.Fatal("fingerprint leaks raw identity")
	}
	if fingerprintOf("native-secret-id") != fp {
		t.Fatal("fingerprint must be deterministic")
	}
}

func TestIdentityFileSecurity(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "id.json")
	if err := os.WriteFile(good, []byte(`{"id":"native-x","cwd":"/tmp/w"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := readIdentityFile(good)
	if err != nil || id.ID != "native-x" || id.Cwd != "/tmp/w" {
		t.Fatalf("read: %v %+v", err, id)
	}
	// Group/world-readable file must be refused.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"id":"x","cwd":"/tmp"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readIdentityFile(bad); err == nil {
		t.Fatal("0644 identity file must be refused")
	}
	// Missing fields must be refused.
	for body, name := range map[string]string{
		`{"id":"","cwd":"/tmp"}`: "empty id",
		`{"id":"x"}`:             "empty cwd",
		`not json`:               "malformed",
	} {
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readIdentityFile(p); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}
