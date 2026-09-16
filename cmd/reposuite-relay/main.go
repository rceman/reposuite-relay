// Command reposuite-relay is the RepoSuite Relay standalone CLI.
// Future umbrella form: `reposuite relay ...` dispatches here.
//
// The CLI is a local HTTP client of relayd (ADR-006): every managed
// command goes through internal/client — descriptor discovery, bearer
// auth, typed methods. Hidden internal modes `__daemon`, `__fixture`, and
// `__fake-codex` are implementation details of the single-binary design —
// never listed in help and not part of the public CLI contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/codex"
	"github.com/rceman/reposuite-relay/internal/daemon"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/version"
)

const usage = `reposuite-relay — RepoSuite Relay (persistent agent-session daemon)

Usage:
  reposuite-relay <command>

Commands:
  version                        print version
  help                           show this help
  paths                          print resolved RepoSuite/Relay state paths
  serve fixture --key <KEY>      start a fixture session
  serve codex --key <KEY>        create a Codex session (COLD)
  prompt <KEY> --text <TEXT>     submit a prompt as a native turn
  cancel <KEY>                   interrupt the in-flight turn
  input <KEY> --input <ID>       answer requested input
  config <KEY> [--model M] [--mode S]
                                 change accepted model/mode
  list                           list managed sessions
  status <KEY>                   show one session
  stop <KEY>                     stop a session
  daemon status                  show daemon status
  daemon stop                    stop the daemon
`

func main() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reposuite-relay:", err)
		os.Exit(1)
	}
	os.Exit(run(os.Args[1:], self))
}

// run dispatches the public CLI plus the two hidden internal modes.
func run(args []string, selfExe string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Printf("reposuite-relay %s\n", version.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "paths":
		p, err := paths.Current()
		if err != nil {
			return fail(err)
		}
		fmt.Printf("reposuite_root:  %s\n", p.RepoSuiteRoot())
		fmt.Printf("relay_root:      %s\n", p.RelayRoot())
		fmt.Printf("run_dir:         %s\n", p.RunDir())
		fmt.Printf("daemon_descriptor: %s\n", p.DaemonDescriptor())
		fmt.Printf("sessions_dir:    %s\n", p.SessionsDir())
		fmt.Printf("log_dir:         %s\n", p.LogDir())
	case "serve":
		return cmdServe(args[1:], selfExe)
	case "prompt":
		return cmdPrompt(args[1:], selfExe)
	case "cancel":
		return cmdCancel(args[1:], selfExe)
	case "input":
		return cmdInput(args[1:], selfExe)
	case "config":
		return cmdConfig(args[1:], selfExe)
	case "list":
		return cmdList(selfExe)
	case "status":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "reposuite-relay: status requires exactly one session key")
			return 2
		}
		return cmdStatus(selfExe, args[1])
	case "stop":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "reposuite-relay: stop requires exactly one session key")
			return 2
		}
		return cmdStop(selfExe, args[1])
	case "daemon":
		return cmdDaemon(args[1:])
	case "__daemon":
		return runDaemon(selfExe)
	case "__fixture":
		return fixture.RunChild()
	case "__fake-codex":
		// Deterministic fake Codex app-server (test seam; no model calls,
		// no network). Reached only by explicit spawn from tests.
		return codex.RunFakeAppServer(os.Stdin, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "reposuite-relay: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	return 0
}

// pathsForClient resolves state paths; errors exit non-zero before any
// daemon contact (relative/forbidden roots fail closed).
func pathsForClient() (paths.Paths, int, bool) {
	p, err := paths.Current()
	if err != nil {
		return paths.Paths{}, fail(err), false
	}
	return p, 0, true
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "reposuite-relay:", err)
	return 1
}

func failAPI(err error) int {
	var ae *client.APIError
	if errors.As(err, &ae) {
		fmt.Fprintf(os.Stderr, "reposuite-relay: %s: %s\n", ae.Code, ae.Message)
		return 1
	}
	return fail(err)
}

// ensureClient resolves paths and returns a live daemon client, starting
// relayd when it is genuinely absent.
func ensureClient(selfExe string) (*client.Client, int, bool) {
	p, code, ok := pathsForClient()
	if !ok {
		return nil, code, false
	}
	c, err := client.Ensure(p, selfExe)
	if err != nil {
		return nil, fail(err), false
	}
	return c, 0, true
}

// cmdServe implements `serve fixture --key <KEY>`.
func cmdServe(args []string, selfExe string) int {
	if len(args) == 0 || (args[0] != "fixture" && args[0] != "codex") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay serve fixture|codex --key <KEY> [--model M] [--mode S]")
		return 2
	}
	harness := args[0]
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	key := fs.String("key", "", "session key")
	model := fs.String("model", "", "model id (codex)")
	mode := fs.String("mode", "", "service tier / mode (codex)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay serve fixture|codex --key <KEY> [--model M] [--mode S]")
		return 2
	}
	if !session.ValidKey(*key) {
		fmt.Fprintf(os.Stderr, "reposuite-relay: invalid session key %q\n", *key)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resp api.SessionResponse
	if harness == "codex" {
		resp, err = c.CreateCodex(ctx, *key, cwd, *model, *mode)
	} else {
		resp, err = c.ServeFixture(ctx, *key, cwd)
	}
	if err != nil {
		return failAPI(err)
	}
	printSession(resp.Session)
	fmt.Printf("daemon_pid=%d\n", resp.Daemon.PID)
	return 0
}

// cmdPrompt submits a prompt as a native turn (codex sessions).
func cmdPrompt(args []string, selfExe string) int {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	text := fs.String("text", "", "prompt text")
	model := fs.String("model", "", "per-turn model override")
	effort := fs.String("effort", "", "per-turn reasoning effort override")
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 || *text == "" {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay prompt <KEY> --text <TEXT> [--model M] [--effort E]")
		return 2
	}
	key := fs.Arg(0)
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Prompt(ctx, key, *text, *model, *effort)
	if err != nil {
		return failAPI(err)
	}
	fmt.Printf("key=%s turn_id=%s native_thread_id=%s runtime_id=%s\n",
		resp.Key, resp.TurnID, resp.NativeThreadID, resp.RuntimeID)
	return 0
}

// cmdCancel interrupts the in-flight turn of a session.
func cmdCancel(args []string, selfExe string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay cancel <KEY>")
		return 2
	}
	key := fs.Arg(0)
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Cancel(ctx, key)
	if err != nil {
		return failAPI(err)
	}
	fmt.Printf("key=%s cancelled=1\n", resp.Key)
	return 0
}

// cmdInput answers a native requested-input request. Answers are supplied
// as QUESTION_ID=ANSWER pairs (repeatable).
func cmdInput(args []string, selfExe string) int {
	fs := flag.NewFlagSet("input", flag.ContinueOnError)
	inputID := fs.String("input", "", "requested input id")
	var pairs multiFlag
	fs.Var(&pairs, "answer", "questionId=answer (repeatable)")
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 || *inputID == "" || len(pairs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay input <KEY> --input <ID> --answer <QID>=<ANSWER> [--answer ...]")
		return 2
	}
	key := fs.Arg(0)
	answers := make([]api.InputAnswer, 0, len(pairs))
	for _, p := range pairs {
		qid, ans, found := strings.Cut(p, "=")
		if !found || qid == "" || ans == "" {
			fmt.Fprintf(os.Stderr, "reposuite-relay: bad answer %q, want questionId=answer\n", p)
			return 2
		}
		answers = append(answers, api.InputAnswer{QuestionID: qid, Answers: []string{ans}})
	}
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.AnswerInput(ctx, key, *inputID, answers)
	if err != nil {
		return failAPI(err)
	}
	fmt.Printf("key=%s input_id=%s answered=1\n", resp.Key, resp.InputID)
	return 0
}

// cmdConfig changes the accepted model/mode of a session.
func cmdConfig(args []string, selfExe string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	model := fs.String("model", "", "model id")
	mode := fs.String("mode", "", "service tier / mode")
	if err := fs.Parse(splitKeyFirst(args)); err != nil || fs.NArg() != 1 || (*model == "" && *mode == "") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay config <KEY> [--model M] [--mode S]")
		return 2
	}
	key := fs.Arg(0)
	var m, md *string
	if *model != "" {
		m = model
	}
	if *mode != "" {
		md = mode
	}
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.SetConfig(ctx, key, m, md)
	if err != nil {
		return failAPI(err)
	}
	printSession(resp.Session)
	return 0
}

// splitKeyFirst reorders `command <KEY> [flags]` into flag-first order:
// the stdlib flag package stops parsing at the first non-flag argument,
// and the documented CLI shape puts the key first.
func splitKeyFirst(args []string) []string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return append(append([]string{}, args[1:]...), args[0])
	}
	return args
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// cmdList implements `list` — ensures the daemon, lists durable sessions
// (COLD sessions included).
func cmdList(selfExe string) int {
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Sessions(ctx)
	if err != nil {
		return failAPI(err)
	}
	for i := range resp.Sessions {
		printSession(resp.Sessions[i])
	}
	fmt.Printf("daemon_pid=%d sessions=%d\n", resp.Daemon.PID, resp.Daemon.SessionCount)
	return 0
}

// cmdStatus implements `status <KEY>`.
func cmdStatus(selfExe, key string) int {
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Status(ctx, key)
	if err != nil {
		return failAPI(err)
	}
	printSession(resp.Session)
	fmt.Printf("daemon_pid=%d\n", resp.Daemon.PID)
	return 0
}

// cmdStop implements `stop <KEY>` — stop the session runtime and delete
// the durable RelaySession.
func cmdStop(selfExe, key string) int {
	c, code, ok := ensureClient(selfExe)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.StopSession(ctx, key); err != nil {
		return failAPI(err)
	}
	fmt.Printf("stopped key=%s\n", key)
	return 0
}

func printSession(s api.SessionInfo) {
	fmt.Printf("key=%s sessionId=%s runtimeId=%s runtimeState=%s harness=%s state=%s generation=%d pid=%d cwd=%s createdAt=%s generationStartedAt=%s\n",
		s.Key, s.SessionID, s.RuntimeID, s.RuntimeState, s.Harness, s.State,
		s.Generation, s.PID, s.Cwd, s.CreatedAt, s.GenerationStartedAt)
}

// cmdDaemon implements `daemon status|stop` — neither auto-starts a
// daemon.
func cmdDaemon(args []string) int {
	if len(args) != 1 || (args[0] != "status" && args[0] != "stop") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay daemon {status|stop}")
		return 2
	}
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	c, err := client.Dial(p)
	if err != nil {
		if errors.Is(err, client.ErrNotRunning) {
			fmt.Fprintln(os.Stderr, "reposuite-relay: daemon not running")
		} else {
			// Bad peer / incompatible endpoint is NOT "not running".
			fmt.Fprintln(os.Stderr, "reposuite-relay:", err)
		}
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if args[0] == "status" {
		info, err := c.DaemonInfo(ctx)
		if err != nil {
			return failAPI(err)
		}
		fmt.Printf("pid=%d apiVersion=%d uptimeSeconds=%.3f sessionCount=%d\n",
			info.PID, info.APIVersion, info.UptimeSeconds, info.SessionCount)
		return 0
	}
	// stop: request shutdown, then wait bounded for the descriptor to go
	// away (the lock owner removes its own descriptor as it exits).
	if _, err := c.Shutdown(ctx); err != nil {
		return failAPI(err)
	}
	desc := p.DaemonDescriptor()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(desc); os.IsNotExist(err) {
			fmt.Println("daemon stopped")
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "reposuite-relay: daemon did not exit within timeout")
	return 1
}

// runDaemon is the hidden `__daemon` mode. Another daemon already owning
// this state root is a normal no-op exit for a losing contender.
func runDaemon(selfExe string) int {
	p, err := paths.Current()
	if err != nil {
		return fail(err)
	}
	if err := daemon.Run(p, selfExe); err != nil {
		if err == daemon.ErrAlreadyRunning {
			return 0
		}
		return fail(err)
	}
	return 0
}
