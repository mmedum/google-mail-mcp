package service

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/mime"
	"github.com/mmedum/google-mail-mcp/internal/model"
)

// Compose is what create_draft was asked for (§7.4).
type Compose struct {
	To, Cc, Bcc []string
	Subject     string
	// Body is the plain text; HTML, when set, goes beside it.
	Body, HTML string
	// Attachments are file names inside LocalDir.
	Attachments []string
	// From is one of the account's send-as addresses; "" is its default.
	From string
	// ReplyTo is a message id, or rfc822:<Message-ID>; ReplyToThread is
	// a thread id, whose newest message is answered. At most one.
	ReplyTo, ReplyToThread string
	ReplyAll               bool
	LocalDir               string
}

// maxReferences bounds the References a reply carries: the thread's
// first ids and its last. A hostile parent cannot make a reply huge.
const maxReferences = 40

// reactionType is the part Gmail's emoji reactions carry. A reaction is
// not a message to reply to (§3.2).
const reactionType = "text/vnd.google.email-reaction+json"

// CreateDraft builds a new draft, or a reply constructed from its parent
// (§4.5), and saves it. On a dry run it builds everything and saves
// nothing.
func (s *Service) CreateDraft(ctx context.Context, in Compose) (model.DraftWrite, error) {
	out := model.DraftWrite{Op: "create", DryRun: gapi.WritesForbidden(ctx)}
	callers, err := in.check()
	if err != nil {
		return out, err
	}
	files, err := readLocalFiles(in.LocalDir, in.Attachments)
	if err != nil {
		return out, err
	}
	// The send-as list and the parent are independent reads.
	var (
		parent model.Message
		perr   error
		wg     sync.WaitGroup
	)
	if in.reply() {
		wg.Go(func() { parent, perr = s.replyParent(ctx, in.ReplyTo, in.ReplyToThread) })
	}
	senders, err := s.client.SendAs(ctx)
	wg.Wait()
	if err != nil {
		return out, err
	}
	if perr != nil {
		return out, perr
	}
	from, err := pickFrom(senders.SendAs, in.From)
	if err != nil {
		return out, err
	}
	o := mime.Outgoing{From: &from, Subject: in.Subject, Text: in.Body, HTML: in.HTML, Attachments: files,
		MessageID: mime.NewMessageID(from.Email)}
	out.Recipients = callers
	threadID := ""
	if in.reply() {
		r := &model.Reply{ParentID: parent.ID, ParentThreadID: parent.ThreadID, FromThread: in.ReplyToThread != "",
			ReplyAll: in.ReplyAll}
		out.Recipients = replyRecipients(parent, senders.SendAs, in.ReplyAll, callers, r)
		o.Subject = replySubject(string(parent.Subject))
		if o.InReplyTo, o.References = threading(parent); o.InReplyTo == "" {
			r.NoMessageID = true
		}
		threadID = parent.ThreadID
		out.Reply = r
	}
	addressTo(&o, out.Recipients)
	raw, err := mime.Build(o)
	if err != nil {
		// Every input was checked above, and the parent's addresses that
		// cannot be written were left out; the cause is not shown, since
		// it may quote a sender's words.
		return out, gapi.Wrap(gapi.ClassInvalid, err, "the draft could not be written as a valid message")
	}
	if err := checkSize(len(raw)); err != nil {
		return out, err
	}
	// The Message-ID written is not reported: Gmail replaces it on
	// drafts.create (§15, spike B), so it would state what was not kept.
	out.From, out.Subject = &from, model.Untrusted(o.Subject)
	out.Files = filesOf(files)
	out.Bytes, out.Upload = len(raw), gapi.Uploads(len(raw))
	out.ThreadID = threadID
	if out.DryRun {
		return out, nil
	}
	d, err := s.client.CreateDraft(ctx, threadID, raw)
	if err != nil {
		return out, err
	}
	s.written(&out, d)
	if out.Reply != nil {
		out.Reply.Joined = d.Message != nil && d.Message.ThreadID == out.Reply.ParentThreadID
	}
	return out, nil
}

// reply reports whether the draft answers another message.
func (in Compose) reply() bool { return in.ReplyTo != "" || in.ReplyToThread != "" }

// check refuses what create_draft cannot build, before anything is read,
// and returns the caller's recipients.
func (in Compose) check() ([]model.Recipient, error) {
	reply := in.reply()
	switch {
	case in.ReplyTo != "" && in.ReplyToThread != "":
		return nil, gapi.Errf(gapi.ClassInvalid, "give reply_to or reply_to_thread, not both")
	case in.ReplyAll && !reply:
		return nil, gapi.Errf(gapi.ClassInvalid, "reply_all needs reply_to or reply_to_thread")
	case reply && in.Subject != "":
		return nil, gapi.Errf(gapi.ClassInvalid,
			"a reply takes its parent's subject, which is one of the three things Gmail threads by; leave subject out")
	case in.HTML != "" && in.Body == "":
		return nil, gapi.Errf(gapi.ClassInvalid, "body_html needs body as well: the plain text is what a reader without HTML sees")
	}
	if _, err := mime.FormatText(in.Subject); err != nil {
		return nil, gapi.Errf(gapi.ClassInvalid, "subject may not contain line breaks or control characters")
	}
	var callers []model.Recipient
	for _, f := range []struct {
		field string
		list  []string
	}{{"to", in.To}, {"cc", in.Cc}, {"bcc", in.Bcc}} {
		_, rs, err := callerRecipients(f.field, f.list)
		if err != nil {
			return nil, err
		}
		callers = append(callers, rs...)
	}
	return callers, nil
}

// callerRecipients reads the addresses a caller gave for one field.
func callerRecipients(field string, list []string) ([]mime.Address, []model.Recipient, error) {
	as, err := parseRecipients(field, list)
	if err != nil {
		return nil, nil, err
	}
	rs := make([]model.Recipient, 0, len(as))
	for _, a := range as {
		rs = append(rs, model.Recipient{Address: a, Field: field, Origin: model.FromCaller})
	}
	return as, rs, nil
}

// addressTo puts each recipient in its header.
func addressTo(o *mime.Outgoing, rs []model.Recipient) {
	for _, rc := range rs {
		switch rc.Field {
		case "to":
			o.To = append(o.To, rc.Address)
		case "cc":
			o.Cc = append(o.Cc, rc.Address)
		default:
			o.Bcc = append(o.Bcc, rc.Address)
		}
	}
}

// written fills a result from Gmail's answer to a create or update.
func (s *Service) written(out *model.DraftWrite, d *gmail.Draft) {
	out.DraftID = d.ID
	if d.Message != nil {
		out.MessageID, out.ThreadID = d.Message.ID, d.Message.ThreadID
		out.Labels = model.NewLabelIndex(nil).Refs(d.Message.LabelIDs)
	}
}

// parseRecipients reads one address per entry. A refusal names the
// entry by position, not by what it says.
func parseRecipients(field string, in []string) ([]mime.Address, error) {
	out := make([]mime.Address, 0, len(in))
	for i, s := range in {
		list, strict := mime.ParseAddressList(strings.TrimSpace(s))
		if !strict || len(list) != 1 || list[0].Email == "" {
			return nil, gapi.Errf(gapi.ClassInvalid,
				"%s[%d] is not one email address; give one per entry, as ada@example.com or Ada Quill <ada@example.com>", field, i)
		}
		if _, err := mime.FormatAddresses(list); err != nil {
			return nil, gapi.Errf(gapi.ClassInvalid,
				"%s[%d] cannot be written: an address must be ASCII local@domain, and a name may not hold line breaks", field, i)
		}
		out = append(out, list[0])
	}
	return out, nil
}

// pickFrom is the send-as address to write as From: the one asked for,
// or the account's default.
func pickFrom(senders []gmail.SendAs, want string) (mime.Address, error) {
	if want != "" {
		list, strict := mime.ParseAddressList(strings.TrimSpace(want))
		if strict && len(list) == 1 {
			for _, s := range senders {
				if strings.EqualFold(s.SendAsEmail, list[0].Email) {
					return mime.Address{Name: s.DisplayName, Email: s.SendAsEmail}, nil
				}
			}
		}
		return mime.Address{}, gapi.Errf(gapi.ClassInvalid,
			"from must be one of the addresses this account sends as; get_settings lists them")
	}
	pick, err := defaultSender(senders)
	if err != nil {
		return mime.Address{}, err
	}
	return mime.Address{Name: senders[pick].DisplayName, Email: senders[pick].SendAsEmail}, nil
}

// defaultSender is the index of the account's default send-as address,
// or of its primary one when Gmail marks none default.
func defaultSender(senders []gmail.SendAs) (int, error) {
	pick := -1
	for i, s := range senders {
		if s.IsDefault || (pick < 0 && s.IsPrimary) {
			pick = i
		}
		if s.IsDefault {
			break
		}
	}
	if pick < 0 {
		return 0, gapi.Errf(gapi.ClassUnavailable, "Gmail listed no address this account sends as")
	}
	return pick, nil
}

// replyParent reads the message a reply answers. A thread id is answered
// through its newest message that is not a draft, not in the trash and
// not a reaction; a message id naming one of those is refused.
func (s *Service) replyParent(ctx context.Context, replyTo, thread string) (model.Message, error) {
	empty := model.NewLabelIndex(nil)
	if thread != "" {
		g, err := s.client.GetThread(ctx, thread, gapi.FormatFull)
		if err != nil {
			return model.Message{}, err
		}
		t, err := model.NewThread(g, empty, nil)
		if err != nil {
			return model.Message{}, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a thread this server could not read")
		}
		for i := len(t.Messages) - 1; i >= 0; i-- {
			if unrepliable(t.Messages[i]) == "" {
				return t.Messages[i], nil
			}
		}
		return model.Message{}, gapi.Errf(gapi.ClassInvalid,
			"thread %s has no message to reply to: each one is a draft, in the trash, or an emoji reaction", thread)
	}
	id, err := s.messageID(ctx, replyTo)
	if err != nil {
		return model.Message{}, err
	}
	g, err := s.client.GetMessage(ctx, id, gapi.FormatFull)
	if err != nil {
		return model.Message{}, err
	}
	m, err := model.NewMessage(g, empty, nil)
	if err != nil {
		return model.Message{}, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a message this server could not read")
	}
	if why := unrepliable(m); why != "" {
		return model.Message{}, gapi.Errf(gapi.ClassInvalid, "reply_to %s; reply to a message that was sent or received", why)
	}
	return m, nil
}

// unrepliable says why a message is not a parent, or "".
func unrepliable(m model.Message) string {
	switch {
	case m.HasLabel("DRAFT"):
		return "is a draft"
	case m.HasLabel("TRASH"):
		return "is in the trash (restore it first, or answer another message)"
	}
	for _, a := range m.Attachments {
		if strings.EqualFold(a.MimeType, reactionType) {
			return "is an emoji reaction"
		}
	}
	return ""
}

// replyRecipients builds a reply's recipients (§4.5): the parent's
// Reply-To or sender, or when the parent is the account's own, its To;
// with reply_all, the parent's other recipients too. The account's own
// addresses are left out, each address appears once, and the caller's
// additions come last. An address from the parent that cannot be written
// into a header is left out and counted.
func replyRecipients(p model.Message, senders []gmail.SendAs, all bool, callers []model.Recipient, r *model.Reply) []model.Recipient {
	own := map[string]bool{}
	for _, s := range senders {
		own[strings.ToLower(s.SendAsEmail)] = true
	}
	seen := map[string]bool{}
	var out []model.Recipient
	add := func(as []mime.Address, field string, origin model.Origin) {
		for _, a := range as {
			key := strings.ToLower(a.Email)
			switch {
			case seen[key]:
				continue
			case own[key]:
				r.DroppedOwn++
				continue
			}
			if _, err := mime.FormatAddresses([]mime.Address{a}); err != nil {
				r.Unwritable++
				continue
			}
			seen[key] = true
			out = append(out, model.Recipient{Address: a, Field: field, Origin: origin})
		}
	}
	fromOwn := own[strings.ToLower(p.Sender().Email)]
	switch {
	case fromOwn:
		add(p.To, "to", model.FromParent)
	case len(p.ReplyTo) > 0:
		add(p.ReplyTo, "to", model.FromParent)
	default:
		add(p.From, "to", model.FromParent)
	}
	if all {
		if !fromOwn {
			add(p.To, "to", model.FromReplyAll)
		}
		add(p.Cc, "cc", model.FromReplyAll)
	}
	for _, c := range callers {
		key := strings.ToLower(c.Address.Email)
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

var rePrefix = regexp.MustCompile(`(?i)^\s*re\s*:`)

// replySubject is the parent's subject with "Re: " before it, unless it
// already starts with one.
func replySubject(s string) string {
	if rePrefix.MatchString(s) {
		return s
	}
	return strings.TrimSpace("Re: " + s)
}

// threading is a reply's In-Reply-To and References (§4.5): the
// parent's Message-ID, and the parent's References with it appended —
// or, when the parent has no References, its single In-Reply-To (RFC
// 5322 §3.6.4). An id that is not <local@domain> is left out; a parent
// with no usable Message-ID gives neither header.
func threading(p model.Message) (string, []string) {
	id := string(p.RFC822MessageID)
	if !mime.ValidMessageID(id) {
		return "", nil
	}
	var refs []string
	src := p.References
	if len(src) == 0 && len(p.InReplyTo) == 1 {
		src = p.InReplyTo
	}
	for _, r := range src {
		if mime.ValidMessageID(string(r)) && string(r) != id {
			refs = append(refs, string(r))
		}
	}
	if len(refs) > maxReferences-1 {
		refs = append(refs[:1], refs[len(refs)-(maxReferences-2):]...)
	}
	return id, append(refs, id)
}

// readLocalFiles reads attachments from dir by base name only (§7.4):
// a path, a "..", or a link out of the directory is refused, and the
// total is checked against Gmail's limit before anything is read.
func readLocalFiles(dir string, names []string) ([]mime.OutAttachment, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if dir == "" {
		return nil, gapi.Errf(gapi.ClassBlocked, "GMAIL_LOCAL_DIR is not set, so this server reads no files to attach")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, gapi.Wrap(gapi.ClassUnavailable, err, "GMAIL_LOCAL_DIR could not be opened")
	}
	defer func() { _ = root.Close() }()
	type opened struct {
		f    *os.File
		name string
		size int64
	}
	var files []opened
	defer func() {
		for _, o := range files {
			_ = o.f.Close()
		}
	}()
	var total int64
	for i, name := range names {
		if !mime.ValidFilename(name) {
			return nil, gapi.Errf(gapi.ClassInvalid, "attachments[%d] must be the name of a file in GMAIL_LOCAL_DIR, not a path", i)
		}
		// Stat first: opening a named pipe would wait for a writer.
		pre, err := root.Stat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, gapi.Errf(gapi.ClassNotFound, "attachments[%d] is not a file in GMAIL_LOCAL_DIR", i)
		case err != nil:
			return nil, gapi.Wrap(gapi.ClassInvalid, err, "attachments[%d] could not be opened inside GMAIL_LOCAL_DIR; a link out of it is refused", i)
		case !pre.Mode().IsRegular():
			return nil, gapi.Errf(gapi.ClassInvalid, "attachments[%d] is not a regular file", i)
		}
		f, err := root.Open(name)
		if err != nil {
			return nil, gapi.Wrap(gapi.ClassInvalid, err, "attachments[%d] could not be opened inside GMAIL_LOCAL_DIR", i)
		}
		files = append(files, opened{f: f, name: name})
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return nil, gapi.Errf(gapi.ClassInvalid, "attachments[%d] is not a regular file", i)
		}
		files[len(files)-1].size = st.Size()
		// Base64 grows a file by a third, plus a line break every 76.
		total += st.Size()*4/3 + st.Size()/57*2
	}
	if total > mime.MaxRawBytes {
		return nil, gapi.Errf(gapi.ClassInvalid, "the attachments come to about %d MB encoded, over the 35 MB Gmail accepts for a draft", total>>20)
	}
	out := make([]mime.OutAttachment, 0, len(files))
	for i, o := range files {
		b, err := io.ReadAll(io.LimitReader(o.f, o.size+1))
		if err != nil || int64(len(b)) != o.size {
			return nil, gapi.Errf(gapi.ClassUnavailable, "attachments[%d] could not be read whole", i)
		}
		out = append(out, mime.OutAttachment{Filename: o.name, MediaType: mime.MediaTypeFor(o.name), Content: b})
	}
	return out, nil
}

// checkSize refuses a message over Gmail's 35 MB, before it is sent.
func checkSize(n int) error {
	if n > mime.MaxRawBytes {
		return gapi.Errf(gapi.ClassInvalid, "the draft would be %.1f MB, over the 35 MB Gmail accepts", float64(n)/(1<<20))
	}
	return nil
}

func filesOf(as []mime.OutAttachment) []model.File {
	out := make([]model.File, 0, len(as))
	for _, a := range as {
		out = append(out, model.File{Name: model.Untrusted(a.Filename), MediaType: model.Untrusted(a.MediaType), Size: len(a.Content)})
	}
	return out
}

// Revise is what update_draft was asked for (§4.4). A nil list or
// pointer leaves that field as it is; an empty list clears it.
type Revise struct {
	DraftID string
	// Witness is the message id get_draft returned; the update is
	// refused if the draft holds another now.
	Witness     string
	To, Cc, Bcc []string
	Subject     *string
	Body, HTML  *string
	// Attach adds files from LocalDir; Remove drops attachments by part id.
	Attach, Remove []string
	LocalDir       string
}

// UpdateDraft re-reads a draft, checks the witness, applies only what
// was given, and saves it (§4.4). Everything not named is carried over
// from what Gmail stored, byte for byte. The window between the re-read
// and the save is not closed; Gmail offers nothing that would close it.
func (s *Service) UpdateDraft(ctx context.Context, in Revise) (model.DraftWrite, error) {
	out := model.DraftWrite{Op: "update", DryRun: gapi.WritesForbidden(ctx), DraftID: in.DraftID, PreviousMessageID: in.Witness}
	if strings.TrimSpace(in.Witness) == "" {
		return out, gapi.Errf(gapi.ClassInvalid, "message_id is required: pass the message_id get_draft returned, so a draft changed since is not overwritten")
	}
	e, set, err := in.edit(&out)
	if err != nil {
		return out, err
	}
	files, err := readLocalFiles(in.LocalDir, in.Attach)
	if err != nil {
		return out, err
	}
	e.Add, out.Added = files, filesOf(files)

	d, err := s.client.GetDraft(ctx, in.DraftID, gapi.FormatRaw)
	if err != nil {
		return out, err
	}
	if d.Message == nil || d.Message.Raw == "" {
		return out, gapi.Errf(gapi.ClassUnavailable, "Gmail returned draft %s without its message", in.DraftID)
	}
	if d.Message.ID != in.Witness {
		return out, gapi.Errf(gapi.ClassStale,
			"draft %s changed since it was read: it holds message %s now, not %s. Read it again with get_draft and reapply the change",
			in.DraftID, d.Message.ID, in.Witness)
	}
	raw, err := mime.DecodeBase64URL(d.Message.Raw)
	if err != nil {
		return out, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a draft that is not base64url")
	}
	before := mime.ParseRaw(raw)
	for _, id := range dedupe(in.Remove) {
		if id == "" {
			return out, gapi.Errf(gapi.ClassInvalid,
				`part_id "" is the whole draft, not an attachment in it; delete_draft removes the draft`)
		}
		a, ok := findAttachment(before.Attachments, id)
		if !ok {
			return out, gapi.Errf(gapi.ClassInvalid, "the draft has no attachment with part_id %q; get_draft lists them", id)
		}
		e.Remove = append(e.Remove, id)
		out.Removed = append(out.Removed, model.File{Name: model.Untrusted(a.Filename),
			MediaType: model.Untrusted(a.MimeType), Size: a.Size, PartID: a.PartID})
	}
	if len(e.Remove) > 0 || len(e.Add) > 0 {
		out.Changed = append(out.Changed, "attachments")
	}
	edited, err := mime.EditRaw(raw, e)
	if err != nil {
		return out, editError(err)
	}
	if err := checkSize(len(edited)); err != nil {
		return out, err
	}
	after := mime.ParseRaw(edited)
	describe(&out, after, set)
	out.ThreadingAtRisk = in.Subject != nil && len(before.InReplyTo) > 0 && after.Subject != before.Subject
	out.ThreadID, out.MessageID = d.Message.ThreadID, d.Message.ID
	out.Bytes, out.Upload = len(edited), gapi.Uploads(len(edited))
	if out.DryRun {
		return out, nil
	}
	saved, err := s.client.UpdateDraft(ctx, in.DraftID, d.Message.ThreadID, edited)
	if err != nil {
		return out, err
	}
	s.written(&out, saved)
	return out, nil
}

// edit turns the fields given into an edit of the draft's headers and
// bodies, naming each in out.Changed, and returns the recipients set.
func (in Revise) edit(out *model.DraftWrite) (mime.Edit, []model.Recipient, error) {
	var e mime.Edit
	if in.To == nil && in.Cc == nil && in.Bcc == nil && in.Subject == nil && in.Body == nil && in.HTML == nil &&
		len(in.Attach) == 0 && len(in.Remove) == 0 {
		return e, nil, gapi.Errf(gapi.ClassInvalid, "nothing to change: give at least one field to update")
	}
	var set []model.Recipient
	for _, f := range []struct {
		field, header string
		list          []string
	}{{"to", "To", in.To}, {"cc", "Cc", in.Cc}, {"bcc", "Bcc", in.Bcc}} {
		if f.list == nil {
			continue
		}
		as, rs, err := callerRecipients(f.field, f.list)
		if err != nil {
			return e, nil, err
		}
		v, _ := mime.FormatAddresses(as)
		e.Headers = append(e.Headers, mime.SetHeader{Name: f.header, Value: v})
		out.Changed = append(out.Changed, f.field)
		set = append(set, rs...)
	}
	if in.Subject != nil {
		v, err := mime.FormatText(*in.Subject)
		if err != nil {
			return e, nil, gapi.Errf(gapi.ClassInvalid, "subject may not contain line breaks or control characters")
		}
		e.Headers = append(e.Headers, mime.SetHeader{Name: "Subject", Value: v})
		out.Changed = append(out.Changed, "subject")
	}
	e.Text, e.HTML = in.Body, in.HTML
	if in.Body != nil {
		out.Changed = append(out.Changed, "body")
	}
	if in.HTML != nil {
		out.Changed = append(out.Changed, "body_html")
	}
	return e, set, nil
}

// describe fills an update's result from the draft as it will be saved.
func describe(out *model.DraftWrite, after *mime.Message, set []model.Recipient) {
	out.Recipients = keptRecipients(after.To, after.Cc, after.Bcc, set)
	out.Subject, out.RFC822MessageID = model.Untrusted(after.Subject), model.Untrusted(after.MessageID)
	if len(after.From) > 0 {
		out.From = &after.From[0]
	}
	for _, a := range after.Attachments {
		out.Files = append(out.Files, model.File{Name: model.Untrusted(a.Filename), MediaType: model.Untrusted(a.MimeType), Size: a.Size})
	}
}

// keptRecipients lists a draft's recipients after an update, marking the
// ones the caller set.
func keptRecipients(to, cc, bcc []mime.Address, set []model.Recipient) []model.Recipient {
	var out []model.Recipient
	for _, f := range []struct {
		field string
		list  []mime.Address
	}{{"to", to}, {"cc", cc}, {"bcc", bcc}} {
		origin := model.FromDraft
		for _, r := range set {
			if r.Field == f.field {
				origin = model.FromCaller
			}
		}
		for _, a := range f.list {
			out = append(out, model.Recipient{Address: a, Field: f.field, Origin: origin})
		}
	}
	return out
}

// editError maps an edit the draft's shape refuses to the caller's fix.
func editError(err error) error {
	switch {
	case errors.Is(err, mime.ErrBothBodies):
		return gapi.Errf(gapi.ClassInvalid, "the draft has a plain-text and an HTML version; give body and body_html together so they agree")
	case errors.Is(err, mime.ErrHTMLOnly):
		return gapi.Errf(gapi.ClassInvalid, "the draft's only text is HTML; give body_html as well as body")
	case errors.Is(err, mime.ErrNoPlainBody):
		return gapi.Errf(gapi.ClassInvalid, "the draft has no text yet; give body as well as body_html")
	case errors.Is(err, mime.ErrTooManyParts):
		return gapi.Errf(gapi.ClassUnsupported, "the draft has more MIME parts than this server edits without losing some; edit it in Gmail")
	case errors.Is(err, mime.ErrNotAttachment):
		return gapi.Errf(gapi.ClassInvalid, "that part_id names no attachment of the draft; get_draft lists them")
	}
	return gapi.Wrap(gapi.ClassInvalid, err, "the draft could not be changed as asked")
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// DeleteDraft deletes a draft, which Gmail does permanently: a draft
// never goes to the trash. confirm must be set (§17.1); a dry run reads
// the draft and deletes nothing.
func (s *Service) DeleteDraft(ctx context.Context, id string, confirm bool) (model.DraftWrite, error) {
	out := model.DraftWrite{Op: "delete", DryRun: gapi.WritesForbidden(ctx), DraftID: id}
	if !confirm && !out.DryRun {
		return out, gapi.Errf(gapi.ClassBlocked,
			"delete_draft deletes the draft permanently; drafts do not go to the trash. Pass confirm: true to delete it")
	}
	g, err := s.client.GetDraft(ctx, id, gapi.FormatMetadata)
	if err != nil {
		return out, err
	}
	d, err := model.NewDraft(g, model.NewLabelIndex(nil), nil)
	if err != nil {
		return out, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned draft %s without its message", id)
	}
	m := d.Message
	out.MessageID, out.ThreadID, out.Subject, out.RFC822MessageID = m.ID, m.ThreadID, m.Subject, m.RFC822MessageID
	if len(m.From) > 0 {
		out.From = &m.From[0]
	}
	out.Recipients = keptRecipients(m.To, m.Cc, m.Bcc, nil)
	if out.DryRun {
		return out, nil
	}
	if err := s.client.DeleteDraft(ctx, id); err != nil {
		if c, _ := gapi.ClassOf(err); c != gapi.ClassNotFound {
			return out, err
		}
		out.Gone = true
	}
	return out, nil
}
