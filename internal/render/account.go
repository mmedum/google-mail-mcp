package render

import (
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// Profile renders the account. Nothing in it came from a sender.
func Profile(p model.Profile) string {
	return plain(func(w *writer) {
		w.say("account: %s", account(p.Email))
		w.say("messages: %s", num(p.MessagesTotal))
		w.say("threads: %s", num(p.ThreadsTotal))
		w.say("history_id: %s", gmailID(p.HistoryID))
	})
}

// Labels renders a label list. Label names are the account's own, not a
// sender's, so they are not in blocks (see labelName).
func Labels(ls []model.Label) string {
	return plain(func(w *writer) {
		sys := 0
		for _, l := range ls {
			if l.Type == "system" {
				sys++
			}
		}
		w.say("%s: %s system, %s user", plural(len(ls), "label", "labels"), num(sys), num(len(ls)-sys))
		w.blank()
		for _, l := range ls {
			w.labelLine(l)
		}
	})
}

func (w *writer) labelLine(l model.Label) {
	counts, colors, listed, hidden := fill(""), fill(""), fill(""), fill("")
	if l.HasCounts {
		counts = fill(" · %s (%s unread), %s (%s unread)",
			plural(l.MessagesTotal, "message", "messages"), num(l.MessagesUnread),
			plural(l.ThreadsTotal, "thread", "threads"), num(l.ThreadsUnread))
	}
	if l.BackgroundColor != "" || l.TextColor != "" {
		colors = fill(" · color %s on %s", color(l.TextColor), color(l.BackgroundColor))
	}
	switch l.LabelListVisibility {
	case "labelHide":
		listed = fill(" · hidden from the label list")
	case "labelShowIfUnread":
		listed = fill(" · listed when unread")
	}
	if l.MessageListVisibility == "hide" {
		hidden = fill(" · hidden in message lists")
	}
	w.say("%s · id %s · %s%s%s%s%s", labelName(l.Name), gmailID(l.ID), labelType(l.Type), counts, colors, listed, hidden)
}
