//go:build live

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
//   - Probe makes one call for a spike and returns only its status.
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
	Probe(ctx context.Context, method, path string, q url.Values) (status int)
	// InternalDate reads the date Gmail recorded for a message the run
	// inserted, in milliseconds, for spike F.
	InternalDate(ctx context.Context, messageID string) (int64, error)
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
}

// owns reports whether an id is one the run inserted.
func (s *seeded) owns(id string) bool {
	return slices.Contains(s.messages, id) || slices.Contains(s.threads, id) || slices.Contains(s.drafts, id)
}

// seedMailbox creates the label and inserts n synthetic messages. If an
// insert fails half-way, what was made is cleaned up before returning.
func seedMailbox(ctx context.Context, box mailbox, r runLabel, n int) (*seeded, error) {
	labelID, err := box.CreateLabel(ctx, r.name)
	if err != nil {
		return nil, fmt.Errorf("create the run's label: %w", err)
	}
	s := &seeded{label: r, labelID: labelID}
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
		if err := box.Trash(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("trash an inserted message: %w", err))
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
// came from anybody.
func syntheticMessage(r runLabel, i int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: Synthetic Sender <sender-%d@example.com>\r\n", i+1)
	b.WriteString("To: Synthetic Reader <reader@example.org>\r\n")
	fmt.Fprintf(&b, "Subject: %s message %d\r\n", r.name, i+1)
	fmt.Fprintf(&b, "Message-ID: <%s.%d@livemail.invalid>\r\n", r.name, i+1)
	b.WriteString("Date: " + syntheticDateHeader + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "Synthetic body %d, written by the live driver for run %s.\r\n", i+1, r.name)
	return []byte(b.String())
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
