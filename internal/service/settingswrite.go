package service

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/model"
)

// The settings writes, registered only with GMAIL_ENABLE_SETTINGS
// (§7.9). Each reads what it changes first, so its result says what was
// there before, and a dry run stops after that read.

// MaxSignature is the longest signature Gmail keeps, in characters of
// its HTML.
const MaxSignature = 10000

// UpdateSignature sets the signature of one of the account's own
// send-as addresses, or its default one when none is named. The text is
// plain: it is escaped, and its line breaks become <br>, so no markup a
// caller passes reaches the signature. An empty text clears it.
func (s *Service) UpdateSignature(ctx context.Context, sendAs, text string) (model.SignatureWrite, error) {
	out := model.SignatureWrite{DryRun: gapi.WritesForbidden(ctx)}
	unlock, err := s.lockSettings(ctx, out.DryRun)
	if err != nil {
		return out, err
	}
	defer unlock()
	list, err := s.client.SendAs(ctx)
	if err != nil {
		return out, err
	}
	i, err := sender(list.SendAs, strings.TrimSpace(sendAs))
	if err != nil {
		return out, err
	}
	current := list.SendAs[i]
	out.Address, out.Before = current.SendAsEmail, model.HTMLText(current.Signature)
	sig := signatureHTML(text)
	if n := utf8.RuneCountInString(sig); n > MaxSignature {
		return out, gapi.Errf(gapi.ClassInvalid, "the signature is %d characters as HTML, over the %d Gmail keeps", n, MaxSignature)
	}
	if out.DryRun {
		out.After = model.HTMLText(sig)
		return out, nil
	}
	patched, err := s.client.PatchSignature(ctx, current.SendAsEmail, sig)
	if err != nil {
		return out, err
	}
	out.After = model.HTMLText(patched.Signature)
	return out, nil
}

// lockSettings holds the client's settings lock for a change that will
// write, from its first read to its write, so two changes sent at once
// neither overlap at Gmail, which refuses that (§18 row 54), nor read
// what the other is replacing. A dry run writes nothing and waits for
// nothing.
func (s *Service) lockSettings(ctx context.Context, dryRun bool) (unlock func(), err error) {
	if dryRun {
		return func() {}, nil
	}
	return s.client.LockSettings(ctx)
}

// sender is the index of the send-as address named, or of the default
// one when none is.
func sender(senders []gmail.SendAs, address string) (int, error) {
	if address == "" {
		return defaultSender(senders)
	}
	if i := slices.IndexFunc(senders, func(a gmail.SendAs) bool { return strings.EqualFold(a.SendAsEmail, address) }); i >= 0 {
		return i, nil
	}
	return 0, gapi.Errf(gapi.ClassInvalid, "send_as is not one of this account's send-as addresses; get_settings lists them")
}

// signatureHTML is plain text as a signature: escaped, lines joined
// with <br>, and runs of spaces kept, which HTML would collapse.
func signatureHTML(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		l = strings.ReplaceAll(html.EscapeString(l), "  ", "&nbsp; ")
		if strings.HasPrefix(l, " ") {
			l = "&nbsp;" + l[1:]
		}
		lines[i] = l
	}
	return strings.Join(lines, "<br>")
}

// FilterSpec is what create_filter was asked for.
type FilterSpec struct {
	Criteria gmail.FilterCriteria
	// AddLabels and RemoveLabels are label names or ids.
	AddLabels, RemoveLabels []string
	// The common actions, as flags.
	Archive, MarkRead, Star, Trash bool
	// Confirm is required when the filter trashes matching mail.
	Confirm bool
}

// Labels a filter may not add, and may not remove. Trash is its own
// flag, which takes confirm; Gmail refuses the rest.
var (
	filterNoAdd    = []string{"SENT", "DRAFT", "SPAM", "TRASH", "CHAT"}
	filterNoRemove = []string{"SENT", "DRAFT", "TRASH", "CHAT"}
)

// CreateFilter creates a filter. It acts on mail that arrives from now
// on; mail already in the mailbox is untouched, which is why a filter is
// not a write that takes a query (§4.7). It can never forward: the wire
// type it writes has no such field (§17.5).
func (s *Service) CreateFilter(ctx context.Context, sp FilterSpec) (model.FilterWrite, error) {
	out := model.FilterWrite{Op: "create", DryRun: gapi.WritesForbidden(ctx)}
	c := sp.Criteria
	for _, f := range []*string{&c.From, &c.To, &c.Subject, &c.Query, &c.NegatedQuery, &c.SizeComparison} {
		*f = strings.TrimSpace(*f)
	}
	// exclude_chats narrows a match; alone it matches nothing in particular.
	match := c
	match.ExcludeChats = false
	if match == (gmail.FilterCriteria{}) {
		return out, gapi.Errf(gapi.ClassInvalid, "a filter needs something to match: from, to, subject, query, negated_query, has_attachment or size")
	}
	if (c.Size != 0) != (c.SizeComparison != "") {
		return out, gapi.Errf(gapi.ClassInvalid, "size and size_comparison go together")
	}
	if c.SizeComparison != "" && c.SizeComparison != "larger" && c.SizeComparison != "smaller" {
		return out, gapi.Errf(gapi.ClassInvalid, "size_comparison is larger or smaller")
	}
	if sp.Trash && !sp.Confirm && !out.DryRun {
		return out, gapi.Errf(gapi.ClassBlocked,
			"this filter moves every matching message that arrives to the trash, unseen. Pass confirm: true to create it")
	}
	unlock, err := s.lockSettings(ctx, out.DryRun)
	if err != nil {
		return out, err
	}
	defer unlock()
	// The labels resolve the actions and the filters find a duplicate;
	// neither needs the other, so they are read at once.
	ls, existing, err := withLabels(ctx, s, s.client.Filters)
	if err != nil {
		return out, err
	}
	add, err := filterLabels(ls.all, sp.AddLabels, filterNoAdd, "add_labels")
	if err != nil {
		return out, err
	}
	remove, err := filterLabels(ls.all, sp.RemoveLabels, filterNoRemove, "remove_labels")
	if err != nil {
		return out, err
	}
	for _, f := range []struct {
		on   bool
		into *[]string
		id   string
	}{{sp.Archive, &remove, "INBOX"}, {sp.MarkRead, &remove, "UNREAD"}, {sp.Star, &add, "STARRED"}, {sp.Trash, &add, "TRASH"}} {
		if f.on && !slices.Contains(*f.into, f.id) {
			*f.into = append(*f.into, f.id)
		}
	}
	if len(add) == 0 && len(remove) == 0 {
		return out, gapi.Errf(gapi.ClassInvalid, "a filter needs an action: add_labels, remove_labels, archive, mark_read, star or trash")
	}
	if err := addedAndRemoved(add, remove); err != nil {
		return out, err
	}
	slices.Sort(add)
	slices.Sort(remove)
	w := gmail.FilterWrite{Criteria: &c, Action: &gmail.FilterWriteAction{AddLabelIDs: add, RemoveLabelIDs: remove}}
	for _, f := range existing.Filter {
		if sameFilter(f, w, false) {
			return out, gapi.Errf(gapi.ClassConflict, "filter %s already does this; list_filters shows it", f.ID)
		}
	}
	made := gmail.Filter{Criteria: w.Criteria, Action: &gmail.FilterAction{AddLabelIDs: add, RemoveLabelIDs: remove}}
	if !out.DryRun {
		created, err := s.client.CreateFilter(ctx, w)
		if classOf(err) == gapi.ClassAmbiguousOutcome {
			return out, s.settleFilter(ctx, w, existing.Filter, err)
		}
		if err != nil {
			return out, err
		}
		made = *created
	}
	out.Filter = model.NewFilters([]gmail.Filter{made}, ls.index)[0]
	return out, nil
}

// filterLabels resolves a filter's labels, refusing the ones it may not
// name in that field.
func filterLabels(all []gmail.Label, names, refused []string, field string) ([]string, error) {
	ids, err := resolveLabels(all, dedupe(names))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if slices.Contains(refused, id) {
			if id == "TRASH" {
				return nil, gapi.Errf(gapi.ClassInvalid, "%s cannot name TRASH; pass trash: true, which takes confirm", field)
			}
			return nil, gapi.Errf(gapi.ClassInvalid, "%s cannot name %s; a filter cannot change it", field, id)
		}
	}
	return ids, nil
}

// sameFilter reports whether an existing filter matches and acts as w
// would, labels in any order. A forward makes it different: w has none.
// With anySize, a size is not compared: Gmail rounds it (§18 row 52).
// A stored SPAM removal does not tell apart a filter w archives into:
// Gmail was seen adding it to every archiving filter some time after a
// filter write (§18 row 56).
func sameFilter(f gmail.Filter, w gmail.FilterWrite, anySize bool) bool {
	if f.Criteria == nil || f.Action == nil || f.Action.Forward != "" {
		return false
	}
	have, want := *f.Criteria, *w.Criteria
	if anySize {
		have.Size, want.Size = 0, 0
	}
	sorted := func(ids []string) []string {
		s := slices.Clone(ids)
		slices.Sort(s)
		return s
	}
	stored := f.Action.RemoveLabelIDs
	if slices.Contains(w.Action.RemoveLabelIDs, "INBOX") && !slices.Contains(w.Action.RemoveLabelIDs, "SPAM") {
		stored = slices.DeleteFunc(slices.Clone(stored), func(id string) bool { return id == "SPAM" })
	}
	return have == want && slices.Equal(sorted(f.Action.AddLabelIDs), sorted(w.Action.AddLabelIDs)) &&
		slices.Equal(sorted(stored), sorted(w.Action.RemoveLabelIDs))
}

// settleFilter reads the filters after a create Google did not confirm
// and returns the [ambiguous_outcome] error with what it found, in the
// spirit of §4.3: the create is never repeated, and the caller learns
// whether to make it again without listing the filters itself. before
// is the list read before the create.
func (s *Service) settleFilter(ctx context.Context, w gmail.FilterWrite, before []gmail.Filter, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	verdict, why := "unknown", "The read afterwards failed, so whether it was created is unknown; "+
		"list_filters shows it if it was. Do not create it again before looking"
	if s.client.Wait(ctx, settleDelay) == nil {
		if after, err := s.client.Filters(ctx); err == nil {
			i := slices.IndexFunc(after.Filter, func(f gmail.Filter) bool {
				return !slices.ContainsFunc(before, func(b gmail.Filter) bool { return b.ID == f.ID }) && sameFilter(f, w, true)
			})
			if i >= 0 {
				verdict, why = "created", fmt.Sprintf("Read afterwards: filter %s does this and was not there before. "+
					"It was created; do not create it again", after.Filter[i].ID)
			} else {
				again := "create_filter refuses it as a conflict if it has appeared by then"
				if w.Criteria.Size != 0 {
					// Gmail rounds a size (§18 row 52), which the duplicate
					// check compares exactly, so it could let a late one by.
					again = "run list_filters first: Gmail rounds a size, so create_filter may not see a late one as a duplicate"
				}
				verdict, why = "not_created", fmt.Sprintf("Read %s afterwards: no new filter does this. It looks not created, "+
					"though Gmail can save a write late; create it again if it is still wanted, and %s", settleDelay, again)
			}
		}
	}
	return gapi.Wrap(gapi.ClassAmbiguousOutcome, cause,
		"Google did not confirm that the filter was created, and the create was not repeated (verdict: %s). %s.", verdict, why)
}

// DeleteFilter deletes a filter. Mail it already acted on stays as it
// is. The filter is read from the list first, so the result says what it
// did; filters.get is written off, since the list returns the same.
func (s *Service) DeleteFilter(ctx context.Context, id string, confirm bool) (model.FilterWrite, error) {
	out := model.FilterWrite{Op: "delete", DryRun: gapi.WritesForbidden(ctx)}
	id = strings.TrimSpace(id)
	if id == "" {
		return out, gapi.Errf(gapi.ClassInvalid, "filter_id is required; list_filters shows the ids")
	}
	if !confirm && !out.DryRun {
		return out, gapi.Errf(gapi.ClassBlocked, "delete_filter cannot be undone. Pass confirm: true to delete it")
	}
	unlock, err := s.lockSettings(ctx, out.DryRun)
	if err != nil {
		return out, err
	}
	defer unlock()
	ls, res, err := withLabels(ctx, s, s.client.Filters)
	if err != nil {
		return out, err
	}
	i := slices.IndexFunc(res.Filter, func(f gmail.Filter) bool { return f.ID == id })
	if i < 0 {
		return out, gapi.Errf(gapi.ClassNotFound, "no filter has id %s; list_filters shows them", id)
	}
	out.Filter = model.NewFilters(res.Filter[i:i+1], ls.index)[0]
	if out.DryRun {
		return out, nil
	}
	if err := s.client.DeleteFilter(ctx, id); err != nil {
		// A 404 here means gone: already deleted elsewhere, or deleted by
		// this call when a retry followed an answer that was lost.
		if classOf(err) != gapi.ClassNotFound {
			return out, err
		}
		out.Gone = true
	}
	return out, nil
}

// VacationSpec is what set_vacation was asked for.
type VacationSpec struct {
	Enable bool
	// Subject and Body are the reply, sent exactly as given.
	Subject, Body string
	// Audience is who is answered: contacts or domain. Required when
	// enabling: an auto-reply to every sender is not offered (§17.4).
	Audience string
	// Start and End bound the reply, RFC 3339; either may be empty.
	Start, End string
	// Confirm is required to turn the reply on.
	Confirm bool
}

// SetVacation turns the vacation reply on with the text and audience
// given, or turns it off and leaves the text as it was. It writes to
// every sender it answers, so it is registered only with both the
// settings and the send flags, and turning it on takes confirm: true.
func (s *Service) SetVacation(ctx context.Context, sp VacationSpec) (model.VacationWrite, error) {
	out := model.VacationWrite{DryRun: gapi.WritesForbidden(ctx)}
	var w gmail.VacationWrite
	if sp.Enable {
		var err error
		if w, err = sp.wire(); err != nil {
			return out, err
		}
		if !sp.Confirm && !out.DryRun {
			return out, gapi.Errf(gapi.ClassBlocked,
				"set_vacation answers every sender it matches, automatically, until it is turned off. Pass confirm: true to turn it on")
		}
	}
	unlock, err := s.lockSettings(ctx, out.DryRun)
	if err != nil {
		return out, err
	}
	defer unlock()
	// audience domain also reads the profile, at once with the reply.
	var current *gmail.VacationSettings
	var profile *gmail.Profile
	reads := []func(ctx context.Context) error{
		func(ctx context.Context) (err error) { current, err = s.client.Vacation(ctx); return err },
	}
	if sp.Enable && sp.Audience == "domain" {
		reads = append(reads, func(ctx context.Context) (err error) { profile, err = s.client.Profile(ctx); return err })
	}
	if _, err := fanOut(ctx, len(reads), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, reads[i](ctx)
	}); err != nil {
		return out, err
	}
	if profile != nil {
		if err := workspaceOnly(profile.EmailAddress); err != nil {
			return out, err
		}
	}
	out.Before = model.NewVacation(*current)
	if !sp.Enable {
		// Off keeps the reply as it was, so turning it on again later
		// does not need it rewritten.
		w = current.Write()
		w.EnableAutoReply = false
	}
	after := w.Settings()
	if !out.DryRun {
		updated, err := s.client.UpdateVacation(ctx, w)
		if err != nil {
			return out, err
		}
		after = *updated
	}
	out.After = model.NewVacation(after)
	return out, nil
}

// consumerDomains are the domains of personal Google accounts, which
// have no domain of their own for restrictToDomain to mean.
var consumerDomains = []string{"gmail.com", "googlemail.com"}

// workspaceOnly refuses audience domain on a personal account. Whether
// Gmail refuses it there, or accepts it and answers every sender, has
// not been checked live, and the second would be the audience §17.4
// rules out; so the server does not ask. It costs one profile read.
func workspaceOnly(address string) error {
	_, domain, _ := strings.Cut(strings.ToLower(address), "@")
	if slices.Contains(consumerDomains, domain) {
		return gapi.Errf(gapi.ClassInvalid, "audience domain needs a Google Workspace account; this one is a personal "+
			"Gmail account, so use contacts")
	}
	return nil
}

// wire checks a reply being turned on and builds it.
func (sp VacationSpec) wire() (gmail.VacationWrite, error) {
	w := gmail.VacationWrite{EnableAutoReply: true, ResponseSubject: strings.TrimSpace(sp.Subject),
		ResponseBodyPlainText: strings.TrimSpace(sp.Body)}
	if w.ResponseBodyPlainText == "" {
		return w, gapi.Errf(gapi.ClassInvalid, "body is required to turn the vacation reply on")
	}
	switch sp.Audience {
	case "contacts":
		w.RestrictToContacts = true
	case "domain":
		w.RestrictToDomain = true
	case "":
		return w, gapi.Errf(gapi.ClassInvalid,
			"audience is required to turn the vacation reply on: contacts, or domain (Google Workspace accounts)")
	default:
		return w, gapi.Errf(gapi.ClassInvalid, "audience is contacts or domain")
	}
	start, end, err := vacationTimes(sp.Start, sp.End)
	if err != nil {
		return w, err
	}
	w.StartTime, w.EndTime = start, end
	return w, nil
}

// vacationTimes reads the bounds as a search reads its own, an RFC 3339
// time or a date (midnight UTC), and writes them as Gmail takes them,
// milliseconds since the epoch.
func vacationTimes(start, end string) (string, string, error) {
	parse := func(name, v string) (time.Time, error) {
		if strings.TrimSpace(v) == "" {
			return time.Time{}, nil
		}
		t, err := parseInstant(strings.TrimSpace(v), time.UTC)
		if err != nil {
			return t, gapi.Errf(gapi.ClassInvalid, "%s is not an RFC 3339 time or a YYYY-MM-DD date", name)
		}
		return t, nil
	}
	s, err := parse("start", start)
	if err != nil {
		return "", "", err
	}
	e, err := parse("end", end)
	if err != nil {
		return "", "", err
	}
	if !s.IsZero() && !e.IsZero() && !e.After(s) {
		return "", "", gapi.Errf(gapi.ClassInvalid, "end is not after start")
	}
	ms := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	return ms(s), ms(e), nil
}
