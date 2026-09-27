package tools

import (
	"context"
	"math"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
	"github.com/mmedum/google-mail-mcp/internal/service"
)

// The settings writes, behind GMAIL_ENABLE_SETTINGS (§7.9): a signature,
// a filter made or deleted, and — with GMAIL_ENABLE_SEND as well — the
// vacation reply.

// UpdateSignatureIn is what update_signature changes.
type UpdateSignatureIn struct {
	SendAs    string `json:"send_as,omitempty" jsonschema:"one of the account's send-as addresses, as get_settings lists them; default: the account's default address"`
	Signature string `json:"signature" jsonschema:"the signature as plain text, line breaks kept; markup is shown as written, not applied. Empty clears the signature"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"read the current signature and report the change without making it"`
}

// SignatureOut is a signature changed.
type SignatureOut struct {
	DryRun          bool            `json:"dry_run"`
	Address         string          `json:"address" jsonschema:"the send-as address whose signature changed"`
	UntrustedBefore model.Untrusted `json:"untrusted_before" jsonschema:"the signature before, as text"`
	UntrustedAfter  model.Untrusted `json:"untrusted_after" jsonschema:"the signature after, as text"`
	Rendered
	Cost
}

// CreateFilterIn is a filter to create. What it matches uses Gmail's
// filter criteria; what it does is labels added or removed, and the
// common ones as flags.
type CreateFilterIn struct {
	From           string   `json:"from,omitempty" jsonschema:"match mail from this sender"`
	To             string   `json:"to,omitempty" jsonschema:"match mail to this recipient"`
	Subject        string   `json:"subject,omitempty" jsonschema:"match mail whose subject contains this"`
	Query          string   `json:"query,omitempty" jsonschema:"match mail that fits this Gmail search"`
	NegatedQuery   string   `json:"negated_query,omitempty" jsonschema:"leave out mail that fits this Gmail search"`
	HasAttachment  bool     `json:"has_attachment,omitempty" jsonschema:"match only mail with an attachment"`
	ExcludeChats   bool     `json:"exclude_chats,omitempty" jsonschema:"leave out chats"`
	Size           int      `json:"size,omitempty" jsonschema:"a size in bytes, with size_comparison"`
	SizeComparison string   `json:"size_comparison,omitempty" jsonschema:"larger or smaller, with size"`
	AddLabels      []string `json:"add_labels,omitempty" jsonschema:"labels to add, by name or id: a user label, STARRED, IMPORTANT, or a category"`
	RemoveLabels   []string `json:"remove_labels,omitempty" jsonschema:"labels to remove, by name or id: INBOX archives, UNREAD marks read, SPAM keeps mail out of spam"`
	Archive        bool     `json:"archive,omitempty" jsonschema:"skip the inbox"`
	MarkRead       bool     `json:"mark_read,omitempty" jsonschema:"mark matching mail read"`
	Star           bool     `json:"star,omitempty" jsonschema:"star matching mail"`
	Trash          bool     `json:"trash,omitempty" jsonschema:"move matching mail to the trash as it arrives; needs confirm"`
	Confirm        bool     `json:"confirm,omitempty" jsonschema:"must be true for a filter that trashes: it hides matching mail as it arrives"`
	DryRun         bool     `json:"dry_run,omitempty" jsonschema:"check the filter and report it without creating it"`
}

// DeleteFilterIn names the filter delete_filter removes.
type DeleteFilterIn struct {
	FilterID string `json:"filter_id" jsonschema:"the filter's id, from list_filters"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"must be true: a deleted filter cannot be restored"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"read the filter and report it without deleting it"`
}

// FilterWriteOut is a filter created or deleted.
type FilterWriteOut struct {
	DryRun  bool   `json:"dry_run"`
	Filter  Filter `json:"filter" jsonschema:"the filter created, or as it was before the delete"`
	Trashes bool   `json:"trashes" jsonschema:"the filter moves matching mail to the trash"`
	Created bool   `json:"created,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Gone    bool   `json:"gone,omitempty" jsonschema:"Gmail answered the delete that the filter was already gone"`
	Cost
	text string
}

// Render implements Renderer.
func (o FilterWriteOut) Render() string { return o.text }

// SetVacationIn is the vacation reply to turn on, or off.
type SetVacationIn struct {
	Enable   bool   `json:"enable" jsonschema:"true turns the reply on; false turns it off and keeps its text"`
	Subject  string `json:"subject,omitempty" jsonschema:"the reply's subject"`
	Body     string `json:"body,omitempty" jsonschema:"the reply, plain text, sent exactly as given; required to turn it on"`
	Audience string `json:"audience,omitempty" jsonschema:"who is answered, required to turn it on: contacts, or domain (Google Workspace accounts). Every sender is not offered"`
	Start    string `json:"start,omitempty" jsonschema:"when the reply starts: an RFC 3339 time, or a YYYY-MM-DD date read as midnight UTC; default: now"`
	End      string `json:"end,omitempty" jsonschema:"when it stops, as start; default: until turned off"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"must be true to turn the reply on: it answers matching mail automatically"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"read the current reply and report the change without making it"`
}

// VacationOut is the vacation reply before and after.
type VacationOut struct {
	DryRun bool     `json:"dry_run"`
	Before Vacation `json:"before"`
	After  Vacation `json:"after"`
	Rendered
	Cost
}

func registerSettings(s *mcp.Server, d Deps) {
	svc := service.New(d.Client)

	register(s, d, Spec{Name: "update_signature", Kind: Settings, Description: "Set the signature of one of the " +
		"account's send-as addresses, the default one unless send_as names another. The text is plain: line breaks " +
		"are kept and markup is not applied. An empty signature clears it. Gmail adds the signature to mail written " +
		"in Gmail itself; drafts this server writes carry none. The result shows the signature before and after. " +
		"101 units." + dryRunNote + untrustedNote},
		func(ctx context.Context, in UpdateSignatureIn) (SignatureOut, error) {
			sw, err := svc.UpdateSignature(ctx, in.SendAs, in.Signature)
			if err != nil {
				return SignatureOut{}, err
			}
			return SignatureOut{DryRun: sw.DryRun, Address: sw.Address, UntrustedBefore: sw.Before,
				UntrustedAfter: sw.After, Rendered: readOf(render.SignatureWrite(sw, render.Options{}))}, nil
		})

	register(s, d, Spec{Name: "create_filter", Kind: Settings, Description: "Create a filter: mail that arrives " +
		"from now on and matches from, to, subject, query or the other criteria gets the actions given — labels " +
		"added or removed, archive, mark_read, star, or trash. Mail already in the mailbox is untouched. A filter " +
		"cannot forward. trash needs confirm: true, since it hides matching mail as it arrives. An identical " +
		"filter is refused as a conflict. 7 units." + dryRunNote},
		func(ctx context.Context, in CreateFilterIn) (FilterWriteOut, error) {
			if in.Size < 0 || in.Size > math.MaxInt32 {
				return FilterWriteOut{}, gapi.Errf(gapi.ClassInvalid, "size is bytes, from 1 to %d", math.MaxInt32)
			}
			fw, err := svc.CreateFilter(ctx, service.FilterSpec{
				Criteria: gmail.FilterCriteria{From: in.From, To: in.To, Subject: in.Subject, Query: in.Query,
					NegatedQuery: in.NegatedQuery, HasAttachment: in.HasAttachment, ExcludeChats: in.ExcludeChats,
					Size: int32(in.Size), SizeComparison: in.SizeComparison}, //nolint:gosec // bounded above
				AddLabels: in.AddLabels, RemoveLabels: in.RemoveLabels,
				Archive: in.Archive, MarkRead: in.MarkRead, Star: in.Star, Trash: in.Trash, Confirm: in.Confirm,
			})
			if err != nil {
				return FilterWriteOut{}, err
			}
			return filterWriteOut(fw), nil
		})

	register(s, d, Spec{Name: "delete_filter", Kind: SettingsForGood, Description: "Delete a filter by the id " +
		"list_filters gives. Mail it already acted on stays as it is. It cannot be restored, so confirm must be " +
		"true. The result shows what the filter did. 7 units." + dryRunNote},
		func(ctx context.Context, in DeleteFilterIn) (FilterWriteOut, error) {
			fw, err := svc.DeleteFilter(ctx, in.FilterID, in.Confirm)
			if err != nil {
				return FilterWriteOut{}, err
			}
			return filterWriteOut(fw), nil
		})

	register(s, d, Spec{Name: "set_vacation", Kind: AutoReply, Description: "Turn the vacation reply on or off. " +
		"On, it answers matching mail automatically, so it reaches other people: audience must say whether only " +
		"contacts or only the account's domain are answered, body is sent exactly as given, and confirm must be " +
		"true. start and end bound it. Off keeps its text for next time. The result shows the reply before and " +
		"after. 6 units, 7 with audience domain." + dryRunNote + untrustedNote},
		func(ctx context.Context, in SetVacationIn) (VacationOut, error) {
			vw, err := svc.SetVacation(ctx, service.VacationSpec{Enable: in.Enable, Subject: in.Subject, Body: in.Body,
				Audience: in.Audience, Start: in.Start, End: in.End, Confirm: in.Confirm})
			if err != nil {
				return VacationOut{}, err
			}
			return VacationOut{DryRun: vw.DryRun, Before: vacation(vw.Before), After: vacation(vw.After),
				Rendered: readOf(render.VacationWrite(vw, render.Options{}))}, nil
		})
}

func filterWriteOut(fw model.FilterWrite) FilterWriteOut {
	return FilterWriteOut{DryRun: fw.DryRun, Filter: filter(fw.Filter), Trashes: fw.Filter.Trashes(),
		Created: fw.Op == "create" && !fw.DryRun, Deleted: fw.Op == "delete" && !fw.DryRun && !fw.Gone, Gone: fw.Gone,
		text: render.FilterWrite(fw)}
}

func vacation(v model.Vacation) Vacation {
	return Vacation{Enabled: v.Enabled, UntrustedSubject: v.Subject, UntrustedBody: v.Body,
		RestrictToContacts: v.RestrictToContacts, RestrictToDomain: v.RestrictToDomain,
		Start: timePtr(v.Start), End: timePtr(v.End)}
}
