package render

import (
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// The writes' renderings say what the write produced, from Gmail's
// answer (§4.12). A dry run says so on its first line and speaks in
// "would". A draft's headers, recipients and file names are text a
// sender may have shaped — a reply's recipients come from the parent, a
// file in GMAIL_LOCAL_DIR may carry a name download_attachment took from
// a sender — so they sit in a block; ids, counts and label names are the
// server's.

var (
	outcomes = map[string]bool{string(model.Changed): true, string(model.Unchanged): true,
		string(model.WouldChange): true, string(model.Failed): true}
	verbNames = set(func(yield func(string) bool) {
		for _, v := range model.Verbs {
			if !yield(v.Name) {
				return
			}
		}
	})
	draftFields    = set(slices.Values(model.DraftFields))
	labelFields    = set(slices.Values(model.LabelFields))
	labelListWords = set(maps.Keys(model.LabelListWords))
	messageList    = set(maps.Keys(model.MessageListWords))
)

// set is the words a producer owns, for fixed: render never types out a
// list the model already holds.
func set(words iter.Seq[string]) map[string]bool {
	out := map[string]bool{}
	for w := range words {
		out[w] = true
	}
	return out
}

// DraftWrite renders a created, updated or deleted draft.
func DraftWrite(d model.DraftWrite, o Options) Result {
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		if d.DryRun {
			w.say("dry run: nothing was saved or deleted; this is what the call would do.")
		}
		switch d.Op {
		case "create":
			w.draftCreated(d)
		case "update":
			w.draftUpdated(d)
		default:
			w.draftDeleted(d)
		}
		if d.Op != "delete" {
			how := fill("as JSON")
			if d.Upload {
				how = fill("as a media upload, since it is over 5 MB")
			}
			w.say("size: %s, sent %s", size(int64(d.Bytes)), how)
		}
		w.block("draft headers", "", d.MessageID, draftHeaders(d))
		return res
	})
}

func (w *writer) draftCreated(d model.DraftWrite) {
	switch {
	case d.DryRun && d.ThreadID != "":
		w.say("would create a draft in thread %s", gmailID(d.ThreadID))
	case d.DryRun:
		w.say("would create a draft in a new thread")
	default:
		w.say("created draft %s · message %s · thread %s · labels: %s", gmailID(d.DraftID), gmailID(d.MessageID),
			gmailID(d.ThreadID), labelList(d.Labels))
		w.say("update_draft needs draft_id %s and message_id %s.", gmailID(d.DraftID), gmailID(d.MessageID))
	}
	if r := d.Reply; r != nil {
		w.replyLines(d, r)
	}
	w.recipientCounts(d.Recipients)
}

func (w *writer) replyLines(d model.DraftWrite, r *model.Reply) {
	if r.FromThread {
		w.say("reply to message %s, the newest in thread %s that is not a draft, in the trash or a reaction",
			gmailID(r.ParentID), gmailID(r.ParentThreadID))
	} else {
		w.say("reply to message %s in thread %s", gmailID(r.ParentID), gmailID(r.ParentThreadID))
	}
	switch {
	case d.DryRun:
	case r.Joined:
		w.say("Gmail filed the draft in the parent's thread.")
	default:
		w.say("note: Gmail filed the draft in a new thread %s, not the parent's; it will not read as a reply there.",
			gmailID(d.ThreadID))
	}
	if r.NoMessageID {
		w.say("note: the parent has no usable Message-ID, so In-Reply-To and References were not written; Gmail may not thread the reply.")
	}
	if r.DroppedOwn > 0 {
		w.say("left out: %s of this account's own.", plural(r.DroppedOwn, "address", "addresses"))
	}
	if r.Unwritable > 0 {
		w.say("left out: %s from the parent that cannot be written into a header (not ASCII local@domain).",
			plural(r.Unwritable, "address", "addresses"))
	}
}

// recipientCounts says where the recipients came from; the block lists
// them, and the structured result pairs each with its origin.
func (w *writer) recipientCounts(rs []model.Recipient) {
	counts := map[model.Origin]int{}
	for _, r := range rs {
		counts[r.Origin]++
	}
	if len(rs) == 0 {
		w.say("recipients: none yet")
		return
	}
	parts := fill("")
	add := func(p part) {
		if parts.s == "" {
			parts = p
			return
		}
		parts = fill("%s; %s", parts, p)
	}
	if n := counts[model.FromParent]; n > 0 {
		add(fill("%s from the parent's sender or Reply-To", num(n)))
	}
	if n := counts[model.FromReplyAll]; n > 0 {
		add(fill("%s of the parent's other recipients (reply_all)", num(n)))
	}
	if n := counts[model.FromCaller]; n > 0 {
		add(fill("%s named in this call", num(n)))
	}
	if n := counts[model.FromDraft]; n > 0 {
		add(fill("%s kept from the draft", num(n)))
	}
	w.say("recipients: %s", parts)
}

func (w *writer) draftUpdated(d model.DraftWrite) {
	if d.DryRun {
		w.say("would update draft %s, which holds message %s as expected", gmailID(d.DraftID), gmailID(d.PreviousMessageID))
	} else {
		w.say("updated draft %s · message %s, was %s · thread %s", gmailID(d.DraftID), gmailID(d.MessageID),
			gmailID(d.PreviousMessageID), gmailID(d.ThreadID))
		w.say("the next update_draft needs message_id %s.", gmailID(d.MessageID))
	}
	w.say("changed: %s; everything else was carried over as Gmail stored it.", fixedList(d.Changed, draftFields))
	if len(d.Removed) > 0 {
		ids := make([]string, len(d.Removed))
		for i, f := range d.Removed {
			ids[i] = f.PartID
		}
		if len(ids) == 1 {
			w.say("removed: 1 attachment, part_id %s", partIDs(ids))
		} else {
			w.say("removed: %s, part_ids %s", num(len(ids)), partIDs(ids))
		}
	}
	if len(d.Added) > 0 {
		w.say("added: %s", plural(len(d.Added), "attachment", "attachments"))
	}
	if d.ThreadingAtRisk {
		w.say("note: this draft is a reply and its subject changed; Gmail threads by subject, so it may leave its thread.")
	}
	w.recipientCounts(d.Recipients)
}

func (w *writer) draftDeleted(d model.DraftWrite) {
	switch {
	case d.DryRun:
		w.say("would delete draft %s (message %s) permanently; drafts do not go to the trash.", gmailID(d.DraftID), gmailID(d.MessageID))
	case d.Gone:
		w.say("draft %s is gone: it was read, and Gmail then answered the delete that no such draft exists. An earlier attempt of this call, or another client, deleted it.",
			gmailID(d.DraftID))
	default:
		w.say("deleted draft %s (message %s) permanently.", gmailID(d.DraftID), gmailID(d.MessageID))
	}
}

// draftHeaders is the draft's headers and files as a block's content.
func draftHeaders(d model.DraftWrite) string {
	var b strings.Builder
	if d.From != nil {
		b.WriteString("From: " + d.From.String() + "\n")
	}
	for _, field := range []string{"to", "cc", "bcc"} {
		var as []string
		for _, r := range d.Recipients {
			if r.Field == field {
				as = append(as, r.Address.String())
			}
		}
		if len(as) > 0 {
			b.WriteString(strings.ToUpper(field[:1]) + field[1:] + ": " + strings.Join(as, ", ") + "\n")
		}
	}
	b.WriteString("Subject: " + string(d.Subject) + "\n")
	if d.RFC822MessageID != "" {
		b.WriteString("Message-ID: " + string(d.RFC822MessageID) + "\n")
	}
	for _, f := range d.Files {
		b.WriteString(fileLine(f))
	}
	for _, f := range d.Removed {
		b.WriteString("Removed: " + string(f.Name) + " (part_id \"" + f.PartID + "\")\n")
	}
	return b.String()
}

// ItemsWrite renders modify_labels, trash or restore, item by item.
func ItemsWrite(iw model.ItemsWrite) string {
	return plain(func(w *writer) {
		if iw.DryRun {
			w.say("dry run: nothing was changed; each line says what the call would do.")
		}
		if iw.Op == "modify_labels" {
			w.say("add: %s · remove: %s", labelList(iw.Add), labelList(iw.Remove))
			if len(iw.Verbs) > 0 {
				w.say("which is: %s", fixedList(iw.Verbs, verbNames))
			}
		}
		// A permanent delete speaks of deleting, and never leaves an item
		// unchanged; the other writes share their words.
		purge := iw.Op == "delete_permanently"
		done, doneWord, would := iw.Count(model.Changed), fill("changed"), fill("would change")
		if purge {
			doneWord, would = fill("deleted for good"), fill("would be deleted for good")
		}
		if iw.DryRun {
			done, doneWord = iw.Count(model.WouldChange), would
		}
		if purge {
			w.say("%s %s · %s failed", num(done), doneWord, num(iw.Count(model.Failed)))
		} else {
			w.say("%s %s · %s unchanged · %s failed", num(done), doneWord, num(iw.Count(model.Unchanged)),
				num(iw.Count(model.Failed)))
		}
		for _, it := range iw.Items {
			kind := fill("message")
			if it.Kind == model.KindThread {
				kind = fill("thread")
			}
			switch o := it.Outcome; {
			case o == model.Failed:
				w.say("%s %s: failed · %s", kind, gmailID(it.ID), failure(it.Class, it.Error))
			case o == model.Unchanged:
				w.say("%s %s: unchanged, %s · labels: %s", kind, gmailID(it.ID), already(iw.Op), labelList(it.Before))
			case o == model.WouldChange:
				w.say("%s %s: %s · labels now: %s", kind, gmailID(it.ID), would, labelList(it.Before))
			case purge:
				w.say("%s %s: deleted for good · labels it had: %s", kind, gmailID(it.ID), labelList(it.Before))
			default:
				w.say("%s %s: %s · labels before: %s · after: %s", kind, gmailID(it.ID),
					fixed(string(it.Outcome), outcomes), labelList(it.Before), labelList(it.After))
			}
		}
	})
}

// already says why an item needed no write.
func already(op string) part {
	switch op {
	case "trash":
		return fill("already in the trash")
	case "restore":
		return fill("not in the trash")
	}
	return fill("already as asked")
}

// LabelWrite renders a created, updated or deleted label.
func LabelWrite(lw model.LabelWrite) string {
	return plain(func(w *writer) {
		if lw.Op == "delete" {
			w.labelDeleted(lw)
			return
		}
		a := lw.After
		switch {
		case lw.Op == "create" && lw.DryRun:
			w.say("dry run: would create label %s; the name is free.", labelName(a.Name))
		case lw.Op == "create":
			w.say("created label %s · id %s", labelName(a.Name), gmailID(a.ID))
		case lw.DryRun:
			w.say("dry run: would update label %s · changing %s", gmailID(a.ID), fixedList(lw.Changed, labelFields))
		default:
			w.say("updated label %s · changed %s", gmailID(a.ID), fixedList(lw.Changed, labelFields))
		}
		if b := lw.Before; b != nil {
			w.say("before: %s", labelLook(*b))
			w.say("after:  %s", labelLook(a))
			return
		}
		w.say("look: %s", labelLook(a))
	})
}

// visible is a visibility word, or "not set" when Gmail did not say.
func visible(word string, known map[string]bool) part {
	if word == "" {
		return fill("not set")
	}
	return fixed(word, known)
}

// labelLook is a label's name, visibility and color.
func labelLook(l model.Label) part {
	look := fill("%s · in label list: %s · in message list: %s", labelName(l.Name),
		visible(l.InLabelList(), labelListWords), visible(l.MessageListVisibility, messageList))
	if l.TextColor != "" || l.BackgroundColor != "" {
		look = fill("%s · color %s on %s", look, color(l.TextColor), color(l.BackgroundColor))
	}
	return look
}

func (w *writer) labelDeleted(lw model.LabelWrite) {
	b := model.Label{}
	if lw.Before != nil {
		b = *lw.Before
	}
	msgs, threads := plural(b.MessagesTotal, "message", "messages"), plural(b.ThreadsTotal, "thread", "threads")
	switch {
	case lw.DryRun:
		w.say("dry run: would delete label %s · id %s, and take it off %s in %s. This cannot be undone.",
			labelName(b.Name), gmailID(b.ID), msgs, threads)
	case lw.Gone:
		w.say("label %s is gone: it was read, and Gmail then answered the delete that no such label exists.", gmailID(b.ID))
	default:
		w.say("deleted label %s · id %s; it came off %s in %s. The mail itself is untouched.",
			labelName(b.Name), gmailID(b.ID), msgs, threads)
	}
	w.say("look: %s", labelLook(b))
}

// SendDraft renders a sent draft, or what a dry run would send (§4.2):
// every recipient, whether the guard clears it, the subject, the files
// and the thread. Addresses, the subject and file names are in a block,
// one recipient per line under its field and position, which is how the
// guard's refusal names them.
func SendDraft(sw model.SendWrite, o Options) Result {
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		switch {
		case sw.DryRun:
			w.say("dry run: nothing was sent; this is what send_draft would send.")
			w.say("draft %s · message %s · thread %s", gmailID(sw.DraftID), gmailID(sw.MessageID), gmailID(sw.ThreadID))
		default:
			w.say("sent draft %s · sent message %s · thread %s · labels: %s", gmailID(sw.DraftID), gmailID(sw.SentID),
				gmailID(sw.SentThreadID), labelList(sw.SentLabels))
			w.say("the draft held message %s.", gmailID(sw.MessageID))
			if sw.ThreadID != "" && sw.SentThreadID != sw.ThreadID {
				w.say("note: Gmail filed the sent message in thread %s, not the draft's thread %s.",
					gmailID(sw.SentThreadID), gmailID(sw.ThreadID))
			}
		}
		if sw.Answers > 0 {
			w.say("it answers a thread of %s; their senders, and whoever this account sent them to, need no confirming.",
				plural(sw.Answers, "message", "messages"))
		} else {
			w.say("it starts a new conversation: every recipient needs confirming.")
		}
		participants, confirmed := 0, 0
		var stopped []part
		for _, r := range sw.Recipients {
			switch {
			case r.Participant:
				participants++
			case r.Confirmed:
				confirmed++
			default:
				stopped = append(stopped, recipientRef(r.Field, r.Position))
			}
		}
		w.say("recipients: %s · %s in the thread · %s confirmed", num(len(sw.Recipients)), num(participants), num(confirmed))
		if len(stopped) > 0 {
			w.say("not confirmed: %s. send_draft refuses until confirm_recipients names each of these addresses.",
				partList(stopped))
		}
		if len(sw.Files) > 0 {
			total := 0
			for _, f := range sw.Files {
				total += f.Size
			}
			w.say("attachments: %s, %s", num(len(sw.Files)), size(int64(total)))
		}
		w.block("outgoing message", "", sw.MessageID, sendHeaders(sw))
		return res
	})
}

// sendHeaders is the outgoing message's headers, one recipient a line.
func sendHeaders(sw model.SendWrite) string {
	var b strings.Builder
	if sw.From != nil {
		b.WriteString("From: " + sw.From.String() + "\n")
	}
	for _, r := range sw.Recipients {
		b.WriteString(r.Field + "[" + strconv.Itoa(r.Position) + "]: " + r.Address.String() + "\n")
	}
	b.WriteString("Subject: " + string(sw.Subject) + "\n")
	if sw.RFC822MessageID != "" {
		b.WriteString("Message-ID: " + string(sw.RFC822MessageID) + "\n")
	}
	for _, f := range sw.Files {
		b.WriteString(fileLine(f))
	}
	return b.String()
}

// fileLine is a file's line in a draft's or a send's block.
func fileLine(f model.File) string {
	return "Attachment: " + string(f.Name) + " (" + string(f.MediaType) + ", " + sizeText(f.Size) + ")\n"
}
