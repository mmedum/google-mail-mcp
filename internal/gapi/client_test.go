package gapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fake is a Gmail stand-in: each request is answered by the next
// response in the script, and every request is recorded.
type fake struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	script   []func(w http.ResponseWriter, r *http.Request)
	requests []*http.Request
	bodies   []string
}

func newFake(t *testing.T, script ...func(w http.ResponseWriter, r *http.Request)) *fake {
	t.Helper()
	f := &fake{t: t, script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.requests)
		f.requests = append(f.requests, r)
		buf := new(strings.Builder)
		if r.Body != nil {
			b := make([]byte, 1<<16)
			k, _ := r.Body.Read(b)
			buf.Write(b[:k])
		}
		f.bodies = append(f.bodies, buf.String())
		f.mu.Unlock()
		if n >= len(f.script) {
			t.Errorf("unexpected request %d: %s %s", n+1, r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		f.script[n](w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}
}

func replyAfter(status int, retryAfter, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}
}

// hangUp drops the connection after reading the request, which is what
// a reset after the write looks like.
func hangUp(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
}

func googleErr(code int, reason, message string) string {
	return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"errors":[{"reason":%q}]}}`, code, message, reason)
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func client(t *testing.T, f *fake, opts ...func(*Options)) (*Client, *sleeps) {
	t.Helper()
	sl := &sleeps{}
	o := Options{
		BaseURL:     f.srv.URL,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-access-token"}),
		Sleep:       sl.sleep,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return New(o), sl
}

var (
	getMessage  = Call{ID: "gmail.users.messages.get", Method: http.MethodGet, Path: "messages/{}", Args: []string{"abc"}}
	listLabels  = Call{ID: "gmail.users.labels.list", Method: http.MethodGet, Path: "labels"}
	createDraft = Call{ID: "gmail.users.drafts.create", Method: http.MethodPost, Path: "drafts", Body: map[string]string{"k": "v"}}
	trash       = Call{ID: "gmail.users.messages.trash", Method: http.MethodPost, Path: "messages/{}/trash", Args: []string{"abc"},
		Repeatable: "trashing a message twice leaves it in trash once"}
	sendDraft = Call{ID: "gmail.users.drafts.send", Method: http.MethodPost, Path: "drafts/send", Body: map[string]string{"id": "d"}}
)

func classOf(t *testing.T, err error) Class {
	t.Helper()
	c, ok := ClassOf(err)
	if !ok {
		t.Fatalf("error is unclassified: %v", err)
	}
	return c
}

func TestDoDecodesAndBuildsTheRequest(t *testing.T) {
	f := newFake(t, reply(200, `{"id":"abc","threadId":"t1"}`))
	c, _ := client(t, f)
	call := getMessage
	call.Args = []string{"a/b c"}
	call.Query = url.Values{"format": {"metadata"}}
	var out struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	ctx := WithCounter(context.Background())
	if err := c.Do(ctx, call, &out); err != nil {
		t.Fatal(err)
	}
	if out.ThreadID != "t1" {
		t.Errorf("decoded %+v", out)
	}
	r := f.requests[0]
	if r.URL.EscapedPath() != "/gmail/v1/users/me/messages/a%2Fb%20c" {
		t.Errorf("path = %s", r.URL.EscapedPath())
	}
	if r.URL.Query().Get("format") != "metadata" || r.URL.Query().Get("prettyPrint") != "false" {
		t.Errorf("query = %s", r.URL.RawQuery)
	}
	if r.Header.Get("Authorization") != "Bearer test-access-token" {
		t.Errorf("authorization = %q", r.Header.Get("Authorization"))
	}
	if Requests(ctx) != 1 || UnitsSpent(ctx) != 20 {
		t.Errorf("counted %d requests, %d units", Requests(ctx), UnitsSpent(ctx))
	}
}

func TestDoSendsTheBody(t *testing.T) {
	f := newFake(t, reply(200, `{}`))
	c, _ := client(t, f)
	if err := c.Do(context.Background(), createDraft, nil); err != nil {
		t.Fatal(err)
	}
	if f.bodies[0] != `{"k":"v"}` || f.requests[0].Header.Get("Content-Type") == "" {
		t.Errorf("body %q, content type %q", f.bodies[0], f.requests[0].Header.Get("Content-Type"))
	}
}

func TestProgrammingErrorsAreRefusedBeforeSending(t *testing.T) {
	f := newFake(t)
	c, _ := client(t, f)
	for name, call := range map[string]Call{
		"unpriced":        {ID: "gmail.users.watch.nothing", Method: http.MethodGet, Path: "x"},
		"method":          {ID: "gmail.users.labels.list", Method: "TRACE", Path: "labels"},
		"too many args":   {ID: "gmail.users.labels.list", Method: http.MethodGet, Path: "labels", Args: []string{"x"}},
		"too few args":    {ID: "gmail.users.messages.get", Method: http.MethodGet, Path: "messages/{}"},
		"repeatable send": {ID: "gmail.users.drafts.send", Method: http.MethodPost, Path: "drafts/send", Repeatable: "no"},
	} {
		if err := c.Do(context.Background(), call, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, arg := range []string{"", ".", ".."} {
		call := getMessage
		call.Args = []string{arg}
		if got := classOf(t, c.Do(context.Background(), call, nil)); got != ClassInvalid {
			t.Errorf("arg %q: class %s", arg, got)
		}
	}
	if f.count() != 0 {
		t.Errorf("%d requests reached Google", f.count())
	}
}

func TestWithoutWritesRefusesEveryWriteBeforeTheNetwork(t *testing.T) {
	f := newFake(t, reply(200, `{}`))
	c, _ := client(t, f)
	ctx := WithoutWrites(context.Background())
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		call := Call{ID: "gmail.users.labels.patch", Method: m, Path: "labels/{}", Args: []string{"L"}}
		err := c.Do(ctx, call, nil)
		if !errors.Is(err, ErrWriteForbidden) || classOf(t, err) != ClassBlocked {
			t.Errorf("%s under WithoutWrites: %v", m, err)
		}
	}
	if err := c.Do(ctx, listLabels, nil); err != nil {
		t.Errorf("a read under WithoutWrites: %v", err)
	}
	if f.count() != 1 {
		t.Errorf("%d requests reached Google, want only the read", f.count())
	}
	if !WritesForbidden(ctx) || WritesForbidden(context.Background()) {
		t.Error("WritesForbidden misreports")
	}
}

func TestTokenFailuresAreAuth(t *testing.T) {
	f := newFake(t)
	c, _ := client(t, f, func(o *Options) { o.TokenSource = nil })
	if got := classOf(t, c.Do(context.Background(), listLabels, nil)); got != ClassAuth {
		t.Errorf("no token source: %s", got)
	}
	cause := errors.New("revoked")
	c, _ = client(t, f, func(o *Options) { o.TokenSource = failingTokens{cause} })
	err := c.Do(context.Background(), listLabels, nil)
	if classOf(t, err) != ClassAuth || !errors.Is(err, cause) {
		t.Errorf("failing token source: %v", err)
	}
}

type failingTokens struct{ err error }

func (f failingTokens) Token() (*oauth2.Token, error) { return nil, f.err }

func TestStatusClasses(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   Class
	}{
		{400, googleErr(400, "invalidArgument", "Invalid id value"), ClassInvalid},
		{401, googleErr(401, "authError", "Invalid Credentials"), ClassAuth},
		{403, googleErr(403, "insufficientPermissions", "Request had insufficient authentication scopes."), ClassAuth},
		{403, `{"error":{"code":403,"message":"Request had insufficient authentication scopes."}}`, ClassAuth},
		{403, googleErr(403, "forbidden", "Delegation denied"), ClassForbidden},
		{403, googleErr(403, "dailyLimitExceeded", "Daily Limit Exceeded"), ClassRateLimited},
		{404, googleErr(404, "notFound", "Requested entity was not found."), ClassNotFound},
		{409, googleErr(409, "alreadyExists", "Label name exists or conflicts"), ClassConflict},
		{412, googleErr(412, "conditionNotMet", "Precondition"), ClassStale},
		{413, `not json`, ClassInvalid},
	}
	for _, tt := range tests {
		f := newFake(t, reply(tt.status, tt.body))
		c, _ := client(t, f)
		err := c.Do(context.Background(), listLabels, nil)
		if got := classOf(t, err); got != tt.want {
			t.Errorf("%d %s: class %s, want %s (%v)", tt.status, tt.body, got, tt.want, err)
		}
		if f.count() != 1 {
			t.Errorf("%d: %d attempts, want 1", tt.status, f.count())
		}
		var e *Error
		if errors.As(err, &e) && e.Status != tt.status {
			t.Errorf("status %d recorded as %d", tt.status, e.Status)
		}
	}
}

func TestReadsRetryTransientFailures(t *testing.T) {
	f := newFake(t,
		reply(500, googleErr(500, "backendError", "Backend Error")),
		reply(502, ``),
		reply(429, googleErr(429, "rateLimitExceeded", "Too many")),
		reply(200, `{}`))
	c, sl := client(t, f)
	ctx := WithCounter(context.Background())
	if err := c.Do(ctx, listLabels, nil); err != nil {
		t.Fatal(err)
	}
	if f.count() != 4 || len(sl.d) != 3 {
		t.Errorf("%d attempts, %d sleeps", f.count(), len(sl.d))
	}
	if Requests(ctx) != 4 || UnitsSpent(ctx) != 4 {
		t.Errorf("retries not counted: %d requests, %d units", Requests(ctx), UnitsSpent(ctx))
	}
}

func TestRetriesStopAtFourAttempts(t *testing.T) {
	var script []func(http.ResponseWriter, *http.Request)
	for range 4 {
		script = append(script, reply(500, ``))
	}
	f := newFake(t, script...)
	c, _ := client(t, f)
	if got := classOf(t, c.Do(context.Background(), listLabels, nil)); got != ClassUnavailable {
		t.Errorf("class %s", got)
	}
	if f.count() != 4 {
		t.Errorf("%d attempts, want 4", f.count())
	}
	f = newFake(t, reply(500, ``))
	c, _ = client(t, f, func(o *Options) { o.MaxRetries = -1 })
	_ = c.Do(context.Background(), listLabels, nil)
	if f.count() != 1 {
		t.Errorf("MaxRetries -1 made %d attempts", f.count())
	}
}

// §4.3 and §11: a POST that may not repeat is never sent twice after a
// failure that could have followed the write.
func TestAPostIsNotRepeatedAfterAnAmbiguousFailure(t *testing.T) {
	for _, status := range []int{500, 502, 504} {
		f := newFake(t, reply(status, ``))
		c, _ := client(t, f)
		if got := classOf(t, c.Do(context.Background(), createDraft, nil)); got != ClassAmbiguousOutcome {
			t.Errorf("%d: class %s", status, got)
		}
		if f.count() != 1 {
			t.Errorf("%d: %d attempts", status, f.count())
		}
	}
	f := newFake(t, hangUp(t))
	c, _ := client(t, f)
	if got := classOf(t, c.Do(context.Background(), createDraft, nil)); got != ClassAmbiguousOutcome {
		t.Errorf("reset after write: class %s", got)
	}
	if f.count() != 1 {
		t.Errorf("reset after write: %d attempts", f.count())
	}
}

func TestAPostIsRepeatedWhenTurnedAway(t *testing.T) {
	f := newFake(t,
		reply(429, googleErr(429, "rateLimitExceeded", "slow down")),
		reply(403, googleErr(403, "userRateLimitExceeded", "slow down")),
		reply(503, ``),
		reply(200, `{}`))
	c, _ := client(t, f)
	if err := c.Do(context.Background(), createDraft, nil); err != nil {
		t.Fatal(err)
	}
	if f.count() != 4 {
		t.Errorf("%d attempts", f.count())
	}
}

func TestADeclaredRepeatablePostRetriesLikeARead(t *testing.T) {
	f := newFake(t, reply(500, ``), hangUp(t), reply(200, `{}`))
	c, _ := client(t, f)
	if err := c.Do(context.Background(), trash, nil); err != nil {
		t.Fatal(err)
	}
	if f.count() != 3 {
		t.Errorf("%d attempts", f.count())
	}
}

// A send is never repeated, not even when Google turned it away.
func TestASendIsNeverRepeated(t *testing.T) {
	tests := []struct {
		reply func(http.ResponseWriter, *http.Request)
		want  Class
	}{
		{reply(429, googleErr(429, "rateLimitExceeded", "User-rate limit exceeded")), ClassRateLimited},
		{reply(503, ``), ClassUnavailable},
		{reply(500, ``), ClassAmbiguousOutcome},
		{hangUp(t), ClassAmbiguousOutcome},
	}
	for i, tt := range tests {
		f := newFake(t, tt.reply)
		c, _ := client(t, f)
		if got := classOf(t, c.Do(context.Background(), sendDraft, nil)); got != tt.want {
			t.Errorf("case %d: class %s, want %s", i, got, tt.want)
		}
		if f.count() != 1 {
			t.Errorf("case %d: a send was attempted %d times", i, f.count())
		}
	}
	// A connection never made is not repeated for a send either.
	c := New(Options{BaseURL: closedOrigin(t), TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}),
		Sleep: func(context.Context, time.Duration) error { t.Error("a send slept to retry"); return nil }})
	if got := classOf(t, c.Do(context.Background(), sendDraft, nil)); got != ClassUnavailable {
		t.Errorf("closed port: %s", got)
	}
}

func TestTheSendingLimitIsNotRetriedAndSaysSo(t *testing.T) {
	for _, body := range []string{
		googleErr(429, "rateLimitExceeded", "Mail sending limit exceeded"),
		googleErr(429, "", "Too many requests"), // a send's bare 429
	} {
		f := newFake(t, reply(429, body))
		c, _ := client(t, f)
		err := c.Do(context.Background(), sendDraft, nil)
		if classOf(t, err) != ClassRateLimited || !strings.Contains(err.Error(), "24 hours") {
			t.Errorf("%s: %v", body, err)
		}
	}
	// On a call that is not a send, the limit is still named when Google
	// names it, and not retried.
	f := newFake(t, reply(429, googleErr(429, "", "Daily user sending limit exceeded")))
	c, _ := client(t, f)
	err := c.Do(context.Background(), listLabels, nil)
	if !strings.Contains(err.Error(), "sending limit") || f.count() != 1 {
		t.Errorf("late sending limit: %v after %d attempts", err, f.count())
	}
}

func TestRetryAfterIsAFloor(t *testing.T) {
	f := newFake(t, replyAfter(429, "7", googleErr(429, "rateLimitExceeded", "wait")), reply(200, `{}`))
	c, sl := client(t, f)
	if err := c.Do(context.Background(), listLabels, nil); err != nil {
		t.Fatal(err)
	}
	if len(sl.d) != 1 || sl.d[0] < 7*time.Second {
		t.Errorf("slept %v, want at least 7s", sl.d)
	}
	f = newFake(t, replyAfter(429, "3600", googleErr(429, "rateLimitExceeded", "wait")))
	c, _ = client(t, f)
	err := c.Do(context.Background(), listLabels, nil)
	if classOf(t, err) != ClassRateLimited || f.count() != 1 || !strings.Contains(err.Error(), "1h0m0s") {
		t.Errorf("an hour's Retry-After: %v after %d attempts", err, f.count())
	}
}

func TestBackoff(t *testing.T) {
	for retry := 1; retry < 10; retry++ {
		d := backoff(retry, 0)
		if d < 0 || d > maxBackoff {
			t.Errorf("backoff(%d) = %s", retry, d)
		}
	}
	if d := backoff(1, 5*time.Second); d < 5*time.Second {
		t.Errorf("Retry-After not honored: %s", d)
	}
	if parseRetryAfter("") != 0 || parseRetryAfter("soon") != 0 || parseRetryAfter("-1") != 0 {
		t.Error("parseRetryAfter accepted nonsense")
	}
	if d := parseRetryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); d < 59*time.Minute {
		t.Errorf("HTTP-date Retry-After = %s", d)
	}
}

func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Error("a canceled sleep returned nil")
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
}

func TestACanceledSleepEndsTheCall(t *testing.T) {
	f := newFake(t, reply(500, ``))
	c, _ := client(t, f, func(o *Options) {
		o.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	})
	if got := classOf(t, c.Do(context.Background(), listLabels, nil)); got != ClassUnavailable {
		t.Errorf("class %s", got)
	}
}

func closedOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// A connection never made cannot have written, so even a create may be
// tried again.
func TestANeverMadeConnectionIsSafeToRepeat(t *testing.T) {
	var slept atomic.Int32
	c := New(Options{BaseURL: closedOrigin(t), TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}),
		Sleep: func(context.Context, time.Duration) error { slept.Add(1); return nil }})
	if got := classOf(t, c.Do(context.Background(), createDraft, nil)); got != ClassUnavailable {
		t.Errorf("class %s", got)
	}
	if slept.Load() != DefaultMaxRetries {
		t.Errorf("retried %d times", slept.Load())
	}
}

// §9.2: a search travels in the query string, and a transport error
// quotes the URL. Neither the query nor the token may survive.
func TestTransportErrorsCarryNoURL(t *testing.T) {
	const canary = "CANARY-search-term"
	f := newFake(t, hangUp(t), hangUp(t), hangUp(t), hangUp(t))
	c, _ := client(t, f)
	call := Call{ID: "gmail.users.messages.list", Method: http.MethodGet, Path: "messages", Query: url.Values{"q": {canary}}}
	err := c.Do(context.Background(), call, nil)
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{canary, "prettyPrint", "test-access-token", f.srv.URL} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error carries %q: %v", leak, err)
		}
	}
	if got := stripURL(`Get "https://x/y?q=secret": EOF`); strings.Contains(got, "secret") {
		t.Errorf("stripURL = %q", got)
	}
	if got := stripURL(`dial https://x/y?q=secret`); strings.Contains(got, "secret") {
		t.Errorf("stripURL at the end = %q", got)
	}
	if got := withoutURL(&url.Error{Op: "Get", URL: "https://x?q=secret"}); strings.Contains(got.Error(), "secret") {
		t.Errorf("withoutURL of a bare url.Error = %v", got)
	}
}

func TestAddressesAreMaskedInGoogleErrors(t *testing.T) {
	// A third party's domain names their employer, so it goes as well as
	// the local part. The error still says an address was there.
	f := newFake(t, reply(403, googleErr(403, "forbidden", "someone@example.com may not read mail of other@thirdparty.test")))
	c, _ := client(t, f)
	err := c.Do(context.Background(), listLabels, nil)
	for _, gone := range []string{"someone", "example", "other", "thirdparty"} {
		if strings.Contains(err.Error(), gone) {
			t.Errorf("%q survived: %v", gone, err)
		}
	}
	if !strings.Contains(err.Error(), "s…@e….com may not read mail of o…@t….test") {
		t.Errorf("error = %v", err)
	}
}

func TestReasonFromErrorInfoDetails(t *testing.T) {
	body := `{"error":{"code":403,"message":"no","details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`
	f := newFake(t, reply(403, body))
	c, _ := client(t, f)
	if got := classOf(t, c.Do(context.Background(), listLabels, nil)); got != ClassAuth {
		t.Errorf("class %s", got)
	}
}

func TestResponsesAreBounded(t *testing.T) {
	big := func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 1<<20)
		for range 33 {
			_, _ = fmt.Fprint(w, chunk)
		}
	}
	f := newFake(t, big)
	c, _ := client(t, f)
	if err := c.Do(context.Background(), listLabels, nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("a 33 MiB body: %v", err)
	}
}

func TestUndecodableSuccessIsUnavailable(t *testing.T) {
	f := newFake(t, reply(200, `not json`))
	c, _ := client(t, f)
	var out map[string]any
	if got := classOf(t, c.Do(context.Background(), listLabels, &out)); got != ClassUnavailable {
		t.Errorf("class %s", got)
	}
}

// The coordinator's rule: the access token goes only to allowed origins,
// and a redirect cannot carry it elsewhere.
func TestARedirectOffTheAllowlistIsRefused(t *testing.T) {
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		if r.Header.Get("Authorization") != "" {
			t.Error("the token followed the redirect")
		}
	}))
	defer elsewhere.Close()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	})
	c, _ := client(t, f)
	err := c.Do(context.Background(), listLabels, nil)
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("err = %v", err)
	}
	if reached.Load() {
		t.Error("the redirect was followed")
	}
}

func TestARedirectWithinTheAllowlistIsFollowed(t *testing.T) {
	f := newFake(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/gmail/v1/users/me/labels2", http.StatusFound)
		},
		reply(200, `{}`))
	c, _ := client(t, f)
	if err := c.Do(context.Background(), listLabels, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.checkRedirect(&http.Request{URL: mustParse(t, f.srv.URL)}, make([]*http.Request, maxRedirects)); err == nil {
		t.Error("an eleventh redirect was allowed")
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestOriginKeyAndAllowlist(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:8080"})
	tests := map[string]bool{
		"https://gmail.googleapis.com/gmail/v1":   true,
		"https://GMAIL.googleapis.com./gmail/v1":  true,
		"https://www.googleapis.com/x":            true,
		"https://oauth2.googleapis.com/revoke":    true,
		"http://127.0.0.1:8080/x":                 true,
		"http://127.0.0.1:8081/x":                 false,
		"http://gmail.googleapis.com/x":           false,
		"https://gmail.googleapis.com:8443/x":     false,
		"https://evil.example/x":                  false,
		"https://gmail.googleapis.com.evil.test/": false,
	}
	for raw, want := range tests {
		if got := c.allowURL(mustParse(t, raw)); got != want {
			t.Errorf("allowURL(%s) = %t, want %t", raw, got, want)
		}
	}
	// Defaults: the production origin, uploads on the same origin.
	d := New(Options{})
	if d.base != DefaultBaseURL || d.upload != DefaultBaseURL || d.maxRetries != DefaultMaxRetries {
		t.Errorf("defaults: %s %s %d", d.base, d.upload, d.maxRetries)
	}
}

func TestAnInjectedHTTPClientKeepsTheAllowlist(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/", http.StatusFound)
	})
	c, _ := client(t, f, func(o *Options) { o.HTTP = &http.Client{} })
	if err := c.Do(context.Background(), listLabels, nil); !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("err = %v", err)
	}
	if c.http.Timeout != DefaultTimeout {
		t.Errorf("timeout = %s", c.http.Timeout)
	}
}

// §4.9: the budget is units, and a canceled wait for units sends
// nothing.
func TestTheUnitBudgetWaitsAndCanBeCanceled(t *testing.T) {
	f := newFake(t, reply(200, `{}`))
	c, _ := client(t, f, func(o *Options) { o.UnitsPerMinute = 60 })
	if c.budget.Burst() < 100 {
		t.Fatalf("burst %d cannot admit a send", c.budget.Burst())
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Spend the burst, then ask for more under a canceled context.
	c.budget.AllowN(time.Now(), c.budget.Burst())
	cancel()
	if got := classOf(t, c.Do(ctx, getMessage, nil)); got != ClassUnavailable {
		t.Errorf("class %s", got)
	}
	if f.count() != 0 {
		t.Error("a call without budget was sent")
	}
}

func TestUnits(t *testing.T) {
	for id, want := range map[string]int{
		"gmail.users.labels.list": 1, "gmail.users.messages.list": 5, "gmail.users.threads.get": 40,
		"gmail.users.drafts.send": 100, "gmail.users.messages.batchModify": 50,
	} {
		if got, ok := Units(id); !ok || got != want {
			t.Errorf("Units(%s) = %d, %t", id, got, ok)
		}
	}
	if _, ok := Units("gmail.users.watch"); ok {
		t.Error("watch is priced; it is written off")
	}
	if maxUnitCost() != 100 {
		t.Errorf("max cost %d", maxUnitCost())
	}
}

func TestCountersWithoutACounter(t *testing.T) {
	ctx := context.Background()
	charge(ctx, 5)
	if Requests(ctx) != 0 || UnitsSpent(ctx) != 0 {
		t.Error("a context with no counter counted")
	}
}

// whenAmbiguous says whether a second attempt cannot apply the call
// twice. A send gets one try and no repeat.
func TestPolicy(t *testing.T) {
	tests := []struct {
		call Call
		want bool
	}{
		{listLabels, true},
		{Call{ID: "gmail.users.drafts.update", Method: http.MethodPut}, true},
		{Call{ID: "gmail.users.labels.patch", Method: http.MethodPatch}, true},
		{Call{ID: "gmail.users.messages.delete", Method: http.MethodDelete}, true},
		{createDraft, false},
		{trash, true},
		{sendDraft, false},
		{Call{ID: "gmail.users.messages.send", Method: http.MethodPost, Repeatable: "never"}, false},
	}
	for _, tt := range tests {
		p, err := New(Options{}).policyFor(tt.call)
		if err != nil {
			if !sendIDs[tt.call.ID] || tt.call.Repeatable == "" {
				t.Errorf("policyFor(%s %s): %v", tt.call.Method, tt.call.ID, err)
			}
			continue
		}
		if p.whenAmbiguous != tt.want {
			t.Errorf("policyFor(%s %s).whenAmbiguous = %t", tt.call.Method, tt.call.ID, p.whenAmbiguous)
		}
		if p.send != sendIDs[tt.call.ID] || (p.send && (p.tries != 1 || p.whenTurnedAway)) {
			t.Errorf("policyFor(%s %s) = %+v", tt.call.Method, tt.call.ID, p)
		}
		if !p.send && (p.tries != 1+DefaultMaxRetries || !p.whenTurnedAway) {
			t.Errorf("policyFor(%s %s) = %+v", tt.call.Method, tt.call.ID, p)
		}
	}
}

// A Retry-After too long to wait for inside one tool call is reported,
// whatever the status that carried it.
func TestALongRetryAfterIsReportedOnEveryStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		call   Call
		want   Class
	}{
		{429, listLabels, ClassRateLimited},
		{503, listLabels, ClassUnavailable},
		{503, createDraft, ClassUnavailable},
		{500, listLabels, ClassUnavailable},
		{502, trash, ClassUnavailable},
	} {
		f := newFake(t, replyAfter(tc.status, "3600", googleErr(tc.status, "backendError", "later")))
		c, sl := client(t, f)
		err := c.Do(context.Background(), tc.call, nil)
		if err == nil {
			t.Fatalf("%d %s: no error", tc.status, tc.call.ID)
		}
		if classOf(t, err) != tc.want || f.count() != 1 || len(sl.d) != 0 || !strings.Contains(err.Error(), "1h0m0s") {
			t.Errorf("%d %s: %v after %d attempts, slept %v", tc.status, tc.call.ID, err, f.count(), sl.d)
		}
	}
}

// The service reads rows concurrently through one client: the budget
// and the per-call counter hold up under that, and -race watches.
func TestConcurrentCallsShareTheBudgetAndCounter(t *testing.T) {
	const n = 8
	script := make([]func(http.ResponseWriter, *http.Request), n)
	for i := range script {
		script[i] = reply(200, `{}`)
	}
	f := newFake(t, script...)
	c, _ := client(t, f)
	cost, _ := Units(getMessage.ID)
	ctx := WithCounter(context.Background())
	before := c.budget.TokensAt(time.Now())
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := c.Do(ctx, getMessage, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if Requests(ctx) != n || UnitsSpent(ctx) != n*cost || f.count() != n {
		t.Errorf("requests %d, units %d, served %d", Requests(ctx), UnitsSpent(ctx), f.count())
	}
	if spent := before - c.budget.TokensAt(time.Now()); spent < float64(n*cost-1) {
		t.Errorf("budget spent %.1f units; want about %d", spent, n*cost)
	}
}

// A fan-out's requests reuse idle connections rather than dialing anew
// past Go's default of two per host.
func TestTheTransportKeepsAFanOutsConnections(t *testing.T) {
	tr, ok := New(Options{}).http.Transport.(*http.Transport)
	if !ok || tr.MaxIdleConnsPerHost < MaxInFlight {
		t.Errorf("transport %T keeps too few idle connections for %d in flight", New(Options{}).http.Transport, MaxInFlight)
	}
	if tr == http.DefaultTransport {
		t.Error("the shared default transport was modified in place")
	}
}
