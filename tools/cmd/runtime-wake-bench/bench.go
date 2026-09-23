// Benchmark scenarios. ZERO MODEL TURNS: this file calls only process
// spawn, initialize, thread/start (setup only, no turn), thread/resume.
// turn/start and session/prompt are never invoked anywhere in this tool.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
)

const procRoot = "/proc"

// runScenario executes warmups + measured iterations for one scenario.
// fn performs ONE iteration: it must fully reap its process tree before
// returning — overlapping runtimes would invalidate the measurements.
func runScenario(name string, warmups, samples int, out map[string][]Sample, fn func(iter int, measured bool) Sample) {
	total := warmups + samples
	for i := 0; i < total; i++ {
		measured := i >= warmups
		s := fn(i, measured)
		if measured {
			s.Scenario, s.Iteration = name, i-warmups+1
			out[name] = append(out[name], s)
			status := "ok"
			if s.Err != "" {
				status = "ERR: " + s.Err
			}
			fmt.Printf("%-22s it=%-3d total=%8.1fms pssTree=%7.1fMiB shutdown=%7.1fms %s\n",
				name, s.Iteration, s.Total, s.PSSTreeMiB, s.Shutdown, status)
		}
		if s.Err != "" {
			fmt.Printf("%s: failed iteration kept as degraded sample — stopping scenario\n", name)
			break
		}
	}
}

// reapWait waits until the server process is gone, bounded.
func reapWait(wait <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-wait:
		return true
	case <-time.After(timeout):
		return false
	}
}

// codexIteration runs one codex app-server lifecycle. resumeID non-empty
// adds the exact thread/resume stage (CODEX_EXACT_RESUME).
func codexIteration(cmd codex.Command, cwd, resumeID string) Sample {
	var s Sample
	t0 := time.Now()
	srv, err := codex.StartServer(cmd, cwd)
	if err != nil {
		s.Err = "spawn: " + err.Error()
		return s
	}
	t1 := time.Now()
	s.ProcessStart = ms(t1.Sub(t0))
	go func() { _ = srv.Serve() }()

	ctx, cancel := callCtx()
	defer cancel()
	if _, err := srv.Initialize(ctx, codex.ClientInfo{
		Name: "reposuite-relay-wake-bench", Title: "RepoSuite Relay wake bench", Version: "0.0.0",
	}); err != nil {
		s.Err = "initialize: " + err.Error()
		stopAndTime(srv, &s)
		return s
	}
	t2 := time.Now()
	s.Initialize = ms(t2.Sub(t1))
	t3 := t2
	if resumeID != "" {
		rres, err := srv.ThreadResume(ctx, codex.ThreadResumeParams{
			ThreadID: resumeID, Cwd: cwd,
		})
		if err != nil {
			s.Err = "thread/resume: " + err.Error()
			stopAndTime(srv, &s)
			return s
		}
		t3 = time.Now()
		s.Attach = ms(t3.Sub(t2))
		if rres.Thread.ID != resumeID {
			s.Err = "thread/resume returned a different thread identity"
			stopAndTime(srv, &s)
			return s
		}
	}
	s.Total = ms(t3.Sub(t0))

	// READY + 1s canonical memory sample (benchmark stabilization delay).
	time.Sleep(time.Second)
	mem := sampleTreeMemory(procRoot, srv.PID())
	s.PSSRootMiB, s.PSSTreeMiB = mib(mem.RootPSSKiB), mib(mem.TreePSSKiB)
	s.RSSRootMiB, s.RSSTreeMiB = mib(mem.RootRSSKiB), mib(mem.TreeRSSKiB)

	stopAndTime(srv, &s)
	// Verify the whole benchmark-owned tree is reaped.
	for _, p := range treePIDs(procRoot, srv.PID()) {
		if alive(p) && s.Err == "" {
			s.Err = fmt.Sprintf("process %d survived teardown", p)
		}
	}
	return s
}

func stopAndTime(srv *codex.Server, s *Sample) {
	t0 := time.Now()
	_ = srv.Stop()
	if !reapWait(srv.Wait(), 10*time.Second) {
		s.Err = "teardown did not reap process tree"
	}
	s.Shutdown = ms(time.Since(t0))
}

// setupCodexThread creates exactly one disposable zero-turn native
// thread for the resume series. No turn is ever started.
func setupCodexThread(cmd codex.Command, work string) (threadID, fingerprint string, err error) {
	srv, err := codex.StartServer(cmd, work)
	if err != nil {
		return "", "", err
	}
	go func() { _ = srv.Serve() }()
	ctx, cancel := callCtx()
	defer cancel()
	defer func() { _ = srv.Stop(); reapWait(srv.Wait(), 10*time.Second) }()
	if _, err := srv.Initialize(ctx, codex.ClientInfo{
		Name: "reposuite-relay-wake-bench", Title: "RepoSuite Relay wake bench", Version: "0.0.0",
	}); err != nil {
		return "", "", err
	}
	res, err := srv.ThreadStart(ctx, codex.ThreadStartParams{Cwd: work})
	if err != nil {
		return "", "", err
	}
	threadID = res.Thread.ID
	return threadID, fingerprintOf(threadID), nil
}

// fingerprintOf renders a native identity as sha256[:12] hex — the only
// committed representation; the raw ID never leaves process memory.
func fingerprintOf(raw string) string {
	fp := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(fp[:])[:12]
}

func runCodex(cfg *Config, out map[string][]Sample, work string, skipped map[string]string) {
	cmd, err := codex.DefaultCommand()
	if err != nil {
		skipped["CODEX_INIT"] = err.Error()
		skipped["CODEX_EXACT_RESUME"] = err.Error()
		return
	}
	cwd := filepath.Join(work, "codex")
	_ = os.MkdirAll(cwd, 0o700)

	runScenario("CODEX_INIT", cfg.warmups, cfg.samples, out,
		func(int, bool) Sample { return codexIteration(cmd, cwd, "") })

	threadID, fp, err := setupCodexThread(cmd, cwd)
	if err != nil {
		skipped["CODEX_EXACT_RESUME"] = "zero-turn thread setup: " + err.Error()
		return
	}
	fmt.Printf("codex disposable zero-turn thread fingerprint=%s (raw ID not stored)\n", fp)
	// Probe once that the zero-turn thread resumes before the series.
	probe := codexIteration(cmd, cwd, threadID)
	if probe.Err != "" {
		skipped["CODEX_EXACT_RESUME"] = "zero-turn thread/resume rejected: " + probe.Err
		return
	}
	runScenario("CODEX_EXACT_RESUME", cfg.warmups, cfg.samples, out,
		func(int, bool) Sample { return codexIteration(cmd, cwd, threadID) })
}

// devinIteration runs one devin acp lifecycle. loadID non-empty adds
// the exact session/load stage plus Relay's attach-mode configuration
// (bypass mode) — DEVIN_EXACT_LOAD. session/prompt is never invoked.
func devinIteration(cmd acp.Command, cwd, loadID string) Sample {
	var s Sample
	t0 := time.Now()
	srv, err := acp.StartServer(cmd, cwd)
	if err != nil {
		s.Err = "spawn: " + err.Error()
		return s
	}
	t1 := time.Now()
	s.ProcessStart = ms(t1.Sub(t0))
	go func() { _ = srv.Serve() }()
	srv.Configure(acp.ServerConfig{
		Client: acp.ClientInfo{Name: "reposuite-relay-wake-bench", Title: "RepoSuite Relay wake bench", Version: "0.0.0"},
	})
	ctx, cancel := callCtx()
	defer cancel()
	if _, err := srv.Initialize(ctx, acp.ClientInfo{
		Name: "reposuite-relay-wake-bench", Title: "RepoSuite Relay wake bench", Version: "0.0.0",
	}); err != nil {
		s.Err = "initialize: " + err.Error()
		t := time.Now()
		_ = srv.Stop()
		reapWait(srv.Wait(), 10*time.Second)
		s.Shutdown = ms(time.Since(t))
		return s
	}
	t2 := time.Now()
	s.Initialize = ms(t2.Sub(t1))
	t3 := t2
	if loadID != "" {
		h, err := acp.AttachSession(ctx, srv, cwd, loadID)
		if err != nil {
			s.Err = "session/load: " + err.Error()
			stopAndTimeACP(srv, &s)
			return s
		}
		t4 := time.Now()
		s.Attach = ms(t4.Sub(t2))
		// Mirror Relay's attach config: select the advertised bypass
		// mode when it exists and is not already current.
		if opt, ok := acp.FindOption(h.Options(), acp.CategoryMode, "mode"); ok {
			if h.CurrentMode() != "bypass" && optionHas(opt, "bypass") {
				if _, err := h.SetConfigValue(ctx, opt.ID, "bypass", false); err != nil {
					s.Err = "set mode: " + err.Error()
					stopAndTimeACP(srv, &s)
					return s
				}
			}
		}
		t3 = time.Now()
		s.Configure = ms(t3.Sub(t4))
	}
	s.Total = ms(t3.Sub(t0))

	time.Sleep(time.Second)
	mem := sampleTreeMemory(procRoot, srv.PID())
	s.PSSRootMiB, s.PSSTreeMiB = mib(mem.RootPSSKiB), mib(mem.TreePSSKiB)
	s.RSSRootMiB, s.RSSTreeMiB = mib(mem.RootRSSKiB), mib(mem.TreeRSSKiB)

	stopAndTimeACP(srv, &s)
	for _, p := range treePIDs(procRoot, srv.PID()) {
		if alive(p) && s.Err == "" {
			s.Err = fmt.Sprintf("process %d survived teardown", p)
		}
	}
	return s
}

func stopAndTimeACP(srv *acp.Server, s *Sample) {
	t := time.Now()
	_ = srv.Stop()
	if !reapWait(srv.Wait(), 10*time.Second) {
		s.Err = "teardown did not reap process tree"
	}
	s.Shutdown = ms(time.Since(t))
}

// optionHas reports whether an advertised config option offers value v.
func optionHas(opt acp.ConfigOption, v string) bool {
	for _, o := range opt.SelectOptions() {
		if o.Value == v {
			return true
		}
	}
	return false
}

func runDevin(cfg *Config, out map[string][]Sample, work string, skipped map[string]string) {
	path, err := lookDevin()
	if err != nil {
		skipped["DEVIN_INIT"] = err.Error()
		return
	}
	cwd := filepath.Join(work, "devin")
	_ = os.MkdirAll(cwd, 0o700)
	cmd := acp.Command{Path: path, Args: []string{"acp"}}
	runScenario("DEVIN_INIT", cfg.warmups, cfg.samples, out,
		func(int, bool) Sample { return devinIteration(cmd, cwd, "") })
	skipped["DEVIN_SESSION_LOAD"] = "NOT_MEASURED_ZERO_MODEL_CONSTRAINT"
	skipped["DEVIN_SESSION_NEW_ONE_OFF"] = "skipped: uncertain one-off semantics; primary metric is process+initialize"
}

func lookDevin() (string, error) {
	path, err := exec.LookPath("devin")
	if err != nil {
		return "", fmt.Errorf("devin executable not found")
	}
	return path, nil
}
