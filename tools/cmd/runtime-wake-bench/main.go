// runtime-wake-bench measures REAL provider runtime cold-start and
// exact-reattach cost with ZERO model turns. It never invokes
// turn/start (Codex) or session/prompt (Devin) — see bench.go.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Config struct {
	codex, devin, yes bool
	samples, warmups  int
	csv, jsonOut      string
}

type Sample struct {
	Scenario     string  `json:"scenario"`
	Iteration    int     `json:"iteration"`
	ProcessStart float64 `json:"processStartMs"`
	Initialize   float64 `json:"initializeMs"`
	Attach       float64 `json:"attachMs"`
	Total        float64 `json:"totalMs"`
	PSSRootMiB   float64 `json:"pssRootMiB"`
	PSSTreeMiB   float64 `json:"pssTreeMiB"`
	RSSRootMiB   float64 `json:"rssRootMiB"`
	RSSTreeMiB   float64 `json:"rssTreeMiB"`
	Shutdown     float64 `json:"shutdownMs"`
	Err          string  `json:"err,omitempty"`
}

type Result struct {
	Scenarios map[string][]Sample `json:"scenarios"`
	Env       map[string]string   `json:"env"`
	Warmups   int                 `json:"warmups"`
	Samples   int                 `json:"samples"`
	Skipped   map[string]string   `json:"skipped,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func mib(kib int64) float64 { return float64(kib) / 1024 }

func main() {
	var cfg Config
	var all bool
	flag.BoolVar(&cfg.codex, "codex", false, "benchmark real codex app-server")
	flag.BoolVar(&cfg.devin, "devin", false, "benchmark real devin acp")
	flag.BoolVar(&all, "all", false, "benchmark all providers")
	flag.IntVar(&cfg.samples, "samples", 30, "measured iterations per scenario")
	flag.IntVar(&cfg.warmups, "warmups", 3, "warm-up iterations per scenario")
	flag.BoolVar(&cfg.yes, "yes", false, "non-interactive confirmation")
	flag.StringVar(&cfg.csv, "csv", "", "write per-sample CSV to path")
	flag.StringVar(&cfg.jsonOut, "json", "", "write JSON results to path")
	flag.Parse()
	if all {
		cfg.codex, cfg.devin = true, true
	}
	if !cfg.codex && !cfg.devin {
		fmt.Fprintln(os.Stderr, "no provider selected (use --codex/--devin/--all)")
		os.Exit(2)
	}
	fmt.Println("REAL PROVIDER PROCESSES WILL BE STARTED")
	fmt.Println("ZERO MODEL TURNS WILL BE SENT")
	if !cfg.yes {
		if isTTY() {
			fmt.Print("Proceed? [y/N] ")
			var ans string
			_, _ = fmt.Fscanln(bufio.NewReader(os.Stdin), &ans)
			if ans != "y" && ans != "yes" {
				fmt.Println("aborted")
				os.Exit(1)
			}
		} else {
			fmt.Fprintln(os.Stderr, "non-interactive execution requires --yes")
			os.Exit(2)
		}
	}
	if cfg.samples < 1 || cfg.warmups < 0 {
		fmt.Fprintln(os.Stderr, "invalid --samples/--warmups")
		os.Exit(2)
	}

	res := Result{
		Scenarios: map[string][]Sample{},
		Env:       hostEnv(),
		Warmups:   cfg.warmups,
		Samples:   cfg.samples,
		Skipped:   map[string]string{},
	}
	work, err := os.MkdirTemp("", "wake-bench-")
	if err != nil {
		fatal(err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	fmt.Printf("load before: %s\n", loadAvg())
	if cfg.codex {
		if _, err := exec.LookPath("codex"); err != nil {
			res.Skipped["CODEX"] = "executable not found"
		} else {
			runCodex(&cfg, res.Scenarios, work, res.Skipped)
		}
	}
	if cfg.devin {
		if _, err := exec.LookPath("devin"); err != nil {
			res.Skipped["DEVIN"] = "executable not found"
		} else {
			runDevin(&cfg, res.Scenarios, work, res.Skipped)
		}
	}
	fmt.Printf("load after:  %s\n", loadAvg())

	if cfg.csv != "" {
		if err := writeCSV(cfg.csv, res); err != nil {
			fatal(err)
		}
	}
	if cfg.jsonOut != "" {
		if err := writeJSON(cfg.jsonOut, res); err != nil {
			fatal(err)
		}
	}
	printSummary(res)
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "FAIL:", err); os.Exit(1) }

func loadAvg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable"
	}
	return strings.Join(strings.Fields(string(b))[:3], " ")
}

func hostEnv() map[string]string {
	env := map[string]string{
		"os": runtime.GOOS, "arch": runtime.GOARCH,
		"logicalCPUs": fmt.Sprint(runtime.NumCPU()),
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				env["cpuModel"] = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "MemTotal") {
				env["memTotal"] = strings.Join(strings.Fields(l)[1:], " ")
				break
			}
		}
	}
	for _, p := range []string{"codex", "devin"} {
		if path, err := exec.LookPath(p); err == nil {
			if out, err := exec.Command(path, "--version").CombinedOutput(); err == nil {
				env[p+"Version"] = strings.TrimSpace(string(out))
			}
		} else {
			env[p+"Version"] = "not found"
		}
	}
	return env
}

// callCtx bounds every single native RPC.
func callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
