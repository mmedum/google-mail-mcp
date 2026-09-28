package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/mime"
	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
)

// MaxSendRecipients is the most recipients one send may have (§4.2).
// Gmail allows 500; a send to more than 50 from an agent is refused
// outright, whatever is confirmed.
const MaxSendRecipients = 50

// settleTimeout bounds the reads that settle an ambiguous send. They run
// even when the call's own context has ended, since a verdict read late
// is still the only thing that tells a person not to send twice.
const settleTimeout = 30 * time.Second

// settleDelay is how long settle waits before reading. A send the
// client gave up on may still be running at Google, and a read in the
// same instant would find the draft there and SENT empty, and call it
// not sent.
const settleDelay = 5 * time.Second

// recipientHeaders are the headers a send goes to.
var recipientHeaders = []string{"To", "Cc", "Bcc"}

// participantHeaders are the headers a thread's participants are read
// from: From on the messages the account received, To and Cc on the
// ones it sent (§4.2).
var participantHeaders = []string{"From", "To", "Cc"}

// Dispatch is what send_draft was asked for.
type Dispatch struct {
	DraftID string
	// Witness is the message id get_draft or create_draft returned; the
	// send is refused if the draft holds another now.
	Witness string
	// Confirm names the recipients the caller vouches for: every one
	// that is not already a participant in the thread the draft answers.
	Confirm []string
}

// SendDraft sends a draft (§4.2, §4.3). It reads the draft, checks the
// witness and the recipient guard, and sends once. A dry run stops
// before the send. A send whose outcome Gmail did not confirm is never
// repeated: it is [ambiguous_outcome], with the verdict of the reads
// that followed it.
func (s *Service) SendDraft(ctx context.Context, in Dispatch) (model.SendWrite, error) {
	out := model.SendWrite{DryRun: gapi.WritesForbidden(ctx), DraftID: strings.TrimSpace(in.DraftID)}
	witness := strings.TrimSpace(in.Witness)
	switch {
	case out.DraftID == "":
		return out, gapi.Errf(gapi.ClassInvalid, "draft_id is required")
	case witness == "":
		return out, gapi.Errf(gapi.ClassInvalid,
			"message_id is required: pass the message_id get_draft or create_draft returned, so a draft changed since is not sent")
	}
	confirmed, err := parseConfirm(in.Confirm)
	if err != nil {
		return out, err
	}
	g, err := s.client.GetDraft(ctx, out.DraftID, gapi.FormatFull)
	if err != nil {
		return out, err
	}
	d, err := model.NewDraft(g, model.NewLabelIndex(nil), nil)
	if err != nil {
		return out, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned draft %s without its message", out.DraftID)
	}
	m := d.Message
	if m.ID != witness {
		return out, gapi.Errf(gapi.ClassStale,
			"draft %s changed since it was read: it holds message %s now, not %s. Read it again with get_draft before sending",
			out.DraftID, m.ID, witness)
	}
	if err := readWhole(out.DraftID, m); err != nil {
		return out, err
	}
	out.MessageID, out.ThreadID, out.Subject, out.RFC822MessageID = m.ID, m.ThreadID, m.Subject, m.RFC822MessageID
	out.Body = model.Untrusted(m.Body.Text)
	if len(m.From) > 0 {
		out.From = &m.From[0]
	}
	for _, a := range m.Attachments {
		out.Files = append(out.Files, model.File{Name: model.Untrusted(a.Filename), MediaType: model.Untrusted(a.MimeType),
			Size: a.Size, PartID: a.PartID})
	}
	for _, f := range []struct {
		field string
		list  []mime.Address
	}{{"to", m.To}, {"cc", m.Cc}, {"bcc", m.Bcc}} {
		for i, a := range f.list {
			out.Recipients = append(out.Recipients, model.SendRecipient{Address: a, Field: f.field, Position: i,
				Confirmed: confirmed[strings.ToLower(a.Email)]})
		}
	}
	if err := validate(out, confirmed); err != nil {
		return out, err
	}
	th, err := s.participants(ctx, m)
	if err != nil {
		return out, err
	}
	out.Answers = th.answers
	for i, r := range out.Recipients {
		out.Recipients[i].Participant = th.seen[strings.ToLower(r.Address.Email)]
	}
	// A dry run is how the caller learns whom to confirm: it reports the
	// recipients the guard would stop instead of refusing.
	if out.DryRun {
		return out, nil
	}
	if err := unconfirmed(out); err != nil {
		return out, err
	}
	if err := ask(ctx, render.AskSend(out)); err != nil {
		return out, err
	}
	sent, err := s.client.SendDraft(ctx, out.DraftID)
	if err != nil {
		// A send that failed as unavailable may still have gone out: an
		// answer that could not be read, or a 503 after Google acted. It
		// is settled like an ambiguous one, never retried.
		if c := classOf(err); c == gapi.ClassAmbiguousOutcome || c == gapi.ClassUnavailable {
			return out, s.settle(ctx, out, th.ids, err)
		}
		return out, err
	}
	out.SentID, out.SentThreadID = sent.ID, sent.ThreadID
	out.SentLabels = model.NewLabelIndex(nil).Refs(sent.LabelIDs)
	return out, nil
}

// parseConfirm reads confirm_recipients: one address per entry, keyed
// by address, ignoring case. Unlike to, an entry need not be writable
// into a header: it only names a recipient the draft already has.
func parseConfirm(list []string) (map[string]bool, error) {
	out := make(map[string]bool, len(list))
	for i, s := range list {
		as, strict := mime.ParseAddressList(strings.TrimSpace(s))
		if !strict || len(as) != 1 || as[0].Email == "" {
			return nil, gapi.Errf(gapi.ClassInvalid,
				"confirm_recipients[%d] is not one email address; write each recipient out, as ada@example.com", i)
		}
		out[strings.ToLower(as[0].Email)] = true
	}
	return out, nil
}

// readWhole refuses a draft whose recipients this server cannot read
// whole: a To, Cc or Bcc header given twice, of which only the first is
// read, or one read leniently, which drops what it cannot parse. Gmail
// would send to every address it finds, so the guard would clear a list
// shorter than the one the mail goes to.
func readWhole(draftID string, m model.Message) error {
	for _, name := range recipientHeaders {
		n := 0
		for _, h := range m.Headers {
			if strings.EqualFold(h.Name, name) {
				n++
			}
		}
		if n > 1 || slices.Contains(m.LenientHeaders, name) {
			return gapi.Errf(gapi.ClassBlocked,
				"draft %s has a %s header this server cannot read whole (repeated, or not well formed), so it cannot "+
					"show everyone the send reaches; correct the recipients in Gmail, or set them with update_draft", draftID, name)
		}
	}
	return nil
}

// threadRead is what a send reads of the thread its draft sits in.
type threadRead struct {
	// seen are the participants' addresses, lowercased.
	seen map[string]bool
	// answers counts the messages that are not drafts, spam or trash.
	answers int
	// ids are every message in the thread before the send, the draft
	// included: a sent message is one that was not among them (§4.3).
	ids map[string]bool
}

// participants reads who is already in the thread the draft answers,
// and how many messages it holds. A participant wrote one of its
// messages, or the account itself sent one of them to that address. The
// To, Cc and Reply-To of a received message are not read: its sender
// wrote them, and counting them would let a correspondent widen a
// reply-all to addresses nobody confirmed (§17.8). A draft is not a
// participant's message, and neither is spam or trash: a message planted
// in a thread and then binned must not vouch for its sender.
func (s *Service) participants(ctx context.Context, draft model.Message) (threadRead, error) {
	out := threadRead{seen: map[string]bool{}, ids: map[string]bool{draft.ID: true}}
	if draft.ThreadID == "" {
		return out, nil
	}
	th, err := s.client.GetThread(ctx, draft.ThreadID, gapi.FormatMetadata, participantHeaders...)
	if err != nil {
		return out, err
	}
	t, err := model.NewThread(th, model.NewLabelIndex(nil), nil)
	if err != nil {
		return out, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a thread this server could not read")
	}
	for _, m := range t.Messages {
		out.ids[m.ID] = true
		if m.ID == draft.ID || m.HasLabel("DRAFT") || m.HasLabel("SPAM") || m.HasLabel("TRASH") {
			continue
		}
		out.answers++
		lists := [][]mime.Address{m.From}
		if m.HasLabel("SENT") {
			lists = append(lists, m.To, m.Cc)
		}
		for _, list := range lists {
			for _, a := range list {
				out.seen[strings.ToLower(a.Email)] = true
			}
		}
	}
	return out, nil
}

// validate refuses a send the guard of §4.2 refuses whatever the thread
// holds: no recipient, more than MaxSendRecipients, or a confirmation of
// an address the draft does not send to. It runs before the thread is
// read.
func validate(out model.SendWrite, confirmed map[string]bool) error {
	n := len(out.Recipients)
	switch {
	case n == 0:
		return gapi.Errf(gapi.ClassInvalid, "draft %s has no recipients; add them with update_draft", out.DraftID)
	case n > MaxSendRecipients:
		return gapi.Errf(gapi.ClassBlocked, "draft %s has %d recipients, over the %d one send may reach; send it from Gmail",
			out.DraftID, n, MaxSendRecipients)
	}
	addressed := map[string]bool{}
	for _, r := range out.Recipients {
		addressed[strings.ToLower(r.Address.Email)] = true
	}
	for c := range confirmed {
		if !addressed[c] {
			return gapi.Errf(gapi.ClassInvalid,
				"confirm_recipients names an address draft %s does not send to; send_draft with dry_run lists its recipients", out.DraftID)
		}
	}
	return nil
}

// unconfirmed is the recipient guard of §4.2: every recipient not in
// the thread must be confirmed. The refusal names recipients by field
// and position, never by address: an address can come from a message
// someone else wrote, and the server does not repeat it in its own
// voice. The dry run lists them inside a block.
func unconfirmed(out model.SendWrite) error {
	var stopped []string
	for _, r := range out.Recipients {
		if !r.Cleared() {
			stopped = append(stopped, fmt.Sprintf("%s[%d]", r.Field, r.Position))
		}
	}
	if len(stopped) == 0 {
		return nil
	}
	where := "are not participants in the thread this draft answers"
	if out.Answers == 0 {
		where = "start a new conversation"
	}
	return gapi.Errf(gapi.ClassBlocked,
		"%d of draft %s's recipients %s and are not in confirm_recipients: %s. "+
			"send_draft with dry_run lists them; name each address in confirm_recipients to send",
		len(stopped), out.DraftID, where, strings.Join(stopped, ", "))
}

// settle reads what an ambiguous send left behind and returns the
// [ambiguous_outcome] error with the verdict (§4.3). It never sends: on
// "not sent" as on "unknown", the person decides. before are the
// thread's message ids read before the send.
func (s *Service) settle(ctx context.Context, out model.SendWrite, before map[string]bool, cause error) error {
	verdict, sentID := model.SettledUnknown, ""
	s.settleAfter(ctx, func(ctx context.Context) { verdict, sentID = s.readBack(ctx, out, before) })
	var why string
	switch verdict {
	case model.SettledSent:
		why = fmt.Sprintf("Read afterwards: draft %s is gone and message %s is new in its thread %s, in SENT. "+
			"It was sent; do not send it again", out.DraftID, sentID, out.ThreadID)
	case model.SettledNotSent:
		why = fmt.Sprintf("Read %s afterwards: draft %s is still there and its thread %s has no new message in SENT. "+
			"It looks not sent, though Gmail can file a send late. Nothing was resent; ask the person before sending it again",
			settleDelay, out.DraftID, out.ThreadID)
	default:
		why = fmt.Sprintf("The reads afterwards could not tell whether draft %s was sent. "+
			"Do not send it again; ask the person to look in Gmail's Sent folder", out.DraftID)
	}
	return gapi.Wrap(gapi.ClassAmbiguousOutcome, cause,
		"Google did not confirm the send of draft %s, and it was not repeated (verdict: %s). %s.", out.DraftID, verdict, why)
}

// settleAfter is the scaffolding of every settle-by-reading (§4.3): it
// waits settleDelay, then runs read on a context that outlives the
// call's own, bounded by settleTimeout. read is skipped when the wait
// fails. It never writes; read must not either.
func (s *Service) settleAfter(ctx context.Context, read func(ctx context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if s.client.Wait(ctx, settleDelay) == nil {
		read(ctx)
	}
}

// readBack is the two reads of §4.3: the draft, and its thread for a
// message in SENT that was not there before the send. Gmail files a
// sent draft in the draft's thread under a new id, and replaces the
// Message-ID the draft carried (spikes B and C), so neither the draft's
// message id nor its Message-ID finds the sent copy.
func (s *Service) readBack(ctx context.Context, out model.SendWrite, before map[string]bool) (model.Settled, string) {
	if out.ThreadID == "" {
		return model.SettledUnknown, ""
	}
	_, err := s.client.GetDraft(ctx, out.DraftID, gapi.FormatMinimal)
	draftThere := err == nil
	if !draftThere && classOf(err) != gapi.ClassNotFound {
		return model.SettledUnknown, ""
	}
	th, err := s.client.GetThread(ctx, out.ThreadID, gapi.FormatMinimal)
	if err != nil && classOf(err) != gapi.ClassNotFound {
		return model.SettledUnknown, ""
	}
	sentID := ""
	if err == nil {
		for _, m := range th.Messages {
			if !before[m.ID] && slices.Contains(m.LabelIDs, "SENT") && !slices.Contains(m.LabelIDs, "DRAFT") {
				sentID = m.ID
			}
		}
	}
	switch {
	case !draftThere && sentID != "":
		return model.SettledSent, sentID
	case draftThere && sentID == "":
		return model.SettledNotSent, ""
	}
	return model.SettledUnknown, ""
}

// classOf is err's class, or "" for an error without one.
func classOf(err error) gapi.Class {
	c, _ := gapi.ClassOf(err)
	return c
}
