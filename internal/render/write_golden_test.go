package render_test

import (
	"testing"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
)

func addr(p gmailtest.Person) mime.Address { return mime.Address{Name: p.Name, Email: p.Email} }

func TestGoldenDraftWrites(t *testing.T) {
	from := addr(gmailtest.Reader)
	reply := model.DraftWrite{
		Op: "create", DraftID: "r0000000000000021", MessageID: "0000000000000022", ThreadID: "0000000000000001",
		Labels: []model.LabelRef{{ID: "DRAFT", Name: "DRAFT"}}, From: &from,
		Recipients: []model.Recipient{
			{Address: addr(gmailtest.Bruno), Field: "to", Origin: model.FromParent},
			{Address: addr(gmailtest.Ada), Field: "to", Origin: model.FromReplyAll},
			{Address: addr(gmailtest.Chiara), Field: "cc", Origin: model.FromCaller},
		},
		Subject: "Re: Offsite venue",
		Files:   []model.File{{Name: "Отчёт.pdf", MediaType: "application/pdf", Size: 20480}},
		Reply:   &model.Reply{ParentID: "0000000000000003", ParentThreadID: "0000000000000001", Joined: true, ReplyAll: true, DroppedOwn: 1},
		Bytes:   29000,
	}
	golden(t, "draft_reply", render.DraftWrite(reply, opts()).Text)

	dry := reply
	dry.DryRun, dry.DraftID, dry.MessageID, dry.Labels = true, "", "", nil
	dry.Reply = &model.Reply{ParentID: "0000000000000003", ParentThreadID: "0000000000000001", FromThread: true, NoMessageID: true, Unwritable: 1}
	golden(t, "draft_reply_dry_run", render.DraftWrite(dry, opts()).Text)

	newThread := reply
	newThread.ThreadID = "0000000000000023"
	newThread.Reply = &model.Reply{ParentID: "0000000000000003", ParentThreadID: "0000000000000001"}
	newThread.Upload, newThread.Bytes = true, 6<<20
	golden(t, "draft_reply_new_thread", render.DraftWrite(newThread, opts()).Text)

	update := model.DraftWrite{
		Op: "update", DraftID: "r0000000000000021", MessageID: "0000000000000024", PreviousMessageID: "0000000000000022",
		ThreadID: "0000000000000001", From: &from,
		Recipients: []model.Recipient{
			{Address: addr(gmailtest.Bruno), Field: "to", Origin: model.FromDraft},
			{Address: addr(gmailtest.Dmitri), Field: "cc", Origin: model.FromCaller},
		},
		Subject: "Offsite, revised", RFC822MessageID: "<0f3c2a@example.com>", Changed: []string{"cc", "subject", "attachments"},
		Files:   []model.File{{Name: "notes.txt", MediaType: "text/plain", Size: 120}},
		Added:   []model.File{{Name: "notes.txt", MediaType: "text/plain", Size: 120}},
		Removed: []model.File{{Name: "Отчёт.pdf", MediaType: "application/pdf", Size: 20480, PartID: "1"}},
		Bytes:   1200, ThreadingAtRisk: true,
	}
	golden(t, "draft_update", render.DraftWrite(update, opts()).Text)

	deleted := model.DraftWrite{Op: "delete", DraftID: "r0000000000000021", MessageID: "0000000000000024",
		ThreadID: "0000000000000001", From: &from, Subject: "Offsite, revised",
		Recipients: []model.Recipient{{Address: addr(gmailtest.Bruno), Field: "to", Origin: model.FromDraft}}}
	golden(t, "draft_delete", render.DraftWrite(deleted, opts()).Text)
	deleted.Gone = true
	golden(t, "draft_delete_gone", render.DraftWrite(deleted, opts()).Text)
}

func TestGoldenItemsWrites(t *testing.T) {
	inbox, unread, starred := model.LabelRef{ID: "INBOX", Name: "INBOX"}, model.LabelRef{ID: "UNREAD", Name: "UNREAD"},
		model.LabelRef{ID: "STARRED", Name: "STARRED"}
	offsite := model.LabelRef{ID: "Label_2", Name: "Projects/Offsite"}
	modify := model.ItemsWrite{Op: "modify_labels", Add: []model.LabelRef{starred}, Remove: []model.LabelRef{inbox, unread},
		Verbs: []string{"archive", "mark read", "star"},
		Items: []model.Item{
			{ID: "0000000000000001", Kind: model.KindMessage, Outcome: model.Changed,
				Before: []model.LabelRef{inbox, unread, offsite}, After: []model.LabelRef{offsite, starred}},
			{ID: "0000000000000002", Kind: model.KindMessage, Outcome: model.Unchanged, Before: []model.LabelRef{offsite, starred}},
			{ID: "0000000000000009", Kind: model.KindThread, Outcome: model.Changed,
				Before: []model.LabelRef{inbox}, After: []model.LabelRef{starred}},
			{ID: "0000000000000021", Kind: model.KindMessage, Outcome: model.Failed, Class: "unsupported",
				Error: "message 0000000000000021 is a draft, and Gmail does not label drafts (§2.12)"},
			{ID: "00000000000fffff", Kind: model.KindMessage, Outcome: model.Failed, Class: "not_found",
				Error: "Google found nothing at that id (gmail.users.messages.get): Requested entity was not found."},
		}}
	golden(t, "modify_labels", render.ItemsWrite(modify))
	modify.DryRun = true
	modify.Items[0].Outcome = model.WouldChange
	modify.Items[2].Outcome = model.WouldChange
	golden(t, "modify_labels_dry_run", render.ItemsWrite(modify))

	trash := model.ItemsWrite{Op: "trash", Items: []model.Item{
		{ID: "0000000000000001", Kind: model.KindMessage, Outcome: model.Changed, Before: []model.LabelRef{inbox},
			After: []model.LabelRef{inbox, {ID: "TRASH", Name: "TRASH"}}},
		{ID: "0000000000000031", Kind: model.KindMessage, Outcome: model.Unchanged, Before: []model.LabelRef{{ID: "TRASH", Name: "TRASH"}}},
	}}
	golden(t, "trash", render.ItemsWrite(trash))
	restore := model.ItemsWrite{Op: "restore", Items: []model.Item{
		{ID: "0000000000000001", Kind: model.KindMessage, Outcome: model.Changed,
			Before: []model.LabelRef{inbox, {ID: "TRASH", Name: "TRASH"}}, After: []model.LabelRef{inbox}},
		{ID: "0000000000000002", Kind: model.KindMessage, Outcome: model.Unchanged, Before: []model.LabelRef{inbox}},
	}}
	golden(t, "restore", render.ItemsWrite(restore))

	purge := model.ItemsWrite{Op: "delete_permanently", Items: []model.Item{
		{ID: "0000000000000001", Kind: model.KindMessage, Outcome: model.Changed, Before: []model.LabelRef{inbox}},
		{ID: "0000000000000009", Kind: model.KindThread, Outcome: model.Changed, Before: []model.LabelRef{inbox, starred}},
		{ID: "0000000000000021", Kind: model.KindMessage, Outcome: model.Failed, Class: "invalid",
			Error: "message 0000000000000021 is a draft; delete_draft removes a draft"},
	}}
	golden(t, "delete_permanently", render.ItemsWrite(purge))
	purge.DryRun = true
	purge.Items[0].Outcome, purge.Items[1].Outcome = model.WouldChange, model.WouldChange
	golden(t, "delete_permanently_dry_run", render.ItemsWrite(purge))
}

func TestGoldenSendDraft(t *testing.T) {
	from := addr(gmailtest.Reader)
	sent := model.SendWrite{DraftID: "r0000000000000021", MessageID: "0000000000000022", ThreadID: "0000000000000001",
		Answers: 2, From: &from, Subject: "Re: Budget sign-off", RFC822MessageID: "<draft.22@mail.example.com>",
		Recipients: []model.SendRecipient{
			{Address: addr(gmailtest.Freya), Field: "to", Participant: true},
			{Address: addr(gmailtest.Ada), Field: "cc", Confirmed: true},
		},
		Files:  []model.File{{Name: "totals.csv", MediaType: "text/csv", Size: 2048, PartID: "1"}},
		SentID: "0000000000000030", SentThreadID: "0000000000000001",
		SentLabels: []model.LabelRef{{ID: "SENT", Name: "SENT"}},
	}
	golden(t, "send_draft", render.SendDraft(sent, opts()).Text)

	dry := sent
	dry.DryRun, dry.SentID, dry.SentThreadID, dry.SentLabels = true, "", "", nil
	dry.Answers = 0
	dry.Recipients = append(dry.Recipients, model.SendRecipient{Address: addr(gmailtest.Bruno), Field: "cc", Position: 1})
	dry.Recipients[0].Participant = false
	golden(t, "send_draft_dry_run", render.SendDraft(dry, opts()).Text)
}

func TestGoldenLabelWrites(t *testing.T) {
	created := model.Label{ID: "Label_5", Name: "Travel", Type: "user", LabelListVisibility: "labelShowIfUnread",
		MessageListVisibility: "show", TextColor: "#ffffff", BackgroundColor: "#16a766"}
	golden(t, "label_create", render.LabelWrite(model.LabelWrite{Op: "create", After: created}))
	golden(t, "label_create_dry_run", render.LabelWrite(model.LabelWrite{Op: "create", DryRun: true,
		After: model.Label{Name: "Travel", Type: "user", LabelListVisibility: "labelShow", MessageListVisibility: "show"}}))
	golden(t, "label_create_unstated", render.LabelWrite(model.LabelWrite{Op: "create",
		After: model.Label{ID: "Label_6", Name: "Later", Type: "user"}}))
	renamed := created
	renamed.Name, renamed.MessageListVisibility = "Trips", "hide"
	golden(t, "label_update", render.LabelWrite(model.LabelWrite{Op: "update", Before: &created, After: renamed,
		Changed: []string{"name", "in_message_list"}}))

	counted := created
	counted.HasCounts, counted.MessagesTotal, counted.ThreadsTotal = true, 12, 1
	golden(t, "label_delete", render.LabelWrite(model.LabelWrite{Op: "delete", Before: &counted}))
	golden(t, "label_delete_dry_run", render.LabelWrite(model.LabelWrite{Op: "delete", DryRun: true, Before: &counted}))
	golden(t, "label_delete_gone", render.LabelWrite(model.LabelWrite{Op: "delete", Before: &counted, Gone: true}))
}
