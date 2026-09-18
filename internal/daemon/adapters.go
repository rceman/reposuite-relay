package daemon

import (
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/harness/opencode"
	"github.com/rceman/reposuite-relay/internal/version"
)

// registerAdapters installs the built-in native harness adapters. Each entry
// carries its own availability probe, so creating a session for a harness this
// machine cannot run fails closed at create instead of at first use. Clients
// never supply a command: this table is the whole command surface.
func (d *Daemon) registerAdapters() {
	d.register(codex.NewAdapter(codex.Deps{
		Broker:        d.broker,
		Supervisor:    d.supervisor,
		Command:       d.opts.CodexCommand,
		Materialize:   d.materialize,
		RandRuntimeID: d.opts.RandRuntimeID,
		Version:       version.Version,
	}), func() error {
		_, err := d.opts.CodexCommand()
		return err
	})
	d.register(opencode.NewAdapter(opencode.Deps{
		Broker:        d.broker,
		Supervisor:    d.supervisor,
		Command:       d.opts.OpenCodeCommand,
		Materialize:   d.materialize,
		RandRuntimeID: d.opts.RandRuntimeID,
		Version:       version.Version,
	}), func() error {
		_, err := d.opts.OpenCodeCommand()
		return err
	})
}

// withAdapterDefaults fills the production command resolvers: the installed
// vendor CLIs, and nothing else.
func (o Options) withAdapterDefaults() Options {
	if o.CodexCommand == nil {
		o.CodexCommand = codex.DefaultCommand
	}
	if o.OpenCodeCommand == nil {
		o.OpenCodeCommand = opencode.DefaultCommand
	}
	return o
}
