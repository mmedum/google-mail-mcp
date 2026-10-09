package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
	"github.com/mmedum/google-mail-mcp/v2/internal/service"
)

// dryRunNote ends the description of every write.
const dryRunNote = " dry_run shows what the call would do and changes nothing."

// CreateDraftIn is a new draft or a reply.
type CreateDraftIn struct {
	To        []string `json:"to,omitempty" jsonschema:"recipients, one address per entry: ada@example.com or Ada Quill <ada@example.com>"`
	Cc        []string `json:"cc,omitempty" jsonschema:"copy recipients, one address per entry"`
	Bcc       []string `json:"bcc,omitempty" jsonschema:"blind copy recipients, one address per entry"`
	Subject   string   `json:"subject,omitempty" jsonschema:"the subject; leave it out of a reply, which takes its parent's"`
	Body      string   `json:"body,omitempty" jsonschema:"the plain-text body, saved exactly as given, with an HTML version made from it unless body_html or plain_only is given. Put each paragraph on one line and separate paragraphs with a blank line (\\n\\n); a single line break stays a line break. Never break a line to a width: the reader's mail client wraps it"`
	BodyHTML  string   `json:"body_html,omitempty" jsonschema:"an HTML version of the body, saved beside the plain text in place of the one made from body; needs body"`
	PlainOnly bool     `json:"plain_only,omitempty" jsonschema:"true saves the body as plain text alone, with no HTML version: for a mailing list that refuses HTML. Gmail's web composer wraps such a draft at 70 columns when a person sends it"`
	// Attachments are base names, never paths: the directory is the
	// person's choice, not the caller's (§3.15).
	Attachments   []string `json:"attachments,omitempty" jsonschema:"names of files in the person's GMAIL_LOCAL_DIR to attach; a name, never a path"`
	From          string   `json:"from,omitempty" jsonschema:"one of the addresses the account sends as (get_settings lists them); default the account's default"`
	ReplyTo       string   `json:"reply_to,omitempty" jsonschema:"a message id to reply to, or rfc822: followed by its Message-ID"`
	ReplyToThread string   `json:"reply_to_thread,omitempty" jsonschema:"a thread id to reply to: its newest message that is not a draft, in the trash or a reaction is answered"`
	ReplyAll      bool     `json:"reply_all,omitempty" jsonschema:"with a reply, also address the parent's other recipients, leaving out this account's own addresses"`
	Quote         bool     `json:"quote,omitempty" jsonschema:"with a reply, put the parent's text below the body: a blank line, a line saying when and who wrote it, then each of its lines starting with '> '. A parent with only HTML is quoted as converted, its links reduced to their host. 20 more units for each part of the parent's text Gmail stored apart. Not with body_html. A later update_draft with body replaces the quote too"`
	Forward       string   `json:"forward,omitempty" jsonschema:"a message id to forward, or rfc822: followed by its Message-ID: the message goes attached as <subject>.eml, unchanged but for its Bcc header, which is left out. The subject is Fwd: and the original's unless subject is given. Not with reply_to, reply_to_thread or reply_all"`
	DryRun        bool     `json:"dry_run,omitempty" jsonschema:"build the draft and report it without saving"`
}

// UpdateDraftIn changes a draft. A field left out stays as it is.
type UpdateDraftIn struct {
	DraftID string `json:"draft_id" jsonschema:"the draft to change"`
	// MessageID is the witness of §4.4.
	MessageID string   `json:"message_id" jsonschema:"the message_id get_draft or the last create_draft or update_draft returned; the update is refused as stale if the draft changed since"`
	To        []string `json:"to,omitempty" jsonschema:"replaces the To recipients; [] clears them; left out, they stay"`
	Cc        []string `json:"cc,omitempty" jsonschema:"replaces the Cc recipients; [] clears them; left out, they stay"`
	Bcc       []string `json:"bcc,omitempty" jsonschema:"replaces the Bcc recipients; [] clears them; left out, they stay"`
	Subject   *string  `json:"subject,omitempty" jsonschema:"replaces the subject; on a reply, a changed subject may take it out of its thread"`
	Body      *string  `json:"body,omitempty" jsonschema:"replaces the plain-text body, a quote create_draft put below it included, written as for create_draft: each paragraph on one line, never broken to a width. The draft keeps its shape: plain text alone stays alone, and an HTML version made from the old body is made again (plain_only changes that); HTML written another way needs body_html too"`
	BodyHTML  *string  `json:"body_html,omitempty" jsonschema:"replaces the HTML body, or adds one beside the plain text"`
	PlainOnly *bool    `json:"plain_only,omitempty" jsonschema:"with body: true drops the HTML version made from the old body, false adds one made from the new; left out, the draft keeps its shape"`
	// AddAttachments are base names in GMAIL_LOCAL_DIR, as create_draft's.
	AddAttachments    []string `json:"add_attachments,omitempty" jsonschema:"names of files in GMAIL_LOCAL_DIR to attach; a name, never a path"`
	RemoveAttachments []string `json:"remove_attachments,omitempty" jsonschema:"part_ids of attachments to remove, as get_draft lists them; attachments not named stay"`
	DryRun            bool     `json:"dry_run,omitempty" jsonschema:"check and build the change and report it without saving"`
}

// DeleteDraftIn deletes a draft.
type DeleteDraftIn struct {
	DraftID string `json:"draft_id" jsonschema:"the draft to delete"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true: a deleted draft is gone for good and does not go to the trash"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"read the draft and report what would be deleted"`
}

// DraftRecipient is one address of a draft and where it came from.
type DraftRecipient struct {
	UntrustedAddress model.Untrusted `json:"untrusted_address"`
	Field            string          `json:"field" jsonschema:"to, cc or bcc"`
	Origin           string          `json:"origin" jsonschema:"caller (named in this call), parent (the replied-to message's sender or Reply-To), reply_all (its other recipients) or draft (already in the draft)"`
}

// DraftFile is an attachment of a draft.
type DraftFile struct {
	UntrustedName     model.Untrusted `json:"untrusted_name"`
	UntrustedMimeType model.Untrusted `json:"untrusted_mime_type"`
	Size              int             `json:"size"`
	PartID            string          `json:"part_id,omitempty"`
}

// ReplyOut is how a draft answers its parent.
type ReplyOut struct {
	ParentID       string `json:"parent_id"`
	ParentThreadID string `json:"parent_thread_id"`
	FromThread     bool   `json:"from_thread" jsonschema:"the parent was chosen as the thread's newest message"`
	Joined         bool   `json:"joined" jsonschema:"Gmail filed the draft in the parent's thread, read from its answer; false on a dry run"`
	NoMessageID    bool   `json:"no_message_id,omitempty" jsonschema:"the parent has no Message-ID, so the reply carries no In-Reply-To or References"`
	ReplyAll       bool   `json:"reply_all"`
	DroppedOwn     int    `json:"dropped_own" jsonschema:"this account's own addresses left out"`
	Unwritable     int    `json:"unwritable,omitempty" jsonschema:"the parent's addresses left out because they cannot be written into a header"`
	QuotedChars    int    `json:"quoted_chars,omitempty" jsonschema:"with quote, the characters of the parent's text quoted below the body; absent when the parent has no text"`
	QuoteFromHTML  bool   `json:"quote_from_html,omitempty" jsonschema:"the quote is the parent's HTML converted to text, so each link in it keeps only its host"`
}

// ForwardOut is the message a draft carries attached.
type ForwardOut struct {
	MessageID  string `json:"message_id"`
	ThreadID   string `json:"thread_id" jsonschema:"the original's thread, which the server asks Gmail to file the draft in; the draft's own thread_id says whether it did"`
	Bytes      int    `json:"bytes" jsonschema:"the size of the attached copy"`
	BccRemoved bool   `json:"bcc_removed" jsonschema:"the original's Bcc header was left out of the attached copy, so the copy does not show who got a blind copy"`
}

// DraftWriteOut is a created, updated or deleted draft.
type DraftWriteOut struct {
	DryRun  bool   `json:"dry_run"`
	DraftID string `json:"draft_id,omitempty"`
	// MessageID is the message inside the draft now: the witness the
	// next update_draft needs. For a delete, the message deleted.
	MessageID                string           `json:"message_id,omitempty" jsonschema:"the message now inside the draft: the message_id the next update_draft needs"`
	PreviousMessageID        string           `json:"previous_message_id,omitempty"`
	ThreadID                 string           `json:"thread_id,omitempty"`
	Labels                   []LabelRef       `json:"labels"`
	UntrustedFrom            model.Untrusted  `json:"untrusted_from,omitempty"`
	Recipients               []DraftRecipient `json:"recipients"`
	UntrustedSubject         model.Untrusted  `json:"untrusted_subject"`
	UntrustedRFC822MessageID model.Untrusted  `json:"untrusted_rfc822_message_id,omitempty"`
	Attachments              []DraftFile      `json:"attachments"`
	Added                    []DraftFile      `json:"added,omitempty"`
	Removed                  []DraftFile      `json:"removed,omitempty"`
	Changed                  []string         `json:"changed,omitempty" jsonschema:"the fields an update changed"`
	Reply                    *ReplyOut        `json:"reply,omitempty"`
	Forwarded                *ForwardOut      `json:"forwarded,omitempty"`
	Bytes                    int              `json:"bytes,omitempty"`
	Upload                   bool             `json:"upload,omitempty" jsonschema:"sent as a media upload, being over 5 MB"`
	ThreadingAtRisk          bool             `json:"threading_at_risk,omitempty" jsonschema:"a reply whose subject changed: Gmail may take it out of its thread"`
	// Deleted and Gone are a delete's outcome.
	Deleted bool `json:"deleted,omitempty"`
	Gone    bool `json:"gone,omitempty" jsonschema:"Gmail answered the delete that the draft was already gone"`
	Rendered
	Cost
}

// ModifyLabelsIn changes labels on explicit ids.
type ModifyLabelsIn struct {
	MessageIDs []string `json:"message_ids,omitempty" jsonschema:"message ids from a search or read you have seen"`
	ThreadIDs  []string `json:"thread_ids,omitempty" jsonschema:"thread ids; the change applies to every message in each thread"`
	Add        []string `json:"add,omitempty" jsonschema:"labels to add, by id or name: STARRED, UNREAD, INBOX, IMPORTANT, SPAM, or a user label"`
	Remove     []string `json:"remove,omitempty" jsonschema:"labels to remove, by id or name"`
	DryRun     bool     `json:"dry_run,omitempty" jsonschema:"read each item's labels and report what would change"`
}

// MoveIn names what trash or restore moves.
type MoveIn struct {
	MessageIDs []string `json:"message_ids,omitempty" jsonschema:"message ids from a search or read you have seen"`
	ThreadIDs  []string `json:"thread_ids,omitempty" jsonschema:"thread ids; every message in each thread moves"`
	DryRun     bool     `json:"dry_run,omitempty" jsonschema:"read each item and report what would move"`
}

// ItemOut is one id's outcome.
type ItemOut struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind" jsonschema:"message or thread"`
	Outcome      string     `json:"outcome" jsonschema:"changed (for delete_permanently, deleted for good), unchanged (already as asked), would_change (dry run) or failed"`
	Error        string     `json:"error,omitempty" jsonschema:"why it failed, as [class] message"`
	LabelsBefore []LabelRef `json:"labels_before"`
	LabelsAfter  []LabelRef `json:"labels_after" jsonschema:"the labels after the write; on a dry run, the labels it would leave, and empty for delete_permanently"`
}

// ItemsOut is a multi-id write, item by item.
type ItemsOut struct {
	DryRun    bool       `json:"dry_run"`
	Add       []LabelRef `json:"add,omitempty"`
	Remove    []LabelRef `json:"remove,omitempty"`
	Verbs     []string   `json:"verbs,omitempty" jsonschema:"what the change amounts to: archive, mark read, star…"`
	Items     []ItemOut  `json:"items"`
	Changed   int        `json:"changed"`
	Unchanged int        `json:"unchanged"`
	Failed    int        `json:"failed"`
	Cost
	text string
}

// Render implements Renderer.
func (o ItemsOut) Render() string { return o.text }

// CreateLabelIn is a new label.
type CreateLabelIn struct {
	Name            string `json:"name" jsonschema:"the label's name; a slash nests it under another, as Projects/Offsite"`
	InLabelList     string `json:"in_label_list,omitempty" jsonschema:"show, show_if_unread or hide in Gmail's label list; default show"`
	InMessageList   string `json:"in_message_list,omitempty" jsonschema:"show or hide on messages in a listing; default show"`
	TextColor       string `json:"text_color,omitempty" jsonschema:"#rrggbb from Gmail's label palette, with background_color"`
	BackgroundColor string `json:"background_color,omitempty" jsonschema:"#rrggbb from Gmail's label palette, with text_color"`
	DryRun          bool   `json:"dry_run,omitempty" jsonschema:"check the name is free and report the label without creating it"`
}

// UpdateLabelIn changes a user label. A field left out stays as it is.
type UpdateLabelIn struct {
	Label           string `json:"label" jsonschema:"the user label to change, by id or name"`
	Name            string `json:"name,omitempty" jsonschema:"a new name"`
	InLabelList     string `json:"in_label_list,omitempty" jsonschema:"show, show_if_unread or hide in Gmail's label list"`
	InMessageList   string `json:"in_message_list,omitempty" jsonschema:"show or hide on messages in a listing"`
	TextColor       string `json:"text_color,omitempty" jsonschema:"#rrggbb from Gmail's label palette, with background_color"`
	BackgroundColor string `json:"background_color,omitempty" jsonschema:"#rrggbb from Gmail's label palette, with text_color"`
	DryRun          bool   `json:"dry_run,omitempty" jsonschema:"report the change without making it"`
}

// LabelLook is a label's name and how it shows.
type LabelLook struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	InLabelList     string `json:"in_label_list,omitempty" jsonschema:"show, show_if_unread or hide"`
	InMessageList   string `json:"in_message_list,omitempty" jsonschema:"show or hide"`
	TextColor       string `json:"text_color,omitempty"`
	BackgroundColor string `json:"background_color,omitempty"`
}

// LabelWriteOut is a created or updated label.
type LabelWriteOut struct {
	DryRun  bool       `json:"dry_run"`
	Before  *LabelLook `json:"before,omitempty"`
	Label   LabelLook  `json:"label" jsonschema:"the label as Gmail answered, or would"`
	Changed []string   `json:"changed,omitempty"`
	Cost
	text string
}

// Render implements Renderer.
func (o LabelWriteOut) Render() string { return o.text }

func registerWrite(s *mcp.Server, d Deps) {
	svc := service.New(d.Client)

	register(s, d, Spec{Name: "create_draft", Kind: Write, Description: "Save a new draft, a reply to a message, or a " +
		"forward. Nothing is sent: the draft waits in Gmail. For a reply, give reply_to (a message id) or reply_to_thread (a " +
		"thread id); the server writes the recipients, the subject and the threading headers from the parent, and says " +
		"whether Gmail filed the draft in the parent's thread. reply_all adds the parent's other recipients, and quote " +
		"puts the parent's text below the body. To forward, " +
		"give forward (a message id): the message goes attached as an .eml file, and the server asks Gmail to file the " +
		"draft in the original's thread; the result says whether it did. " +
		"Addresses you add are extra recipients. Attachments are files the person put in GMAIL_LOCAL_DIR, named, never a " +
		"path. About 11 units, plus 20 for reply_to or forward, or 40 for reply_to_thread, and 20 for each part of a " +
		"quoted parent's text Gmail stored apart." + dryRunNote + untrustedNote},
		func(ctx context.Context, in CreateDraftIn) (DraftWriteOut, error) {
			dw, err := svc.CreateDraft(ctx, service.Compose{To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject,
				Body: in.Body, HTML: in.BodyHTML, PlainOnly: in.PlainOnly, Attachments: in.Attachments, From: in.From, ReplyTo: in.ReplyTo,
				ReplyToThread: in.ReplyToThread, ReplyAll: in.ReplyAll, Quote: in.Quote, Forward: in.Forward, LocalDir: d.Config.LocalDir})
			if err != nil {
				return DraftWriteOut{}, err
			}
			return draftWriteOut(dw), nil
		})

	register(s, d, Spec{Name: "update_draft", Kind: Write, Description: "Change a draft: only the fields given change, " +
		"and everything else — other recipients, the body, attachments, headers — is carried over as Gmail stored it. " +
		"Pass the message_id get_draft returned: Gmail cannot guard an update, so the server re-reads the draft and " +
		"refuses with [stale] if it changed since. Each save gives the draft a new message_id, which the result names. " +
		"35 units." + dryRunNote + untrustedNote},
		func(ctx context.Context, in UpdateDraftIn) (DraftWriteOut, error) {
			dw, err := svc.UpdateDraft(ctx, service.Revise{DraftID: in.DraftID, Witness: in.MessageID, To: in.To, Cc: in.Cc,
				Bcc: in.Bcc, Subject: in.Subject, Body: in.Body, HTML: in.BodyHTML, PlainOnly: in.PlainOnly, Attach: in.AddAttachments,
				Remove: in.RemoveAttachments, LocalDir: d.Config.LocalDir})
			if err != nil {
				return DraftWriteOut{}, err
			}
			return draftWriteOut(dw), nil
		})

	register(s, d, Spec{Name: "delete_draft", Kind: WriteForGood, AskUnits: "20 more units", Description: "Delete a draft. Gmail deletes a draft for " +
		"good: it does not go to the trash, so confirm must be true. Only the draft goes; the thread it answers is " +
		"left alone. 30 units." + dryRunNote + untrustedNote},
		func(ctx context.Context, in DeleteDraftIn) (DraftWriteOut, error) {
			dw, err := svc.DeleteDraft(ctx, in.DraftID, in.Confirm)
			if err != nil {
				return DraftWriteOut{}, err
			}
			return draftWriteOut(dw), nil
		})

	register(s, d, Spec{Name: "modify_labels", Kind: Write, Description: "Add and remove labels on up to 100 messages " +
		"or threads, named by id — never a search. Archiving is removing INBOX, marking read is removing UNREAD, " +
		"starring is adding STARRED; the result names what the change amounts to. Each item is reported with its " +
		"labels before and after, and one that fails does not stop the rest. Drafts cannot be labeled. 1 unit, then " +
		"25 per message and 50 per thread, less for items already as asked." + dryRunNote},
		func(ctx context.Context, in ModifyLabelsIn) (ItemsOut, error) {
			iw, err := svc.ModifyLabels(ctx, service.Relabel{Targets: service.Targets{MessageIDs: in.MessageIDs,
				ThreadIDs: in.ThreadIDs}, Add: in.Add, Remove: in.Remove})
			if err != nil {
				return ItemsOut{}, err
			}
			return itemsOut(iw), nil
		})

	register(s, d, Spec{Name: "trash", Kind: Write, Description: "Move up to 100 messages or threads, named by id, " +
		"to the trash, where Gmail keeps them for 30 days; restore brings them back. Nothing is deleted for good. " +
		"Each item is reported, and one already in the trash is left as it is. 1 unit, then 40 per message and 60 " +
		"per thread." + dryRunNote},
		func(ctx context.Context, in MoveIn) (ItemsOut, error) {
			iw, err := svc.Trash(ctx, service.Move{Targets: service.Targets{MessageIDs: in.MessageIDs, ThreadIDs: in.ThreadIDs}})
			if err != nil {
				return ItemsOut{}, err
			}
			return itemsOut(iw), nil
		})

	register(s, d, Spec{Name: "restore", Kind: Write, Description: "Take up to 100 messages or threads, named by " +
		"id, out of the trash. Each item is reported, and one not in the trash is left as it is. 1 unit, then 25 per " +
		"message and 50 per thread." + dryRunNote},
		func(ctx context.Context, in MoveIn) (ItemsOut, error) {
			iw, err := svc.Trash(ctx, service.Move{Targets: service.Targets{MessageIDs: in.MessageIDs, ThreadIDs: in.ThreadIDs},
				Restore: true})
			if err != nil {
				return ItemsOut{}, err
			}
			return itemsOut(iw), nil
		})

	register(s, d, Spec{Name: "create_label", Kind: Write, Description: "Create a user label. Gmail refuses a name " +
		"another label has in any case, and a system label's name; the server checks first and says which. " +
		"6 units." + dryRunNote},
		func(ctx context.Context, in CreateLabelIn) (LabelWriteOut, error) {
			lw, err := svc.CreateLabel(ctx, service.LabelSpec{Name: in.Name, InLabelList: in.InLabelList,
				InMessageList: in.InMessageList, TextColor: in.TextColor, BackgroundColor: in.BackgroundColor})
			if err != nil {
				return LabelWriteOut{}, err
			}
			return labelWriteOut(lw), nil
		})

	register(s, d, Spec{Name: "update_label", Kind: Write, Description: "Rename a user label, or change how it shows " +
		"or its color. Only the fields given change. System labels cannot be changed. 6 units." + dryRunNote},
		func(ctx context.Context, in UpdateLabelIn) (LabelWriteOut, error) {
			lw, err := svc.UpdateLabel(ctx, in.Label, service.LabelSpec{Name: in.Name, InLabelList: in.InLabelList,
				InMessageList: in.InMessageList, TextColor: in.TextColor, BackgroundColor: in.BackgroundColor})
			if err != nil {
				return LabelWriteOut{}, err
			}
			return labelWriteOut(lw), nil
		})
}

func draftWriteOut(dw model.DraftWrite) DraftWriteOut {
	out := DraftWriteOut{DryRun: dw.DryRun, DraftID: dw.DraftID, MessageID: dw.MessageID,
		PreviousMessageID: dw.PreviousMessageID, ThreadID: dw.ThreadID, Labels: labelRefs(dw.Labels),
		UntrustedSubject: dw.Subject, UntrustedRFC822MessageID: dw.RFC822MessageID, Changed: dw.Changed,
		Bytes: dw.Bytes, Upload: dw.Upload, ThreadingAtRisk: dw.ThreadingAtRisk, Gone: dw.Gone,
		Deleted: dw.Op == "delete" && !dw.DryRun,
		Recipients: mapSlice(dw.Recipients, func(r model.Recipient) DraftRecipient {
			return DraftRecipient{UntrustedAddress: model.Untrusted(r.Address.String()), Field: r.Field, Origin: string(r.Origin)}
		}),
		Attachments: mapSlice(dw.Files, draftFile),
		Added:       mapSlice(dw.Added, draftFile),
		Removed:     mapSlice(dw.Removed, draftFile),
		Rendered:    readOf(render.DraftWrite(dw, render.Options{})),
	}
	if dw.From != nil {
		out.UntrustedFrom = model.Untrusted(dw.From.String())
	}
	if r := dw.Reply; r != nil {
		out.Reply = &ReplyOut{ParentID: r.ParentID, ParentThreadID: r.ParentThreadID, FromThread: r.FromThread,
			Joined: r.Joined, NoMessageID: r.NoMessageID, ReplyAll: r.ReplyAll, DroppedOwn: r.DroppedOwn, Unwritable: r.Unwritable,
			QuotedChars: r.QuotedChars, QuoteFromHTML: r.QuoteFromHTML}
	}
	if f := dw.Forward; f != nil {
		out.Forwarded = &ForwardOut{MessageID: f.MessageID, ThreadID: f.ThreadID, Bytes: f.Bytes, BccRemoved: f.BccRemoved}
	}
	return out
}

func draftFile(f model.File) DraftFile {
	return DraftFile{UntrustedName: f.Name, UntrustedMimeType: f.MediaType, Size: f.Size, PartID: f.PartID}
}

func itemsOut(iw model.ItemsWrite) ItemsOut {
	return ItemsOut{DryRun: iw.DryRun, Add: labelRefs(iw.Add), Remove: labelRefs(iw.Remove), Verbs: iw.Verbs,
		Items: mapSlice(iw.Items, func(it model.Item) ItemOut {
			out := ItemOut{ID: it.ID, Kind: string(it.Kind), Outcome: string(it.Outcome),
				LabelsBefore: labelRefs(it.Before), LabelsAfter: labelRefs(it.After)}
			if it.Outcome == model.Failed {
				out.Error = "[" + it.Class + "] " + it.Error
			}
			return out
		}),
		Changed: iw.Count(model.Changed), Unchanged: iw.Count(model.Unchanged), Failed: iw.Count(model.Failed),
		text: render.ItemsWrite(iw)}
}

func labelWriteOut(lw model.LabelWrite) LabelWriteOut {
	out := LabelWriteOut{DryRun: lw.DryRun, Label: labelLook(lw.After), Changed: lw.Changed, text: render.LabelWrite(lw)}
	if lw.Before != nil {
		b := labelLook(*lw.Before)
		out.Before = &b
	}
	return out
}

func labelLook(l model.Label) LabelLook {
	return LabelLook{ID: l.ID, Name: l.Name, Type: l.Type, InLabelList: l.InLabelList(),
		InMessageList: l.MessageListVisibility, TextColor: l.TextColor, BackgroundColor: l.BackgroundColor}
}
