package auth_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/auth"
	"github.com/mmedum/google-mail-mcp/v2/internal/scopes"
)

func TestClientProject(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for path, want := range map[string]string{
		write("named.json", `{"installed":{"client_id":"x","project_id":"example-project"}}`): "example-project",
		write("unnamed.json", `{"installed":{"client_id":"x"}}`):                              "",
		write("bad.json", `not json`):                                                         "",
		filepath.Join(dir, "missing.json"):                                                    "",
	} {
		if got := auth.ClientProject(path); got != want {
			t.Errorf("ClientProject(%s) = %q, want %q", filepath.Base(path), got, want)
		}
	}
}

func TestParseClientSecret(t *testing.T) {
	good := `{"installed":{"client_id":"id.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://a","token_uri":"https://t"}}`
	cfg, err := auth.ParseClientSecret([]byte(good), []string{"scope"})
	if err != nil {
		t.Fatalf("ParseClientSecret: %v", err)
	}
	if cfg.ClientID != "id.apps.googleusercontent.com" || cfg.Endpoint.AuthURL != "https://a" {
		t.Fatalf("parsed wrong: %+v", cfg)
	}

	// Endpoints default when the file omits them.
	cfg, err = auth.ParseClientSecret([]byte(`{"installed":{"client_id":"x"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint.AuthURL != auth.GoogleAuthURL || cfg.Endpoint.TokenURL != auth.GoogleTokenURL {
		t.Fatalf("endpoints did not default: %+v", cfg.Endpoint)
	}

	// A Web client is the common mistake and gets its own message.
	_, err = auth.ParseClientSecret([]byte(`{"web":{"client_id":"x"}}`), nil)
	if !errors.Is(err, auth.ErrNotDesktopClient) {
		t.Fatalf("web client error = %v, want ErrNotDesktopClient", err)
	}
	if !strings.Contains(err.Error(), "Desktop app client instead") {
		t.Fatalf("web client error does not say what to do: %v", err)
	}

	for _, bad := range []string{`not json`, `{}`, `{"installed":{}}`} {
		if _, err := auth.ParseClientSecret([]byte(bad), nil); err == nil {
			t.Fatalf("ParseClientSecret(%q) succeeded", bad)
		}
	}
}

// TestLoginUsesLoopbackLiteralAndPKCE checks the two properties RFC 8252
// requires, by driving the whole flow against a fake Google.
func TestLoginUsesLoopbackLiteralAndPKCE(t *testing.T) {
	var gotRedirect, gotChallengeMethod, gotChallenge string

	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotRedirect = r.Form.Get("redirect_uri")
		if r.Form.Get("code_verifier") == "" {
			t.Error("token exchange carried no code_verifier")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`))
	}))
	defer token.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &oauth2.Config{
		ClientID: "id", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.test/auth", TokenURL: token.URL, AuthStyle: oauth2.AuthStyleInParams},
	}

	// The "browser": parse the URL, check PKCE, then call back.
	browser := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		gotChallengeMethod = q.Get("code_challenge_method")
		gotChallenge = q.Get("code_challenge")
		go func() {
			cb := q.Get("redirect_uri") + "?state=" + url.QueryEscape(q.Get("state")) + "&code=authcode"
			resp, err := http.Get(cb) //nolint:noctx // test callback
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}

	tok, err := auth.Login(context.Background(), cfg, auth.LoginOptions{
		Listener: ln, OpenBrowser: browser, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok.RefreshToken != "rt" {
		t.Fatalf("refresh token = %q", tok.RefreshToken)
	}
	if gotChallengeMethod != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256 (RFC 8252 §8.1)", gotChallengeMethod)
	}
	if gotChallenge == "" {
		t.Fatal("no code_challenge was sent")
	}
	if !strings.HasPrefix(gotRedirect, "http://127.0.0.1:") {
		t.Fatalf("redirect_uri = %q, want the 127.0.0.1 literal (RFC 8252 §7.3), never localhost", gotRedirect)
	}
	if strings.Contains(gotRedirect, "localhost") {
		t.Fatalf("redirect_uri used localhost: %q", gotRedirect)
	}
}

func TestLoginRejectsAStateMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://a", TokenURL: "https://t"}}
	browser := func(raw string) error {
		u, _ := url.Parse(raw)
		go func() {
			cb := u.Query().Get("redirect_uri") + "?state=wrong&code=authcode"
			resp, err := http.Get(cb) //nolint:noctx // test callback
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	_, err = auth.Login(context.Background(), cfg, auth.LoginOptions{
		Listener: ln, OpenBrowser: browser, Timeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("Login accepted a mismatched state: %v", err)
	}
}

// TestNoBrowserPrintsThePortToForward: the SSH case, which the standard
// says to give people rather than let them discover.
func TestNoBrowserPrintsThePortToForward(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	var out strings.Builder
	cfg := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://a", TokenURL: "https://t"}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = auth.Login(ctx, cfg, auth.LoginOptions{Listener: ln, NoBrowser: true, Out: &out, Timeout: time.Minute})

	s := out.String()
	if !strings.Contains(s, "ssh -L") {
		t.Fatalf("--no-browser did not print the port-forward line:\n%s", s)
	}
	if !strings.Contains(s, "127.0.0.1:"+itoa(port)) {
		t.Fatalf("--no-browser did not print the actual port %d:\n%s", port, s)
	}
}

func TestInspectAndRevoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "revoke") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scope":"a b","email":"someone@example.test","expires_in":"3599","aud":"id"}`))
	}))
	defer srv.Close()

	oldInfo, oldRevoke := auth.TokenInfoURL, auth.RevokeURL
	auth.TokenInfoURL, auth.RevokeURL = srv.URL+"/tokeninfo", srv.URL+"/revoke"
	defer func() { auth.TokenInfoURL, auth.RevokeURL = oldInfo, oldRevoke }()

	info, err := auth.Inspect(context.Background(), srv.Client(), "at")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(info.Scopes) != 2 || info.Email != "someone@example.test" || info.ExpiresIn != 3599*time.Second {
		t.Fatalf("Inspect = %+v", info)
	}
	if err := auth.Revoke(context.Background(), srv.Client(), "rt"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestInspectDoesNotLeakTheTokenOnATransportError.
//
// The access token is a query parameter on the tokeninfo URL, and a
// transport failure stringifies the whole URL. `doctor` prints that
// error, and `doctor` output is what a user pastes into a bug report.
func TestInspectDoesNotLeakTheTokenOnATransportError(t *testing.T) {
	const token = "ya29.not-a-real-one"
	// A client that always fails the way a dead network does.
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	})}
	_, err := auth.Inspect(context.Background(), client, token)
	if err == nil {
		t.Fatal("a dead transport produced no error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the access token is in the error text: %v", err)
	}
	if strings.Contains(err.Error(), "access_token") {
		t.Fatalf("the token-bearing URL is in the error text: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeGoogle is a token endpoint and a browser that calls back, so the
// whole flow runs in-process. It records the authorization URL.
func fakeGoogle(t *testing.T, tokenBody string) (*oauth2.Config, net.Listener, func(string) error, *url.Values) {
	t.Helper()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenBody))
	}))
	t.Cleanup(token.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{
		ClientID: "id", Scopes: []string{scopes.Modify},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.test/auth", TokenURL: token.URL, AuthStyle: oauth2.AuthStyleInParams},
	}
	seen := &url.Values{}
	browser := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		*seen = q
		go func() {
			cb := q.Get("redirect_uri") + "?state=" + url.QueryEscape(q.Get("state")) + "&code=authcode"
			resp, err := http.Get(cb) //nolint:noctx,gosec // test callback
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	return cfg, ln, browser, seen
}

// §10: consent is forced only when the scope set is not already
// granted, because each forced consent mints a refresh token and an
// account holds only 100 per client.
func TestConsentOnlyWhenScopesGrow(t *testing.T) {
	tests := []struct {
		name    string
		opts    auth.LoginOptions
		consent bool
	}{
		{"first login", auth.LoginOptions{}, true},
		{"covered", auth.LoginOptions{Granted: []string{scopes.Modify}}, false},
		{"covered by a wider scope", auth.LoginOptions{Granted: []string{scopes.Full}}, false},
		{"grown", auth.LoginOptions{Granted: []string{scopes.Readonly}}, true},
		{"forced", auth.LoginOptions{Granted: []string{scopes.Modify}, ForceConsent: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, ln, browser, seen := fakeGoogle(t, `{"access_token":"at","refresh_token":"rt","token_type":"Bearer"}`)
			tt.opts.Listener, tt.opts.OpenBrowser, tt.opts.Timeout = ln, browser, 10*time.Second
			if _, err := auth.Login(context.Background(), cfg, tt.opts); err != nil {
				t.Fatal(err)
			}
			if got := seen.Get("prompt") == "consent"; got != tt.consent {
				t.Errorf("prompt=consent sent = %t, want %t", got, tt.consent)
			}
			if seen.Get("access_type") != "offline" {
				t.Error("access_type=offline missing")
			}
		})
	}
}

func TestNoRefreshTokenAfterForcedConsentIsAnError(t *testing.T) {
	cfg, ln, browser, _ := fakeGoogle(t, `{"access_token":"at","token_type":"Bearer"}`)
	_, err := auth.Login(context.Background(), cfg, auth.LoginOptions{Listener: ln, OpenBrowser: browser, Timeout: 10 * time.Second})
	if !errors.Is(err, auth.ErrNoRefreshToken) {
		t.Fatalf("err = %v, want ErrNoRefreshToken", err)
	}
}

func TestNoRefreshTokenWithoutConsentKeepsTheStoredOne(t *testing.T) {
	cfg, ln, browser, _ := fakeGoogle(t, `{"access_token":"at","token_type":"Bearer"}`)
	tok, err := auth.Login(context.Background(), cfg, auth.LoginOptions{
		Listener: ln, OpenBrowser: browser, Timeout: 10 * time.Second, Granted: []string{scopes.Modify},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "" || tok.AccessToken != "at" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestLoginReportsADeniedAuthorization(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://a", TokenURL: "https://t"}}
	for _, query := range []string{"&error=access_denied", ""} {
		browser := func(raw string) error {
			u, _ := url.Parse(raw)
			go func() {
				cb := u.Query().Get("redirect_uri") + "?state=" + url.QueryEscape(u.Query().Get("state")) + query
				resp, err := http.Get(cb) //nolint:noctx,gosec // test callback
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return errors.New("no browser here")
		}
		ln2 := ln
		if query == "" {
			if ln2, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
				t.Fatal(err)
			}
		}
		_, err := auth.Login(context.Background(), cfg, auth.LoginOptions{Listener: ln2, OpenBrowser: browser, Timeout: 5 * time.Second})
		if err == nil {
			t.Errorf("callback %q accepted", query)
		}
	}
}

func TestLoginTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://a", TokenURL: "https://t"}}
	_, err = auth.Login(context.Background(), cfg, auth.LoginOptions{Listener: ln, NoBrowser: true, Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadClientSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, []byte(`{"installed":{"client_id":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.LoadClientSecret(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.LoadClientSecret(filepath.Join(dir, "missing.json"), nil); err == nil {
		t.Fatal("a missing file loaded")
	}
}

func TestRevokeReportsARefusal(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		body = r.Form.Get("token")
		if r.URL.RawQuery != "" {
			t.Errorf("the token traveled in the URL: %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()
	old := auth.RevokeURL
	auth.RevokeURL = srv.URL
	defer func() { auth.RevokeURL = old }()
	if err := auth.Revoke(context.Background(), nil, "rt"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if body != "rt" {
		t.Fatalf("revoked %q", body)
	}
}

func TestInspectReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	old := auth.TokenInfoURL
	auth.TokenInfoURL = srv.URL
	defer func() { auth.TokenInfoURL = old }()
	if _, err := auth.Inspect(context.Background(), nil, "at"); err == nil {
		t.Fatal("a 400 inspected fine")
	}
}
