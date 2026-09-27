package render

import (
	"github.com/mmedum/google-mail-mcp/internal/model"
)

// ChangeList is one page of list_changes.
type ChangeList struct {
	// Start is the history id the page was read from.
	Start   string
	Changes []model.Change
	// Expired is set when Gmail no longer holds history from Start.
	Expired bool
	// HistoryID is the mailbox's current history id.
	HistoryID     string
	NextPageToken string
	// Label is the label the listing was limited to, if any.
	Label *model.LabelRef
}

// Changes renders changes since a history id. Nothing in a change was
// written by a sender: ids, kinds and the account's label names. An
// expired cursor is said plainly and never shown as an empty page
// (§7.6).
func Changes(l ChangeList, o Options) Result {
	if l.Expired {
		return render(o, func(w *writer) Result {
			res := Result{Budget: o.budget(), Truncated: true}
			w.say("budget: %s characters", num(res.Budget))
			w.say("cursor expired: Gmail no longer keeps history from %s, so the changes after it are gone and cannot be listed.",
				gmailID(l.Start))
			w.say("history_id=%s is the mailbox's current point; list_changes from it sees what changes next. To catch up on what was missed, search the mail instead.",
				gmailID(l.HistoryID))
			return res
		})
	}
	return listing(o, l.Changes, l.NextPageToken, "change", "changes",
		func(c model.Change) string { return c.MessageID },
		func(w *writer, c model.Change) {
			switch c.Kind {
			case model.ChangeAdded:
				w.say("history %s · message %s added · thread %s · labels: %s",
					gmailID(c.HistoryID), gmailID(c.MessageID), gmailID(c.ThreadID), labelList(c.Labels))
			case model.ChangeDeleted:
				w.say("history %s · message %s deleted permanently · thread %s",
					gmailID(c.HistoryID), gmailID(c.MessageID), gmailID(c.ThreadID))
			case model.ChangeLabelsAdded:
				w.say("history %s · message %s · thread %s · labels added: %s",
					gmailID(c.HistoryID), gmailID(c.MessageID), gmailID(c.ThreadID), labelList(c.Labels))
			case model.ChangeLabelsRemoved:
				w.say("history %s · message %s · thread %s · labels removed: %s",
					gmailID(c.HistoryID), gmailID(c.MessageID), gmailID(c.ThreadID), labelList(c.Labels))
			}
		},
		func(w *writer) {
			if l.Label != nil {
				w.say("since history %s, messages labeled %s only", gmailID(l.Start), labelName(l.Label.Name))
			} else {
				w.say("since history %s", gmailID(l.Start))
			}
			if l.NextPageToken == "" {
				w.say("history_id=%s continues from here next time.", gmailID(l.HistoryID))
			} else {
				w.say("history_id=%s is where to continue once every page is read; until then keep history_id and pass page_token.",
					gmailID(l.HistoryID))
			}
		})
}
