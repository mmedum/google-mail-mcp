package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
	"github.com/mmedum/google-mail-mcp/internal/service"
)

// ListChangesIn asks what changed after a history id.
type ListChangesIn struct {
	HistoryID string   `json:"history_id" jsonschema:"history_id from get_profile or from the previous list_changes"`
	Label     string   `json:"label,omitempty" jsonschema:"only changes to messages with this label, by id or name"`
	Kinds     []string `json:"kinds,omitempty" jsonschema:"only these kinds of change: added, deleted, labels_added, labels_removed; default all"`
	Max       int      `json:"max,omitempty" jsonschema:"history records per page, 1 to 500; default 100"`
	PageToken string   `json:"page_token,omitempty" jsonschema:"next_page_token from the previous page, with the same history_id"`
}

// Change is one message-level change.
type Change struct {
	HistoryID string     `json:"history_id"`
	Kind      string     `json:"kind" jsonschema:"added, deleted (permanently), labels_added or labels_removed"`
	MessageID string     `json:"message_id"`
	ThreadID  string     `json:"thread_id"`
	Labels    []LabelRef `json:"labels" jsonschema:"the labels added or removed, or the message's labels when it was added"`
}

// ChangesOut is one page of changes, or an expired cursor.
type ChangesOut struct {
	Changes []Change `json:"changes"`
	// Expired is set when Gmail no longer keeps history from the id
	// given. The changes in between are lost, not absent.
	Expired bool `json:"expired" jsonschema:"Gmail no longer keeps history from history_id: the changes since are lost, not absent; restart from the history_id returned"`
	// HistoryID is where the next call starts, once no page is left.
	HistoryID     string    `json:"history_id" jsonschema:"the mailbox's current history id: the next call's history_id once complete is true"`
	Label         *LabelRef `json:"label,omitempty"`
	NextPageToken string    `json:"next_page_token,omitempty"`
	Complete      bool      `json:"complete"`
	Rendered
	Cost
}

// GetSettingsIn takes nothing.
type GetSettingsIn struct{}

// AutoForwarding is whether every incoming message is forwarded.
type AutoForwarding struct {
	Enabled     bool   `json:"enabled"`
	Address     string `json:"address,omitempty"`
	Disposition string `json:"disposition,omitempty" jsonschema:"what happens to the original: leaveInInbox, archive, trash or markRead"`
}

// ForwardingAddress is an address mail may be forwarded to.
type ForwardingAddress struct {
	Address            string `json:"address"`
	VerificationStatus string `json:"verification_status"`
}

// Vacation is the vacation responder.
type Vacation struct {
	Enabled            bool            `json:"enabled"`
	UntrustedSubject   model.Untrusted `json:"untrusted_subject,omitempty"`
	UntrustedBody      model.Untrusted `json:"untrusted_body,omitempty"`
	RestrictToContacts bool            `json:"restrict_to_contacts"`
	RestrictToDomain   bool            `json:"restrict_to_domain"`
	Start              *time.Time      `json:"start,omitempty"`
	End                *time.Time      `json:"end,omitempty"`
}

// SendAs is an address the account sends as.
type SendAs struct {
	Address            string          `json:"address"`
	DisplayName        string          `json:"display_name,omitempty"`
	ReplyTo            string          `json:"reply_to,omitempty"`
	Primary            bool            `json:"primary"`
	Default            bool            `json:"default"`
	Alias              bool            `json:"alias"`
	VerificationStatus string          `json:"verification_status,omitempty"`
	UntrustedSignature model.Untrusted `json:"untrusted_signature,omitempty"`
}

// Imap is IMAP access.
type Imap struct {
	Enabled         bool   `json:"enabled"`
	AutoExpunge     bool   `json:"auto_expunge"`
	ExpungeBehavior string `json:"expunge_behavior,omitempty"`
	MaxFolderSize   int    `json:"max_folder_size,omitempty"`
}

// Pop is POP access.
type Pop struct {
	AccessWindow string `json:"access_window,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
}

// SettingsOut is the account's settings.
type SettingsOut struct {
	AutoForwarding      AutoForwarding      `json:"auto_forwarding"`
	ForwardingAddresses []ForwardingAddress `json:"forwarding_addresses"`
	Vacation            Vacation            `json:"vacation"`
	SendAs              []SendAs            `json:"send_as"`
	Imap                Imap                `json:"imap"`
	Pop                 Pop                 `json:"pop"`
	Language            string              `json:"language,omitempty"`
	Rendered
	Cost
}

// ListFiltersIn takes nothing.
type ListFiltersIn struct{}

// FilterCriteria is what a filter matches. Written by the account, not
// by a sender.
type FilterCriteria struct {
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
	Subject        string `json:"subject,omitempty"`
	Query          string `json:"query,omitempty"`
	NegatedQuery   string `json:"negated_query,omitempty"`
	HasAttachment  bool   `json:"has_attachment,omitempty"`
	ExcludeChats   bool   `json:"exclude_chats,omitempty"`
	Size           int    `json:"size,omitempty"`
	SizeComparison string `json:"size_comparison,omitempty"`
}

// Filter is a mail filter.
type Filter struct {
	ID           string         `json:"id"`
	Criteria     FilterCriteria `json:"criteria"`
	AddLabels    []LabelRef     `json:"add_labels"`
	RemoveLabels []LabelRef     `json:"remove_labels"`
	Forward      string         `json:"forward,omitempty" jsonschema:"the address matching mail is forwarded to: mail leaving the account"`
	NeverSpam    bool           `json:"never_spam,omitempty" jsonschema:"matching mail is never sent to spam: Gmail stores this as SPAM in remove_labels"`
}

// FiltersOut is every filter.
type FiltersOut struct {
	Filters    []Filter `json:"filters"`
	Forwarding int      `json:"forwarding" jsonschema:"how many filters forward mail out of the account"`
	Rendered
	Cost
}

func registerAccount(s *mcp.Server, d Deps) {
	svc := service.New(d.Client)

	register(s, d, Spec{Name: "list_changes", Kind: Read, Description: "What changed in the mailbox after a " +
		"history_id: messages added, deleted permanently, and labels added or removed, each by message id. Start " +
		"from get_profile's history_id and pass the history_id each call returns to the next. If Gmail no longer " +
		"keeps history that far back, the result says the cursor expired and gives a fresh history_id; the changes " +
		"in between cannot be listed. label limits it to one label. Three units per page, and one more when the cursor has expired."},
		func(ctx context.Context, in ListChangesIn) (ChangesOut, error) {
			c, err := svc.Changes(ctx, service.ChangesQuery{HistoryID: in.HistoryID, Label: in.Label, Kinds: in.Kinds,
				Max: in.Max, PageToken: in.PageToken})
			if err != nil {
				return ChangesOut{}, err
			}
			// Every change stays a row: a row holds nothing a sender wrote,
			// and one dropped could not be read again.
			rows := mapSlice(c.Changes, change)
			r := render.Changes(render.ChangeList{Start: c.Start, Changes: c.Changes, Expired: c.Expired,
				HistoryID: c.HistoryID, NextPageToken: c.NextPageToken, Label: c.Label}, keptWhole(rows))
			out := ChangesOut{Changes: rows, Expired: c.Expired, HistoryID: c.HistoryID,
				NextPageToken: c.NextPageToken, Complete: c.NextPageToken == "", Rendered: readOf(r)}
			if c.Label != nil {
				out.Label = &LabelRef{ID: c.Label.ID, Name: c.Label.Name}
			}
			return out, nil
		})

	// What this server can change depends on the flags, and the
	// descriptions say so, since a model reads them as the truth.
	settingsChange, filtersChange := " This server cannot change any of them.", " This server cannot create or change filters."
	if d.Config.EnableSettings && !d.Config.ReadOnly {
		settingsChange = " update_signature changes a signature; forwarding and the rest cannot be changed here."
		if d.Config.EnableSend {
			settingsChange = " update_signature and set_vacation change a signature and the vacation reply; forwarding " +
				"and the rest cannot be changed here."
		}
		filtersChange = " create_filter and delete_filter make and remove filters."
	}

	register(s, d, Spec{Name: "get_settings", Kind: Read, Description: "The account's mail settings, read-only: " +
		"whether incoming mail is forwarded and where, the forwarding addresses, the vacation reply, the addresses it " +
		"sends as with their signatures, IMAP, POP and the display language." + settingsChange +
		" Seven units." + untrustedNote},
		func(ctx context.Context, _ GetSettingsIn) (SettingsOut, error) {
			st, err := svc.Settings(ctx)
			if err != nil {
				return SettingsOut{}, err
			}
			return settingsOut(st), nil
		})

	register(s, d, Spec{Name: "list_filters", Kind: Read, Description: "The account's filters: what each " +
		"matches and what it does, with labels by name. A filter that forwards mail out of the account is flagged." +
		filtersChange + " Two units."},
		func(ctx context.Context, _ ListFiltersIn) (FiltersOut, error) {
			fs, err := svc.Filters(ctx)
			if err != nil {
				return FiltersOut{}, err
			}
			rows := mapSlice(fs, filter)
			// Every filter stays a row: they are the account's own settings,
			// and one that forwards must never be out of sight.
			r := render.Filters(fs, keptWhole(rows))
			return FiltersOut{Filters: rows, Forwarding: model.Forwarding(fs), Rendered: readOf(r)}, nil
		})
}

func change(c model.Change) Change {
	return Change{HistoryID: c.HistoryID, Kind: string(c.Kind), MessageID: c.MessageID, ThreadID: c.ThreadID,
		Labels: labelRefs(c.Labels)}
}

func settingsOut(st model.Settings) SettingsOut {
	return SettingsOut{
		AutoForwarding: AutoForwarding{Enabled: st.AutoForwarding.Enabled, Address: st.AutoForwarding.Address,
			Disposition: st.AutoForwarding.Disposition},
		ForwardingAddresses: mapSlice(st.ForwardingAddresses, func(f model.ForwardingAddress) ForwardingAddress {
			return ForwardingAddress{Address: f.Address, VerificationStatus: f.VerificationStatus}
		}),
		Vacation: vacation(st.Vacation),
		SendAs: mapSlice(st.SendAs, func(s model.SendAs) SendAs {
			return SendAs{Address: s.Address, DisplayName: s.DisplayName, ReplyTo: s.ReplyTo, Primary: s.Primary,
				Default: s.Default, Alias: s.Alias, VerificationStatus: s.VerificationStatus, UntrustedSignature: s.Signature}
		}),
		Imap: Imap{Enabled: st.Imap.Enabled, AutoExpunge: st.Imap.AutoExpunge, ExpungeBehavior: st.Imap.ExpungeBehavior,
			MaxFolderSize: int(st.Imap.MaxFolderSize)},
		Pop:      Pop{AccessWindow: st.Pop.AccessWindow, Disposition: st.Pop.Disposition},
		Language: st.Language,
		Rendered: readOf(render.Settings(st, render.Options{})),
	}
}

func filter(f model.Filter) Filter {
	c := f.Criteria
	return Filter{ID: f.ID, AddLabels: labelRefs(f.Add), RemoveLabels: labelRefs(f.Remove), Forward: f.Forward, NeverSpam: f.NeverSpam(),
		Criteria: FilterCriteria{From: c.From, To: c.To, Subject: c.Subject, Query: c.Query, NegatedQuery: c.NegatedQuery,
			HasAttachment: c.HasAttachment, ExcludeChats: c.ExcludeChats, Size: int(c.Size), SizeComparison: c.SizeComparison}}
}
