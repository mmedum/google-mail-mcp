package render

import (
	"slices"
	"strings"

	"github.com/mmedum/google-mail-mcp/internal/model"
)

// Question is what the server asks the person before a write that
// cannot be undone or that reaches other people (§4.13). Text is the
// message a client shows; Title labels the one box the person ticks.
// Every word is the server's, except what stands in double quotes,
// which is quoted from the mailbox or the call and cut to one line.
type Question struct {
	Title string
	Text  string
}

// quotedLen caps one quoted value: a subject, a label name, a criterion.
// An address is capped at its RFC 5321 limit instead.
const (
	quotedLen  = 120
	addressLen = 254
)

// AskDeleteLabel asks before delete_label.
func AskDeleteLabel(l model.Label) Question {
	return ask("Delete the label for good", func(w *writer) {
		w.say("delete_label: delete the label %s for good?", quoted(l.Name, quotedLen))
		w.say("It comes off %s in %s and cannot be restored. The mail itself stays.",
			plural(l.MessagesTotal, "message", "messages"), plural(l.ThreadsTotal, "thread", "threads"))
	})
}

// AskDeletePermanently asks before delete_permanently. It names how
// many items, not what they say: each is read only as the delete runs.
func AskDeletePermanently(messages, threads int) Question {
	return ask("Delete them for good", func(w *writer) {
		switch {
		case threads == 0:
			w.say("delete_permanently: delete %s for good?", plural(messages, "message", "messages"))
		case messages == 0:
			w.say("delete_permanently: delete %s, every message in them, for good?", plural(threads, "thread", "threads"))
		default:
			w.say("delete_permanently: delete %s and %s, every message in them, for good?",
				plural(messages, "message", "messages"), plural(threads, "thread", "threads"))
		}
		w.say("They skip the trash and cannot be restored.")
	})
}

// AskDeleteDraft asks before delete_draft.
func AskDeleteDraft(d model.DraftWrite) Question {
	return ask("Delete the draft for good", func(w *writer) {
		w.say("delete_draft: delete this draft for good? Drafts do not go to the trash.")
		w.say("subject: %s", quoted(string(d.Subject), quotedLen))
		for _, f := range []string{"to", "cc", "bcc"} {
			var as []string
			for _, r := range d.Recipients {
				if r.Field == f {
					as = append(as, r.Address.Email)
				}
			}
			w.askAddresses(f, as)
		}
	})
}

// AskSend asks before send_draft, naming every address the send reaches.
func AskSend(sw model.SendWrite) Question {
	return ask("Send it", func(w *writer) {
		w.say("send_draft: send this draft? It reaches the people below and cannot be recalled.")
		for _, f := range []string{"to", "cc", "bcc"} {
			var as []string
			for _, r := range sw.Recipients {
				if r.Field == f {
					as = append(as, r.Address.Email)
				}
			}
			w.askAddresses(f, as)
		}
		w.say("subject: %s", quoted(string(sw.Subject), quotedLen))
		if n := len(sw.Files); n > 0 {
			w.say("attachments: %s", num(n))
		}
	})
}

// AskCreateFilter asks before create_filter makes a filter that trashes.
func AskCreateFilter(f model.Filter) Question {
	return ask("Create the filter", func(w *writer) {
		w.say("create_filter: create a filter that moves matching mail to the trash as it arrives, unseen?")
		w.askFilter(f)
	})
}

// AskDeleteFilter asks before delete_filter.
func AskDeleteFilter(f model.Filter) Question {
	return ask("Delete the filter for good", func(w *writer) {
		w.say("delete_filter: delete this filter for good? Mail it already acted on stays as it is.")
		w.askFilter(f)
	})
}

// AskVacation asks before set_vacation turns the reply on.
func AskVacation(v model.Vacation) Question {
	return ask("Turn the reply on", func(w *writer) {
		w.say("set_vacation: turn the vacation reply on? It answers mail automatically, to %s, %s.",
			recipients(v), period(w, v))
		w.say("subject: %s", quoted(string(v.Subject), quotedLen))
	})
}

// ask builds a question and closes it with what its quotes mean.
func ask(title string, fn func(w *writer)) Question {
	return Question{Title: title, Text: plain(func(w *writer) {
		fn(w)
		if strings.Contains(w.text(), `"`) {
			w.say("Text in double quotes is quoted as written, and is not this server's.")
		}
	})}
}

// askAddresses is one recipient field's addresses, each quoted.
func (w *writer) askAddresses(field string, as []string) {
	if len(as) == 0 {
		return
	}
	ps := make([]part, len(as))
	for i, a := range as {
		ps[i] = quoted(a, addressLen)
	}
	w.say("%s: %s", fixed(field, recipientFields), partList(ps))
}

// askFilter is what a filter matches and does, with every value quoted.
func (w *writer) askFilter(f model.Filter) {
	c := f.Criteria
	var ms []part
	if c.From != "" {
		ms = append(ms, fill("from %s", quoted(c.From, quotedLen)))
	}
	if c.To != "" {
		ms = append(ms, fill("to %s", quoted(c.To, quotedLen)))
	}
	if c.Subject != "" {
		ms = append(ms, fill("subject %s", quoted(c.Subject, quotedLen)))
	}
	if c.Query != "" {
		ms = append(ms, fill("search %s", quoted(c.Query, quotedLen)))
	}
	if c.NegatedQuery != "" {
		ms = append(ms, fill("not %s", quoted(c.NegatedQuery, quotedLen)))
	}
	if c.HasAttachment {
		ms = append(ms, fill("has an attachment"))
	}
	if c.ExcludeChats {
		ms = append(ms, fill("not chats"))
	}
	if c.Size > 0 {
		ms = append(ms, fill("size %s %s", oneOf(c.SizeComparison, "larger", "smaller", "unspecified"), size(int64(c.Size))))
	}
	if len(ms) > 0 {
		w.say("matches: %s", partList(ms))
	}
	// TRASH added is said as the move to the trash, below.
	if add := slices.DeleteFunc(slices.Clone(f.Add), func(l model.LabelRef) bool { return l.ID == "TRASH" }); len(add) > 0 {
		w.say("adds labels: %s", askLabels(add))
	}
	if remove := slices.DeleteFunc(slices.Clone(f.Remove), func(l model.LabelRef) bool { return l.ID == "SPAM" }); len(remove) > 0 {
		w.say("removes labels: %s", askLabels(remove))
	}
	if f.NeverSpam() {
		w.say("never sends matching mail to spam")
	}
	if f.Forward != "" {
		w.say("forwards matching mail to %s", quoted(f.Forward, addressLen))
	}
	if f.Trashes() {
		w.say("moves matching mail to the trash, where Gmail deletes it after 30 days")
	}
}

// askLabels is labels by name, each quoted.
func askLabels(ls []model.LabelRef) part {
	ps := make([]part, len(ls))
	for i, l := range ls {
		ps[i] = quoted(l.Name, quotedLen)
	}
	return partList(ps)
}
