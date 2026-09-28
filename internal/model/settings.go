package model

import (
	"slices"
	"strconv"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/mime"
)

// Settings are the account's settings, read-only (§7.7). The addresses
// and names in them are the account's own configuration. The vacation
// reply and the signatures are free text that goes out as mail, so they
// are kept as Untrusted like mail: whoever set them, the model reads
// them as data.
type Settings struct {
	Vacation            Vacation
	AutoForwarding      AutoForwarding
	ForwardingAddresses []ForwardingAddress
	Imap                gmail.ImapSettings
	Pop                 gmail.PopSettings
	Language            string
	SendAs              []SendAs
}

// Vacation is the vacation responder.
type Vacation struct {
	Enabled            bool
	Subject            Untrusted
	Body               Untrusted
	BodyFromHTML       bool
	RestrictToContacts bool
	RestrictToDomain   bool
	Start, End         time.Time // zero when unset
}

// AutoForwarding is whether every incoming message is forwarded.
type AutoForwarding struct {
	Enabled bool
	Address string
	// Disposition is what happens to the original: leaveInInbox,
	// archive, trash or markRead.
	Disposition string
}

// ForwardingAddress is an address mail may be forwarded to, and whether
// its owner accepted.
type ForwardingAddress struct {
	Address            string
	VerificationStatus string
}

// SendAs is an address the account sends as.
type SendAs struct {
	Address            string
	DisplayName        string
	ReplyTo            string
	Primary, Default   bool
	Alias              bool
	VerificationStatus string
	// Signature is the signature as text, converted from its HTML.
	Signature Untrusted
}

// NewSettings converts the seven settings reads.
func NewSettings(v gmail.VacationSettings, af gmail.AutoForwarding, fwd []gmail.ForwardingAddress,
	imap gmail.ImapSettings, pop gmail.PopSettings, lang gmail.LanguageSettings, sendAs []gmail.SendAs,
) Settings {
	out := Settings{
		Vacation: NewVacation(v),
		AutoForwarding: AutoForwarding{Enabled: af.Enabled, Address: af.EmailAddress,
			Disposition: af.Disposition},
		ForwardingAddresses: make([]ForwardingAddress, 0, len(fwd)),
		Imap:                imap, Pop: pop, Language: lang.DisplayLanguage,
		SendAs: make([]SendAs, 0, len(sendAs)),
	}
	for _, f := range fwd {
		out.ForwardingAddresses = append(out.ForwardingAddresses,
			ForwardingAddress{Address: f.ForwardingEmail, VerificationStatus: f.VerificationStatus})
	}
	for _, s := range sendAs {
		out.SendAs = append(out.SendAs, SendAs{
			Address: s.SendAsEmail, DisplayName: s.DisplayName, ReplyTo: s.ReplyToAddress,
			Primary: s.IsPrimary, Default: s.IsDefault, Alias: s.TreatAsAlias,
			VerificationStatus: s.VerificationStatus, Signature: HTMLText(s.Signature),
		})
	}
	return out
}

// NewVacation converts the vacation responder.
func NewVacation(v gmail.VacationSettings) Vacation {
	out := Vacation{
		Enabled: v.EnableAutoReply, Subject: Untrusted(v.ResponseSubject),
		Body:               Untrusted(v.ResponseBodyPlainText),
		RestrictToContacts: v.RestrictToContacts, RestrictToDomain: v.RestrictToDomain,
		Start: millis(v.StartTime), End: millis(v.EndTime),
	}
	if out.Body == "" && v.ResponseBodyHTML != "" {
		out.Body, out.BodyFromHTML = HTMLText(v.ResponseBodyHTML), true
	}
	return out
}

// HTMLText converts settings HTML to text the way a message body is
// converted: nothing fetched, hidden text dropped.
func HTMLText(s string) Untrusted {
	if s == "" {
		return ""
	}
	return Untrusted(mime.HTMLToText([]byte(s)).Text)
}

// millis reads a settings time, milliseconds since the epoch as a
// string. Zero or absent is no time.
func millis(s string) time.Time {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// Filter is a mail filter, its labels named (§7.7).
type Filter struct {
	ID       string
	Criteria gmail.FilterCriteria
	Add      []LabelRef
	Remove   []LabelRef
	// Forward is the address matching mail is forwarded to, or "". It is
	// the one action that sends mail out of the account, and every
	// rendering flags it.
	Forward string
}

// Trashes reports whether the filter moves matching mail to the trash.
func (f Filter) Trashes() bool {
	return slices.ContainsFunc(f.Add, func(l LabelRef) bool { return l.ID == "TRASH" })
}

// NeverSpam reports whether the filter keeps matching mail out of spam:
// Gmail's "Never send it to Spam", which it stores as removing SPAM
// (§18 row 56).
func (f Filter) NeverSpam() bool {
	return slices.ContainsFunc(f.Remove, func(l LabelRef) bool { return l.ID == "SPAM" })
}

// Forwarding counts the filters that forward mail out of the account.
func Forwarding(fs []Filter) int {
	n := 0
	for _, f := range fs {
		if f.Forward != "" {
			n++
		}
	}
	return n
}

// NewFilters converts a filter list.
func NewFilters(fs []gmail.Filter, labels LabelIndex) []Filter {
	out := make([]Filter, 0, len(fs))
	for _, f := range fs {
		m := Filter{ID: f.ID}
		if f.Criteria != nil {
			m.Criteria = *f.Criteria
		}
		if f.Action != nil {
			m.Add, m.Remove = labels.Refs(f.Action.AddLabelIDs), labels.Refs(f.Action.RemoveLabelIDs)
			m.Forward = f.Action.Forward
		}
		out = append(out, m)
	}
	return out
}
