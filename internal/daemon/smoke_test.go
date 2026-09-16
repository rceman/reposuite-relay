package daemon_test

import (
	"strings"
	"testing"
)

// TestSmokeSequence: the section-48 manual flow end to end.
func TestSmokeSequence(t *testing.T) {
	home, root := testRoot(t)

	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("status before any daemon: %s", out)
	}
	_, a := serveKey(t, home, root, "alpha")
	_, b := serveKey(t, home, root, "beta")
	dp, count := daemonStatus(t, home, root)
	if count != 2 {
		t.Fatalf("sessionCount=%d", count)
	}
	out := mustCLI(t, home, root, "list")
	ai := strings.Index(out, "key=alpha")
	bi := strings.Index(out, "key=beta")
	if ai < 0 || bi < 0 || ai > bi {
		t.Fatalf("list order/content: %s", out)
	}
	sa := mustCLI(t, home, root, "status", "alpha")
	if field(t, sa, "state") != "idle" || field(t, sa, "generation") != "1" ||
		field(t, sa, "runtimeState") != "warm" {
		t.Fatalf("status alpha: %s", sa)
	}
	mustCLI(t, home, root, "stop", "alpha")
	sb := mustCLI(t, home, root, "status", "beta")
	if field(t, sb, "pid") != b["pid"] || field(t, sb, "runtimeId") != b["runtimeId"] {
		t.Fatalf("beta mutated: %s", sb)
	}
	mustCLI(t, home, root, "stop", "beta")
	mustCLI(t, home, root, "daemon", "stop")
	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("daemon status after stop: %s", out)
	}
	_ = a
	_ = dp
}
