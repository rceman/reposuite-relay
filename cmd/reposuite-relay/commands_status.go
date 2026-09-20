package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rceman/reposuite-relay/internal/auth"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/config"
)

// statusReport is the stable `status --json` projection. Runtime-only
// fields are omitted when the daemon is stopped; the configured endpoint
// remains available whenever the config exists. No secret is ever part
// of this shape.
type statusReport struct {
	Running           bool    `json:"running"`
	Configured        bool    `json:"configured"`
	Status            string  `json:"status"` // running | stopped | unreachable
	PID               int     `json:"pid,omitempty"`
	Address           string  `json:"address,omitempty"`
	WebURL            string  `json:"webUrl,omitempty"`
	APIURL            string  `json:"apiUrl,omitempty"`
	UptimeSeconds     float64 `json:"uptimeSeconds,omitempty"`
	APIVersion        int     `json:"apiVersion,omitempty"`
	Sessions          int     `json:"sessions"`
	Active            int     `json:"active"`
	Cold              int     `json:"cold"`
	APIAuthConfigured bool    `json:"apiAuthConfigured"`
}

// cmdStatus implements `reposuite-relay status [--json]` — the product
// control-plane status. It NEVER starts the daemon and never wakes a
// harness: persistent config, the current descriptor, and one bounded
// authenticated health request are its only inputs. A stale or foreign
// descriptor is reported, never mutated.
func cmdStatus(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a != "--json" {
			fmt.Fprintln(os.Stderr, "reposuite-relay: usage: status [--json]")
			return 2
		}
		jsonOut = true
	}
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	rep := statusReport{Status: "stopped"}

	// The persistent endpoint identity — malformed config is a hard
	// error even for a diagnostic command: never guess at corruption.
	cfg, err := config.Load(p.RelayConfig())
	switch {
	case err == nil:
		rep.Configured = true
		rep.Address = cfg.Address()
		rep.WebURL = "http://" + cfg.Address() + "/"
		rep.APIURL = "http://" + cfg.Address() + "/v1"
	case errors.Is(err, os.ErrNotExist):
	default:
		return fail(err)
	}

	// Machine API auth presence: the credential file is validated, never
	// read into output.
	if tok, err := auth.Load(p.MachineToken()); err == nil && auth.Valid(tok) {
		rep.APIAuthConfigured = true
	}

	// A live daemon generation? Dial validates the descriptor and
	// performs one bounded authenticated health request — it never
	// spawns a process.
	if c, err := client.Dial(p); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := c.DaemonInfo(ctx)
		cancel()
		if err != nil {
			return failAPI(err)
		}
		rep.Running = true
		rep.Status = "running"
		rep.PID = info.PID
		rep.UptimeSeconds = info.UptimeSeconds
		rep.Sessions = info.SessionCount
		rep.Active = info.ActiveSessions
		rep.Cold = info.ColdSessions
		rep.APIVersion = info.APIVersion
	} else if !errors.Is(err, client.ErrNotRunning) {
		// Something answers but is not a compatible relayd generation —
		// report unreachable; never mutate another owner's descriptor.
		rep.Status = "unreachable"
	}

	if jsonOut {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(raw))
		return 0
	}
	printStatus(rep)
	return 0
}

// printStatus renders the human form. It never prints credentials.
func printStatus(r statusReport) {
	fmt.Println("RepoSuite Relay")
	fmt.Println()
	fmt.Printf("Status:       %s\n", strings.ToUpper(r.Status))
	if r.Running {
		fmt.Printf("PID:          %d\n", r.PID)
	}
	if r.Address == "" {
		fmt.Println("Address:      not configured")
	} else {
		fmt.Printf("Address:      %s\n", r.Address)
		fmt.Printf("Web:          %s\n", r.WebURL)
		fmt.Printf("API:          %s\n", r.APIURL)
	}
	if r.Running {
		up := time.Duration(r.UptimeSeconds * float64(time.Second)).Truncate(time.Second)
		fmt.Printf("Uptime:       %s\n", up)
		fmt.Printf("Sessions:     %d\n", r.Sessions)
		fmt.Printf("Active:       %d\n", r.Active)
		fmt.Printf("Cold:         %d\n", r.Cold)
	}
	auth := "not configured"
	if r.APIAuthConfigured {
		auth = "configured"
	}
	fmt.Printf("API auth:     %s\n", auth)
}
