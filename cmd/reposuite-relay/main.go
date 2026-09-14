// Command reposuite-relay is the RepoSuite Relay standalone CLI.
// Future umbrella form: `reposuite relay ...` dispatches here.
package main

import (
	"fmt"
	"os"

	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/version"
)

const usage = `reposuite-relay — RepoSuite Relay (managed agent runtime/session relay)

Usage:
  reposuite-relay <command>

Commands:
  version    print version
  paths      print resolved RepoSuite/Relay state paths
  help       show this help
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
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
			fmt.Fprintf(os.Stderr, "reposuite-relay: %v\n", err)
			return 1
		}
		fmt.Printf("reposuite_root: %s\n", p.RepoSuiteRoot())
		fmt.Printf("relay_root:     %s\n", p.RelayRoot())
		fmt.Printf("run_dir:        %s\n", p.RunDir())
		fmt.Printf("daemon_socket:  %s\n", p.DaemonSocket())
		fmt.Printf("log_dir:        %s\n", p.LogDir())
	default:
		fmt.Fprintf(os.Stderr, "reposuite-relay: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	return 0
}
