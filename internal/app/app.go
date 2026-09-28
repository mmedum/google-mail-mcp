// Package app is the startup assembly every entry point shares: open
// the profile, resolve the client JSON and the refresh token, build the
// Gmail client and wire the MCP server.
//
// It lives outside package main so the schema dump, the tests and any
// later driver assemble the server the same way the binary does, rather
// than each re-deriving the sequence and dropping a step on the way.
// Nothing here prints, exits or reaches the network; the caller decides
// what a missing token means.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/auth"
	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/credentials"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/redact"
	"github.com/mmedum/google-mail-mcp/v2/internal/scopes"
	"github.com/mmedum/google-mail-mcp/v2/internal/server"
	"github.com/mmedum/google-mail-mcp/v2/internal/tools"
	"github.com/mmedum/google-mail-mcp/v2/internal/userconfig"
)

// Profile is one configured profile: where its files are, what the last
// login stored, and its credential store.
type Profile struct {
	Config config.Config
	// Dir is the profile's directory.
	Dir string
	// ClientSecretPath is the OAuth client JSON in effect: the override,
	// then the stored path, then the default location.
	ClientSecretPath string
	// User is the stored profile state; HasUser says a file existed.
	User    userconfig.Config
	HasUser bool
	Store   *credentials.Store
}

// OpenProfile reads a profile's stored state and builds its credential
// store. warn receives the plaintext-file warning on every use of it.
func OpenProfile(cfg config.Config, keyring credentials.Backend, env func(string) string, warn func(string)) (*Profile, error) {
	dir := cfg.ConfigDir
	p := &Profile{Config: cfg, Dir: dir.Profile(cfg.Profile)}
	var err error
	p.User, err = dir.Load(cfg.Profile)
	switch {
	case err == nil:
		p.HasUser = true
	case !errors.Is(err, userconfig.ErrNotFound):
		return nil, err
	}
	if p.ClientSecretPath, err = dir.ResolveClientSecretPath(cfg.Profile, cfg.ClientSecretPath); err != nil {
		return nil, err
	}
	p.Store = &credentials.Store{
		Profile: cfg.Profile, Keyring: keyring, FilePath: dir.TokenFilePath(cfg.Profile),
		Env: env, Warn: warn,
		ExpectKeyring: p.User.TokenStore == string(credentials.SourceKeyring),
	}
	return p, nil
}

// OAuthConfig loads the client JSON for this configuration's scopes.
func (p *Profile) OAuthConfig() (*oauth2.Config, error) {
	return auth.LoadClientSecret(p.ClientSecretPath, p.Config.Scopes())
}

// TokenSource resolves the refresh token and wraps it in a refreshing
// source bounded by the configured timeout.
func (p *Profile) TokenSource(ctx context.Context) (oauth2.TokenSource, credentials.Source, error) {
	oc, err := p.OAuthConfig()
	if err != nil {
		return nil, "", err
	}
	refresh, src, err := p.Store.Resolve()
	if err != nil {
		return nil, "", err
	}
	return auth.TokenSource(ctx, oc, refresh, p.Config.HTTPTimeout), src, nil
}

// MissingScopes is what the configuration needs that the last login
// was not granted. Empty when nothing was stored, since a profile with
// no login is reported as that rather than as a scope problem.
func (p *Profile) MissingScopes() []string {
	if !p.HasUser || len(p.User.Scopes) == 0 {
		return nil
	}
	return scopes.Missing(p.User.Scopes, p.Config.Scopes())
}

// Options are the process-level inputs to Assemble.
type Options struct {
	Env     func(string) string
	Keyring credentials.Backend
	Logger  *slog.Logger
	// UserAgent identifies the build to Google.
	UserAgent string
}

// Runtime is an assembled server's state. It holds no token: the
// refresh token lives inside TokenSource, which neither String nor
// LogValue reaches.
type Runtime struct {
	Config  config.Config
	Profile *Profile
	// Client is always usable. Without credentials its token source
	// answers every call with [auth] until login.
	Client *gapi.Client
	// TokenSource is nil when CredentialsErr is set.
	TokenSource oauth2.TokenSource
	// Source says where the refresh token came from.
	Source credentials.Source
	// CredentialsErr is why no token source could be built. The server
	// still starts: a client lists tools before anyone has logged in,
	// and exiting would look like a crash in every host's log.
	CredentialsErr error
	logger         *slog.Logger
}

// Assemble builds the runtime. It fails only when the profile itself
// cannot be read; a missing login is recorded in CredentialsErr.
func Assemble(ctx context.Context, cfg config.Config, o Options) (*Runtime, error) {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p, err := OpenProfile(cfg, o.Keyring, o.Env, func(msg string) { logger.Warn(redact.Text(msg)) })
	if err != nil {
		return nil, err
	}
	r := &Runtime{Config: cfg, Profile: p, logger: logger}
	var ts oauth2.TokenSource
	ts, r.Source, r.CredentialsErr = p.TokenSource(ctx)
	if r.CredentialsErr != nil {
		ts = auth.NoCredentials{Reason: r.CredentialsErr}
	} else {
		r.TokenSource = ts
	}
	r.Client = gapi.New(gapi.Options{
		BaseURL: cfg.APIBase, Timeout: cfg.HTTPTimeout, TokenSource: ts,
		Logger: logger, UserAgent: o.UserAgent,
	})
	return r, nil
}

// ScopeWarning is the sentence startup and doctor print when a flag
// changed the scopes since the last login, or "" when none is needed.
func (r *Runtime) ScopeWarning() string {
	missing := r.Profile.MissingScopes()
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("the configuration needs %s, which the last login was not granted; run `google-mail-mcp login` again",
		strings.Join(missing, ", "))
}

// Server wires the MCP server for this runtime.
func (r *Runtime) Server(version string) *mcp.Server {
	return server.New(server.Deps{
		Deps:    tools.Deps{Config: r.Config, Client: r.Client, Logger: r.logger},
		Version: version,
	})
}

// String keeps %v and %+v to the same safe fields as LogValue.
func (r *Runtime) String() string { return r.LogValue().String() }

// LogValue renders the runtime for slog: profile, flags and where the
// token came from. Never the token, the account or the client path.
func (r *Runtime) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("profile", r.Config.Profile),
		slog.String("token_source", string(r.Source)),
		slog.Bool("signed_in", r.CredentialsErr == nil),
		slog.Bool("read_only", r.Config.ReadOnly),
		slog.Bool("send", r.Config.EnableSend),
		slog.Bool("destructive", r.Config.EnableDestructive),
		slog.Bool("local_dir", r.Config.LocalDir != ""),
	)
}
