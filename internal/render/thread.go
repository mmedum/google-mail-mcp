package render

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/model"
)

// Thread renders a conversation newest first under the budget, starting
// o.Cursor messages from the newest. Messages that do not fit are
// listed by id and date with the cursor that reads them, and their
// senders in a block. When even the first message does not fit, its
// body is cut and the result says where to continue. The thread's
// drafts follow the conversation in a section of their own, on the
// first read only, and the cursor counts only what was said (§17.2).
func Thread(t model.Thread, o Options) Result {
	said, drafts := t.SplitDrafts()
	if o.Cursor > 0 {
		drafts = nil
	}
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		n := len(said)
		skip := min(max(o.Cursor, 0), n)
		count := plural(n, "message", "messages")
		if len(drafts) > 0 {
			count = fill("%s and %s", count, plural(len(drafts), "draft", "drafts"))
		}
		w.say("budget: %s characters", num(res.Budget))
		if skip > 0 {
			w.say("thread %s · %s · newest first · starting after the newest %s · labels: %s",
				gmailID(t.ID), count, num(skip), labelList(t.Labels()))
		} else {
			w.say("thread %s · %s · newest first · labels: %s",
				gmailID(t.ID), count, labelList(t.Labels()))
		}

		// The drafts' own lines are kept room for; their bodies are
		// shown if they fit after the conversation.
		brief := w.sub(func(s *writer) { s.drafts(drafts, o, false) })
		end := res.Budget - brief.len()

		shown := 0
		for i := n - 1 - skip; i >= 0; i-- {
			m := said[i]
			// Showing message i leaves 0..i-1 for the "not shown" list,
			// which keeps room for its first few rows.
			reserve := w.sub(func(s *writer) { s.omittedList(said, i-1, skip+shown+1, min(i, minListed)) })
			whole := w.sub(func(s *writer) {
				s.blank()
				s.messageLine(m, i+1, n)
				s.messageBody(m, o, 0, 1<<30)
			})
			if w.len()+whole.len()+reserve.len() <= end {
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
				if off := w.messageBody(m, o, 0, end-reserve.len()-tail.len()); off > 0 {
					w.add(tail)
					res.Truncated = true
					res.NextOffset = off
				}
				shown++
				continue
			}
			w.omitted(said, i, skip+shown, end, &res)
			break
		}
		if full := w.sub(func(s *writer) { s.drafts(drafts, o, true) }); w.len()+full.len() <= res.Budget {
			w.add(full)
		} else {
			w.add(brief)
		}
		return res
	})
}

// drafts writes the section that follows a conversation: its drafts,
// which were never sent. With bodies false it lists them by id and date
// only, and says how to read them.
func (w *writer) drafts(ds []model.Message, o Options, bodies bool) {
	if len(ds) == 0 {
		return
	}
	w.blank()
	w.say("drafts in this thread, not sent: %s", plural(len(ds), "draft", "drafts"))
	if !bodies {
		// Newest first, and few enough that the room kept for them never
		// crowds out the conversation.
		for i := len(ds) - 1; i >= max(len(ds)-minListed, 0); i-- {
			w.say("  message %s · %s", gmailID(ds[i].ID), w.when(ds[i].Date))
		}
		if rest := len(ds) - minListed; rest > 0 {
			w.say("  … and %s more; list_drafts lists them", num(rest))
		}
		w.say("(over the budget; get_message reads each draft by its message id)")
		return
	}
	for i := len(ds) - 1; i >= 0; i-- {
		w.blank()
		w.draftLine(ds[i], i+1, len(ds))
		w.messageBody(ds[i], o, 0, 1<<30)
	}
}

// minListed is how many rows of the "not shown" list a thread keeps
// room for when deciding whether one more message fits.
const minListed = 5

// omitted lists messages ms[0..last] as not shown, as many
// rows as fit before the writer position end, up to maxListed. Every
// id is in res.Omitted either way.
func (w *writer) omitted(ms []model.Message, last, next, end int, res *Result) {
	if last < 0 {
		return
	}
	res.Truncated = true
	res.NextCursor = next
	for i := last; i >= 0; i-- {
		res.Omitted = append(res.Omitted, ms[i].ID)
	}
	rows := 0
	for rows < min(last+1, maxListed) {
		if w.len()+w.sub(func(s *writer) { s.omittedList(ms, last, next, rows+1) }).len() > end {
			break
		}
		rows++
	}
	w.omittedList(ms, last, next, rows)
}

// omittedList writes the "not shown" list with its first rows messages:
// ids and dates in the server's voice, senders in a block of their own.
func (w *writer) omittedList(ms []model.Message, last, next, rows int) {
	if last < 0 {
		return
	}
	w.blank()
	w.say("not shown (older, over the budget): %s; cursor=%s reads them.", plural(last+1, "message", "messages"), num(next))
	var senders strings.Builder
	for k := range rows {
		m := ms[last-k]
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
// row per item while the row and the line naming the rows after it fit.
// head, when set, adds lines after the page's own. The rows that do not
// fit are counted and their ids named in the text; Omitted holds each
// of those ids once, less any a shown row carries, and Shown is how
// many rows were shown.
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
		whole := o.Reply != nil
		cost := func(text string, n int) int {
			if whole {
				return 2 * (JSONChars(text) - 2)
			}
			return n
		}
		spent := cost(w.text(), w.len())
		if whole {
			spent += o.Reply.Fixed
		}
		rows := make([]fragment, len(items))
		costs := make([]int, len(items))
		total := spent
		for i, it := range items {
			rows[i] = w.sub(func(s *writer) {
				s.blank()
				row(s, it)
			})
			costs[i] = cost(rows[i].s, rows[i].len())
			if whole && i < len(o.Reply.Rows) {
				costs[i] += o.Reply.Rows[i]
			}
			total += costs[i]
		}
		// A page that fits whole needs no line naming what was left out,
		// so nothing is reserved for one.
		if total <= res.Budget {
			for _, r := range rows {
				w.add(r)
			}
			res.Shown = len(items)
			return res
		}
		// Otherwise the line, at most: every row counted, and with Reply
		// every id as omitted_ids. Reserved once, not measured per row.
		all := w.sub(func(s *writer) { s.rowsOmitted(len(items), one, many, distinct(ids)) })
		reserve := cost(all.s, all.len())
		if whole {
			reserve += JSONChars(ids)
		}
		for i := range items {
			if spent+costs[i]+reserve > res.Budget {
				res.Truncated = true
				res.Shown = i
				res.Omitted = unseen(ids[:i], ids[i:])
				w.rowsOmitted(len(items)-i, one, many, distinct(ids[i:]))
				return res
			}
			w.add(rows[i])
			spent += costs[i]
		}
		res.Shown = len(items)
		return res
	})
}

// distinct is ids without repeats, in order: a listing of changes can
// name one message twice.
func distinct(ids []string) []string { return unseen(nil, ids) }

// unseen is rest without the ids shown already carries and without
// repeats, in order.
func unseen(shown, rest []string) []string {
	seen := make(map[string]bool, len(shown))
	for _, id := range shown {
		seen[id] = true
	}
	out := make([]string, 0, len(rest))
	for _, id := range rest {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// JSONChars is how many characters v takes as JSON, as encoding/json
// writes it: a line break in a string is two, and each angle bracket of
// a block's boundary six.
func JSONChars(v any) int {
	b, _ := json.Marshal(v)
	return utf8.RuneCount(b)
}

// rowsOmitted counts the rows of this page that did not fit and names
// their ids, each once.
func (w *writer) rowsOmitted(n int, one, many phrase, ids []string) {
	if n == 0 {
		return
	}
	w.blank()
	w.say("not shown (over the budget), %s from this page: %s", plural(n, one, many), idList(ids))
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
