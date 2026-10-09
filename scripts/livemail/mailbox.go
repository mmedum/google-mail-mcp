//go:build live

package main

import (
	"context"
	"encoding/json"

	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"net/url"
	"slices"
	"strings"
	"time"
)

// mailbox is what the driver does to the mailbox itself, outside the
// server: the fixture it reads from. It is the §9.1 contract, and these
// calls are all of it:
//
//   - CreateLabel makes the run's label. Every read a step makes is
//     constrained to it.
//
//   - Insert puts one synthetic RFC 5322 message under that label with
//     messages.insert, which plants mail without delivering it. The server
//     never calls insert (§8a); only this driver does.
//
//   - Trash moves one inserted message to the trash.
//
//   - DeleteLabel removes the run's label.
//
//   - CreateDraft and DeleteDraft make and remove the run's own drafts,
//     whose subjects carry the run's name, since a draft cannot be labeled.
//
//   - Probe makes one call for a spike and returns its status, Google's
//     reason and message on a refusal, and the id of anything it created,
//     so cleanup can remove it.
//
//   - UpdateDraft and DraftMessageID save a run draft again and read which
//     message it holds, for spike A.
//
//   - Send and Label send one message the run built, to the address the
//     maintainer passed as -send-to and nowhere else, and put it under the
//     run's label so cleanup trashes it. Only spikes D and E send (§15).
//
//   - SentCopy reads back a message the run sent: its thread, and its
//     bytes for spike E. Header reads one header of it, for spike D.
//
//   - Account reads the signed-in address, which a spike's Message-ID
//     and a create_draft step's from take their domain and value from.
//
//   - FullScope says whether the profile's login granted
//     https://mail.google.com/, read from the profile rather than asked of
//     Google, so the delete steps know whether they can run.
//
//   - HistoryID reads the mailbox's current history id before the run
//     inserts anything, so list_changes has a start that precedes the
//     run's own mail. It is a counter, not mail.
//
// It never lists, searches or reads the mailbox: that is the server's
// job, and the driver only checks the server against what it inserted.
type mailbox interface {
	CreateLabel(ctx context.Context, name string) (labelID string, err error)
	Insert(ctx context.Context, labelID string, raw []byte) (messageID, threadID string, err error)
	Trash(ctx context.Context, messageID string) error
	DeleteLabel(ctx context.Context, labelID string) error
	CreateDraft(ctx context.Context, raw []byte) (draftID string, err error)
	DeleteDraft(ctx context.Context, draftID string) error
	Probe(ctx context.Context, method, path string, q url.Values, body any) probeResult
	HistoryID(ctx context.Context) (string, error)
	// InternalDate reads the date Gmail recorded for a message the run
	// inserted, in milliseconds, for spike F.
	InternalDate(ctx context.Context, messageID string) (int64, error)
	UpdateDraft(ctx context.Context, draftID string, raw []byte) (messageID string, err error)
	DraftMessageID(ctx context.Context, draftID string) (string, error)
	// Send sends o to the one address to: its own To name is kept, and
	// every other recipient is dropped before it is built.
	Send(ctx context.Context, o mime.Outgoing, to, threadID string) (messageID, sentThreadID string, err error)
	Label(ctx context.Context, messageID, labelID string) error
	SentCopy(ctx context.Context, messageID string) (threadID string, raw []byte, err error)
	Header(ctx context.Context, messageID, name string) (string, error)
	Account(ctx context.Context) (string, error)
	FullScope() bool
	// SettingsScope is whether the profile's login granted
	// gmail.settings.basic, which the settings steps need.
	SettingsScope() bool
	// SaveSettings reads what the settings steps change — the default
	// address's signature, exactly, and the vacation reply — so
	// RestoreSettings can put it back. Neither is printed.
	SaveSettings(ctx context.Context) (savedSettings, error)
	RestoreSettings(ctx context.Context, s savedSettings) error
	// FiltersFrom lists the ids of the filters that match mail from from:
	// the run's own filters.
	FiltersFrom(ctx context.Context, from string) ([]string, error)
	// DeleteFiltersFrom deletes the filters that match mail from from:
	// the run's own, left by a step that failed.
	DeleteFiltersFrom(ctx context.Context, from string) (int, error)
}

// savedSettings are the account's own settings as the run found them.
type savedSettings struct {
	address, signature string
	vacation           json.RawMessage
}

// probeResult is how Gmail answered a probe. Reason and Message are
// Google's own words on a refusal; ID is set when the call created
// something.
type probeResult struct {
	status          int
	reason, message string
	id              string
	// retryAfter is the Retry-After header, for spike H.
	retryAfter string
}

// runLabel names one run. The name is what every read is scoped to, so
// it has to be unique to the run: a timestamp and random suffix.
type runLabel struct {
	name string
}

// labelPrefix starts every run label, so a leftover one from a crashed
// run is recognizable in the web UI.
const labelPrefix = "livemail-"

func newRunLabel(now time.Time) runLabel {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return runLabel{name: labelPrefix + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])}
}

// query scopes a Gmail search to the run's label.
func (r runLabel) query(q string) string {
	scoped := "label:" + r.name
	if q = strings.TrimSpace(q); q != "" {
		scoped += " " + q
	}
	return scoped
}

// seeded is what a run inserted: the label, every message and thread,
// and its drafts.
type seeded struct {
	label    runLabel
	labelID  string
	messages []string
	threads  []string
	drafts   []string
	// historyStart is the mailbox's history id before the first insert.
	historyStart string
	// extraLabels are labels a spike or step created, deleted at cleanup.
	extraLabels []string
	// draftMessages are the ids of messages inside the run's drafts, now
	// and before each save; a draft's message is the run's own.
	draftMessages []string
	// gone are messages the run deleted for good.
	gone []string
}

// owns reports whether an id is one the run made.
func (s *seeded) owns(id string) bool {
	return slices.Contains(s.messages, id) || slices.Contains(s.threads, id) || slices.Contains(s.drafts, id) ||
		slices.Contains(s.draftMessages, id)
}

// deleted marks messages the run deleted for good, so cleanup does not
// try to trash them. They stay in messages, whose order the spikes read
// beside threads.
func (s *seeded) deleted(ids ...string) { s.gone = append(s.gone, ids...) }

// forgetLabel drops a label the run deleted itself.
func (s *seeded) forgetLabel(id string) {
	s.extraLabels = slices.DeleteFunc(s.extraLabels, func(l string) bool { return l == id })
}

// forget drops a draft the run deleted itself, so cleanup does not.
func (s *seeded) forget(draftID string) {
	if i := slices.Index(s.drafts, draftID); i >= 0 {
		s.drafts = slices.Delete(s.drafts, i, i+1)
	}
}

// seedMailbox creates the label and inserts n synthetic messages. If an
// insert fails half-way, what was made is cleaned up before returning.
func seedMailbox(ctx context.Context, box mailbox, r runLabel, n int) (*seeded, error) {
	labelID, err := box.CreateLabel(ctx, r.name)
	if err != nil {
		return nil, fmt.Errorf("create the run's label: %w", err)
	}
	s := &seeded{label: r, labelID: labelID}
	if s.historyStart, err = box.HistoryID(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("read the history id: %w", err), cleanUp(ctx, box, s))
	}
	for i := range n {
		msg, thread, err := box.Insert(ctx, labelID, syntheticMessage(r, i))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("insert message %d: %w", i+1, err), cleanUp(ctx, box, s))
		}
		s.messages = append(s.messages, msg)
		s.threads = append(s.threads, thread)
	}
	for i := range runDrafts {
		id, err := box.CreateDraft(ctx, syntheticDraft(r, i))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("create draft %d: %w", i+1, err), cleanUp(ctx, box, s))
		}
		s.drafts = append(s.drafts, id)
	}
	return s, nil
}

// runDrafts is how many drafts a run makes: two, so list_drafts has a
// second page at max 1.
const runDrafts = 2

// cleanUp trashes every inserted message and deletes the label. It keeps
// going past a failure, so one stuck message does not strand the rest.
func cleanUp(ctx context.Context, box mailbox, s *seeded) error {
	var errs []error
	for _, id := range s.drafts {
		if err := box.DeleteDraft(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete a run draft: %w", err))
		}
	}
	for _, id := range s.messages {
		if slices.Contains(s.gone, id) {
			continue
		}
		if err := box.Trash(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("trash an inserted message: %w", err))
		}
	}
	for _, id := range s.extraLabels {
		if err := box.DeleteLabel(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete a label a spike created: %w", err))
		}
	}
	if s.labelID != "" {
		if err := box.DeleteLabel(ctx, s.labelID); err != nil {
			errs = append(errs, fmt.Errorf("delete the run's label: %w", err))
		}
	}
	return errors.Join(errs...)
}

// syntheticMessage builds message i of a run: addresses at reserved
// domains, a subject naming the run, a body from a template. Nothing in it
// came from anybody. The first carries an attachment, for
// download_attachment. The second offers to unsubscribe, one-click, at
// addresses that cannot resolve.
func syntheticMessage(r runLabel, i int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: Synthetic Sender <sender-%d@example.com>\r\n", i+1)
	b.WriteString("To: Synthetic Reader <reader@example.org>\r\n")
	fmt.Fprintf(&b, "Subject: %s message %d\r\n", r.name, i+1)
	fmt.Fprintf(&b, "Message-ID: <%s.%d@livemail.invalid>\r\n", r.name, i+1)
	b.WriteString("Date: " + syntheticDateHeader + "\r\n")
	if i == 1 {
		b.WriteString("List-Unsubscribe: <" + syntheticUnsubscribeMail + ">,\r\n <" + syntheticUnsubscribeURL(r) + ">\r\n")
		b.WriteString("List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	body := fmt.Sprintf("Synthetic body %d, written by the live driver for run %s.\r\n", i+1, r.name)
	if i != 0 {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n" + body)
		return []byte(b.String())
	}
	const boundary = "livemail-synthetic-boundary"
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Disposition: attachment; filename=\"" + syntheticAttachmentName + "\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString(syntheticAttachment(r)) + "\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

// syntheticUnsubscribeMail and syntheticUnsubscribeURL are the second
// message's List-Unsubscribe, at domains that never resolve.
const syntheticUnsubscribeMail = "mailto:leave@livemail.invalid?subject=unsubscribe"

func syntheticUnsubscribeURL(r runLabel) string {
	return "https://livemail.invalid/unsubscribe?run=" + r.name
}

// syntheticAttachmentName names the first message's attachment.
const syntheticAttachmentName = "livemail-synthetic.txt"

// syntheticAttachment is the first message's attachment, whose hash the
// download step checks.
func syntheticAttachment(r runLabel) []byte {
	return []byte("Synthetic attachment, written by the live driver for run " + r.name + ".\n")
}

// syntheticDraft builds draft i of a run, its subject naming the run so
// list_drafts can be scoped to it.
func syntheticDraft(r runLabel, i int) []byte {
	var b strings.Builder
	b.WriteString("To: Synthetic Reader <reader@example.org>\r\n")
	fmt.Fprintf(&b, "Subject: %s draft %d\r\n", r.name, i+1)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "Synthetic draft %d, written by the live driver for run %s.\r\n", i+1, r.name)
	return []byte(b.String())
}

// syntheticDateHeader is every inserted message's Date header, fixed and
// in the past, so spike F can tell it from the time of the insert.
const syntheticDateHeader = "Thu, 24 Sep 2026 10:00:00 +0000"
