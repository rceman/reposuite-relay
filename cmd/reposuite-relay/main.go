// Command reposuite-relay is the RepoSuite Relay standalone CLI.
// Future umbrella form: `reposuite relay ...` dispatches here.
//
// Hidden internal modes `__daemon` and `__fixture` are implementation
// details of the single-binary design — never listed in help and not part
// of the public CLI contract.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rceman/reposuite-relay/internal/daemon"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/protocol"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/version"
)

const usage = `reposuite-relay — RepoSuite Relay (managed agent runtime/session relay)

Usage:
  reposuite-relay <command>

Commands:
  version                        print version
  help                           show this help
  paths                          print resolved RepoSuite/Relay state paths
  serve fixture --key <KEY>      start a fixture session
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
		fmt.Printf("reposuite_root: %s\n", p.RepoSuiteRoot())
		fmt.Printf("relay_root:     %s\n", p.RelayRoot())
		fmt.Printf("run_dir:        %s\n", p.RunDir())
		fmt.Printf("daemon_socket:  %s\n", p.DaemonSocket())
		fmt.Printf("log_dir:        %s\n", p.LogDir())
	case "serve":
		return cmdServe(args[1:], selfExe)
	case "list":
		return cmdManaged(selfExe, protocol.OpListSessions, "")
	case "status":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "reposuite-relay: status requires exactly one session key")
			return 2
		}
		return cmdManaged(selfExe, protocol.OpSessionStatus, args[1])
	case "stop":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "reposuite-relay: stop requires exactly one session key")
			return 2
		}
		return cmdManaged(selfExe, protocol.OpStopSession, args[1])
	case "daemon":
		return cmdDaemon(args[1:])
	case "__daemon":
		return runDaemon(selfExe)
	case "__fixture":
		return fixture.RunChild()
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

func failResp(r *protocol.Response) int {
	fmt.Fprintf(os.Stderr, "reposuite-relay: %s: %s\n", r.Code, r.Error)
	return 1
}

// cmdServe implements `serve fixture --key <KEY>`.
func cmdServe(args []string, selfExe string) int {
	if len(args) == 0 || args[0] != "fixture" {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay serve fixture --key <KEY>")
		return 2
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	key := fs.String("key", "", "session key")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay serve fixture --key <KEY>")
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
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	c, err := daemon.Ensure(p, selfExe)
	if err != nil {
		return fail(err)
	}
	r, err := c.Do(protocol.Request{Op: protocol.OpServeFixture, Key: *key, Cwd: cwd})
	if err != nil {
		return fail(err)
	}
	if !r.OK {
		return failResp(r)
	}
	printSession(r.Session)
	fmt.Printf("daemon_pid=%d\n", r.Daemon.PID)
	return 0
}

// cmdManaged implements list/status/stop — all of which ensure the daemon.
func cmdManaged(selfExe, op, key string) int {
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	c, err := daemon.Ensure(p, selfExe)
	if err != nil {
		return fail(err)
	}
	r, err := c.Do(protocol.Request{Op: op, Key: key})
	if err != nil {
		return fail(err)
	}
	if !r.OK {
		return failResp(r)
	}
	switch op {
	case protocol.OpListSessions:
		for i := range r.Sessions {
			printSession(&r.Sessions[i])
		}
		fmt.Printf("daemon_pid=%d sessions=%d\n", r.Daemon.PID, r.Daemon.SessionCount)
	case protocol.OpSessionStatus:
		printSession(r.Session)
	case protocol.OpStopSession:
		fmt.Printf("stopped key=%s\n", key)
	}
	return 0
}

func printSession(s *protocol.SessionInfo) {
	fmt.Printf("key=%s runtimeId=%s harness=%s state=%s generation=%d pid=%d cwd=%s createdAt=%s generationStartedAt=%s\n",
		s.Key, s.RuntimeID, s.Harness, s.State, s.Generation, s.PID,
		s.Cwd, s.CreatedAt, s.GenerationStartedAt)
}

// cmdDaemon implements `daemon status|stop` — neither auto-starts a daemon.
func cmdDaemon(args []string) int {
	if len(args) != 1 || (args[0] != "status" && args[0] != "stop") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay daemon {status|stop}")
		return 2
	}
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	c, err := daemon.Dial(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reposuite-relay: daemon not running")
		return 1
	}
	if args[0] == "status" {
		r, err := c.Do(protocol.Request{Op: protocol.OpDaemonStatus})
		if err != nil {
			return fail(err)
		}
		fmt.Printf("pid=%d protocolVersion=%d uptimeSeconds=%.3f sessionCount=%d\n",
			r.Daemon.PID, r.Daemon.ProtocolVersion, r.Daemon.UptimeSeconds, r.Daemon.SessionCount)
		return 0
	}
	// stop: request shutdown, then wait bounded for the socket to go away.
	r, err := c.Do(protocol.Request{Op: protocol.OpShutdown})
	if err != nil {
		return fail(err)
	}
	if !r.OK {
		return failResp(r)
	}
	sock := p.DaemonSocket()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); os.IsNotExist(err) {
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
