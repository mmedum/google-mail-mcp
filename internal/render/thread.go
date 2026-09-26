package render

import (
	"strings"

	"github.com/mmedum/google-mail-mcp/internal/model"
)

// Thread renders a conversation newest first under the budget, starting
// o.Cursor messages from the newest. Messages that do not fit are
// listed by id and date with the cursor that reads them, and their
// senders in a block. When even the first message does not fit, its
// body is cut and the result says where to continue.
func Thread(t model.Thread, o Options) Result {
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		n := len(t.Messages)
		skip := min(max(o.Cursor, 0), n)
		w.say("budget: %s characters", num(res.Budget))
		if skip > 0 {
			w.say("thread %s · %s · newest first · starting after the newest %s · labels: %s",
				gmailID(t.ID), plural(n, "message", "messages"), num(skip), labelList(t.Labels()))
		} else {
			w.say("thread %s · %s · newest first · labels: %s",
				gmailID(t.ID), plural(n, "message", "messages"), labelList(t.Labels()))
		}

		shown := 0
		for i := n - 1 - skip; i >= 0; i-- {
			m := t.Messages[i]
			// Showing message i leaves 0..i-1 for the "not shown" list,
			// which keeps room for its first few rows.
			reserve := w.sub(func(s *writer) { s.omittedList(t, i-1, skip+shown+1, min(i, minListed)) })
			whole := w.sub(func(s *writer) {
				s.blank()
				s.messageLine(m, i+1, n)
				s.messageBody(m, o, 0, 1<<30)
			})
			if w.len()+whole.len()+reserve.len() <= res.Budget {
				w.add(whole)
				shown++
				continue
			}
			if shown == 0 {
				// The newest message alone is over budget: cut its body.
				tail := w.sub(func(s *writer) {
					s.say("(the rest of this body is read with the message's own id, %s)", gmailID(m.ID))
				})
				w.blank()
				w.messageLine(m, i+1, n)
				if off := w.messageBody(m, o, 0, res.Budget-reserve.len()-tail.len()); off > 0 {
					w.add(tail)
					res.Truncated = true
					res.NextOffset = off
				}
				shown++
				continue
			}
			w.omitted(t, i, skip+shown, res.Budget, &res)
			return res
		}
		return res
	})
}

// minListed is how many rows of the "not shown" list a thread keeps
// room for when deciding whether one more message fits.
const minListed = 5

// omitted lists messages t.Messages[0..last] as not shown, as many
// rows as fit before the writer position end, up to maxListed. Every
// id is in res.Omitted either way.
func (w *writer) omitted(t model.Thread, last, next, end int, res *Result) {
	if last < 0 {
		return
	}
	res.Truncated = true
	res.NextCursor = next
	for i := last; i >= 0; i-- {
		res.Omitted = append(res.Omitted, t.Messages[i].ID)
	}
	rows := 0
	for rows < min(last+1, maxListed) {
		if w.len()+w.sub(func(s *writer) { s.omittedList(t, last, next, rows+1) }).len() > end {
			break
		}
		rows++
	}
	w.omittedList(t, last, next, rows)
}

// omittedList writes the "not shown" list with its first rows messages:
// ids and dates in the server's voice, senders in a block of their own.
func (w *writer) omittedList(t model.Thread, last, next, rows int) {
	if last < 0 {
		return
	}
	w.blank()
	w.say("not shown (older, over the budget): %s; cursor=%s reads them.", plural(last+1, "message", "messages"), num(next))
	var senders strings.Builder
	for k := range rows {
		m := t.Messages[last-k]
		w.say("  %s · %s", gmailID(m.ID), w.when(m.Date))
		senders.WriteString(m.ID + ": " + originText(m.Sender().Email) + "\n")
	}
	if rest := last + 1 - rows; rest > 0 {
		w.say("  … and %s more", num(rest))
	}
	if rows > 0 {
		w.block("senders of the messages not shown", "", "", senders.String())
	}
}

// ThreadList is one page of search_threads. Threads carry their
// messages read with format=metadata.
type ThreadList struct {
	Threads            []model.Thread
	NextPageToken      string
	ResultSizeEstimate int
}

// Threads renders a thread listing.
func Threads(l ThreadList, o Options) Result {
	return listing(o, l.Threads, l.NextPageToken, "thread", "threads",
		func(t model.Thread) string { return t.ID },
		func(w *writer, t model.Thread) {
			latest := t.Latest()
			count := plural(len(t.Messages), "message", "messages")
			if u := t.Unread(); u > 0 {
				w.say("thread %s · %s (%s unread) · %s · labels: %s · attachments: %s", gmailID(t.ID), count, num(u),
					w.when(latest.Date), labelList(t.Labels()), yesNo(t.HasAttachments()))
			} else {
				w.say("thread %s · %s · %s · labels: %s · attachments: %s", gmailID(t.ID), count,
					w.when(latest.Date), labelList(t.Labels()), yesNo(t.HasAttachments()))
			}
			snip := t.Snippet
			if snip == "" {
				snip = latest.Snippet
			}
			w.block("thread summary", latest.Sender().Email, latest.ID,
				"Participants: "+string(addrList(t.Participants()))+"\n"+
					"Subject: "+string(t.Subject())+"\n"+
					"Snippet: "+string(snip)+"\n")
		}, nil)
}

// MessageList is one page of search_messages, messages read with
// format=metadata.
type MessageList struct {
	Messages           []model.Message
	NextPageToken      string
	ResultSizeEstimate int
}

// Messages renders a message listing.
func Messages(l MessageList, o Options) Result {
	return listing(o, l.Messages, l.NextPageToken, "message", "messages",
		func(m model.Message) string { return m.ID },
		func(w *writer, m model.Message) {
			w.say("message %s · thread %s · %s · labels: %s · attachments: %s",
				gmailID(m.ID), gmailID(m.ThreadID), w.when(m.Date), labelList(m.Labels), yesNo(m.HasAttachments))
			w.block("message summary", m.Sender().Email, m.ID,
				"From: "+string(addrList(m.From))+"\n"+
					"To: "+string(addrList(m.To))+"\n"+
					"Subject: "+string(m.Subject)+"\n"+
					"Snippet: "+string(m.Snippet)+"\n")
		}, nil)
}

// DraftList is one page of list_drafts, each draft's message read with
// format=metadata.
type DraftList struct {
	Drafts             []model.Draft
	NextPageToken      string
	ResultSizeEstimate int
}

// Drafts renders a draft listing. Both ids are named on every row
// (§6.1).
func Drafts(l DraftList, o Options) Result {
	return listing(o, l.Drafts, l.NextPageToken, "draft", "drafts",
		func(d model.Draft) string { return d.ID },
		func(w *writer, d model.Draft) {
			m := d.Message
			w.say("draft %s · message %s · thread %s · %s", gmailID(d.ID), gmailID(m.ID), gmailID(m.ThreadID), w.when(m.Date))
			var b strings.Builder
			b.WriteString("To: " + string(addrList(m.To)) + "\n")
			if len(m.Cc) > 0 {
				b.WriteString("Cc: " + string(addrList(m.Cc)) + "\n")
			}
			b.WriteString("Subject: " + string(m.Subject) + "\n")
			b.WriteString("Snippet: " + string(m.Snippet) + "\n")
			w.block("draft summary", "", m.ID, b.String())
		}, nil)
}

// listing renders one page of a listing: what the page holds, then a
// row per item while the row and the list of rows after it fit. head,
// when set, adds lines after the page's own.
func listing[T any](o Options, items []T, token string, one, many phrase,
	id func(T) string, row func(w *writer, item T), head func(w *writer),
) Result {
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		w.say("budget: %s characters", num(res.Budget))
		w.listingHead(len(items), one, many, token)
		if head != nil {
			head(w)
		}
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = id(it)
		}
		for i, it := range items {
			r := w.sub(func(s *writer) {
				s.blank()
				row(s, it)
			})
			rest := w.sub(func(s *writer) { s.rowsOmitted(ids[i+1:]) })
			if w.len()+r.len()+rest.len() > res.Budget {
				w.rowsOmitted(ids[i:])
				res.Truncated = true
				res.Omitted = ids[i:]
				return res
			}
			w.add(r)
		}
		return res
	})
}

// rowsOmitted names rows of this page that did not fit.
func (w *writer) rowsOmitted(ids []string) {
	if len(ids) == 0 {
		return
	}
	w.blank()
	w.say("not shown (over the budget), from this page: %s", idList(ids))
}

// listingHead states what the page holds and whether the listing is
// complete — including the page Google returns empty with a token,
// which is not the end (§7.1). Gmail's resultSizeEstimate is not stated:
// a live run saw it answer 201 for a search that matched 3 (§18 row 33),
// and a model reads "about 201" as a count.
func (w *writer) listingHead(n int, one, many phrase, token string) {
	w.say("%s on this page", plural(n, one, many))
	switch {
	case token == "":
		w.say("complete: yes")
	case n == 0:
		w.say("complete: no — this page is empty but the listing continues; page_token=%s reads the next page.", gmailID(token))
	default:
		w.say("complete: no — page_token=%s reads the next page.", gmailID(token))
	}
}
