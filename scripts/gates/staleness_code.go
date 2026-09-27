package main

import (
	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/scopes"
)

// The one file that imports the server's packages for staleness: the
// scope set per mode, which docs/gcp-setup.md's block is generated
// from, and the settings the configuration reads.
func init() {
	stalenessScopeModes = func() []stalenessMode {
		modes := scopes.Modes()
		out := make([]stalenessMode, 0, len(modes))
		for _, m := range modes {
			out = append(out, stalenessMode{Name: m.Name, Flags: m.Flags, Scopes: m.Scopes})
		}
		return out
	}
	stalenessConfigEnv = config.EnvVars
}
