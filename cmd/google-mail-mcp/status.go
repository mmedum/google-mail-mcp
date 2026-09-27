package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/app"
	"github.com/mmedum/google-mail-mcp/internal/auth"
	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/redact"
	"github.com/mmedum/google-mail-mcp/internal/scopes"
	"github.com/mmedum/google-mail-mcp/internal/version"
)

// statusSchemaVersion changes only when a field of `status --json` is
// removed or changes meaning, never when one is added.
const statusSchemaVersion = 1

// statusReport is everything `status` knows, collected once and written
// either for a person or for a script. One collector, two renderers,
// because a script reading the text breaks the day a label is reworded.
//
// Nothing here contacts Google; `doctor` does that.
type statusReport struct {
	SchemaVersion int    `json:"schema_version"`
	Binary        string `json:"binary"`
	Version       string `json:"version"`
	Profile       string `json:"profile"`
	ConfigDir     string `json:"config_dir"`
	// Account is masked to its domain here, at the collector, because
	// the JSON encoder does not pass through outf.
	Account     *string           `json:"account"`
	Credentials statusCredentials `json:"credentials"`
	Scopes      statusScopes      `json:"scopes"`
	Settings    statusSettings    `json:"settings"`
	Profiles    []string          `json:"profiles"`
}

type statusCredentials struct {
	// Resolved is the field to branch on: false means every tool
	// answers [auth] until login.
	Resolved   bool    `json:"resolved"`
	TokenStore *string `json:"token_store"`
	// Reason says why nothing resolved; null when something did.
	Reason              *string `json:"reason"`
	ClientSecretPath    string  `json:"client_secret_path"`
	ClientSecretPresent bool    `json:"client_secret_present"`
}

type statusScopes struct {
	// Granted is what the last login was granted.
	Granted []string `json:"granted"`
	// Required is what this configuration asks for.
	Required []string `json:"required"`
	// Missing is Required less what Granted covers; non-empty means a
	// flag changed since the last login.
	Missing []string `json:"missing"`
}

type statusSettings struct {
	ReadOnly          bool    `json:"read_only"`
	EnableSend        bool    `json:"enable_send"`
	EnableDestructive bool    `json:"enable_destructive"`
	LocalDir          *string `json:"local_dir"`
	HTTPTimeout       string  `json:"http_timeout"`
	LogLevel          string  `json:"log_level"`
	LogFormat         string  `json:"log_format"`
}

func newStatusReport(p *app.Profile) statusReport {
	cfg := p.Config
	r := statusReport{
		SchemaVersion: statusSchemaVersion,
		Binary:        "google-mail-mcp",
		Version:       version.String(),
		Profile:       cfg.Profile,
		ConfigDir:     redact.Path(p.Dir),
		Account:       orNil(redact.Account(p.User.AccountEmail)),
		Credentials: statusCredentials{
			ClientSecretPath:    redact.Path(p.ClientSecretPath),
			ClientSecretPresent: fileExists(p.ClientSecretPath),
		},
		Scopes: statusScopes{
			Granted:  orEmpty(p.User.Scopes),
			Required: cfg.Scopes(),
			Missing:  orEmpty(p.MissingScopes()),
		},
		Settings: statusSettings{
			ReadOnly: cfg.ReadOnly, EnableSend: cfg.EnableSend, EnableDestructive: cfg.EnableDestructive,
			LocalDir: orNil(cfg.LocalDir), HTTPTimeout: cfg.HTTPTimeout.String(),
			LogLevel: string(cfg.LogLevel), LogFormat: string(cfg.LogFormat),
		},
		Profiles: []string{},
	}
	if names, err := cfg.ConfigDir.Profiles(); err == nil && names != nil {
		r.Profiles = names
	}
	if _, src, err := p.Store.Resolve(); err == nil {
		r.Credentials.Resolved = true
		r.Credentials.TokenStore = orNil(string(src))
	} else {
		r.Credentials.Reason = orNil(redact.Text(err.Error()))
	}
	return r
}

func (r statusReport) writeText(w io.Writer) {
	outf(w, "%s\n\n", version.Info())
	outf(w, "profile:        %s\n", r.Profile)
	outf(w, "config dir:     %s\n", r.ConfigDir)
	outf(w, "account:        %s\n", orUnknown(deref(r.Account)))
	present := "missing"
	if r.Credentials.ClientSecretPresent {
		present = "present"
	}
	outf(w, "client secret:  %s (%s)\n", r.Credentials.ClientSecretPath, present)
	if r.Credentials.Resolved {
		outf(w, "token store:    %s\n", deref(r.Credentials.TokenStore))
	} else {
		outf(w, "token store:    none — %s\n", deref(r.Credentials.Reason))
	}
	outf(w, "scopes granted: %s\n", orUnknown(strings.Join(r.Scopes.Granted, " ")))
	outf(w, "scopes needed:  %s\n", strings.Join(r.Scopes.Required, " "))
	if len(r.Scopes.Missing) > 0 {
		outf(w, "                missing %s: run `google-mail-mcp login` again\n", strings.Join(r.Scopes.Missing, " "))
	}
	outf(w, "read-only:      %t\n", r.Settings.ReadOnly)
	outf(w, "send:           %t\n", r.Settings.EnableSend)
	outf(w, "destructive:    %t\n", r.Settings.EnableDestructive)
	outf(w, "local dir:      %s\n", orUnknown(deref(r.Settings.LocalDir)))
	outf(w, "http timeout:   %s\n", r.Settings.HTTPTimeout)
	if len(r.Profiles) > 1 {
		outf(w, "profiles:       %s\n", strings.Join(r.Profiles, ", "))
	}
}

// writeJSON writes the report as one JSON value. The masking was done
// at the collector; the address rule is applied once more on the way
// out, for a field added later by someone who did not know it.
func (r statusReport) writeJSON(w io.Writer) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, redact.Text(string(b))+"\n")
	return err
}

func cmdStatus(args []string, stdout, stderr io.Writer, env func(string) string) int {
	var asJSON bool
	cfg, code := parseConfig("status", args, stderr, env, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print the same state as one JSON object")
	})
	if code != nil {
		return *code
	}
	p, err := openProfile(cfg, env, stderr)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	r := newStatusReport(p)
	if asJSON {
		if err := r.writeJSON(stdout); err != nil {
			return fail(stderr, "%v", err)
		}
		return 0
	}
	r.writeText(stdout)
	return 0
}

// cmdDoctor checks what goes wrong at setup and names what is missing:
// client JSON, then the token, then the granted scopes, then whether the
// API answers one getProfile. Exit 1 when anything failed.
func cmdDoctor(args []string, stdout, stderr io.Writer, env func(string) string) int {
	cfg, code := parseConfig("doctor", args, stderr, env, nil)
	if code != nil {
		return *code
	}
	p, err := openProfile(cfg, env, stderr)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	outf(stdout, "%s\nprofile: %s\n\n", version.Info(), cfg.Profile)
	d := doctor{w: stdout}
	d.run(ctx, cfg, p)
	if d.problems == 0 {
		outf(stdout, "\nNo problems found.\n")
		return 0
	}
	outf(stdout, "\n%d problem(s).\n", d.problems)
	return 1
}

type doctor struct {
	w        io.Writer
	problems int
}

func (d *doctor) report(ok bool, label, detail string) {
	mark := "ok  "
	if !ok {
		mark = "FAIL"
		d.problems++
	}
	outf(d.w, "[%s] %s\n", mark, label)
	if detail != "" {
		outf(d.w, "       %s\n", strings.ReplaceAll(detail, "\n", "\n       "))
	}
}

// run stops at the first failure a later check depends on, because the
// checks after it would only repeat it.
func (d *doctor) run(ctx context.Context, cfg config.Config, p *app.Profile) {
	oc, err := p.OAuthConfig()
	if err != nil {
		d.report(false, "OAuth Desktop client JSON", fmt.Sprintf("%v\ncreate a Desktop app client and put its JSON at %s, or pass --client-secret",
			err, cfg.ConfigDir.DefaultClientSecretPath(cfg.Profile)))
		return
	}
	d.report(true, "OAuth Desktop client JSON", p.ClientSecretPath)

	refresh, src, err := p.Store.Resolve()
	if err != nil {
		d.report(false, "refresh token", err.Error())
		return
	}
	d.report(true, "refresh token", "from the "+string(src))

	ts := auth.TokenSource(ctx, oc, refresh, cfg.HTTPTimeout)
	tok, err := ts.Token()
	if err != nil {
		d.report(false, "access token", err.Error()+"\na client in Testing status issues refresh tokens that expire after 7 days; run `google-mail-mcp login`")
		return
	}
	d.report(true, "access token", "")

	required := cfg.Scopes()
	if info, err := auth.Inspect(ctx, nil, tok.AccessToken); err != nil {
		d.report(false, "granted scopes", err.Error())
	} else if missing := scopes.Missing(info.Scopes, required); len(missing) > 0 {
		d.report(false, "granted scopes", "not granted: "+strings.Join(missing, ", ")+
			"\na flag that changes scopes needs `google-mail-mcp login` again; accept every scope asked for")
	} else {
		detail := strings.Join(info.Scopes, " ")
		if excess := scopes.Excess(info.Scopes, required); len(excess) > 0 {
			detail += "\nwider than this configuration needs: " + strings.Join(excess, ", ") +
				"\nto hold only what it needs, run `google-mail-mcp logout`, then `google-mail-mcp login`"
		}
		d.report(true, "granted scopes", detail)
	}

	email, err := profileEmail(ctx, cfg, ts)
	if err != nil {
		detail := err.Error()
		if c, ok := gapi.ClassOf(err); ok && (c == gapi.ClassForbidden || c == gapi.ClassAuth) {
			detail += "\nif Google says the API is disabled, enable the Gmail API in the Cloud project that issued this client"
		}
		d.report(false, "Gmail API (getProfile)", detail)
		return
	}
	d.report(true, "Gmail API (getProfile)", "as "+email)
	if stored := p.User.AccountEmail; stored != "" && !strings.EqualFold(stored, email) {
		d.report(false, "account", "the token belongs to a different account than the last login recorded; run `google-mail-mcp login`")
	}
}

// orNil turns an unset string into JSON null, which a caller cannot
// mistake for a value.
func orNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// orEmpty keeps a list a list: a nil slice marshals as null.
func orEmpty(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
