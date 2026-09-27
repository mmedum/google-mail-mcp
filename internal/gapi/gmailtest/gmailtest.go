// Package gmailtest is an in-memory Gmail, served over HTTP at the REST
// paths the client calls, for tests.
//
// Everything in it is GENERATED (docs/architecture.md §9.1). Nothing
// was recorded from a real mailbox, and nothing may be: a fixture copied
// from a live response is itself the leak, whatever a scanner says. The
// names come from a fixed invented list, addresses are at example.com,
// example.org and .invalid, bodies come from templates, and ids come
// from a counter formatted as 16 lowercase hex digits, so every id the
// fake hands out starts with a run of zeros no real Gmail id has.
//
// The fake builds each message once as a MIME tree and derives both
// views from it — the raw RFC 5322 bytes of format=raw and the payload
// of format=full — so a parser reading either must reach the same
// answer.
//
// It models what the server's logic depends on: search with a subset of
// Gmail's operators, spam and trash hidden unless asked for, paging that
// can return an empty page with a token, bodies stored behind an
// attachment id, a history cursor that expires (404), unit costs per
// method, and injected failures. The writes are in write.go.
package gmailtest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
)

// Account is the mailbox owner's address.
const Account = "reader@example.com"

// unitCost is Google's quota cost per method (§2.1). The fake keeps its
// own table rather than importing the client's, so a test can hold the
// client's accounting against an independent one.
var unitCost = map[string]int{
	"gmail.users.getProfile":               1,
	"gmail.users.labels.list":              1,
	"gmail.users.labels.get":               1,
	"gmail.users.history.list":             2,
	"gmail.users.messages.list":            5,
	"gmail.users.drafts.list":              5,
	"gmail.users.threads.list":             10,
	"gmail.users.messages.get":             20,
	"gmail.users.messages.attachments.get": 20,
	"gmail.users.drafts.get":               20,
	"gmail.users.threads.get":              40,

	"gmail.users.settings.getVacation":              1,
	"gmail.users.settings.getAutoForwarding":        1,
	"gmail.users.settings.forwardingAddresses.list": 1,
	"gmail.users.settings.getImap":                  1,
	"gmail.users.settings.getPop":                   1,
	"gmail.users.settings.getLanguage":              1,
	"gmail.users.settings.sendAs.list":              1,
	"gmail.users.settings.filters.list":             1,

	"gmail.users.drafts.create":    10,
	"gmail.users.drafts.update":    15,
	"gmail.users.drafts.delete":    10,
	"gmail.users.messages.modify":  5,
	"gmail.users.threads.modify":   10,
	"gmail.users.messages.trash":   20,
	"gmail.users.messages.untrash": 5,
	"gmail.users.threads.trash":    20,
	"gmail.users.threads.untrash":  10,
	"gmail.users.labels.create":    5,
	"gmail.users.labels.patch":     5,
	"gmail.users.labels.delete":    5,
	"gmail.users.drafts.send":      100,
	"gmail.users.messages.delete":  10,
	"gmail.users.threads.delete":   20,
}

// Failure makes matching requests fail instead of being served.
type Failure struct {
	// Method is the discovery method id to match; "" matches any.
	Method string
	// Status and Reason are the HTTP status and Google's reason, e.g.
	// 429 "rateLimitExceeded", 503 "backendError".
	Status  int
	Reason  string
	Message string
	// RetryAfter is sent as the Retry-After header when set.
	RetryAfter string
	// Reset closes the connection without answering, as a network reset.
	Reset bool
	// Served acts on the request before failing it, as when Google did
	// the work and its answer was lost on the way back.
	Served bool
	// Times is how many requests fail; 0 means one.
	Times int
	// Then runs when the failure is served, outside the fake's lock, for
	// a change that happens between two of a call's requests.
	Then func(s *Server)
}

// Call is one request the fake served.
type Call struct {
	Method string // discovery id
	Path   string
	Query  string
	Units  int
}

// Server is an in-memory Gmail.
type Server struct {
	mu sync.Mutex
	ts *httptest.Server

	messages map[string]*message
	threads  map[string][]string // thread id → message ids, oldest first
	drafts   map[string]string   // draft id → message id
	labels   map[string]*gmail.Label
	store    map[string][]byte // attachment id → bytes
	history  []historyRecord
	// historyID is the mailbox's current history id.
	historyID uint64
	// HistoryFloor is the oldest history id still kept: a
	// history.list starting before it is 404, as Google answers a
	// cursor that has expired (§2.8).
	HistoryFloor uint64

	// EmptyFirstPage makes the first page of every listing come back
	// empty with a nextPageToken, which Google does when it applies the
	// page size before filtering. A client that stops there has read a
	// mailbox with mail in it as empty.
	EmptyFirstPage bool

	// FullScope is whether the token holds https://mail.google.com/.
	// Without it, a permanent delete is refused 403, as Gmail refuses it
	// under gmail.modify (spike G).
	FullScope bool

	// settings are the account's settings and filters, generated like
	// the mail. UpdateSettings changes them.
	settings Settings

	failures  []*Failure
	calls     []Call
	units     int
	scenarios map[string]Scenario

	counter uint64
	clock   time.Time
}

// New returns a running fake holding the generated mailbox. Close it
// when done.
func New() *Server {
	s := newEmpty()
	seed(s)
	s.ts = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func newEmpty() *Server {
	s := &Server{
		messages:  map[string]*message{},
		threads:   map[string][]string{},
		drafts:    map[string]string{},
		labels:    map[string]*gmail.Label{},
		store:     map[string][]byte{},
		scenarios: map[string]Scenario{},
		historyID: 1000,
		clock:     baseTime,
	}
	for _, l := range systemLabels {
		s.labels[l] = &gmail.Label{ID: l, Name: l, Type: gmail.LabelTypeSystem}
	}
	return s
}

// URL is the origin to give the client as its base URL.
func (s *Server) URL() string { return s.ts.URL }

// Close stops the server.
func (s *Server) Close() { s.ts.Close() }

// Fail queues an injected failure.
func (s *Server) Fail(f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Times <= 0 {
		f.Times = 1
	}
	s.failures = append(s.failures, &f)
}

// Units is the quota spent so far, at Google's price per method.
func (s *Server) Units() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.units
}

// Calls returns every request served, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsOf returns the requests served for one method, such as
// "gmail.users.drafts.send", in order.
func (s *Server) CallsOf(method string) []Call {
	return slices.DeleteFunc(s.Calls(), func(c Call) bool { return c.Method != method })
}

// ResetAccounting clears the calls and units.
func (s *Server) ResetAccounting() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls, s.units = nil, 0
}

// HistoryID is the mailbox's current history id.
func (s *Server) HistoryID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyID
}

// Message returns the stored message in the given format ("full",
// "metadata", "minimal" or "raw"), for tests that need the wire value
// without a request. ok is false for an unknown id.
func (s *Server) Message(id, format string) (gmail.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return gmail.Message{}, false
	}
	return s.render(m, format, nil), true
}

// Attachment returns stored attachment bytes by attachment id.
func (s *Server) Attachment(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.store[id]
	return b, ok
}

// Labels returns every label, as labels.list would.
func (s *Server) Labels() []gmail.Label {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.labelList()
}

func (s *Server) labelList() []gmail.Label {
	out := make([]gmail.Label, 0, len(s.labels))
	for _, l := range s.labels {
		c := *l
		c.MessagesTotal, c.MessagesUnread, c.ThreadsTotal, c.ThreadsUnread = 0, 0, 0, 0
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// nextID returns the next id: 16 lowercase hex digits from a counter.
func (s *Server) nextID() string {
	s.counter++
	return fmt.Sprintf("%016x", s.counter)
}

type route struct {
	method string
	// upload marks a method that also answers at the media upload path.
	upload bool
	// pattern is split on "/"; "{}" matches one segment.
	pattern []string
	id      string
	handle  func(s *Server, w http.ResponseWriter, r *http.Request, args []string)
}

// routes are every method the client calls.
var routes = []route{
	{"GET", false, []string{"profile"}, "gmail.users.getProfile", (*Server).getProfile},
	{"GET", false, []string{"messages"}, "gmail.users.messages.list", (*Server).listMessages},
	{"GET", false, []string{"messages", "{}"}, "gmail.users.messages.get", (*Server).getMessage},
	{"GET", false, []string{"messages", "{}", "attachments", "{}"}, "gmail.users.messages.attachments.get", (*Server).getAttachment},
	{"GET", false, []string{"threads"}, "gmail.users.threads.list", (*Server).listThreads},
	{"GET", false, []string{"threads", "{}"}, "gmail.users.threads.get", (*Server).getThread},
	{"GET", false, []string{"labels"}, "gmail.users.labels.list", (*Server).listLabels},
	{"GET", false, []string{"labels", "{}"}, "gmail.users.labels.get", (*Server).getLabel},
	{"GET", false, []string{"drafts"}, "gmail.users.drafts.list", (*Server).listDrafts},
	{"GET", false, []string{"drafts", "{}"}, "gmail.users.drafts.get", (*Server).getDraft},
	{"GET", false, []string{"history"}, "gmail.users.history.list", (*Server).listHistory},
	{"GET", false, []string{"settings", "vacation"}, "gmail.users.settings.getVacation", (*Server).getVacation},
	{"GET", false, []string{"settings", "autoForwarding"}, "gmail.users.settings.getAutoForwarding", (*Server).getAutoForwarding},
	{"GET", false, []string{"settings", "forwardingAddresses"}, "gmail.users.settings.forwardingAddresses.list", (*Server).listForwardingAddresses},
	{"GET", false, []string{"settings", "imap"}, "gmail.users.settings.getImap", (*Server).getImap},
	{"GET", false, []string{"settings", "pop"}, "gmail.users.settings.getPop", (*Server).getPop},
	{"GET", false, []string{"settings", "language"}, "gmail.users.settings.getLanguage", (*Server).getLanguage},
	{"GET", false, []string{"settings", "sendAs"}, "gmail.users.settings.sendAs.list", (*Server).listSendAs},
	{"GET", false, []string{"settings", "filters"}, "gmail.users.settings.filters.list", (*Server).listFilters},

	{"POST", true, []string{"drafts"}, "gmail.users.drafts.create", (*Server).createDraft},
	{"PUT", true, []string{"drafts", "{}"}, "gmail.users.drafts.update", (*Server).updateDraft},
	{"DELETE", false, []string{"drafts", "{}"}, "gmail.users.drafts.delete", (*Server).deleteDraft},
	{"POST", false, []string{"messages", "{}", "modify"}, "gmail.users.messages.modify", (*Server).modifyMessage},
	{"POST", false, []string{"threads", "{}", "modify"}, "gmail.users.threads.modify", (*Server).modifyThread},
	{"POST", false, []string{"messages", "{}", "trash"}, "gmail.users.messages.trash", (*Server).trashMessage},
	{"POST", false, []string{"messages", "{}", "untrash"}, "gmail.users.messages.untrash", (*Server).untrashMessage},
	{"POST", false, []string{"threads", "{}", "trash"}, "gmail.users.threads.trash", (*Server).trashThread},
	{"POST", false, []string{"threads", "{}", "untrash"}, "gmail.users.threads.untrash", (*Server).untrashThread},
	{"POST", false, []string{"labels"}, "gmail.users.labels.create", (*Server).createLabel},
	{"PATCH", false, []string{"labels", "{}"}, "gmail.users.labels.patch", (*Server).patchLabel},
	{"DELETE", false, []string{"labels", "{}"}, "gmail.users.labels.delete", (*Server).deleteLabel},
	{"POST", false, []string{"drafts", "send"}, "gmail.users.drafts.send", (*Server).sendDraft},
	{"DELETE", false, []string{"messages", "{}"}, "gmail.users.messages.delete", (*Server).deleteMessage},
	{"DELETE", false, []string{"threads", "{}"}, "gmail.users.threads.delete", (*Server).deleteThread},
}

const prefix = "/gmail/v1/users/me/"

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	upload := strings.HasPrefix(path, "/upload"+prefix)
	rest, ok := strings.CutPrefix(strings.TrimPrefix(path, "/upload"), prefix)
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", "Not Found")
		return
	}
	segs := strings.Split(rest, "/")
	for _, rt := range routes {
		args, ok := match(rt.pattern, segs)
		if !ok || rt.method != r.Method || (upload && !rt.upload) {
			continue
		}
		s.mu.Lock()
		cost := unitCost[rt.id]
		s.calls = append(s.calls, Call{Method: rt.id, Path: r.URL.Path, Query: r.URL.RawQuery, Units: cost})
		s.units += cost
		f := s.takeFailure(rt.id)
		if f != nil {
			if f.Served {
				rt.handle(s, discard{}, r, args)
			}
			s.mu.Unlock()
			if f.Then != nil {
				f.Then(s)
			}
			fail(w, f)
			return
		}
		rt.handle(s, w, r, args)
		s.mu.Unlock()
		return
	}
	writeError(w, http.StatusNotFound, "notFound", "Method not found.")
}

// discard is a response nobody reads: a served request whose answer is
// lost.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

func match(pattern, segs []string) ([]string, bool) {
	if len(pattern) != len(segs) {
		return nil, false
	}
	var args []string
	for i, p := range pattern {
		if p == "{}" {
			if segs[i] == "" {
				return nil, false
			}
			a, err := url.PathUnescape(segs[i])
			if err != nil {
				return nil, false
			}
			args = append(args, a)
			continue
		}
		if p != segs[i] {
			return nil, false
		}
	}
	return args, true
}

func (s *Server) takeFailure(id string) *Failure {
	for i, f := range s.failures {
		if f.Method != "" && f.Method != id {
			continue
		}
		f.Times--
		if f.Times <= 0 {
			s.failures = append(s.failures[:i], s.failures[i+1:]...)
		}
		return f
	}
	return nil
}

func fail(w http.ResponseWriter, f *Failure) {
	if f.Reset {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_ = conn.Close()
				return
			}
		}
	}
	if f.RetryAfter != "" {
		w.Header().Set("Retry-After", f.RetryAfter)
	}
	msg := f.Message
	if msg == "" {
		msg = http.StatusText(f.Status)
	}
	writeError(w, f.Status, f.Reason, msg)
}

var statusNames = map[int]string{
	400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED",
	404: "NOT_FOUND", 409: "ABORTED", 429: "RESOURCE_EXHAUSTED", 500: "INTERNAL",
	503: "UNAVAILABLE",
}

// writeError answers in Google's error envelope.
func writeError(w http.ResponseWriter, status int, reason, message string) {
	type item struct {
		Message string `json:"message"`
		Domain  string `json:"domain"`
		Reason  string `json:"reason"`
	}
	body := map[string]any{"error": map[string]any{
		"code":    status,
		"message": message,
		"status":  statusNames[status],
		"errors":  []item{{Message: message, Domain: "global", Reason: reason}},
	}}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// DraftIDs returns every draft id, newest first.
func (s *Server) DraftIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draftIDs()
}

// DraftMessage returns the message a draft holds now, in the given
// format. ok is false for an unknown draft.
func (s *Server) DraftMessage(id, format string) (gmail.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mid, ok := s.drafts[id]
	if !ok {
		return gmail.Message{}, false
	}
	return s.render(s.messages[mid], format, nil), true
}

// Raw returns a stored message's RFC 5322 bytes, as a draft write sent
// them or as the generator built them.
func (s *Server) Raw(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return nil, false
	}
	return slices.Clone(m.raw), true
}
