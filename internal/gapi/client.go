// Package gapi is the raw REST client for the Gmail API v1.
//
// Raw, because CLAUDE.md rule 10 forbids the generated client. Every
// request goes through Client.Do, which owns the rules that must hold
// for every call: an access token only goes to an allowed origin; a
// write never runs under a dry run; every call is charged its unit cost
// against the per-user budget before it is sent; only what cannot apply
// twice is retried, and a send is never retried at all; and every
// failure comes back as a classified *Error whose text carries no URL,
// query or address.
package gapi

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/time/rate"

	"github.com/mmedum/google-mail-mcp/internal/redact"
)

// Defaults for Options.
const (
	DefaultBaseURL        = "https://gmail.googleapis.com"
	DefaultTimeout        = 60 * time.Second
	DefaultMaxRetries     = 3
	DefaultUnitsPerMinute = 6000
)

// apiPrefix is where every method lives under the base URL.
const apiPrefix = "/gmail/v1/users/me/"

// maxResponseBytes bounds one JSON response. Attachments stream through
// a separate path in a later phase; this is a ceiling on an envelope.
const maxResponseBytes = 32 << 20

// maxBackoff caps one jittered wait, as Google's own algorithm does.
const maxBackoff = 32 * time.Second

// MaxInFlight is the most requests one tool call keeps in flight at
// once. The service's fan-out uses it, and the transport keeps as many
// idle connections, so a fan-out reuses its connections instead of
// dialing a new one for each request past Go's default of two.
const MaxInFlight = 5

// maxRetryAfter is the longest Retry-After worth waiting for inside one
// tool call. A longer one is reported to the caller instead.
const maxRetryAfter = 64 * time.Second

// Options configure a Client. Zero values take the defaults.
type Options struct {
	// BaseURL is the API origin, with no path. Tests point it at a local
	// server.
	BaseURL string
	// UploadBaseURL is the media upload origin. Unset means BaseURL,
	// where Google serves /upload/gmail/v1/.
	UploadBaseURL string
	// Timeout bounds one HTTP attempt.
	Timeout time.Duration
	// TokenSource supplies the access token. nil answers every call
	// with [auth].
	TokenSource oauth2.TokenSource
	// MaxRetries is the number of attempts after the first, for a call
	// that may be repeated. Zero takes DefaultMaxRetries; negative means
	// none.
	MaxRetries int
	// UnitsPerMinute is the per-user quota budget. Zero takes 6,000.
	UnitsPerMinute int
	// Logger receives one debug line per attempt. Never the payload.
	Logger *slog.Logger
	// HTTP is the client to send with, for a test transport. Its
	// redirect policy is replaced by the allowlist check.
	HTTP *http.Client
	// UserAgent identifies this build to Google.
	UserAgent string
	// Sleep waits between attempts; tests stub it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client calls the Gmail API.
type Client struct {
	base, upload string
	tokens       oauth2.TokenSource
	http         *http.Client
	maxRetries   int
	budget       *rate.Limiter
	log          *slog.Logger
	userAgent    string
	sleep        func(ctx context.Context, d time.Duration) error
	allowed      map[string]bool
	// streamHTTP is http without the whole-request timeout, for Stream;
	// idleTimeout bounds each wait for progress instead.
	streamHTTP  *http.Client
	idleTimeout time.Duration
}

// googleOrigins are Google's own API origins an access token may go to,
// whatever the configuration says.
var googleOrigins = []string{
	"https://gmail.googleapis.com",
	"https://www.googleapis.com",
	"https://oauth2.googleapis.com",
}

// New builds a Client.
func New(o Options) *Client {
	c := &Client{
		base:       strings.TrimRight(o.BaseURL, "/"),
		upload:     strings.TrimRight(o.UploadBaseURL, "/"),
		tokens:     o.TokenSource,
		maxRetries: o.MaxRetries,
		log:        o.Logger,
		userAgent:  o.UserAgent,
		sleep:      o.Sleep,
	}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.upload == "" {
		c.upload = c.base
	}
	switch {
	case c.maxRetries == 0:
		c.maxRetries = DefaultMaxRetries
	case c.maxRetries < 0:
		c.maxRetries = 0
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.userAgent == "" {
		c.userAgent = "google-mail-mcp"
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	perMinute := o.UnitsPerMinute
	if perMinute <= 0 {
		perMinute = DefaultUnitsPerMinute
	}
	// The burst lets one tool call's fan-out — a search that opens
	// twenty threads is 810 units — go at once, and is never below the
	// dearest single call, which WaitN could otherwise never admit.
	burst := max(perMinute/4, maxUnitCost())
	c.budget = rate.NewLimiter(rate.Limit(float64(perMinute)/60), burst)

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	hc := &http.Client{Timeout: timeout, Transport: newTransport()}
	if o.HTTP != nil {
		copied := *o.HTTP
		hc = &copied
		if hc.Timeout == 0 {
			hc.Timeout = timeout
		}
	}
	hc.CheckRedirect = c.checkRedirect
	c.http = hc
	// A stream is bounded by how long it goes without progress rather
	// than by how long it takes: a large attachment on a slow link must
	// finish, and a stalled one must not hang (§3.16).
	streaming := *hc
	streaming.Timeout = 0
	c.streamHTTP, c.idleTimeout = &streaming, hc.Timeout

	c.allowed = map[string]bool{}
	for _, origin := range append([]string{c.base, c.upload}, googleOrigins...) {
		if u, err := url.Parse(origin); err == nil {
			c.allowed[originKey(u)] = true
		}
	}
	return c
}

// newTransport is Go's default transport keeping MaxInFlight idle
// connections per host.
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = MaxInFlight
	return t
}

// ErrHostNotAllowed is returned when a request would carry the access
// token to an origin this client was not configured for.
var ErrHostNotAllowed = errors.New("gapi: refusing to send credentials to an origin outside the allowlist")

// originKey is what the allowlist compares: scheme, lowercased host
// without a trailing dot, and port. The port is kept, because an
// unexpected port is a different endpoint, and this check decides
// whether a token leaves the machine.
func originKey(u *url.URL) string {
	key := strings.ToLower(u.Scheme) + "://" + strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if port := u.Port(); port != "" {
		key += ":" + port
	}
	return key
}

func (c *Client) allowURL(u *url.URL) bool { return c.allowed[originKey(u)] }

// maxRedirects is Go's default, restated because replacing
// CheckRedirect replaces the check that enforced it.
const maxRedirects = 10

// checkRedirect runs the allowlist on every hop. net/http keeps the
// Authorization header on a redirect to a subdomain of the original
// host, so without this a 302 could carry the token somewhere this
// client never chose.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("gapi: stopped after %d redirects", maxRedirects)
	}
	if !c.allowURL(req.URL) {
		return ErrHostNotAllowed
	}
	return nil
}

// ------------------------------------------------------------ dry run

type noWritesKey struct{}

// WithoutWrites returns a context under which the client refuses every
// request that is not a GET, before anything reaches the network.
//
// It is what dry_run is. A tool still shapes its own preview, but the
// guarantee that a preview changes nothing is kept here, where every
// request passes, so a handler that forgets its preview branch fails
// loudly instead of writing.
func WithoutWrites(ctx context.Context) context.Context {
	return context.WithValue(ctx, noWritesKey{}, true)
}

// WritesForbidden reports whether ctx refuses writes.
func WritesForbidden(ctx context.Context) bool {
	v, _ := ctx.Value(noWritesKey{}).(bool)
	return v
}

// ErrWriteForbidden is the cause of a write refused under WithoutWrites.
// Reaching it is a bug in this server, never something a caller did.
var ErrWriteForbidden = errors.New("gapi: this call may not write")

// ------------------------------------------------------------ counting

type counterKey struct{}

type counter struct{ requests, units atomic.Int64 }

// WithCounter returns a context that counts the requests made and units
// spent under it. The server installs one per tool call, so the log
// line and the result report what Google was actually asked, retries
// included, and two calls in flight do not count each other's work.
func WithCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, counterKey{}, &counter{})
}

// Requests is how many HTTP requests were made under ctx.
func Requests(ctx context.Context) int {
	if n, ok := ctx.Value(counterKey{}).(*counter); ok {
		return int(n.requests.Load())
	}
	return 0
}

// UnitsSpent is how many quota units were charged under ctx.
func UnitsSpent(ctx context.Context) int {
	if n, ok := ctx.Value(counterKey{}).(*counter); ok {
		return int(n.units.Load())
	}
	return 0
}

func charge(ctx context.Context, units int) {
	if n, ok := ctx.Value(counterKey{}).(*counter); ok {
		n.requests.Add(1)
		n.units.Add(int64(units))
	}
}

// ------------------------------------------------------------ the call

// isWrite reports whether a call changes something. Gmail has no
// read-only POST, so the method decides.
func isWrite(call Call) bool { return call.Method != http.MethodGet }

// policy is how one call may be repeated. Do decides it once, from the
// call's method and id, and everything after reads it from here: this
// is the one place that says a send is never repeated (§4.3).
type policy struct {
	// send marks messages.send and drafts.send, which a 429 with no rate
	// reason is read against as Gmail's sending limit.
	send bool
	// tries is the most attempts the call gets, the first included.
	tries int
	// whenTurnedAway allows another attempt where Google turned the call
	// away before acting on it: 429, a 403 rate limit, 503, or a
	// connection that was never made.
	whenTurnedAway bool
	// whenAmbiguous allows another attempt after a failure that may have
	// followed the write: a timeout, a reset or another 5xx. GET, PUT,
	// PATCH and DELETE name a fixed target and a fixed end state. A POST
	// fails closed: it repeats only when its call site declares why.
	whenAmbiguous bool
}

// policyFor decides call's policy. A send declared repeatable is a
// programming error, refused before anything is sent.
func (c *Client) policyFor(call Call) (policy, error) {
	if sendIDs[call.ID] {
		if call.Repeatable != "" {
			return policy{}, fmt.Errorf("gapi: %s is a send and may never be declared repeatable", call.ID)
		}
		return policy{send: true, tries: 1}, nil
	}
	return policy{
		tries:          1 + c.maxRetries,
		whenTurnedAway: true,
		whenAmbiguous:  call.Method != http.MethodPost || call.Repeatable != "",
	}, nil
}

// Do sends one call and decodes the response into out, which may be nil.
func (c *Client) Do(ctx context.Context, call Call, out any) error {
	return c.do(ctx, call, out, nil)
}

// Stream sends one GET and hands a successful response body to consume
// instead of reading it whole, for a body that may be larger than
// maxResponseBytes. consume runs once per attempt, so it starts its
// output over each time. A *Error it returns is final; any other error
// is a failed read of the response, retried as a GET is.
func (c *Client) Stream(ctx context.Context, call Call, consume func(io.Reader) error) error {
	if call.Method != http.MethodGet {
		return fmt.Errorf("gapi: %s: only a GET streams", call.ID)
	}
	return c.do(ctx, call, nil, consume)
}

func (c *Client) do(ctx context.Context, call Call, out any, consume func(io.Reader) error) error {
	cost, ok := unitCost[call.ID]
	if !ok {
		return fmt.Errorf("gapi: %q has no unit cost in units.go; price it before calling it", call.ID)
	}
	switch call.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Errorf("gapi: %s: unsupported method %q", call.ID, call.Method)
	}
	p, err := c.policyFor(call)
	if err != nil {
		return err
	}
	path, err := fillPath(call.Path, call.Args)
	if err != nil {
		return err
	}
	if isWrite(call) && WritesForbidden(ctx) {
		return Wrap(ClassBlocked, ErrWriteForbidden,
			"this was a dry run and %s would have changed the mailbox, so it was refused before it was sent", call.ID)
	}

	var payload []byte
	contentType := "application/json; charset=UTF-8"
	if call.Body != nil {
		if payload, err = json.Marshal(call.Body); err != nil {
			return Wrap(ClassInvalid, err, "the request for %s could not be encoded", call.ID)
		}
	}
	endpoint := c.base + apiPrefix + path
	query := call.Query
	if call.Media != nil {
		if call.Method == http.MethodGet {
			return Errf(ClassInvalid, "%s is a read and uploads nothing", call.ID)
		}
		payload, contentType = multipartRelated(payload, call.Media)
		endpoint = c.upload + "/upload" + apiPrefix + path
		query = url.Values{}
		for k, v := range call.Query {
			query[k] = v
		}
		query.Set("uploadType", "multipart")
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	token, err := c.token()
	if err != nil {
		return err
	}

	var last verdict
	for attempt := 1; attempt <= p.tries; attempt++ {
		if attempt > 1 {
			if err := c.sleep(ctx, backoff(attempt-1, last.after)); err != nil {
				return last.err
			}
		}
		if err := c.budget.WaitN(ctx, cost); err != nil {
			return Wrap(ClassUnavailable, err, "%s was not sent: the call was canceled while waiting for quota", call.ID)
		}
		charge(ctx, cost)
		start := time.Now()
		body, status, header, sendErr := c.attempt(ctx, call.Method, endpoint, payload, contentType, token,
			call.Media != nil, consume)
		v := decide(ctx, call, p, status, header, body, sendErr)
		c.log.Debug("gmail_request", "id", call.ID, "attempt", attempt, "status", status,
			"ms", time.Since(start).Milliseconds(), "units", cost, "outcome", v.outcome())
		if v.err == nil {
			if out == nil || len(body) == 0 {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return Wrap(ClassUnavailable, err, "Google's answer to %s was not the JSON this server expected", call.ID)
			}
			return nil
		}
		if !v.retry || attempt == p.tries {
			return v.err
		}
		last = v
	}
	return last.err // unreachable: the loop returns on its last attempt
}

// token resolves the access token once per call. A refusal is final.
func (c *Client) token() (string, error) {
	if c.tokens == nil {
		return "", Errf(ClassAuth, "not signed in: run `google-mail-mcp login`")
	}
	tok, err := c.tokens.Token()
	if err != nil {
		return "", Wrap(ClassAuth, err,
			"the stored credentials could not be used: run `google-mail-mcp login`, then `google-mail-mcp doctor` if it persists")
	}
	return tok.AccessToken, nil
}

// fillPath fills a template's {} placeholders with escaped Args. A count
// mismatch is a programming error. An empty, "." or ".." argument is the
// caller's, and would address a different resource than it names.
func fillPath(template string, args []string) (string, error) {
	if n := strings.Count(template, "{}"); n != len(args) {
		return "", fmt.Errorf("gapi: path %q has %d placeholders and %d arguments", template, n, len(args))
	}
	var b strings.Builder
	rest := template
	for _, a := range args {
		if a == "" || a == "." || a == ".." {
			return "", Errf(ClassInvalid, "an id is empty or not an id: %q", a)
		}
		i := strings.Index(rest, "{}")
		b.WriteString(rest[:i])
		b.WriteString(url.PathEscape(a))
		rest = rest[i+2:]
	}
	b.WriteString(rest)
	return b.String(), nil
}

// attempt makes one HTTP request. The allowlist is checked before the
// token is attached, so no path attaches a credential and validates
// after. A successful answer goes to consume when it is set, unread
// here; any other answer is read, bounded, for its error.
//
// A stream, and an upload, is bounded by time without progress rather
// than in total: a 35 MB draft on a slow uplink must finish, and a
// timeout there would leave a draft that may or may not exist.
func (c *Client) attempt(ctx context.Context, method, endpoint string, payload []byte, contentType, token string,
	upload bool, consume func(io.Reader) error,
) ([]byte, int, http.Header, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	hc := c.http
	var progress func()
	if consume != nil || upload {
		hc = c.streamHTTP
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		idle := time.AfterFunc(c.idleTimeout, cancel)
		defer idle.Stop()
		progress = func() { idle.Reset(c.idleTimeout) }
	}
	if upload && reader != nil {
		reader = progressReader{r: reader, progress: progress}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, nil, withoutURL(err)
	}
	if upload {
		req.ContentLength = int64(len(payload))
	}
	if !c.allowURL(req.URL) {
		return nil, 0, nil, ErrHostNotAllowed
	}
	askCompactJSON(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, nil, withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if consume != nil && resp.StatusCode/100 == 2 {
		if err := consume(progressReader{r: resp.Body, progress: progress}); err != nil {
			var classified *Error
			if errors.As(err, &classified) {
				return nil, resp.StatusCode, resp.Header, classified
			}
			return nil, resp.StatusCode, resp.Header, withoutURL(err)
		}
		return nil, resp.StatusCode, resp.Header, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, withoutURL(err)
	}
	if len(body) > maxResponseBytes {
		return nil, resp.StatusCode, resp.Header, errTooLarge
	}
	return body, resp.StatusCode, resp.Header, nil
}

// multipartRelated is Google's multipart upload body: the JSON metadata,
// then the message. The boundary is drawn until neither part holds it.
func multipartRelated(metadata, media []byte) ([]byte, string) {
	if metadata == nil {
		metadata = []byte("{}")
	}
	var boundary string
	for {
		var b [12]byte
		_, _ = crand.Read(b[:])
		boundary = "gapi_" + hex.EncodeToString(b[:])
		if !bytes.Contains(media, []byte(boundary)) && !bytes.Contains(metadata, []byte(boundary)) {
			break
		}
	}
	var b bytes.Buffer
	b.Grow(len(metadata) + len(media) + 4*len(boundary) + 128)
	b.WriteString("--" + boundary + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n")
	b.Write(metadata)
	b.WriteString("\r\n--" + boundary + "\r\nContent-Type: message/rfc822\r\n\r\n")
	b.Write(media)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.Bytes(), "multipart/related; boundary=" + boundary
}

// progressReader reports each read that returned bytes.
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
}

var errTooLarge = fmt.Errorf("the response is larger than %d bytes", maxResponseBytes)

// askCompactJSON sets Google's prettyPrint system parameter to false,
// which drops the indentation it adds by default. Appended rather than
// re-encoded, so a query is never rewritten.
func askCompactJSON(req *http.Request) {
	if req.URL.RawQuery == "" {
		req.URL.RawQuery = "prettyPrint=false"
		return
	}
	req.URL.RawQuery += "&prettyPrint=false"
}

// ------------------------------------------------------------ outcomes

// verdict is what one attempt came to.
type verdict struct {
	err *Error
	// after is the wait Google asked for, a floor on the next backoff.
	after time.Duration
	// retry says another attempt may be made.
	retry bool
}

func (v verdict) outcome() string {
	if v.err == nil {
		return "ok"
	}
	return string(v.err.Class)
}

// repeat is which failures a classification says may be tried again.
type repeat int

const (
	// never: trying again cannot help.
	never repeat = iota
	// ifTurnedAway: Google turned the call away before acting on it, so
	// it repeats under a policy that allows that.
	ifTurnedAway
	// again: the call may be repeated. classify gives it only where the
	// policy already allows it.
	again
)

// failure is a classified attempt before the Retry-After step.
type failure struct {
	err    *Error
	repeat repeat
	// advice is when to try again, for a failure that may clear on its
	// own, and said is what Google said. settle appends both to the
	// message. Empty for a failure that will not clear.
	advice, said string
	after        time.Duration
}

// decide classifies one attempt and says whether it may be repeated.
func decide(ctx context.Context, call Call, p policy, status int, header http.Header, body []byte, sendErr error) verdict {
	var classified *Error
	switch {
	case errors.As(sendErr, &classified):
		// Already judged by the consumer of a stream, which knows whether
		// its own failure can clear.
		return verdict{err: classified}
	case sendErr != nil:
		return p.settle(classifyTransport(ctx, call, p, sendErr))
	case status >= 200 && status < 300:
		return verdict{}
	}
	return p.settle(classifyStatus(call, p, status, header, body))
}

// settle turns a failure into a verdict under the policy. It is the one
// place a Retry-After is weighed: a wait longer than maxRetryAfter is
// not waited for, and the message says how long Google asked for
// instead of the usual advice.
func (p policy) settle(f failure) verdict {
	v := verdict{err: f.err, after: f.after,
		retry: f.repeat == again || (f.repeat == ifTurnedAway && p.whenTurnedAway)}
	if f.advice == "" {
		return v
	}
	advice := f.advice
	if f.after > maxRetryAfter {
		v.retry = false
		advice = fmt.Sprintf("Google asked for a wait of %s, longer than one call waits; retry after that", f.after.Round(time.Second))
	}
	f.err.Message += "; " + advice + ". Google said: " + f.said
	return v
}

// classifyTransport classifies a request that got no answer.
func classifyTransport(ctx context.Context, call Call, p policy, sendErr error) failure {
	switch {
	case errors.Is(sendErr, ErrHostNotAllowed):
		return failure{err: Wrap(ClassBlocked, sendErr, "%s was refused: its address is outside the allowed Google origins", call.ID)}
	case errors.Is(sendErr, errTooLarge):
		return failure{err: Wrap(ClassUnavailable, sendErr, "Google's answer to %s was too large to read", call.ID)}
	case neverSent(sendErr):
		return failure{err: Wrap(ClassUnavailable, sendErr, "could not reach Google for %s: %s", call.ID, sendErr), repeat: ifTurnedAway}
	case !p.whenAmbiguous:
		return failure{err: Wrap(ClassAmbiguousOutcome, sendErr, ambiguous, call.ID)}
	case ctx.Err() != nil:
		return failure{err: Wrap(ClassUnavailable, sendErr, "%s was canceled", call.ID)}
	default:
		return failure{err: Wrap(ClassUnavailable, sendErr, "Google did not answer %s: %s", call.ID, sendErr), repeat: again}
	}
}

// classifyStatus classifies an answer that was not a success.
func classifyStatus(call Call, p policy, status int, header http.Header, body []byte) failure {
	env := parseEnvelope(body)
	detail := env.message
	if detail == "" {
		detail = http.StatusText(status)
	}
	final := func(c Class, format string, args ...any) failure {
		return failure{err: &Error{Class: c, Message: fmt.Sprintf(format, args...), Status: status, Reason: env.reason},
			repeat: never}
	}
	// transient is a failure that may clear on its own, with the advice
	// on when and the wait Google asked for.
	transient := func(r repeat, advice string, c Class, format string, args ...any) failure {
		f := final(c, format, args...)
		f.repeat, f.advice, f.said, f.after = r, advice, detail, parseRetryAfter(header.Get("Retry-After"))
		return f
	}

	switch {
	case isSendingLimit(p, status, env):
		return final(ClassRateLimited,
			"Gmail's sending limit for this account is reached. It lifts within 24 hours; do not resend in the meantime. Google said: %s", detail)
	case status == http.StatusTooManyRequests || (status == http.StatusForbidden && matches(env.reason, rateReasons)):
		return transient(ifTurnedAway, "wait a minute and retry", ClassRateLimited, "Google is rate limiting this account")
	case status == http.StatusForbidden && matches(env.reason, quotaReasons):
		return final(ClassRateLimited, "the Cloud project's daily Gmail quota is spent; it resets at midnight Pacific time. Google said: %s", detail)
	case status == http.StatusForbidden && isMissingScope(env):
		return final(ClassAuth,
			"the signed-in token lacks a scope %s needs: run `google-mail-mcp login` again and grant every scope asked for. Google said: %s", call.ID, detail)
	case status == http.StatusForbidden:
		return final(ClassForbidden, "Google refused %s for this account: %s", call.ID, detail)
	case status == http.StatusUnauthorized:
		return final(ClassAuth, "Google rejected the access token: run `google-mail-mcp login`. Google said: %s", detail)
	case status == http.StatusNotFound:
		return final(ClassNotFound, "Google found nothing at that id (%s): %s", call.ID, detail)
	case status == http.StatusConflict:
		return final(ClassConflict, "the mailbox refused %s in its current state: %s", call.ID, detail)
	case status == http.StatusPreconditionFailed:
		return final(ClassStale, "this changed since it was read; read it again and retry. Google said: %s", detail)
	case status == http.StatusServiceUnavailable:
		return transient(ifTurnedAway, "retry shortly", ClassUnavailable, "Gmail is not serving right now (%s)", call.ID)
	case status >= 500 && !p.whenAmbiguous:
		return final(ClassAmbiguousOutcome, ambiguous+" Google said: %s", call.ID, detail)
	case status >= 500:
		return transient(again, "retry shortly", ClassUnavailable, "Gmail failed on %s", call.ID)
	default:
		return final(ClassInvalid, "Google refused %s as invalid: %s", call.ID, detail)
	}
}

const ambiguous = "Google did not confirm whether %s took effect, and it was not repeated. " +
	"Read the mailbox to find out before doing anything again."

// neverSent reports a transport failure that happened before any byte
// of the request could have reached Google: the connection was never
// made. Only these are safe to repeat for a call that may not repeat.
func neverSent(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// Google's reasons, compared across its two spellings (camelCase in the
// older envelope, UPPER_SNAKE in an ErrorInfo detail).
var (
	rateReasons  = []string{"rateLimitExceeded", "userRateLimitExceeded"}
	quotaReasons = []string{"dailyLimitExceeded", "quotaExceeded"}
	scopeReasons = []string{"insufficientPermissions", "ACCESS_TOKEN_SCOPE_INSUFFICIENT"}
	// scopeMessages catch the refusal where no reason is set.
	scopeMessages = []string{"insufficient authentication scopes", "insufficient permission"}
	// sendingLimitMarkers are how a sending-limit refusal is told from
	// a request-rate one. Spike H (§15) records the real shape; until
	// then the match is deliberately wide, because reading a rate limit
	// as a sending limit costs one call a retry, while the reverse
	// retries against a wait of hours.
	sendingLimitMarkers = []string{"sending limit", "mail sending", "sendinglimit", "daily user sending", "sending quota"}
)

func matches(reason string, want []string) bool {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for _, w := range want {
		if reason != "" && norm(reason) == norm(w) {
			return true
		}
	}
	return false
}

func isMissingScope(env envelope) bool {
	if matches(env.reason, scopeReasons) {
		return true
	}
	msg := strings.ToLower(env.message)
	for _, m := range scopeMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// isSendingLimit reports Gmail's per-account sending limit. It arrives
// as 429 (§2.2), and is matched by what Google says rather than by
// which call met it, because the limit can surface minutes late on a
// call that is not a send.
func isSendingLimit(p policy, status int, env envelope) bool {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(env.reason + " " + env.message)
	for _, m := range sendingLimitMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	// A send refused with 429 and no rate reason is read as the sending
	// limit: waiting a minute is the wrong advice if it is.
	return p.send && status == http.StatusTooManyRequests && !matches(env.reason, rateReasons)
}

// envelope is the part of Google's error body this client reads.
type envelope struct {
	reason, message string
}

func parseEnvelope(body []byte) envelope {
	var raw struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return envelope{}
	}
	var env envelope
	// Google's message can name the account it refused, or a third
	// party. It reaches the model and, through an error, a person's
	// terminal: every address is masked here, domain included.
	env.message = redact.Addresses(strings.TrimSpace(raw.Error.Message))
	for _, e := range raw.Error.Errors {
		if e.Reason != "" {
			env.reason = e.Reason
			break
		}
	}
	for _, d := range raw.Error.Details {
		if env.reason == "" && d.Reason != "" {
			env.reason = d.Reason
		}
	}
	return env
}

// ------------------------------------------------------------ waiting

// backoff is full jitter over min(2^n s, 32 s), with a Retry-After
// honored as a floor: Google's figure is a minimum, not a target to
// jitter below.
func backoff(retry int, retryAfter time.Duration) time.Duration {
	ceiling := min(time.Second<<min(retry, 6), maxBackoff)
	d := time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // jitter, not a secret
	return max(d, retryAfter)
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ------------------------------------------------------------ URLs

// withoutURL strips the request URL from a transport error. net/http
// wraps every transport failure in a *url.Error whose text renders the
// whole URL, and a search travels in its query string (§9.2).
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	if errors.As(err, &ue) {
		return errors.New(ue.Op + " failed")
	}
	return errors.New(stripURL(err.Error()))
}

// stripURL cuts anything URL-shaped out of free text, for an error that
// did not arrive as a *url.Error and so is not known to be URL-free.
func stripURL(s string) string {
	for {
		i := strings.Index(s, "http")
		if i < 0 {
			return s
		}
		j := strings.IndexAny(s[i:], " \"'")
		if j < 0 {
			return strings.TrimSpace(s[:i]) + " <url>"
		}
		s = s[:i] + "<url>" + s[i+j:]
	}
}
