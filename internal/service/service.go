// Package service holds the rules of reading and writing a mailbox:
// which requests a tool makes, in what order, and what it refuses. The
// tools are thin over it so the rules are tested in one place.
package service

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// Page sizes for listings (architecture §7.1). The default is small on
// purpose: every row of a thread listing is a threads.get at 40 units.
const (
	DefaultMax = 20
	MaxMax     = 100
)

// listingHeaders are what a listing reads per row with format=metadata.
// Content-Type is how a metadata read infers attachments. A read costs
// the same whatever headers it names (§18 row 80).
var listingHeaders = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "Content-Type",
	"List-Unsubscribe", "List-Unsubscribe-Post"}

// Service reads the mailbox through one client.
type Service struct {
	client *gapi.Client
}

// New returns a Service over c.
func New(c *gapi.Client) *Service { return &Service{client: c} }

// Search is what a search tool was asked for.
type Search struct {
	Q             string
	After, Before string
	// Location is the zone a YYYY-MM-DD bound is midnight in; nil is UTC.
	Location         *time.Location
	Labels           []string
	IncludeSpamTrash bool
	Max              int
	PageToken        string
}

// Searched is the query a search actually ran, so the result can state
// it: after and before become epoch seconds appended to q (§6.3).
type Searched struct {
	Q      string
	After  time.Time
	Before time.Time
}

// labelSet is the label list, and the index that names a label id.
type labelSet struct {
	index model.LabelIndex
	all   []gmail.Label
}

// labels reads the label list. It is read per call, never cached across
// calls, so a label renamed in the web UI is never resolved to its old
// name (§6.2). It costs one unit.
func (s *Service) labels(ctx context.Context) (labelSet, error) {
	res, err := s.client.ListLabels(ctx)
	if err != nil {
		return labelSet{}, err
	}
	return labelSet{index: model.NewLabelIndex(res.Labels), all: res.Labels}, nil
}

// withLabels runs read while the label list is read alongside it, since
// every read names labels and neither waits for the other. A failure of
// the label list is reported first, as when it was read first, and
// cancels read.
func withLabels[T any](ctx context.Context, s *Service, read func(context.Context) (T, error)) (labelSet, T, error) {
	var (
		ls   labelSet
		lerr error
		wg   sync.WaitGroup
	)
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg.Go(func() {
		if ls, lerr = s.labels(ctx); lerr != nil {
			cancel()
		}
	})
	v, err := read(readCtx)
	wg.Wait()
	if lerr != nil {
		var zero T
		return labelSet{}, zero, lerr
	}
	return ls, v, err
}

// ParseZone reads an IANA zone name. The empty name is no zone, nil.
func ParseZone(name string) (*time.Location, error) {
	if name == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, gapi.Errf(gapi.ClassInvalid, "time_zone %q is not an IANA zone name such as Europe/Copenhagen", name)
	}
	return loc, nil
}

// pageSize applies a listing's default and bound to a requested page
// size: DefaultMax and MaxMax for mail listings.
func pageSize(max int) (int, error) { return pageSizeOf(max, DefaultMax, MaxMax) }

func pageSizeOf(max, def, limit int) (int, error) {
	switch {
	case max == 0:
		return def, nil
	case max < 0 || max > limit:
		return 0, gapi.Errf(gapi.ClassInvalid, "max must be between 1 and %d", limit)
	}
	return max, nil
}

// resolveLabels turns ids or names into ids (§6.2): a system label by
// its id in any case, a user label by id, exact name, or name in any
// case. A name that matches nothing is not_found. Gmail refuses two
// labels differing only in case (spike I), so two case-insensitive
// matches should not happen; if a label list ever has them, the answer
// is ambiguous rather than a guess.
func resolveLabels(all []gmail.Label, wanted []string) ([]string, error) {
	out := make([]string, 0, len(wanted))
	for _, w := range wanted {
		id, err := resolveLabel(all, w)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func resolveLabel(all []gmail.Label, w string) (string, error) {
	for _, l := range all {
		if l.ID == w || (l.Type == gmail.LabelTypeSystem && strings.EqualFold(l.ID, w)) {
			return l.ID, nil
		}
	}
	for _, l := range all {
		if l.Name == w {
			return l.ID, nil
		}
	}
	var folded []string
	for _, l := range all {
		if strings.EqualFold(l.Name, w) {
			folded = append(folded, l.ID)
		}
	}
	switch len(folded) {
	case 1:
		return folded[0], nil
	case 0:
		return "", gapi.Errf(gapi.ClassNotFound, "no label is named %q; list_labels shows the names and ids", w)
	}
	return "", gapi.Errf(gapi.ClassAmbiguous,
		"%d labels match %q when case is ignored (ids %s); pass the id", len(folded), w, strings.Join(folded, ", "))
}

// query builds q from the search and its after and before bounds, which
// are sent as epoch seconds because Gmail reads a date in q as midnight
// Pacific (§2.9).
func query(in Search) (Searched, error) {
	out := Searched{Q: strings.TrimSpace(in.Q)}
	loc := in.Location
	if loc == nil {
		loc = time.UTC
	}
	var parts []string
	if out.Q != "" {
		parts = append(parts, out.Q)
	}
	for _, b := range []struct {
		name, val string
		dst       *time.Time
	}{{"after", in.After, &out.After}, {"before", in.Before, &out.Before}} {
		if b.val == "" {
			continue
		}
		t, err := parseInstant(b.val, loc)
		if err != nil {
			return out, gapi.Errf(gapi.ClassInvalid, "%s must be an RFC 3339 time or a YYYY-MM-DD date: %v", b.name, err)
		}
		*b.dst = t
		parts = append(parts, b.name+":"+strconv.FormatInt(t.Unix(), 10))
	}
	if !out.After.IsZero() && !out.Before.IsZero() && !out.After.Before(out.Before) {
		return out, gapi.Errf(gapi.ClassInvalid, "after must be earlier than before")
	}
	out.Q = strings.Join(parts, " ")
	return out, nil
}

// parseInstant reads an RFC 3339 time, or a date meaning its midnight in
// loc.
func parseInstant(v string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.ParseInLocation(time.DateOnly, v, loc)
}

// listOptions checks a search and builds its listing, before anything
// is sent. The label filter is resolved later, against the label list.
func listOptions(in Search) (gapi.ListOptions, Searched, error) {
	searched, err := query(in)
	if err != nil {
		return gapi.ListOptions{}, searched, err
	}
	size, err := pageSize(in.Max)
	if err != nil {
		return gapi.ListOptions{}, searched, err
	}
	return gapi.ListOptions{Q: searched.Q, IncludeSpamTrash: in.IncludeSpamTrash, Max: size, PageToken: in.PageToken},
		searched, nil
}

// fetchParts reads every part the messages left behind an attachment id
// that their bodies need, all in one fan-out, and rebuilds only the
// messages that had any. wire[i] is what msgs[i] was built from.
func (s *Service) fetchParts(ctx context.Context, labels model.LabelIndex, wire []*gmail.Message, msgs []model.Message) error {
	type part struct {
		msg int
		ref mime.PartRef
	}
	var parts []part
	for i, m := range msgs {
		for _, p := range m.NeedsFetch {
			parts = append(parts, part{msg: i, ref: p})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	got, err := fanOut(ctx, len(parts), func(ctx context.Context, i int) ([]byte, error) {
		body, err := s.client.GetAttachment(ctx, msgs[parts[i].msg].ID, parts[i].ref.AttachmentID)
		if err != nil {
			return nil, err
		}
		return decodePart(body.Data)
	})
	if err != nil {
		return err
	}
	// Per message, keyed by part id, as model expects.
	fetched := make([]map[string][]byte, len(msgs))
	for i, p := range parts {
		if fetched[p.msg] == nil {
			fetched[p.msg] = map[string][]byte{}
		}
		fetched[p.msg][p.ref.PartID] = got[i]
	}
	for i, w := range wire {
		if fetched[i] == nil {
			continue
		}
		if msgs[i], err = model.NewMessage(w, labels, fetched[i]); err != nil {
			return err
		}
	}
	return nil
}
