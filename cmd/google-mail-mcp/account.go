package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/app"
	"github.com/mmedum/google-mail-mcp/internal/auth"
	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/credentials"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/scopes"
	"github.com/mmedum/google-mail-mcp/internal/version"
)

// keyringBackend is the credential store's keyring. A var so the tests
// replace it package-wide in TestMain: a test can isolate the config
// directory and the environment, and cannot isolate the OS keyring, so
// `go test` against the real one would revoke and delete the person's
// own refresh token.
var keyringBackend = credentials.OSKeyring()

// openBrowser is how login reaches a browser. nil means the platform's
// handler; tests substitute one that calls back.
var openBrowser func(string) error

// loginTimeout bounds the person's trip through the browser.
var loginTimeout = 10 * time.Minute

func openProfile(cfg config.Config, env func(string) string, stderr io.Writer) (*app.Profile, error) {
	return app.OpenProfile(cfg, keyringBackend, env, func(msg string) { outf(stderr, "warning: %s\n", msg) })
}

// cmdLogin runs the loopback OAuth flow and stores the refresh token.
func cmdLogin(args []string, stdout, stderr io.Writer, env func(string) string) int {
	var noBrowser, consent bool
	cfg, code := parseConfig("login", args, stderr, env, func(fs *flag.FlagSet) {
		fs.BoolVar(&noBrowser, "no-browser", false, "print the authorization URL instead of opening a browser")
		fs.BoolVar(&consent, "consent", false, "show Google's consent screen even if every scope is already granted")
	})
	if code != nil {
		return *code
	}
	p, err := openProfile(cfg, env, stderr)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	oc, err := p.OAuthConfig()
	if err != nil {
		return fail(stderr, "%v\n\nCreate an OAuth Desktop app client in the Google Cloud console, download its JSON, "+
			"and pass it with --client-secret (%sCLIENT_SECRET) or put it at %s", err, config.EnvPrefix, cfg.ConfigDir.DefaultClientSecretPath(cfg.Profile))
	}
	requested := cfg.Scopes()
	outf(stdout, "Signing in to profile %q, asking for:\n", cfg.Profile)
	for _, s := range requested {
		outf(stdout, "  %s\n", s)
	}
	outf(stdout, "\n")

	// What was granted before decides whether the consent screen is
	// shown again (§10). It counts only while a refresh token is still
	// stored: without one, a new one is needed and only consent mints it.
	_, storedSource, storedErr := p.Store.ResolveStored()
	var granted []string
	if storedErr == nil {
		granted = p.User.Scopes
	}

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	tok, err := auth.Login(ctx, oc, auth.LoginOptions{
		Out: stdout, NoBrowser: noBrowser, OpenBrowser: openBrowser,
		Granted: granted, ForceConsent: consent, HTTPTimeout: cfg.HTTPTimeout, Timeout: loginTimeout,
	})
	if err != nil {
		return fail(stderr, "login failed: %v", err)
	}

	src := storedSource
	switch {
	case tok.RefreshToken != "":
		if src, err = p.Store.Save(tok.RefreshToken); err != nil {
			return fail(stderr, "store the token: %v", err)
		}
	case storedErr == nil:
		// Google kept the refresh token it had already issued; so do we.
	default:
		return fail(stderr, "%v", auth.ErrNoRefreshToken)
	}

	uc := p.User
	uc.ClientSecretPath = p.ClientSecretPath
	uc.TokenStore = string(src)
	// The profile records what Google GRANTED. A scope requested and
	// refused is the failure worth seeing, and storing the request would
	// present it as a grant.
	uc.Scopes = requested
	if info, err := auth.Inspect(ctx, nil, tok.AccessToken); err == nil && len(info.Scopes) > 0 {
		uc.Scopes = info.Scopes
		if missing := scopes.Missing(info.Scopes, requested); len(missing) > 0 {
			outf(stderr, "warning: Google did not grant %s, so some tools will fail; run login again and accept every scope\n",
				strings.Join(missing, ", "))
		}
	}
	if email, err := profileEmail(ctx, cfg, oauth2.StaticTokenSource(tok)); err == nil {
		uc.AccountEmail = email
	}
	if err := cfg.ConfigDir.Save(cfg.Profile, uc); err != nil {
		return fail(stderr, "save the profile: %v", err)
	}
	outf(stdout, "\nSigned in as %s (profile %q, token in the %s).\n", orUnknown(uc.AccountEmail), cfg.Profile, src)
	return 0
}

// profileEmail reads the account's address from getProfile, which every
// scope this server requests covers.
func profileEmail(ctx context.Context, cfg config.Config, ts oauth2.TokenSource) (string, error) {
	c := gapi.New(gapi.Options{BaseURL: cfg.APIBase, Timeout: cfg.HTTPTimeout, TokenSource: ts,
		UserAgent: "google-mail-mcp/" + version.String()})
	out, err := c.Profile(ctx)
	if err != nil {
		return "", err
	}
	if out.EmailAddress == "" {
		return "", errors.New("getProfile returned no address")
	}
	return out.EmailAddress, nil
}

// cmdLogout revokes the stored refresh token at Google, then removes the
// local copy and the profile state. A token supplied through the
// environment is neither revoked nor removed: it is not this command's.
func cmdLogout(args []string, stdout, stderr io.Writer, env func(string) string) int {
	cfg, code := parseConfig("logout", args, stderr, env, nil)
	if code != nil {
		return *code
	}
	p, err := openProfile(cfg, env, stderr)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	// Google revokes the grant, not one token, so every profile sharing
	// this OAuth client is signed out too. Said before, not after.
	if others, err := cfg.ConfigDir.SharingClient(cfg.Profile); err == nil && len(others) > 0 {
		outf(stdout, "Note: these profiles use the same OAuth client and will also be signed out: %s\n",
			strings.Join(others, ", "))
	}

	token, _, err := p.Store.ResolveStored()
	switch {
	case err == nil:
		ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTPTimeout)
		defer cancel()
		if rerr := auth.Revoke(ctx, &http.Client{Timeout: cfg.HTTPTimeout}, token); rerr != nil {
			outf(stdout, "Could not revoke the token at Google (%v); removing the local copy anyway.\n", rerr)
		} else {
			outf(stdout, "Revoked the grant at Google.\n")
		}
	case errors.Is(err, credentials.ErrNotFound):
		outf(stdout, "No stored token to revoke.\n")
	default:
		outf(stdout, "Could not read the stored token (%v); removing what is there.\n", err)
	}
	if env(credentials.EnvVar) != "" {
		outf(stderr, "warning: %s is set; logout neither revokes nor removes it\n", credentials.EnvVar)
	}

	if err := p.Store.Delete(); err != nil {
		return fail(stderr, "%v", err)
	}
	if err := cfg.ConfigDir.Remove(cfg.Profile); err != nil {
		return fail(stderr, "%v", err)
	}
	outf(stdout, "Signed out of profile %q.\n", cfg.Profile)
	return 0
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// fileExists reports whether path names a readable file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
