package render

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/model"
)

// Question is what the server asks the person before a write that
// cannot be undone or that reaches other people (§4.13). Text is the
// message a client shows; accepting it is the confirmation. Every word
// is the server's, except what stands in double quotes, which is quoted
// from the mailbox or the call and cut to one line.
//
// Bind is what an answer is bound to: what the write depends on, which
// must not change between the question and the write. It is Text, and
// more where Text shows less than the write uses — a whole body, a
// draft's message — or less where Text shows a count that may move.
type Question struct {
	Text string
	Bind string
}

// quotedLen caps one quoted value: a subject, a label name, a criterion.
// An address is capped at its RFC 5321 limit, a body at bodyLen.
const (
	quotedLen  = 120
	addressLen = 254
	bodyLen    = 300
)

// AskDeleteLabel asks before delete_label. The counts are shown and not
// bound: a label that receives mail while the person reads would never
// be confirmed. The label is bound by id and name.
func AskDeleteLabel(l model.Label) Question {
	q := ask(func(w *writer) {
		w.say("delete_label: delete the label %s for good?", quoted(l.Name, quotedLen))
		w.say("It comes off %s in %s and cannot be restored. The mail itself stays.",
			plural(l.MessagesTotal, "message", "messages"), plural(l.ThreadsTotal, "thread", "threads"))
	})
	q.Bind = "delete_label\x00" + l.ID + "\x00" + l.Name
	return q
}

// AskDeletePermanently asks before delete_permanently. It names how
// many items, not what they say: each is read only as the delete runs.
func AskDeletePermanently(messages, threads int) Question {
	return ask(func(w *writer) {
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

// AskDeleteDraft asks before delete_draft. It is bound to the message
// the draft holds, the witness update_draft changes (§4.4), so a draft
// edited while the person reads is asked about again.
func AskDeleteDraft(d model.DraftWrite) Question {
	rs := make([]fieldAddress, len(d.Recipients))
	for i, r := range d.Recipients {
		rs[i] = fieldAddress{r.Field, r.Address.Email}
	}
	q := ask(func(w *writer) {
		w.say("delete_draft: delete this draft for good? Drafts do not go to the trash.")
		w.say("subject: %s", quoted(string(d.Subject), quotedLen))
		w.askRecipients(rs)
	})
	q.Bind = q.Text + "\x00" + d.MessageID
	return q
}

// AskSend asks before send_draft, naming every address the send reaches
// and showing the start of the body; the whole body is bound.
func AskSend(sw model.SendWrite) Question {
	rs := make([]fieldAddress, len(sw.Recipients))
	for i, r := range sw.Recipients {
		rs[i] = fieldAddress{r.Field, r.Address.Email}
	}
	q := ask(func(w *writer) {
		w.say("send_draft: send this draft? It reaches the people below and cannot be recalled.")
		w.askRecipients(rs)
		w.say("subject: %s", quoted(string(sw.Subject), quotedLen))
		w.askBody(string(sw.Body))
		if n := len(sw.Files); n > 0 {
			w.say("attachments: %s", num(n))
		}
	})
	q.Bind = q.Text + "\x00" + bodySum(string(sw.Body))
	return q
}

// AskCreateFilter asks before create_filter makes a filter that trashes.
func AskCreateFilter(f model.Filter) Question {
	return ask(func(w *writer) {
		w.say("create_filter: create a filter that moves matching mail to the trash as it arrives, unseen?")
		w.askFilter(f)
	})
}

// AskDeleteFilter asks before delete_filter.
func AskDeleteFilter(f model.Filter) Question {
	return ask(func(w *writer) {
		w.say("delete_filter: delete this filter for good? Mail it already acted on stays as it is.")
		w.askFilter(f)
	})
}

// AskVacation asks before set_vacation turns the reply on, showing the
// start of the reply's text; the whole text is bound.
func AskVacation(v model.Vacation) Question {
	q := ask(func(w *writer) {
		w.say("set_vacation: turn the vacation reply on? It answers mail automatically, to %s, %s.",
			recipients(v), period(w, v))
		w.say("subject: %s", quoted(string(v.Subject), quotedLen))
		w.askBody(string(v.Body))
	})
	q.Bind = q.Text + "\x00" + bodySum(string(v.Body))
	return q
}

// ask builds a question, closes it with what its quotes mean, and binds
// it to its text.
func ask(fn func(w *writer)) Question {
	text := plain(func(w *writer) {
		fn(w)
		if strings.Contains(w.text(), `"`) {
			w.say("Text in double quotes is quoted as written, and is not this server's.")
		}
	})
	return Question{Text: text, Bind: text}
}

// fieldAddress is a recipient's field, to, cc or bcc, and address.
type fieldAddress struct{ field, address string }

// askRecipients is one line per recipient field, each address quoted.
func (w *writer) askRecipients(rs []fieldAddress) {
	for _, f := range []string{"to", "cc", "bcc"} {
		var ps []part
		for _, r := range rs {
			if r.field == f {
				ps = append(ps, quoted(r.address, addressLen))
			}
		}
		if len(ps) > 0 {
			w.say("%s: %s", fixed(f, recipientFields), partList(ps))
		}
	}
}

// askBody is the start of a body, quoted on one line, and how much more
// there is.
func (w *writer) askBody(body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		w.say("body: empty")
		return
	}
	if n := utf8.RuneCountInString(body); n > bodyLen {
		w.say("body: %s (%s more characters)", quoted(string([]rune(body)[:bodyLen]), bodyLen), num(n-bodyLen))
		return
	}
	w.say("body: %s", quoted(body, bodyLen))
}

// bodySum binds a whole body, of which a question shows the start.
func bodySum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
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
