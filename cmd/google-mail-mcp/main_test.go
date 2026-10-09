package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/mmedum/google-mail-mcp/v2/internal/auth"
	"github.com/mmedum/google-mail-mcp/v2/internal/credentials"
	"github.com/mmedum/google-mail-mcp/v2/internal/scopes"
	"github.com/mmedum/google-mail-mcp/v2/internal/userconfig"
)

// fakeClientID is shaped like an OAuth client id so the masking bites.
// It is built by concatenation because the leak gate rightly flags the
// shape wherever it appears whole, and no allow-list entry should exist
// for it.
const fakeClientID = "123456789012-" + "abcdefghijklmnopqrstuvwxyz012345" + ".apps.googleusercontent.com"

// fakeProject is the Cloud project the fake client JSON names.
const fakeProject = "example-project"

const (
	account      = "someone.private@example.com"
	refreshToken = "1//test-refresh-token-value"
)

// google is a fake of every Google endpoint the commands reach.
type google struct {
	srv *httptest.Server
	mu  sync.Mutex
	// issueRefresh makes the token endpoint return a refresh token.
	issueRefresh bool
	granted      string
	revoked      []string
	authURLs     []url.Values
}

func newGoogle(t *testing.T) *google {
	t.Helper()
	g := &google{issueRefresh: true, granted: scopes.Modify}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if g.issueRefresh {
				_, _ = fmt.Fprintf(w, `{"access_token":"at","refresh_token":%q,"token_type":"Bearer","expires_in":3600}`, refreshToken)
				return
			}
			_, _ = fmt.Fprint(w, `{"access_token":"at","token_type":"Bearer","expires_in":3600}`)
		case "/tokeninfo":
			_, _ = fmt.Fprintf(w, `{"scope":%q,"expires_in":"3599"}`, g.granted)
		case "/revoke":
			_ = r.ParseForm()
			g.revoked = append(g.revoked, r.Form.Get("token"))
		case "/gmail/v1/users/me/profile":
			_, _ = fmt.Fprintf(w, `{"emailAddress":%q,"messagesTotal":1}`, account)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)

	prevInfo, prevRevoke := auth.TokenInfoURL, auth.RevokeURL
	auth.TokenInfoURL, auth.RevokeURL = g.srv.URL+"/tokeninfo", g.srv.URL+"/revoke"
	prevOpen := openBrowser
	openBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		g.mu.Lock()
		g.authURLs = append(g.authURLs, q)
		g.mu.Unlock()
		go func() {
			cb := q.Get("redirect_uri") + "?state=" + url.QueryEscape(q.Get("state")) + "&code=c"
			resp, err := http.Get(cb) //nolint:noctx,gosec // test callback
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() {
		auth.TokenInfoURL, auth.RevokeURL = prevInfo, prevRevoke
		openBrowser = prevOpen
		keyringBackend.(*packageKeyring).reset()
	})
	return g
}

// home gives the test its own home and config directory, and an
// environment pointing at the fake.
func home(t *testing.T, g *google) map[string]string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	env := map[string]string{"GMAIL_CONFIG_DIR": filepath.Join(h, "cfg")}
	if g != nil {
		env["GMAIL_API_BASE"] = g.srv.URL
	}
	return env
}

// clientSecret writes a Desktop client JSON under a name shaped like the
// console's download, which carries the client id.
func clientSecret(t *testing.T, env map[string]string, g *google) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(env["GMAIL_CONFIG_DIR"]), "Downloads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "client_secret_"+fakeClientID+".json")
	body := fmt.Sprintf(`{"installed":{"client_id":"`+fakeClientID+`",
		"client_secret":"s","token_uri":%q,"project_id":%q}}`, g.srv.URL+"/token", fakeProject)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env["GMAIL_CLIENT_SECRET"] = p
	return p
}

type result struct {
	code           int
	stdout, stderr string
}

func runWith(env map[string]string, stdin io.Reader, args ...string) result {
	var out, errb bytes.Buffer
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code := run(args, stdin, &out, &errb, func(k string) string { return env[k] })
	return result{code, out.String(), errb.String()}
}

func TestHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		r := runWith(nil, nil, arg)
		if r.code != 0 || !strings.Contains(r.stdout, "Usage:") {
			t.Errorf("%s: code %d, stdout %q", arg, r.code, r.stdout)
		}
	}
}

func TestAnUnknownCommandPrintsUsageToStderr(t *testing.T) {
	r := runWith(nil, nil, "statsu")
	if r.code == 0 || !strings.Contains(r.stderr, "Usage:") || !strings.Contains(r.stderr, `unknown command "statsu"`) {
		t.Errorf("code %d, stderr %q", r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout carried %q", r.stdout)
	}
}

func TestVersion(t *testing.T) {
	r := runWith(nil, nil, "--version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "google-mail-mcp ") {
		t.Errorf("code %d, stdout %q", r.code, r.stdout)
	}
}

// TestEveryReleaseTargetEmbedsZoneData holds the time/tzdata import. A
// test cannot watch it work: on a machine with zone files the time
// package reads them, even with ZONEINFO pointed at an empty directory.
// So the build of each target the release makes is read for it instead.
func TestEveryReleaseTargetEmbedsZoneData(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		cmd := exec.Command("go", "list", "-deps", ".")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list for %s: %v", goos, err)
		}
		if !slices.Contains(strings.Fields(string(out)), "time/tzdata") {
			t.Errorf("the %s build does not embed time/tzdata, so it refuses every zone on a system with no zone files", goos)
		}
	}
}

func TestArgumentErrors(t *testing.T) {
	env := home(t, nil)
	env["GMAIL_READ_ONLY"] = "maybe"
	if r := runWith(env, nil, "status"); r.code != 1 || !strings.Contains(r.stderr, "GMAIL_READ_ONLY") {
		t.Errorf("bad setting: %+v", r)
	}
	delete(env, "GMAIL_READ_ONLY")
	if r := runWith(env, nil, "status", "extra"); r.code != 1 {
		t.Errorf("extra argument: %+v", r)
	}
	if r := runWith(env, nil, "status", "-nope"); r.code != 2 {
		t.Errorf("unknown flag: %+v", r)
	}
	if r := runWith(env, nil, "doctor", "-h"); r.code != 0 {
		t.Errorf("subcommand -h: %+v", r)
	}
}

func TestDumpSchemas(t *testing.T) {
	env := home(t, nil)
	r := runWith(env, nil, "--dump-schemas")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	var dump map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &dump); err != nil || dump["server"] != "google-mail-mcp" {
		t.Errorf("dump = %v, %v", dump, err)
	}
}

// The serve path, signed out: it starts, answers over stdout with
// frames only, and exits 0 when the client closes stdin.
func TestServeSignedOutAndDisconnect(t *testing.T) {
	env := home(t, nil)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"serve"}, inR, outW, &stderr, func(k string) string { return env[k] })
		_ = outW.Close()
	}()
	send := func(line string) {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	await := func(id float64) {
		for sc.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(sc.Bytes(), &frame); err != nil || frame["jsonrpc"] != "2.0" {
				t.Fatalf("stdout carried a line that is not a JSON-RPC frame: %q", sc.Text())
			}
			if frame["id"] == id {
				return
			}
		}
		t.Fatalf("no answer to request %v", id)
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	await(1)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	await(2)
	// A request in flight when stdin closes: still an ordinary exit.
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	_ = inW.Close()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no usable credentials") {
		t.Errorf("stderr does not say why tools will fail:\n%s", stderr.String())
	}
}

func TestIsDisconnect(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&jsonrpc.Error{Code: codeServerClosing, Message: "server is closing: EOF"}, true},
		{fmt.Errorf("wrapped: %w", &jsonrpc.Error{Code: codeClientClosing}), true},
		{&jsonrpc.Error{Code: -32600, Message: "EOF"}, false},
		{io.EOF, true},
		{context.Canceled, true},
		{errors.New("EOF"), false},
	}
	for _, tt := range tests {
		if got := isDisconnect(tt.err); got != tt.want {
			t.Errorf("isDisconnect(%v) = %t", tt.err, got)
		}
	}
}

func TestReadWriteClosers(t *testing.T) {
	if rc := readCloser(strings.NewReader("x")); rc.Close() != nil {
		t.Error("reader close failed")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	// Closed before the directory is removed: Windows cannot delete an
	// open file.
	t.Cleanup(func() { _ = f.Close() })
	if readCloser(f) != io.ReadCloser(f) || writeCloser(f) != io.WriteCloser(f) {
		t.Error("a real closer was wrapped")
	}
	if wc := writeCloser(&bytes.Buffer{}); wc.Close() != nil {
		t.Error("writer close failed")
	}
}

// The whole account lifecycle against the fake: login, status, doctor,
// a second login that keeps the token, logout that revokes it.
func TestLoginStatusDoctorLogout(t *testing.T) {
	g := newGoogle(t)
	env := home(t, g)
	clientSecret(t, env, g)

	r := runWith(env, nil, "login")
	if r.code != 0 {
		t.Fatalf("login: %+v", r)
	}
	// The authorization URL carries the client id, and has to: the
	// person opens it. Everything else is masked.
	var printed []string
	for line := range strings.SplitSeq(r.stdout+r.stderr, "\n") {
		if !strings.HasPrefix(line, "https://") {
			printed = append(printed, line)
		}
	}
	for _, leak := range []string{"someone.private", "123456789012"} {
		if strings.Contains(strings.Join(printed, "\n"), leak) {
			t.Errorf("login printed %q:\n%s%s", leak, r.stdout, r.stderr)
		}
	}
	if !strings.Contains(r.stdout, "…@example.com") {
		t.Errorf("login did not name the account's domain:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "Google issued a new refresh token") {
		t.Errorf("login did not say a refresh token was issued:\n%s", r.stdout)
	}
	if got, _ := keyringBackend.Get(credentials.ServiceName, "default"); got != refreshToken {
		t.Fatalf("keyring holds %q", got)
	}
	uc, err := userconfig.Dir(env["GMAIL_CONFIG_DIR"]).Load("default")
	if err != nil || uc.AccountEmail != account || uc.TokenStore != "keyring" || len(uc.Scopes) != 1 || uc.ClientProject != fakeProject {
		t.Fatalf("profile = %+v, %v", uc, err)
	}
	if g.authURLs[len(g.authURLs)-1].Get("prompt") != "consent" {
		t.Error("the first login did not ask for consent")
	}

	r = runWith(env, nil, "status")
	if r.code != 0 || !strings.Contains(r.stdout, "token store:    keyring") || strings.Contains(r.stdout, "someone.private") ||
		strings.Contains(r.stdout, "123456789012") {
		t.Errorf("status:\n%s", r.stdout)
	}
	r = runWith(env, nil, "status", "--json")
	var st statusReport
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, r.stdout)
	}
	if !st.Credentials.Resolved || deref(st.Account) != "…@example.com" || st.SchemaVersion != 1 || len(st.Scopes.Missing) != 0 {
		t.Errorf("status --json = %+v", st)
	}
	if strings.Contains(r.stdout, "123456789012") {
		t.Errorf("status --json carries the client id:\n%s", r.stdout)
	}

	r = runWith(env, nil, "doctor")
	if r.code != 0 || !strings.Contains(r.stdout, "No problems found") {
		t.Errorf("doctor:\n%s", r.stdout)
	}

	// Covered scopes and no new refresh token: the stored one is kept
	// and consent is not forced.
	g.mu.Lock()
	g.issueRefresh = false
	g.mu.Unlock()
	r = runWith(env, nil, "login")
	if r.code != 0 {
		t.Fatalf("second login: %+v", r)
	}
	if g.authURLs[len(g.authURLs)-1].Get("prompt") == "consent" {
		t.Error("a login with every scope granted forced consent")
	}
	if got, _ := keyringBackend.Get(credentials.ServiceName, "default"); got != refreshToken {
		t.Errorf("the stored token was lost: %q", got)
	}
	if !strings.Contains(r.stdout, "Google issued no new refresh token; the one stored before is kept.") {
		t.Errorf("the second login did not say the stored token was kept:\n%s", r.stdout)
	}

	// A flag that grows the scopes shows in status and doctor.
	env["GMAIL_ENABLE_DESTRUCTIVE"] = "true"
	r = runWith(env, nil, "status", "--json")
	_ = json.Unmarshal([]byte(r.stdout), &st)
	if len(st.Scopes.Missing) != 1 || st.Scopes.Missing[0] != scopes.Full {
		t.Errorf("missing = %v", st.Scopes.Missing)
	}
	r = runWith(env, nil, "doctor")
	if r.code != 1 || !strings.Contains(r.stdout, "not granted") {
		t.Errorf("doctor with a grown scope set:\n%s", r.stdout)
	}

	// Logging in again asks for consent to the wider scope, and then
	// nothing is missing.
	g.mu.Lock()
	g.granted, g.issueRefresh = scopes.Full, true
	g.mu.Unlock()
	r = runWith(env, nil, "login")
	if r.code != 0 || g.authURLs[len(g.authURLs)-1].Get("prompt") != "consent" ||
		!strings.Contains(g.authURLs[len(g.authURLs)-1].Get("scope"), scopes.Full) {
		t.Fatalf("login with a grown scope set: %+v %v", r, g.authURLs[len(g.authURLs)-1])
	}
	r = runWith(env, nil, "doctor")
	if r.code != 0 || strings.Contains(r.stdout, "wider than") {
		t.Errorf("doctor after the login:\n%s", r.stdout)
	}

	// The flag off again: the token is wider than needed, which works
	// and is said.
	delete(env, "GMAIL_ENABLE_DESTRUCTIVE")
	r = runWith(env, nil, "doctor")
	if r.code != 0 || !strings.Contains(r.stdout, "wider than this configuration needs: "+scopes.Full) {
		t.Errorf("doctor with a wider token:\n%s", r.stdout)
	}

	// Another profile of the same account in the same project loses its
	// grant with this one, and logout names it; another account's does not.
	dir := userconfig.Dir(env["GMAIL_CONFIG_DIR"])
	for name, c := range map[string]userconfig.Config{
		"work":     {AccountEmail: account, ClientProject: fakeProject},
		"personal": {AccountEmail: "other@example.com", ClientProject: fakeProject},
	} {
		if err := dir.Save(name, c); err != nil {
			t.Fatal(err)
		}
	}
	r = runWith(env, nil, "logout")
	if r.code != 0 || !strings.Contains(r.stdout, "Revoked") {
		t.Fatalf("logout: %+v", r)
	}
	if !strings.Contains(r.stdout, "signed out too: work\n") {
		t.Errorf("logout did not name the profile sharing the grant, or named another:\n%s", r.stdout)
	}
	if len(g.revoked) != 1 || g.revoked[0] != refreshToken {
		t.Errorf("revoked %v", g.revoked)
	}
	if _, err := keyringBackend.Get(credentials.ServiceName, "default"); err == nil {
		t.Error("the token is still in the keyring")
	}
	if _, err := userconfig.Dir(env["GMAIL_CONFIG_DIR"]).Load("default"); !errors.Is(err, userconfig.ErrNotFound) {
		t.Errorf("profile after logout: %v", err)
	}
	r = runWith(env, nil, "logout")
	if r.code != 0 || !strings.Contains(r.stdout, "No stored token") || strings.Contains(r.stdout, "signed out too") {
		t.Errorf("second logout: %+v", r)
	}
}

func TestNoBrowserPrintsTheForwardingLine(t *testing.T) {
	g := newGoogle(t)
	env := home(t, g)
	clientSecret(t, env, g)
	prev := loginTimeout
	loginTimeout = 200 * time.Millisecond
	defer func() { loginTimeout = prev }()
	r := runWith(env, nil, "login", "--no-browser")
	if r.code == 0 || !strings.Contains(r.stdout, "ssh -L") {
		t.Errorf("--no-browser: %+v", r)
	}
	if len(g.authURLs) != 0 {
		t.Error("--no-browser opened a browser")
	}
}

// The coordinator's rule: logout leaves an environment token alone.
func TestLogoutLeavesAnEnvironmentTokenAlone(t *testing.T) {
	g := newGoogle(t)
	env := home(t, g)
	env[credentials.EnvVar] = "1//env-supplied-token"
	r := runWith(env, nil, "logout")
	if r.code != 0 || !strings.Contains(r.stderr, credentials.EnvVar) {
		t.Errorf("logout: %+v", r)
	}
	if len(g.revoked) != 0 {
		t.Errorf("revoked %v", g.revoked)
	}
}

func TestDoctorAndLoginSignedOut(t *testing.T) {
	env := home(t, nil)
	r := runWith(env, nil, "doctor")
	if r.code != 1 || !strings.Contains(r.stdout, "[FAIL] OAuth Desktop client JSON") {
		t.Errorf("doctor with nothing set up:\n%s", r.stdout)
	}
	r = runWith(env, nil, "login")
	if r.code != 1 || !strings.Contains(r.stderr, "Desktop app") {
		t.Errorf("login with no client JSON: %+v", r)
	}
	r = runWith(env, nil, "status")
	if r.code != 0 || !strings.Contains(r.stdout, "token store:    none") {
		t.Errorf("status signed out:\n%s", r.stdout)
	}
}

func TestDoctorWithoutAToken(t *testing.T) {
	g := newGoogle(t)
	env := home(t, g)
	clientSecret(t, env, g)
	r := runWith(env, nil, "doctor")
	if r.code != 1 || !strings.Contains(r.stdout, "[FAIL] refresh token") || strings.Contains(r.stdout, "123456789012") {
		t.Errorf("doctor:\n%s", r.stdout)
	}
}

func TestServeSignedIn(t *testing.T) {
	g := newGoogle(t)
	env := home(t, g)
	clientSecret(t, env, g)
	env[credentials.EnvVar] = refreshToken
	r := runWith(env, strings.NewReader(""), "serve")
	if r.code != 0 || strings.Contains(r.stderr, "no usable credentials") {
		t.Errorf("serve: %+v", r)
	}
}

func TestFailRedacts(t *testing.T) {
	var b bytes.Buffer
	fail(&b, "read /x/client_secret_123456789012-abcdefghijk.json for %s", account)
	if strings.Contains(b.String(), "123456789012") || strings.Contains(b.String(), "someone.private") {
		t.Errorf("fail printed %q", b.String())
	}
}
