package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
	"github.com/mmedum/google-mail-mcp/internal/service"
)

// The tools behind a flag: send_draft behind GMAIL_ENABLE_SEND (§4.2),
// delete_permanently and delete_label behind GMAIL_ENABLE_DESTRUCTIVE
// (§4.6). Kind is what keeps them unregistered otherwise.

// SendDraftIn sends a draft.
type SendDraftIn struct {
	DraftID string `json:"draft_id" jsonschema:"the draft to send"`
	// MessageID is the witness of §4.4, held for a send as for an update.
	MessageID string `json:"message_id" jsonschema:"the message_id get_draft, create_draft or update_draft returned; the send is refused as stale if the draft changed since"`
	// ConfirmRecipients is the guard of §4.2.
	ConfirmRecipients []string `json:"confirm_recipients,omitempty" jsonschema:"every recipient who is not already in the thread the draft answers, each address written out, as ada@example.com; for a new conversation, every recipient"`
	DryRun            bool     `json:"dry_run,omitempty" jsonschema:"read the draft and report who it would reach and whom to confirm, without sending"`
}

// SendRecipient is one address a send reaches.
type SendRecipient struct {
	UntrustedAddress model.Untrusted `json:"untrusted_address"`
	Field            string          `json:"field" jsonschema:"to, cc or bcc"`
	Position         int             `json:"position" jsonschema:"its place in its field, from 0: the guard names it field[position]"`
	Participant      bool            `json:"participant" jsonschema:"on a message of the thread the draft answers, so it needs no confirming"`
	Confirmed        bool            `json:"confirmed" jsonschema:"named in confirm_recipients"`
}

// SendDraftOut is a sent draft, or what a dry run would send.
type SendDraftOut struct {
	DryRun  bool   `json:"dry_run"`
	DraftID string `json:"draft_id"`
	// MessageID is the message the draft held, read before the send.
	MessageID                string          `json:"message_id" jsonschema:"the message the draft held when it was read"`
	ThreadID                 string          `json:"thread_id"`
	Answers                  int             `json:"answers" jsonschema:"messages of the thread the draft answers; 0 is a new conversation"`
	UntrustedFrom            model.Untrusted `json:"untrusted_from,omitempty"`
	Recipients               []SendRecipient `json:"recipients"`
	Unconfirmed              int             `json:"unconfirmed" jsonschema:"recipients the guard stops until confirm_recipients names them"`
	UntrustedSubject         model.Untrusted `json:"untrusted_subject"`
	UntrustedRFC822MessageID model.Untrusted `json:"untrusted_rfc822_message_id,omitempty"`
	Attachments              []DraftFile     `json:"attachments"`
	Sent                     bool            `json:"sent"`
	SentMessageID            string          `json:"sent_message_id,omitempty"`
	SentThreadID             string          `json:"sent_thread_id,omitempty"`
	SentLabels               []LabelRef      `json:"sent_labels"`
	Rendered
	Cost
}

// DeletePermanentlyIn names what delete_permanently removes.
type DeletePermanentlyIn struct {
	MessageIDs []string `json:"message_ids,omitempty" jsonschema:"message ids from a search or read you have seen"`
	ThreadIDs  []string `json:"thread_ids,omitempty" jsonschema:"thread ids; every message in each thread is deleted"`
	Confirm    bool     `json:"confirm,omitempty" jsonschema:"must be true: deleted mail skips the trash and cannot be restored"`
	DryRun     bool     `json:"dry_run,omitempty" jsonschema:"read each item and report what would be deleted"`
}

// DeleteLabelIn names the label delete_label removes.
type DeleteLabelIn struct {
	Label   string `json:"label" jsonschema:"the user label to delete, by id or name"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true: the label comes off every message and cannot be restored"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"read the label and report how much mail carries it"`
}

// LabelDeleteOut is a deleted label.
type LabelDeleteOut struct {
	DryRun   bool      `json:"dry_run"`
	Label    LabelLook `json:"label" jsonschema:"the label as it was before the delete"`
	Messages int       `json:"messages" jsonschema:"messages that carried it"`
	Threads  int       `json:"threads" jsonschema:"threads that carried it"`
	Deleted  bool      `json:"deleted"`
	Gone     bool      `json:"gone,omitempty" jsonschema:"Gmail answered the delete that the label was already gone"`
	Cost
	text string
}

// Render implements Renderer.
func (o LabelDeleteOut) Render() string { return o.text }

func registerGated(s *mcp.Server, d Deps) {
	svc := service.New(d.Client)

	register(s, d, Spec{Name: "send_draft", Kind: Send, Description: "Send a draft to its recipients. This reaches " +
		"other people and cannot be recalled. Pass the draft's message_id, as get_draft or create_draft returned it; " +
		"the send is refused as stale if the draft changed since. Every recipient who is not already in the thread the " +
		"draft answers — for a new conversation, every recipient — must be written out in confirm_recipients, or the " +
		"send is [blocked]; more than 50 recipients is [blocked] outright. Run it first with dry_run: it lists the " +
		"recipients, the subject, the attachments and the thread, and whom to confirm. The body goes exactly as the " +
		"draft holds it. A send is never retried: if Gmail does not confirm it, the result is [ambiguous_outcome] with " +
		"what a read found afterwards, and the draft must not be sent again without the person. 160 units: the " +
		"draft, its thread and the send." + dryRunNote + untrustedNote},
		func(ctx context.Context, in SendDraftIn) (SendDraftOut, error) {
			sw, err := svc.SendDraft(ctx, service.Dispatch{DraftID: in.DraftID, Witness: in.MessageID,
				Confirm: in.ConfirmRecipients})
			if err != nil {
				return SendDraftOut{}, err
			}
			return sendDraftOut(sw), nil
		})

	register(s, d, Spec{Name: "delete_permanently", Kind: Destructive, Description: "Delete up to 100 messages or " +
		"threads, named by id — never a search — for good. They skip the trash and cannot be restored; trash is the " +
		"way to remove mail that may be wanted back. confirm must be true. Each item is read first and reported with " +
		"the labels it had, and one that fails does not stop the rest. A draft goes with delete_draft. 1 unit, then " +
		"30 per message and 60 per thread." + dryRunNote},
		func(ctx context.Context, in DeletePermanentlyIn) (ItemsOut, error) {
			iw, err := svc.DeletePermanently(ctx, service.Purge{Targets: service.Targets{MessageIDs: in.MessageIDs,
				ThreadIDs: in.ThreadIDs}, Confirm: in.Confirm})
			if err != nil {
				return ItemsOut{}, err
			}
			return itemsOut(iw), nil
		})

	register(s, d, Spec{Name: "delete_label", Kind: Destructive, Description: "Delete a user label. It comes off " +
		"every message and thread that carries it, and cannot be restored; the mail itself stays. confirm must be " +
		"true. The result says how many messages carried it. System labels cannot be deleted. 7 units." + dryRunNote},
		func(ctx context.Context, in DeleteLabelIn) (LabelDeleteOut, error) {
			lw, err := svc.DeleteLabel(ctx, in.Label, in.Confirm)
			if err != nil {
				return LabelDeleteOut{}, err
			}
			return labelDeleteOut(lw), nil
		})
}

func sendDraftOut(sw model.SendWrite) SendDraftOut {
	out := SendDraftOut{DryRun: sw.DryRun, DraftID: sw.DraftID, MessageID: sw.MessageID, ThreadID: sw.ThreadID,
		Answers: sw.Answers, Unconfirmed: sw.Unconfirmed(), UntrustedSubject: sw.Subject,
		UntrustedRFC822MessageID: sw.RFC822MessageID, Sent: !sw.DryRun, SentMessageID: sw.SentID,
		SentThreadID: sw.SentThreadID, SentLabels: labelRefs(sw.SentLabels),
		Recipients: mapSlice(sw.Recipients, func(r model.SendRecipient) SendRecipient {
			return SendRecipient{UntrustedAddress: model.Untrusted(r.Address.String()), Field: r.Field, Position: r.Position,
				Participant: r.Participant, Confirmed: r.Confirmed}
		}),
		Attachments: mapSlice(sw.Files, draftFile),
		Rendered:    readOf(render.SendDraft(sw, render.Options{})),
	}
	if sw.From != nil {
		out.UntrustedFrom = model.Untrusted(sw.From.String())
	}
	return out
}

func labelDeleteOut(lw model.LabelWrite) LabelDeleteOut {
	out := LabelDeleteOut{DryRun: lw.DryRun, Deleted: !lw.DryRun, Gone: lw.Gone, text: render.LabelWrite(lw)}
	if b := lw.Before; b != nil {
		out.Label, out.Messages, out.Threads = labelLook(*b), b.MessagesTotal, b.ThreadsTotal
	}
	return out
}
