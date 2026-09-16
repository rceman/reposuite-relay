package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/daemon"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
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
