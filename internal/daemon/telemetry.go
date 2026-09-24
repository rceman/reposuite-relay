package daemon

// RepoDex telemetry integration: config mapping + project grouping.
// The service itself lives in internal/telemetry; these helpers bridge
// the daemon's config/presentation authorities.

import (
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/telemetry"
	"github.com/rceman/reposuite-relay/internal/version"
)

// telemetryConfig maps the durable relay.json RepoDex section into the
// telemetry service config. A missing/malformed config fails startup the
// same way the listener path does — telemetry never adds its own config
// domain. Absent section = telemetry disabled.
func (d *Daemon) telemetryConfig() telemetry.Config {
	cfg, err := config.Load(d.paths.RelayConfig())
	if err != nil {
		return telemetry.Config{} // disabled
	}
	rc := cfg.RepoDex
	return telemetry.Config{
		Enabled:         rc.Enabled,
		StateDir:        rc.StateDir,
		BatchMaxEvents:  rc.BatchMaxEvents,
		BatchMaxBytes:   rc.BatchMaxBytes,
		BatchMaxDelayMs: rc.BatchMaxDelayMs,
		QueueLimit:      rc.QueueLimit,
		SpoolLimitBytes: rc.SpoolLimitBytes,
	}
}

// projectIDForCwd resolves the Relay presentation grouping for telemetry
// project_id. Presentation authority only — never Task authority.
func (d *Daemon) projectIDForCwd(cwd string) string {
	if d.pres == nil {
		return ""
	}
	if p, ok := d.pres.Match(cwd); ok {
		return p.ID
	}
	return ""
}

// initTelemetry constructs the RepoDex telemetry service from relay.json.
// The sequence allocator's durable reservation runs through the same
// materialize path as every other durable metadata write; project grouping
// comes from the presentation catalog (presentation authority only).
func (d *Daemon) initTelemetry(p paths.Paths) {
	d.telemetry = telemetry.New(d.telemetryConfig(), telemetry.Deps{
		SpoolDir: p.TelemetrySpool(),
		ReserveSeq: func(m *session.Managed, mark uint64) error {
			return d.materialize(m, harness.SessionUpdate{TelemetrySeqWatermark: &mark})
		},
		ProjectID:      d.projectIDForCwd,
		AdapterVersion: version.Version,
	})
}
