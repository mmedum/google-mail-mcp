package gmailtest

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// Settings are the account's settings and filters as the settings reads
// return them. Everything in them is generated, like the mail.
type Settings struct {
	Vacation            gmail.VacationSettings
	AutoForwarding      gmail.AutoForwarding
	ForwardingAddresses []gmail.ForwardingAddress
	Imap                gmail.ImapSettings
	Pop                 gmail.PopSettings
	Language            gmail.LanguageSettings
	SendAs              []gmail.SendAs
	Filters             []gmail.Filter
}

// The generated settings' addresses.
const (
	AliasAddress   = "rae.alias@example.org"
	BackupAddress  = "backup@example.org"
	PendingAddress = "pending@example.invalid"
)

// Filter ids in the generated settings.
const (
	FilterNewsletters = "ANe1BmgFixture01"
	FilterForward     = "ANe1BmgFixture02"
	FilterReceipts    = "ANe1BmgFixture03"
)

// seedSettings generates the account's settings: a vacation reply that
// is set up and off, forwarding off with one verified address and one
// pending, and three filters, one of which forwards.
func seedSettings(s *Server) {
	start := baseTime.AddDate(0, 0, 10)
	s.settings = Settings{
		Vacation: gmail.VacationSettings{
			EnableAutoReply: false, ResponseSubject: "Away until Monday",
			ResponseBodyPlainText: "I am away until Monday and will answer when I am back.\n\nRae",
			RestrictToContacts:    true,
			StartTime:             strconv.FormatInt(start.UnixMilli(), 10),
			EndTime:               strconv.FormatInt(start.AddDate(0, 0, 4).UnixMilli(), 10),
		},
		AutoForwarding: gmail.AutoForwarding{Enabled: false},
		ForwardingAddresses: []gmail.ForwardingAddress{
			{ForwardingEmail: BackupAddress, VerificationStatus: "accepted"},
			{ForwardingEmail: PendingAddress, VerificationStatus: "pending"},
		},
		Imap:     gmail.ImapSettings{Enabled: true, AutoExpunge: true, ExpungeBehavior: "archive"},
		Pop:      gmail.PopSettings{AccessWindow: "disabled", Disposition: "leaveInInbox"},
		Language: gmail.LanguageSettings{DisplayLanguage: "en-GB"},
		SendAs: []gmail.SendAs{
			{SendAsEmail: Account, DisplayName: Reader.Name, IsPrimary: true, IsDefault: true,
				Signature: "<div>Rae Reader<br>Example Org</div>"},
			{SendAsEmail: AliasAddress, DisplayName: "Rae (projects)", ReplyToAddress: Account,
				TreatAsAlias: true, VerificationStatus: "accepted"},
		},
		Filters: []gmail.Filter{
			{ID: FilterNewsletters, Criteria: &gmail.FilterCriteria{From: Harbor.Email},
				Action: &gmail.FilterAction{AddLabelIDs: []string{userLabelNewsletters}, RemoveLabelIDs: []string{"INBOX", "SPAM"}}},
			{ID: FilterForward, Criteria: &gmail.FilterCriteria{Query: "invoice", HasAttachment: true},
				Action: &gmail.FilterAction{Forward: BackupAddress}},
			{ID: FilterReceipts, Criteria: &gmail.FilterCriteria{Subject: "receipt", NegatedQuery: "from:" + Prize.Email,
				Size: 1 << 20, SizeComparison: "smaller"},
				Action: &gmail.FilterAction{AddLabelIDs: []string{userLabelReceipts, "STARRED"}}},
		},
	}
}

// RemoveLabel deletes a user label, as the web UI would between two
// requests.
func (s *Server) RemoveLabel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.labels, id)
}

// UpdateSettings changes the settings under the fake's lock.
func (s *Server) UpdateSettings(change func(*Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.settings)
}

func (s *Server) getVacation(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, s.settings.Vacation)
}

func (s *Server) getAutoForwarding(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, s.settings.AutoForwarding)
}

func (s *Server) listForwardingAddresses(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, gmail.ListForwardingAddressesResponse{ForwardingAddresses: slices.Clone(s.settings.ForwardingAddresses)})
}

func (s *Server) getImap(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, s.settings.Imap)
}

func (s *Server) getPop(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, s.settings.Pop)
}

func (s *Server) getLanguage(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, s.settings.Language)
}

func (s *Server) listSendAs(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, gmail.ListSendAsResponse{SendAs: slices.Clone(s.settings.SendAs)})
}

func (s *Server) listFilters(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, gmail.ListFiltersResponse{Filter: slices.Clone(s.settings.Filters)})
}

// Settings returns a copy of the settings, for tests that need the wire
// values without a request.
func (s *Server) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.settings
	c.ForwardingAddresses = slices.Clone(c.ForwardingAddresses)
	c.SendAs = slices.Clone(c.SendAs)
	c.Filters = slices.Clone(c.Filters)
	return c
}

// needsSettings refuses a settings write when the fake's token does not
// hold gmail.settings.basic.
func (s *Server) needsSettings(w http.ResponseWriter) bool { return needsScope(w, s.SettingsScope) }

// createFilter models filters.create: a criterion and an action are
// required and every label must exist. An identical filter is kept, as
// Gmail kept one live (§18 row 52), and the filter is stored with the
// labels sent, as Gmail answers a create (§18 row 56). The seeded
// newsletter filter carries the SPAM Gmail was seen adding later.
func (s *Server) createFilter(w http.ResponseWriter, r *http.Request, _ []string) {
	if s.needsSettings(w) {
		return
	}
	var in gmail.Filter
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Criteria == nil || *in.Criteria == (gmail.FilterCriteria{}) {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Filter doesn't have any criteria")
		return
	}
	if in.Action == nil || (len(in.Action.AddLabelIDs) == 0 && len(in.Action.RemoveLabelIDs) == 0 && in.Action.Forward == "") {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Filter doesn't have any actions")
		return
	}
	for _, id := range slices.Concat(in.Action.AddLabelIDs, in.Action.RemoveLabelIDs) {
		if _, ok := s.labels[id]; !ok {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid label: "+id)
			return
		}
	}
	in.ID = "ANe1Bmg" + s.nextID()
	s.settings.Filters = append(s.settings.Filters, in)
	writeJSON(w, in)
}

func (s *Server) deleteFilter(w http.ResponseWriter, _ *http.Request, args []string) {
	if s.needsSettings(w) {
		return
	}
	i := slices.IndexFunc(s.settings.Filters, func(f gmail.Filter) bool { return f.ID == args[0] })
	if i < 0 {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	s.settings.Filters = slices.Delete(s.settings.Filters, i, i+1)
	w.WriteHeader(http.StatusNoContent)
}

// patchSendAs models sendAs.patch for the signature, the one field the
// server writes.
func (s *Server) patchSendAs(w http.ResponseWriter, r *http.Request, args []string) {
	if s.needsSettings(w) {
		return
	}
	i := slices.IndexFunc(s.settings.SendAs, func(a gmail.SendAs) bool { return strings.EqualFold(a.SendAsEmail, args[0]) })
	if i < 0 {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	var in struct {
		Signature *string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid JSON")
		return
	}
	if in.Signature != nil {
		s.settings.SendAs[i].Signature = *in.Signature
	}
	writeJSON(w, s.settings.SendAs[i])
}

// updateVacation models settings.updateVacation, a full replacement.
func (s *Server) updateVacation(w http.ResponseWriter, r *http.Request, _ []string) {
	if s.needsSettings(w) {
		return
	}
	raw, err := io.ReadAll(r.Body)
	var in gmail.VacationSettings
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &in) != nil || json.Unmarshal(raw, &fields) != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid JSON")
		return
	}
	// An HTML body sent empty beside a plain one leaves the reply with no
	// body at all, as Gmail answered live (§18 row 52).
	if html, ok := fields["responseBodyHtml"]; ok && string(html) == `""` {
		in.ResponseBodyPlainText = ""
	}
	s.settings.Vacation = in
	writeJSON(w, in)
}
