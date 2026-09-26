// Package tools registers the MCP tools. A handler validates its input,
// calls the service and returns a result that renders itself; the rules
// live in internal/service and the policy of registration lives in
// register.go.
package tools

import (
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
)

// Deps are what the tools need.
type Deps struct {
	Config config.Config
	// Client is the Gmail REST client. Never nil when serving; the
	// schema dump builds one that is never called.
	Client *gapi.Client
	Logger *slog.Logger

	// registered collects the name of each tool register adds, when set.
	registered *[]string
	// namesOnly makes register collect names and add nothing, for Names.
	namesOnly bool
}

// registrations is every group of tools, in the order they appear in
// tools/list. Each entry calls register once per tool, and register
// decides whether the configuration keeps it.
var registrations = []func(*mcp.Server, Deps){registerRead}

// Register adds every tool the configuration allows and returns their
// names, in tools/list order. s may be nil only under Names.
func Register(s *mcp.Server, d Deps) []string {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	names := []string{}
	d.registered = &names
	for _, add := range registrations {
		add(s, d)
	}
	return names
}

// Names is what Register adds under d's configuration, found by running
// the same registrations with register stopping at the name, so no
// schema is built. The server's instructions are fixed before any tool
// is added, and this is how they describe only the tools that exist.
func Names(d Deps) []string {
	d.namesOnly = true
	return Register(nil, d)
}

// FullSurface is the configuration under which every tool registers,
// for the schema dump. It sits beside Kind.allowed because it must
// clear every gate allowed reads, and the dump cannot know when a new
// gate appears.
func FullSurface(cfg config.Config) config.Config {
	cfg.ReadOnly = false
	cfg.EnableSend = true
	cfg.EnableDestructive = true
	if cfg.LocalDir == "" {
		// Never written to: the dump lists tools and calls none.
		cfg.LocalDir = "schema-dump"
	}
	return cfg
}
