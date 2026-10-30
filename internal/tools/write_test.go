package tools_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v3/internal/config"
	"github.com/mmedum/google-mail-mcp/v3/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v3/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v3/internal/mime"
	"github.com/mmedum/google-mail-mcp/v3/internal/model"
	"github.com/mmedum/google-mail-mcp/v3/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v3/internal/tools"
)

// refused calls a tool that must fail and returns its error text.
func refused(t *testing.T, h *testutil.Harness, name string, args map[string]any, class gapi.Class) string {
	t.Helper()
	res := h.Call(t, name, args)
	text := testutil.Text(res)
	if !res.IsError {
		t.Fatalf("%s succeeded: %s", name, text)
	}
	if !strings.HasPrefix(text, "["+string(class)+"]") {
		t.Fatalf("%s: %s; want [%s]", name, text, class)
	}
	return text
}

// writeCalls counts the requests that changed something.
func writeCalls(fake *gmailtest.Server) int {
	n := 0
	for _, c := range fake.Calls() {
		for _, w := range []string{".create", ".update", ".delete", ".modify", "trash", ".patch"} {
			if strings.HasSuffix(c.Method, w) || strings.Contains(c.Method, "trash") && w == "trash" {
				n++
				break
			}
		}
	}
	return n
}

func storedDraft(t *testing.T, fake *gmailtest.Server, messageID string) *mime.Message {
	t.Helper()
	raw, ok := fake.Raw(messageID)
	if !ok {
		t.Fatalf("no stored message %s", messageID)
	}
	return mime.ParseRaw(raw)
}

// outside is a rendering with its blocks cut out: the server's own lines.
func outside(text string) string {
	var b strings.Builder
	for {
		i := strings.Index(text, "<<<untrusted-mail")
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:i])
		j := strings.Index(text[i:], "<<<end-untrusted-mail")
		if j < 0 {
			return b.String()
		}
		text = text[i+j+len("<<<end-untrusted-mail"):]
	}
}

func labelIDs(ls []tools.LabelRef) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}

func TestCreateDraft(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{
		"to": []any{"Ada Quill <ada.quill@example.com>"}, "cc": []any{"bruno.fennick@example.org"},
		"bcc": []any{"Zoë Ångström <zoe@example.org>"}, "subject": "Plan für März", "body": "Line one\nLine two",
		"body_html": "<p>Line one</p>",
	}, &out)
	if out.DryRun || out.DraftID == "" || out.MessageID == "" || !slices.Contains(labelIDs(out.Labels), "DRAFT") || out.Units != 11 {
		t.Fatalf("out = %+v", out)
	}
	if string(out.UntrustedFrom) != gmailtest.Reader.Name+" <"+gmailtest.Account+">" || len(out.Recipients) != 3 ||
		out.Recipients[2].Field != "bcc" || out.Recipients[0].Origin != "caller" {
		t.Fatalf("from %q recipients %+v", out.UntrustedFrom, out.Recipients)
	}
	m := storedDraft(t, fake, out.MessageID)
	// Gmail replaces the Message-ID the server wrote, so the result names
	// none rather than one that was not kept.
	if m.Subject != "Plan für März" || len(m.Bcc) != 1 || m.Bcc[0].Name != "Zoë Ångström" || m.Body.Source != mime.SourcePlain ||
		out.UntrustedRFC822MessageID != "" || !strings.HasPrefix(m.MessageID, "<draft.") {
		t.Fatalf("stored %q %v %q %q", m.Subject, m.Bcc, m.Body.Source, m.MessageID)
	}
	if !strings.Contains(text, "update_draft needs draft_id "+out.DraftID+" and message_id "+out.MessageID) {
		t.Errorf("the witness is not named:\n%s", text)
	}
	if o := outside(text); strings.Contains(o, "März") || strings.Contains(o, "ada.quill") {
		t.Errorf("mail text outside the block:\n%s", text)
	}

	drafts := len(fake.DraftIDs())
	var dry tools.DraftWriteOut
	text = call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "x", "dry_run": true}, &dry)
	if !dry.DryRun || dry.DraftID != "" || len(fake.DraftIDs()) != drafts || !strings.HasPrefix(text, "dry run") {
		t.Fatalf("a dry run saved: %+v\n%s", dry, text)
	}

	var from tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"from": gmailtest.AliasAddress, "body": "x"}, &from)
	if !strings.Contains(string(from.UntrustedFrom), gmailtest.AliasAddress) {
		t.Errorf("from alias: %q", from.UntrustedFrom)
	}
}

// An address a result shows can be passed back as one recipient: a name
// with a comma or an address in it comes back quoted, not as text that
// splits in two or names the wrong address.
func TestARecipientShownCanBeGivenBack(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	for _, give := range []string{`"Quill, Ada" <ada@example.com>`, `"Boss <boss@example.org>" <ada@example.com>`} {
		var first, again tools.DraftWriteOut
		call(t, h, "create_draft", map[string]any{"to": []any{give}, "body": "x", "dry_run": true}, &first)
		if len(first.Recipients) != 1 || first.Recipients[0].UntrustedAddress != model.Untrusted(give) {
			t.Fatalf("given %s, the result shows %+v", give, first.Recipients)
		}
		shown := string(first.Recipients[0].UntrustedAddress)
		call(t, h, "create_draft", map[string]any{"to": []any{shown}, "body": "x", "dry_run": true}, &again)
		if len(again.Recipients) != 1 || again.Recipients[0].UntrustedAddress != model.Untrusted(give) {
			t.Errorf("given back %s, the result shows %+v", shown, again.Recipients)
		}
	}
}

// With no from, a draft is from the account's default send-as address,
// or from its primary one when Gmail marks none default.
func TestCreateDraftFromThePrimaryWhenNoneIsDefault(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	fake.UpdateSettings(func(s *gmailtest.Settings) {
		for i := range s.SendAs {
			s.SendAs[i].IsDefault = false
		}
	})
	var out tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "x"}, &out)
	if want := gmailtest.Reader.Name + " <" + gmailtest.Account + ">"; string(out.UntrustedFrom) != want {
		t.Errorf("from %q; want the primary address %q", out.UntrustedFrom, want)
	}

	fake.UpdateSettings(func(s *gmailtest.Settings) {
		for i := range s.SendAs {
			s.SendAs[i].IsPrimary = false
		}
	})
	refused(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "x"}, gapi.ClassUnavailable)
}

func TestCreateDraftRefusals(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	cases := []struct {
		name  string
		args  map[string]any
		class gapi.Class
	}{
		{"two addresses in one entry", map[string]any{"to": []any{"a@example.com, b@example.com"}}, gapi.ClassInvalid},
		{"not an address", map[string]any{"cc": []any{"nobody"}}, gapi.ClassInvalid},
		{"non-ASCII address", map[string]any{"to": []any{"zoë@example.org"}}, gapi.ClassInvalid},
		{"line break in subject", map[string]any{"subject": "a\nBcc: x@example.com"}, gapi.ClassInvalid},
		{"html without text", map[string]any{"body_html": "<p>x</p>"}, gapi.ClassInvalid},
		{"from not a send-as", map[string]any{"from": "someone@example.com"}, gapi.ClassInvalid},
		{"reply_all alone", map[string]any{"reply_all": true}, gapi.ClassInvalid},
		{"both reply fields", map[string]any{"reply_to": plain.MessageIDs[0], "reply_to_thread": plain.ThreadID}, gapi.ClassInvalid},
		{"a subject on a reply", map[string]any{"reply_to": plain.MessageIDs[0], "subject": "New"}, gapi.ClassInvalid},
		{"reply to a draft", map[string]any{"reply_to": fake.Scenario(gmailtest.ScenarioDraftReply).MessageIDs[2]}, gapi.ClassInvalid},
		{"reply to the trash", map[string]any{"reply_to": fake.Scenario(gmailtest.ScenarioTrash).MessageIDs[0]}, gapi.ClassInvalid},
		{"reply to nothing", map[string]any{"reply_to": "00000000000fffff"}, gapi.ClassNotFound},
		{"attachments without a directory", map[string]any{"attachments": []any{"a.txt"}}, gapi.ClassBlocked},
		{"quote without a reply", map[string]any{"quote": true, "body": "x"}, gapi.ClassInvalid},
		{"quote and body_html", map[string]any{"reply_to": plain.MessageIDs[0], "quote": true, "body": "x", "body_html": "<p>x</p>"},
			gapi.ClassInvalid},
		{"quote a forward", map[string]any{"forward": plain.MessageIDs[0], "quote": true}, gapi.ClassInvalid},
		{"signature and plain_only", map[string]any{"body": "x", "signature": true, "plain_only": true}, gapi.ClassInvalid},
		{"signature in body as well", map[string]any{"body": "Hi.\n\nRae Reader\nExample Org\n\n> earlier", "signature": true},
			gapi.ClassInvalid},
		{"signature below one in body_html", map[string]any{"body": "x", "signature": true,
			"body_html": `<p>x</p><div class="gmail_signature" data-smartmail="gmail_signature">Rae</div>`}, gapi.ClassInvalid},
		{"forward and reply_to", map[string]any{"forward": plain.MessageIDs[0], "reply_to": plain.MessageIDs[1]}, gapi.ClassInvalid},
		{"forward and reply_to_thread", map[string]any{"forward": plain.MessageIDs[0], "reply_to_thread": plain.ThreadID}, gapi.ClassInvalid},
		{"forward and reply_all", map[string]any{"forward": plain.MessageIDs[0], "reply_all": true}, gapi.ClassInvalid},
		{"forward a draft", map[string]any{"forward": fake.Scenario(gmailtest.ScenarioDraftReply).MessageIDs[2]}, gapi.ClassInvalid},
		{"forward from the trash", map[string]any{"forward": fake.Scenario(gmailtest.ScenarioTrash).MessageIDs[0]}, gapi.ClassInvalid},
		{"forward a reaction", map[string]any{"forward": fake.AddPartsMessage(&gmailtest.Part{
			ContentType: model.ReactionType + "; charset=utf-8", CTE: "7bit", Content: []byte(`{"version":1,"emoji":"+"}`)})},
			gapi.ClassInvalid},
		{"forward nothing", map[string]any{"forward": "00000000000fffff"}, gapi.ClassNotFound},
		{"forward a line too long to attach", map[string]any{"forward": fake.AddPartsMessage(&gmailtest.Part{
			ContentType: "text/plain; charset=us-ascii", CTE: "7bit", Content: []byte(strings.Repeat("x", 999) + "\n")})},
			gapi.ClassUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.ResetAccounting()
			refused(t, h, "create_draft", tc.args, tc.class)
			if n := writeCalls(fake); n != 0 {
				t.Errorf("%d writes reached Gmail", n)
			}
		})
	}
}

func TestCreateDraftReplies(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	parentID := plain.MessageIDs[2] // from Bruno to the reader and Ada
	parentRaw, _ := fake.Raw(parentID)
	parent := mime.ParseRaw(parentRaw)

	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{"reply_to": parentID, "body": "Thanks.", "cc": []any{"chiara@example.com"}}, &out)
	if out.Reply == nil || out.Reply.ParentID != parentID || !out.Reply.Joined || out.ThreadID != plain.ThreadID || out.Units != 31 {
		t.Fatalf("reply %+v thread %s units %d", out.Reply, out.ThreadID, out.Units)
	}
	if len(out.Recipients) != 2 || !strings.Contains(string(out.Recipients[0].UntrustedAddress), gmailtest.Bruno.Email) ||
		out.Recipients[0].Origin != "parent" || out.Recipients[1].Origin != "caller" {
		t.Fatalf("recipients %+v", out.Recipients)
	}
	m := storedDraft(t, fake, out.MessageID)
	if m.Subject != "Re: Offsite venue" || strings.Join(m.InReplyTo, "") != parent.MessageID ||
		m.References[len(m.References)-1] != parent.MessageID || len(m.References) != len(parent.References)+1 {
		t.Fatalf("threading: subject %q in-reply-to %v references %v", m.Subject, m.InReplyTo, m.References)
	}
	if !strings.Contains(text, "Gmail filed the draft in the parent's thread.") {
		t.Errorf("text:\n%s", text)
	}

	var all tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to": parentID, "reply_all": true, "body": "All."}, &all)
	var origins []string
	for _, r := range all.Recipients {
		origins = append(origins, r.Field+":"+r.Origin)
		if strings.Contains(string(r.UntrustedAddress), gmailtest.Account) {
			t.Errorf("the account's own address is a recipient: %v", all.Recipients)
		}
	}
	if strings.Join(origins, ",") != "to:parent,to:reply_all" || all.Reply.DroppedOwn != 1 {
		t.Fatalf("reply-all recipients %v dropped %d", origins, all.Reply.DroppedOwn)
	}

	// The thread's newest message is a draft; the one before it was sent
	// by the account, so the reply goes to that message's recipients.
	dr := fake.Scenario(gmailtest.ScenarioDraftReply)
	var th tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to_thread": dr.ThreadID, "body": "Following up."}, &th)
	if !th.Reply.FromThread || th.Reply.ParentID != dr.MessageIDs[1] || !th.Reply.Joined || th.Units != 51 ||
		len(th.Recipients) != 1 || !strings.Contains(string(th.Recipients[0].UntrustedAddress), gmailtest.Freya.Email) {
		t.Fatalf("thread reply %+v %+v units %d", th.Reply, th.Recipients, th.Units)
	}
	if string(th.UntrustedSubject) != "Re: Budget sign-off" {
		t.Errorf("subject %q", th.UntrustedSubject)
	}
}

// A reply's In-Reply-To and References are built from the parent's own
// headers (§4.5): its References with its Message-ID appended, or its
// one In-Reply-To when it has no References, kept to 40 ids. A parent
// with no Message-ID gives neither header, and the reply says so.
func TestCreateDraftReplyThreadingHeaders(t *testing.T) {
	const parent = "<parent.1@example.com>"
	// ids is <r{from}@example.com> through <r{to}@example.com>.
	ids := func(from, to int) []string {
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, "<r"+strconv.Itoa(i)+"@example.com>")
		}
		return out
	}
	tests := []struct {
		name                       string
		messageID, inReplyTo, refs string
		byThread                   bool
		wantInReplyTo, wantRefs    []string
		wantNoMessageID            bool
	}{
		{"references, the parent appended", parent, "<b@example.com>", "<a@example.com> <b@example.com>", false,
			[]string{parent}, []string{"<a@example.com>", "<b@example.com>", parent}, false},
		{"no references: the one in-reply-to starts the chain", parent, "<b@example.com>", "", false,
			[]string{parent}, []string{"<b@example.com>", parent}, false},
		{"a one-message thread answered by thread", parent, "<b@example.com>", "", true,
			[]string{parent}, []string{"<b@example.com>", parent}, false},
		{"no references and two in-reply-to ids: neither is the chain", parent, "<b@example.com> <c@example.com>", "", false,
			[]string{parent}, []string{parent}, false},
		{"39 references and the parent fit", parent, "", strings.Join(ids(1, 39), " "), false,
			[]string{parent}, append(ids(1, 39), parent), false},
		{"40 references keep the first and the newest 38", parent, "", strings.Join(ids(1, 40), " "), false,
			[]string{parent}, append(append(ids(1, 1), ids(3, 40)...), parent), false},
		{"no message-id: no threading headers", "", "<b@example.com>", "<a@example.com> <b@example.com>", false,
			nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := connectFake(t, config.Config{})
			id := fake.AddThreadingParent(tc.messageID, tc.inReplyTo, tc.refs)
			args := map[string]any{"reply_to": id, "body": "Booked."}
			if tc.byThread {
				args = map[string]any{"reply_to_thread": id, "body": "Booked."}
			}
			var out tools.DraftWriteOut
			call(t, h, "create_draft", args, &out)
			if out.Reply == nil || out.Reply.ParentID != id || out.Reply.NoMessageID != tc.wantNoMessageID {
				t.Fatalf("reply %+v; want parent %s, no_message_id %v", out.Reply, id, tc.wantNoMessageID)
			}
			m := storedDraft(t, fake, out.MessageID)
			if !slices.Equal(m.InReplyTo, tc.wantInReplyTo) {
				t.Errorf("In-Reply-To %v; want %v", m.InReplyTo, tc.wantInReplyTo)
			}
			if !slices.Equal(m.References, tc.wantRefs) {
				t.Errorf("References %v; want %v", m.References, tc.wantRefs)
			}
		})
	}
}

// A quote puts the parent's text below the body, after a line naming its
// Date header as written and its sender, each line after "> " (§7.4).
// The HTML version is made from the whole text, quote included.
func TestCreateDraftQuotes(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	id := fake.AddThreadingParent("<parent.1@example.com>", "", "")
	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{"reply_to": id, "quote": true, "body": "Yes, booked."}, &out)
	if out.Reply == nil || out.Reply.QuotedChars != 39 || out.Reply.QuoteFromHTML || out.Units != 31 {
		t.Fatalf("reply %+v units %d; want 39 characters quoted from plain text, 31 units", out.Reply, out.Units)
	}
	const quote = "On Wed, 04 Mar 2026 05:01:00 +0000, Bruno Fennick <bruno.fennick@example.org> wrote:\n> Is the room booked for Thursday?\n>\n> Bruno"
	raw, _ := fake.Raw(out.MessageID)
	html, _ := gmailtest.HTMLPart(raw)
	if got := mime.ParseRaw(raw).Body.Text; got != "Yes, booked.\n\n"+quote {
		t.Fatalf("stored text\n%q\nwant\n%q", got, "Yes, booked.\n\n"+quote)
	}
	// An empty line of the parent's is quoted as ">", with no space after.
	if !bytes.Contains(raw, []byte("Thursday?\r\n>\r\n> Bruno")) {
		t.Errorf("the plain part does not quote the empty line as >:\n%s", raw)
	}
	if !strings.Contains(html, "&gt; Is the room booked for Thursday?<br>") {
		t.Errorf("the HTML version does not carry the quote:\n%s", html)
	}
	if !strings.Contains(text, "quoted below the body: 39 characters of the parent's text") {
		t.Errorf("text:\n%s", text)
	}
	// The HTML version, quote included, reads as made from the text: a
	// body-only update makes it again, and replaces the quote with the rest.
	var upd tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": out.DraftID, "message_id": out.MessageID, "body": "Booked."}, &upd)
	raw, _ = fake.Raw(upd.MessageID)
	html, _ = gmailtest.HTMLPart(raw)
	if !slices.Equal(upd.Changed, []string{"body", "body_html"}) || mime.ParseRaw(raw).Body.Text != "Booked." || html != "<p>Booked.</p>\n" {
		t.Fatalf("update changed %v; stored %q and HTML %q", upd.Changed, mime.ParseRaw(raw).Body.Text, html)
	}

	var bare tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to": id, "quote": true, "plain_only": true}, &bare)
	raw, _ = fake.Raw(bare.MessageID)
	if got := mime.ParseRaw(raw).Body.Text; got != quote {
		t.Fatalf("with no body, stored\n%q\nwant the quote alone", got)
	}
	if _, ok := gmailtest.HTMLPart(raw); ok {
		t.Error("plain_only kept an HTML version")
	}
}

// A parent with only HTML, or whose plain part only points at its HTML,
// is quoted as converted: hidden text left out, links reduced to their
// host, and quote_from_html set.
func TestCreateDraftQuotesHTMLAsConverted(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	for _, tc := range []struct {
		scenario, want, never string
	}{
		{gmailtest.ScenarioNewsletter, "> Members can renew at https://members.example.com/renew <track.harbor-weekly.invalid> before Friday.",
			"collector@attacker.invalid"},
		{gmailtest.ScenarioPlaceholder, "> Your booking is confirmed.", "Please view the HTML version"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			var out tools.DraftWriteOut
			text := call(t, h, "create_draft", map[string]any{"reply_to": fake.Scenario(tc.scenario).MessageIDs[0],
				"quote": true, "body": "Noted."}, &out)
			got := storedDraft(t, fake, out.MessageID).Body.Text
			if !out.Reply.QuoteFromHTML || !strings.Contains(got, tc.want) || strings.Contains(got, tc.never) ||
				strings.Contains(got, "/c/1?u=") {
				t.Fatalf("quote_from_html %v, stored:\n%s", out.Reply.QuoteFromHTML, got)
			}
			if !strings.Contains(text, "converted from its HTML, so each link in the quote keeps only its host") {
				t.Errorf("text:\n%s", text)
			}
		})
	}
}

// A quote reads the parts of the parent's text Gmail stored apart, 20
// units each, and says so; a reply without quote reads none.
func TestCreateDraftQuoteReadsTextStoredApart(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	thread, _ := fake.AddBackedThread()
	invite := fake.AddPartsMessage(gmailtest.File("application/ics", "invite.ics", []byte(
		"BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:quote@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")))
	for _, tc := range []struct {
		name    string
		args    map[string]any
		units   int
		want    string
		fetched bool
	}{
		{"by thread", map[string]any{"reply_to_thread": thread, "quote": true}, 71, "> Backed body 2.", true},
		{"by message", map[string]any{"reply_to": thread, "quote": true}, 51, "> Backed body 1.", true},
		{"no quote", map[string]any{"reply_to_thread": thread, "body": "x"}, 51, "x", false},
		// A read of the message would fetch the calendar file; a quote does not.
		{"a calendar file stored apart", map[string]any{"reply_to": invite, "quote": true}, 31, "> The files are attached.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out tools.DraftWriteOut
			text := call(t, h, "create_draft", tc.args, &out)
			got := storedDraft(t, fake, out.MessageID).Body.Text
			if out.Units != tc.units || !strings.HasSuffix(got, tc.want) {
				t.Fatalf("units %d, stored %q; want %d units and the text ending %q", out.Units, got, tc.units, tc.want)
			}
			if said := strings.Contains(text, "read 1 part of the parent's text that Gmail stored apart"); said != tc.fetched {
				t.Errorf("the text says a part was read: %v; want %v\n%s", said, tc.fetched, text)
			}
		})
	}
}

// A forward attaches the original as Gmail stored it, named after its
// subject, under a subject of its own, and asks for the original's
// thread with no threading headers (§7.4).
func TestCreateDraftForwards(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	id := plain.MessageIDs[0] // Ada's "Offsite venue"
	orig, _ := fake.Raw(id)

	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{"forward": id, "to": []any{"chiara@example.com"}, "body": "See below."}, &out)
	want := tools.ForwardOut{MessageID: id, ThreadID: plain.ThreadID, Bytes: len(orig)}
	if out.Forwarded == nil || *out.Forwarded != want || out.Reply != nil || out.Units != 31 {
		t.Fatalf("forwarded %+v reply %+v units %d; want %+v, no reply, 31 units", out.Forwarded, out.Reply, out.Units, want)
	}
	if string(out.UntrustedSubject) != "Fwd: Offsite venue" || len(out.Attachments) != 1 ||
		out.Attachments[0] != (tools.DraftFile{UntrustedName: "Offsite venue.eml", UntrustedMimeType: "message/rfc822", Size: len(orig)}) {
		t.Fatalf("subject %q attachments %+v", out.UntrustedSubject, out.Attachments)
	}
	raw, _ := fake.Raw(out.MessageID)
	m := mime.ParseRaw(raw)
	if !bytes.Contains(raw, orig) || len(m.Attachments) != 1 || m.Attachments[0].MimeType != "message/rfc822" ||
		m.Attachments[0].Size != len(orig) || m.Body.Text != "See below." || len(m.InReplyTo) > 0 || len(m.References) > 0 {
		t.Fatalf("stored: attachments %+v, body %q, in-reply-to %v, references %v, original whole %v",
			m.Attachments, m.Body.Text, m.InReplyTo, m.References, bytes.Contains(raw, orig))
	}
	// Gmail filed a forward in the original's thread by its threadId
	// alone when tested live (§18 row 85), and the fake does the same.
	if out.ThreadID != plain.ThreadID || !strings.Contains(text, "forward of message "+id+" in thread "+plain.ThreadID) ||
		!strings.Contains(text, "\nGmail filed the draft in the original's thread.\n") || strings.Contains(text, "a new thread") {
		t.Errorf("thread %s; text:\n%s", out.ThreadID, text)
	}

	var dry tools.DraftWriteOut
	text = call(t, h, "create_draft", map[string]any{"forward": "rfc822:<fixture." + id + "@mail.example.com>",
		"subject": "For the offsite", "dry_run": true}, &dry)
	if !dry.DryRun || dry.ThreadID != plain.ThreadID || dry.Forwarded == nil || dry.Forwarded.MessageID != id ||
		string(dry.UntrustedSubject) != "For the offsite" || dry.Units != 26 {
		t.Fatalf("dry run %+v", dry)
	}
	if !strings.Contains(text, "would create a draft in thread "+plain.ThreadID) {
		t.Errorf("text:\n%s", text)
	}

	// Ivan's message is 8-bit windows-1251, so it goes declared 8bit, and
	// its name carries the subject's Cyrillic and colon as written.
	ivan := fake.Scenario(gmailtest.ScenarioInternational).MessageIDs[2]
	orig, _ = fake.Raw(ivan)
	var intl tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"forward": ivan}, &intl)
	raw, _ = fake.Raw(intl.MessageID)
	cte := regexp.MustCompile(`Content-Type: message/rfc822;[^\r]*\r\n(?:[^\r]*\r\n)*?Content-Transfer-Encoding: (\S+)`).FindSubmatch(raw)
	if intl.Attachments[0].UntrustedName != "Re: Отчёт по проекту.eml" || cte == nil || string(cte[1]) != "8bit" ||
		!bytes.Contains(raw, orig) || mime.ParseRaw(raw).Attachments[0].DeclaredName != "Re: Отчёт по проекту.eml" {
		t.Fatalf("attachments %+v, encoding %q, original whole %v", intl.Attachments, cte, bytes.Contains(raw, orig))
	}
}

// A forward's copy is named after the original's subject, made a base
// name: a slash or backslash is "_", the subject is cut to 200 bytes at
// a character, and a message with no subject is "forwarded message".
func TestCreateDraftForwardNamesTheCopy(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	for _, tc := range []struct{ subject, want string }{
		{`Q3/Q4 plan \ draft`, "Q3_Q4 plan _ draft.eml"},
		{"a" + strings.Repeat("é", 150), "a" + strings.Repeat("é", 99) + ".eml"},
		{"", "forwarded message.eml"},
	} {
		var out tools.DraftWriteOut
		call(t, h, "create_draft", map[string]any{"forward": fake.AddSubjectMessage(tc.subject)}, &out)
		if len(out.Attachments) != 1 || string(out.Attachments[0].UntrustedName) != tc.want {
			t.Errorf("subject %q: attachments %+v; want one named %q", tc.subject, out.Attachments, tc.want)
		}
	}
}

// When Gmail files a forward in a thread of its own, as Google
// documents for a draft with no threading headers, the result says so
// and names the new thread.
func TestCreateDraftForwardInANewThread(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	fake.ThreadAsDocumented = true
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{"forward": plain.MessageIDs[0]}, &out)
	if out.ThreadID == plain.ThreadID || out.Forwarded == nil || out.Forwarded.ThreadID != plain.ThreadID {
		t.Fatalf("thread %s, forwarded %+v; want a new thread, the original in %s", out.ThreadID, out.Forwarded, plain.ThreadID)
	}
	if !strings.Contains(text, "\nnote: Gmail filed the draft in a new thread "+out.ThreadID+", not the original's.\n") {
		t.Errorf("text:\n%s", text)
	}
}

// A sent message's own copy carries its Bcc; a forward leaves it out of
// the attached copy, and says so.
func TestCreateDraftForwardLeavesOutBcc(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	id := fake.AddSentWithBcc()
	if orig, _ := fake.Raw(id); !bytes.Contains(orig, []byte(gmailtest.Bruno.Email)) {
		t.Fatal("the fixture carries no Bcc")
	}
	var out tools.DraftWriteOut
	text := call(t, h, "create_draft", map[string]any{"forward": id}, &out)
	raw, _ := fake.Raw(out.MessageID)
	if !out.Forwarded.BccRemoved || bytes.Contains(raw, []byte(gmailtest.Bruno.Email)) {
		t.Fatalf("bcc_removed %v; the draft carries the blind recipient: %v", out.Forwarded.BccRemoved,
			bytes.Contains(raw, []byte(gmailtest.Bruno.Email)))
	}
	if !strings.Contains(text, "leaves out the original's Bcc header") {
		t.Errorf("text:\n%s", text)
	}
}

// A forward, reply or quote by rfc822: takes one Message-ID: one with a
// search term after it is refused before any search, so it cannot
// attach or quote a message the caller never saw.
func TestRFC822WithASearchTermIsRefused(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	id := fake.Scenario(gmailtest.ScenarioInternational).MessageIDs[0]
	smuggled := "rfc822:<fixture." + id + `@mail.example.com> OR subject:"password reset"`
	fake.ResetAccounting()
	for _, args := range []map[string]any{
		{"forward": smuggled},
		{"reply_to": smuggled, "body": "Yes."},
		{"reply_to": smuggled, "body": "Yes.", "quote": true},
	} {
		text := refused(t, h, "create_draft", args, gapi.ClassInvalid)
		if !strings.Contains(text, "rfc822: takes one Message-ID, written <local@domain>, and nothing else") {
			t.Errorf("%v: %s", args, text)
		}
	}
	if n := len(fake.CallsOf("gmail.users.messages.list")); n != 0 || writeCalls(fake) != 0 {
		t.Errorf("%d searches and %d writes ran; want none", n, writeCalls(fake))
	}
}

// An original over what one read carries is refused, naming the limit,
// before anything is written.
func TestCreateDraftForwardRefusesAnOriginalTooLargeToRead(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	id, _ := fake.AddAttachmentMessage("big.bin", make([]byte, 19<<20))
	text := refused(t, h, "create_draft", map[string]any{"forward": id}, gapi.ClassInvalid)
	if !strings.Contains(text, "larger than the 24 MB this server reads in one answer") || writeCalls(fake) != 0 {
		t.Fatalf("%s; %d writes", text, writeCalls(fake))
	}
}

func TestCreateDraftAttachments(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "Отчёт.pdf"), []byte("%PDF fake"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, gapi.UploadThreshold+10), 0o600))
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	must(t, os.WriteFile(outsideFile, []byte("secret"), 0o600))
	if err := os.Symlink(outsideFile, filepath.Join(dir, "link.txt")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	h, fake := connectFake(t, config.Config{LocalDir: dir})

	var out tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "see attached",
		"attachments": []any{"Отчёт.pdf"}}, &out)
	if len(out.Attachments) != 1 || out.Attachments[0].UntrustedName != "Отчёт.pdf" ||
		out.Attachments[0].UntrustedMimeType != "application/pdf" || out.Upload {
		t.Fatalf("attachments %+v upload %v", out.Attachments, out.Upload)
	}
	if m := storedDraft(t, fake, out.MessageID); len(m.Attachments) != 1 || m.Attachments[0].DeclaredName != "Отчёт.pdf" {
		t.Fatalf("stored attachments %+v", m.Attachments)
	}

	var big tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "big", "attachments": []any{"big.bin"}}, &big)
	if !big.Upload || big.Bytes <= gapi.UploadThreshold {
		t.Fatalf("a %d-byte draft was not uploaded", big.Bytes)
	}

	for name, class := range map[string]gapi.Class{
		"../secret.txt": gapi.ClassInvalid, "sub/x": gapi.ClassInvalid, "missing.txt": gapi.ClassNotFound,
		"link.txt": gapi.ClassInvalid, "sub": gapi.ClassInvalid, "..": gapi.ClassInvalid,
	} {
		refused(t, h, "create_draft", map[string]any{"body": "x", "attachments": []any{name}}, class)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateDraft(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("first"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("second"), 0o600))
	h, fake := connectFake(t, config.Config{LocalDir: dir})

	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "cc": []any{"c@example.com"},
		"subject": "Draft", "body": "one", "attachments": []any{"a.txt", "b.txt"}}, &made)
	beforeRaw, _ := fake.Raw(made.MessageID)

	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID}, gapi.ClassInvalid)
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": "0000000000000abc",
		"subject": "x"}, gapi.ClassStale)

	var up tools.DraftWriteOut
	text := call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"subject": "Draft, revised", "cc": []any{}, "remove_attachments": []any{"1"}}, &up)
	if up.MessageID == made.MessageID || up.PreviousMessageID != made.MessageID || up.DraftID != made.DraftID || up.Units != 35 {
		t.Fatalf("update %+v", up)
	}
	if strings.Join(up.Changed, ",") != "cc,subject,attachments" || len(up.Removed) != 1 || up.Removed[0].UntrustedName != "a.txt" {
		t.Fatalf("changed %v removed %+v", up.Changed, up.Removed)
	}
	// A new subject puts only a reply's threading at risk; this draft
	// answers nothing.
	if up.ThreadingAtRisk || string(up.UntrustedFrom) != gmailtest.Reader.Name+" <"+gmailtest.Account+">" {
		t.Errorf("threading at risk %v, from %q; want false and the account's address", up.ThreadingAtRisk, up.UntrustedFrom)
	}
	m := storedDraft(t, fake, up.MessageID)
	if m.Subject != "Draft, revised" || len(m.Cc) != 0 || len(m.To) != 1 || m.Body.Text != "one" ||
		len(m.Attachments) != 1 || m.Attachments[0].DeclaredName != "b.txt" {
		t.Fatalf("stored: %q cc %v to %v body %q attachments %+v", m.Subject, m.Cc, m.To, m.Body.Text, m.Attachments)
	}
	if before := mime.ParseRaw(beforeRaw); m.MessageID != before.MessageID {
		t.Errorf("the Message-ID changed from %q to %q", before.MessageID, m.MessageID)
	}
	afterRaw, _ := fake.Raw(up.MessageID)
	if !strings.Contains(string(afterRaw), "c2Vjb25k") { // b.txt, base64, as it was
		t.Error("the kept attachment is not in the saved draft")
	}
	if !strings.Contains(text, "the next update_draft needs message_id "+up.MessageID) {
		t.Errorf("text:\n%s", text)
	}

	// The old witness is stale now.
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "two"}, gapi.ClassStale)

	var dry tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"body": "three", "add_attachments": []any{"a.txt"}, "dry_run": true}, &dry)
	if !dry.DryRun || storedDraft(t, fake, up.MessageID).Body.Text != "one" || len(dry.Added) != 1 {
		t.Fatalf("a dry run saved: %+v", dry)
	}
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"remove_attachments": []any{"7"}}, gapi.ClassInvalid)
	text = refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"remove_attachments": []any{""}}, gapi.ClassInvalid)
	if !strings.Contains(text, "whole draft") {
		t.Errorf("text: %s", text)
	}
}

func TestUpdateDraftKeepsTheTwoBodiesInStep(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "plain", "body_html": "<p>html</p>"}, &made)
	text := refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "new"}, gapi.ClassInvalid)
	if !strings.Contains(text, "body_html") {
		t.Errorf("text: %s", text)
	}
	var up tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "new", "body_html": "<p>new</p>"}, &up)
	if strings.Join(up.Changed, ",") != "body,body_html" {
		t.Errorf("changed %v", up.Changed)
	}
}

// storedHTML is the text/html part of a stored message, decoded by the
// standard library rather than by internal/mime, or "" when it has none.
func storedHTML(t *testing.T, fake *gmailtest.Server, messageID string) string {
	t.Helper()
	raw, ok := fake.Raw(messageID)
	if !ok {
		t.Fatalf("no stored message %s", messageID)
	}
	html, _ := gmailtest.HTMLPart(raw)
	return html
}

// A body given without body_html is saved beside HTML made from it, which
// a new body makes again; a draft keeps its shape unless plain_only
// changes it. HTML written another way is not replaced
// (TestUpdateDraftKeepsTheTwoBodiesInStep).
func TestADraftCarriesHTMLMadeFromItsBody(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"},
		"body": "Hi Ada,\n\nThe totals look right, and I approve them.\nRae\n"}, &made)
	if got, want := storedHTML(t, fake, made.MessageID),
		"<p>Hi Ada,</p>\n<p>The totals look right, and I approve them.<br>\nRae</p>\n"; got != want {
		t.Errorf("create_draft stored HTML %q, want %q", got, want)
	}
	if got := storedDraft(t, fake, made.MessageID).Body.Text; got != "Hi Ada,\n\nThe totals look right, and I approve them.\nRae" {
		t.Errorf("create_draft stored plain text %q, want the body as given", got)
	}

	var up tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "Revised: Tom & Jerry <tj@example.com>.", "subject": "Totals"}, &up)
	if got, want := storedHTML(t, fake, up.MessageID), "<p>Revised: Tom &amp; Jerry &lt;tj@example.com&gt;.</p>\n"; got != want {
		t.Errorf("update_draft stored HTML %q, want %q", got, want)
	}
	if strings.Join(up.Changed, ",") != "subject,body,body_html" {
		t.Errorf("update_draft changed %v, want subject,body,body_html", up.Changed)
	}

	var plain tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": up.MessageID,
		"body": "Plain now.", "plain_only": true}, &plain)
	if got := storedHTML(t, fake, plain.MessageID); got != "" || strings.Join(plain.Changed, ",") != "body,body_html" {
		t.Errorf("plain_only true: stored HTML %q, changed %v; want none, body,body_html", got, plain.Changed)
	}
	var still tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": plain.MessageID, "body": "Still plain."}, &still)
	if got := storedHTML(t, fake, still.MessageID); got != "" || strings.Join(still.Changed, ",") != "body" {
		t.Errorf("a plain draft given a body: stored HTML %q, changed %v; want none, body", got, still.Changed)
	}
	var again tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": still.MessageID,
		"body": "Approved.", "plain_only": false}, &again)
	if got := storedHTML(t, fake, again.MessageID); got != "<p>Approved.</p>\n" {
		t.Errorf("plain_only false stored HTML %q, want <p>Approved.</p>", got)
	}

	var bare tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "For the list.", "plain_only": true}, &bare)
	if got := storedHTML(t, fake, bare.MessageID); got != "" {
		t.Errorf("create_draft plain_only stored HTML %q, want none", got)
	}
	refused(t, h, "create_draft", map[string]any{"body": "x", "body_html": "<p>x</p>", "plain_only": true}, gapi.ClassInvalid)
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": again.MessageID,
		"subject": "x", "plain_only": true}, gapi.ClassInvalid)
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": again.MessageID,
		"body": "x", "body_html": "<p>x</p>", "plain_only": false}, gapi.ClassInvalid)
}

// The HTML version made again is named right after the body, in the
// order the fields are listed.
func TestTheMadeHTMLIsNamedAfterTheBody(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("file"), 0o600))
	h, _ := connectFake(t, config.Config{LocalDir: dir})
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "One.", "attachments": []any{"a.txt"}}, &made)
	var up tools.DraftWriteOut
	call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "Two.", "remove_attachments": []any{"1"}}, &up)
	if strings.Join(up.Changed, ",") != "body,body_html,attachments" {
		t.Errorf("changed %v, want body,body_html,attachments", up.Changed)
	}
}

// plain_only does not delete HTML written another way.
func TestPlainOnlyKeepsHTMLWrittenByHand(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "plain", "body_html": "<p><b>bold</b></p>"}, &made)
	refused(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"body": "new", "plain_only": true}, gapi.ClassConflict)
}

func TestUpdateDraftWarnsWhenAReplyLosesItsSubject(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	var made tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to": plain.MessageIDs[2], "body": "x"}, &made)
	var up tools.DraftWriteOut
	text := call(t, h, "update_draft", map[string]any{"draft_id": made.DraftID, "message_id": made.MessageID,
		"subject": "Unrelated"}, &up)
	if !up.ThreadingAtRisk || !strings.Contains(text, "may leave its thread") {
		t.Fatalf("no warning:\n%s", text)
	}
}

func TestDeleteDraft(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	args := map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2]}

	refused(t, h, "delete_draft", args, gapi.ClassBlocked)
	var dry tools.DraftWriteOut
	call(t, h, "delete_draft", with(args, "dry_run", true), &dry)
	if !dry.DryRun || dry.Deleted || !slices.Contains(fake.DraftIDs(), sc.DraftID) || dry.MessageID != sc.MessageIDs[2] ||
		string(dry.UntrustedFrom) != gmailtest.Reader.Name+" <"+gmailtest.Account+">" {
		t.Fatalf("dry run %+v", dry)
	}
	var out tools.DraftWriteOut
	text := call(t, h, "delete_draft", with(args, "confirm", true), &out)
	if !out.Deleted || out.Gone || slices.Contains(fake.DraftIDs(), sc.DraftID) || out.Units != 30 {
		t.Fatalf("delete %+v", out)
	}
	if !strings.Contains(text, "permanently") || string(out.UntrustedSubject) != "Re: Budget sign-off" {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "delete_draft", with(args, "confirm", true), gapi.ClassNotFound)
}

// The draft's message_id is the witness, as for an update (§4.4): a
// draft changed since it was read is not deleted, and a dry run says so
// too, so what is deleted is what the caller read.
func TestDeleteDraftNeedsItsWitness(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	args := map[string]any{"draft_id": sc.DraftID, "confirm": true}

	text := refused(t, h, "delete_draft", with(args, "message_id", " "), gapi.ClassInvalid)
	if !strings.Contains(text, "message_id is required") {
		t.Errorf("a blank witness: %s", text)
	}
	for _, dry := range []bool{true, false} {
		text = refused(t, h, "delete_draft", with(with(args, "message_id", sc.MessageIDs[1]), "dry_run", dry), gapi.ClassStale)
		if !strings.Contains(text, "holds message "+sc.MessageIDs[2]+" now, not "+sc.MessageIDs[1]) {
			t.Errorf("dry run %t: %s", dry, text)
		}
	}
	if !slices.Contains(fake.DraftIDs(), sc.DraftID) || len(fake.CallsOf("gmail.users.drafts.delete")) != 0 {
		t.Fatal("a draft was deleted against a stale witness")
	}
	call(t, h, "delete_draft", with(args, "message_id", " "+sc.MessageIDs[2]+" "), &tools.DraftWriteOut{})
	if slices.Contains(fake.DraftIDs(), sc.DraftID) {
		t.Error("the witness, with spaces around it, did not delete the draft")
	}
}

func TestDeleteDraftGoneBetweenReadAndDelete(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.drafts.delete", Status: 404, Reason: "notFound"})
	var out tools.DraftWriteOut
	text := call(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2], "confirm": true}, &out)
	if !out.Gone || !out.Deleted || !strings.Contains(text, "is gone") {
		t.Fatalf("out %+v\n%s", out, text)
	}
}

func TestModifyLabels(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	draft := fake.Scenario(gmailtest.ScenarioDraftReply)

	var out tools.ItemsOut
	text := call(t, h, "modify_labels", map[string]any{
		"message_ids": []any{plain.MessageIDs[0], plain.MessageIDs[2], draft.MessageIDs[2], "00000000000fffff"},
		"add":         []any{"starred"}, "remove": []any{"INBOX"},
	}, &out)
	if strings.Join(out.Verbs, ",") != "archive,star" || len(out.Items) != 4 {
		t.Fatalf("verbs %v items %+v", out.Verbs, out.Items)
	}
	want := []string{"changed", "changed", "failed", "failed"}
	for i, it := range out.Items {
		if it.Outcome != want[i] {
			t.Errorf("item %d: %s (%s); want %s", i, it.Outcome, it.Error, want[i])
		}
	}
	if !strings.HasPrefix(out.Items[2].Error, "[unsupported]") || !strings.HasPrefix(out.Items[3].Error, "[not_found]") {
		t.Errorf("errors %q %q", out.Items[2].Error, out.Items[3].Error)
	}
	if after := labelIDs(out.Items[1].LabelsAfter); !slices.Contains(after, "STARRED") || slices.Contains(after, "INBOX") {
		t.Errorf("after %v", after)
	}
	if out.Changed != 2 || out.Failed != 2 || !strings.Contains(text, "2 changed · 0 unchanged · 2 failed") {
		t.Errorf("counts:\n%s", text)
	}

	// Again: both are already as asked, and nothing is written.
	fake.ResetAccounting()
	var again tools.ItemsOut
	call(t, h, "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[0], plain.MessageIDs[2]},
		"add": []any{"STARRED"}, "remove": []any{"INBOX"}}, &again)
	if again.Unchanged != 2 || writeCalls(fake) != 0 || again.Units != 41 {
		t.Fatalf("again %+v, %d writes", again, writeCalls(fake))
	}

	var th tools.ItemsOut
	call(t, h, "modify_labels", map[string]any{"thread_ids": []any{plain.ThreadID}, "add": []any{"Projects"}}, &th)
	if th.Items[0].Kind != "thread" || th.Items[0].Outcome != "changed" || !slices.Contains(labelIDs(th.Items[0].LabelsAfter), "Label_1") {
		t.Fatalf("thread %+v", th.Items)
	}
	// The labels after come from Gmail's answer to the write, not a
	// read again: labels.list 1, threads.get 40, threads.modify 10.
	if th.Units != 51 {
		t.Errorf("thread write spent %d units; want 51", th.Units)
	}

	var dry tools.ItemsOut
	fake.ResetAccounting()
	call(t, h, "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[1]}, "add": []any{"UNREAD"}, "dry_run": true}, &dry)
	if !dry.DryRun || dry.Items[0].Outcome != "would_change" || writeCalls(fake) != 0 {
		t.Fatalf("dry %+v", dry)
	}

	many := make([]any, 101)
	for i := range many {
		many[i] = strings.Repeat("0", 15) + string(rune('a'+i%6)) + strings.Repeat("x", i/6)
	}
	for _, args := range []map[string]any{
		{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"SENT"}},
		{"message_ids": []any{plain.MessageIDs[0]}, "remove": []any{"DRAFT"}},
	} {
		refused(t, h, "modify_labels", args, gapi.ClassConflict)
	}
	for _, args := range []map[string]any{
		{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"TRASH"}},
		{"message_ids": []any{plain.MessageIDs[0]}},
		{"add": []any{"STARRED"}},
		{"message_ids": many, "add": []any{"STARRED"}},
		{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"STARRED"}, "remove": []any{"starred"}},
	} {
		refused(t, h, "modify_labels", args, gapi.ClassInvalid)
	}
	refused(t, h, "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[0]}, "add": []any{"No such label"}}, gapi.ClassNotFound)
}

func TestTrashAndRestore(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	trashed := fake.Scenario(gmailtest.ScenarioTrash)
	draft := fake.Scenario(gmailtest.ScenarioDraftReply)

	var out tools.ItemsOut
	text := call(t, h, "trash", map[string]any{"message_ids": []any{plain.MessageIDs[0], trashed.MessageIDs[0], draft.MessageIDs[2]}}, &out)
	outcomes := []string{out.Items[0].Outcome, out.Items[1].Outcome, out.Items[2].Outcome}
	if strings.Join(outcomes, ",") != "changed,unchanged,failed" || !slices.Contains(labelIDs(out.Items[0].LabelsAfter), "TRASH") {
		t.Fatalf("items %+v", out.Items)
	}
	if !strings.Contains(text, "unchanged, already in the trash") || !strings.Contains(out.Items[2].Error, "delete_draft") {
		t.Errorf("text:\n%s", text)
	}
	if out.Units != 1+40+20+20 {
		t.Errorf("units %d", out.Units)
	}

	var back tools.ItemsOut
	text = call(t, h, "restore", map[string]any{"message_ids": []any{plain.MessageIDs[0], plain.MessageIDs[1]}}, &back)
	if back.Items[0].Outcome != "changed" || back.Items[1].Outcome != "unchanged" || !strings.Contains(text, "not in the trash") {
		t.Fatalf("restore %+v\n%s", back.Items, text)
	}

	var th tools.ItemsOut
	call(t, h, "trash", map[string]any{"thread_ids": []any{plain.ThreadID}}, &th)
	if th.Items[0].Outcome != "changed" {
		t.Fatalf("thread trash %+v", th.Items)
	}
	var thBack tools.ItemsOut
	call(t, h, "restore", map[string]any{"thread_ids": []any{plain.ThreadID}, "dry_run": true}, &thBack)
	if thBack.Items[0].Outcome != "would_change" {
		t.Fatalf("thread restore dry run %+v", thBack.Items)
	}
}

func TestCreateAndUpdateLabel(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var made tools.LabelWriteOut
	text := call(t, h, "create_label", map[string]any{"name": "Travel", "in_label_list": "show_if_unread",
		"text_color": "#FFFFFF", "background_color": "#16a766"}, &made)
	if made.Label.ID == "" || made.Label.InLabelList != "show_if_unread" || made.Label.TextColor != "#ffffff" || made.Units != 6 {
		t.Fatalf("created %+v", made)
	}
	if !strings.Contains(text, "created label Travel · id "+made.Label.ID) {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "create_label", map[string]any{"name": "travel"}, gapi.ClassConflict)
	refused(t, h, "create_label", map[string]any{"name": "inbox"}, gapi.ClassInvalid)
	refused(t, h, "create_label", map[string]any{"name": "x", "in_label_list": "sometimes"}, gapi.ClassInvalid)
	refused(t, h, "create_label", map[string]any{"name": "x", "text_color": "#fff"}, gapi.ClassInvalid)

	var dry tools.LabelWriteOut
	call(t, h, "create_label", map[string]any{"name": "Later", "dry_run": true}, &dry)
	if !dry.DryRun || dry.Label.ID != "" {
		t.Fatalf("dry %+v", dry)
	}

	var up tools.LabelWriteOut
	text = call(t, h, "update_label", map[string]any{"label": "Travel", "name": "Trips", "in_message_list": "hide"}, &up)
	if up.Before == nil || up.Before.Name != "Travel" || up.Label.Name != "Trips" || up.Label.InMessageList != "hide" ||
		up.Label.InLabelList != "show_if_unread" || strings.Join(up.Changed, ",") != "name,in_message_list" {
		t.Fatalf("updated %+v", up)
	}
	if !strings.Contains(text, "before: Travel") || !strings.Contains(text, "after:  Trips") {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "update_label", map[string]any{"label": "INBOX", "name": "Mine"}, gapi.ClassInvalid)
	refused(t, h, "update_label", map[string]any{"label": "Trips"}, gapi.ClassInvalid)
	refused(t, h, "update_label", map[string]any{"label": "Trips", "name": "Receipts"}, gapi.ClassConflict)
	refused(t, h, "update_label", map[string]any{"label": "Nothing"}, gapi.ClassInvalid)
	refused(t, h, "update_label", map[string]any{"label": "Nothing", "name": "x"}, gapi.ClassNotFound)

	// A dry run answers what Gmail would: a taken name is refused, and
	// the label shown is the label as it would be.
	refused(t, h, "create_label", map[string]any{"name": "trips", "dry_run": true}, gapi.ClassConflict)
	refused(t, h, "update_label", map[string]any{"label": "Trips", "name": "receipts", "dry_run": true}, gapi.ClassConflict)
	var preview tools.LabelWriteOut
	call(t, h, "update_label", map[string]any{"label": "Trips", "name": "Journeys", "in_label_list": "hide",
		"in_message_list": "show", "text_color": "#000000", "background_color": "#fad165", "dry_run": true}, &preview)
	wantPreview := tools.LabelLook{ID: up.Label.ID, Name: "Journeys", Type: "user", InLabelList: "hide", InMessageList: "show",
		TextColor: "#000000", BackgroundColor: "#fad165"}
	if !preview.DryRun || preview.Label != wantPreview || len(fake.CallsOf("gmail.users.labels.patch")) != 1 {
		t.Fatalf("dry run label %+v; want %+v and no patch past the first", preview.Label, wantPreview)
	}
}

// A label name is 1 to 225 characters, counted in runes.
func TestLabelNameLength(t *testing.T) {
	h, _ := connectFake(t, config.Config{})
	var dry tools.LabelWriteOut
	call(t, h, "create_label", map[string]any{"name": strings.Repeat("ä", 225), "dry_run": true}, &dry)
	refused(t, h, "create_label", map[string]any{"name": strings.Repeat("ä", 226), "dry_run": true}, gapi.ClassInvalid)
}

// A dry run reports the labels each item would have, and the write that
// follows leaves exactly those.
func TestDryRunPredictsLabelsAfter(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	reply := fake.Scenario(gmailtest.ScenarioDraftReply)

	for _, c := range []struct {
		name, tool string
		args       map[string]any
		want       []string
	}{
		{"archive a message left with no labels", "modify_labels", map[string]any{"message_ids": []any{reply.MessageIDs[0]},
			"remove": []any{"INBOX"}}, []string{}},
		{"modify a message", "modify_labels", map[string]any{"message_ids": []any{plain.MessageIDs[0]},
			"add": []any{"STARRED"}, "remove": []any{"INBOX"}}, []string{"IMPORTANT", "Label_2", "STARRED"}},
		{"modify a thread with a draft", "modify_labels", map[string]any{"thread_ids": []any{reply.ThreadID},
			"add": []any{"STARRED"}}, []string{"DRAFT", "SENT", "STARRED"}},
		{"trash a message, which leaves the inbox", "trash", map[string]any{"message_ids": []any{plain.MessageIDs[2]}},
			[]string{"Label_2", "TRASH", "UNREAD"}},
		{"restore it, which does not return it to the inbox", "restore", map[string]any{"message_ids": []any{plain.MessageIDs[2]}},
			[]string{"Label_2", "UNREAD"}},
	} {
		dryArgs := maps.Clone(c.args)
		dryArgs["dry_run"] = true
		var dry, real tools.ItemsOut
		text := call(t, h, c.tool, dryArgs, &dry)
		if len(dry.Items) != 1 {
			t.Fatalf("%s: dry run gave %d items", c.name, len(dry.Items))
		}
		if dry.Items[0].Outcome != "would_change" || !slices.Equal(labelIDs(dry.Items[0].LabelsAfter), c.want) {
			t.Errorf("%s: dry run %+v", c.name, dry.Items[0])
		}
		if !strings.Contains(text, "after: ") {
			t.Errorf("%s: dry-run text does not say the labels after:\n%s", c.name, text)
		}
		call(t, h, c.tool, c.args, &real)
		if len(real.Items) != 1 {
			t.Fatalf("%s: the write gave %d items", c.name, len(real.Items))
		}
		if got := slices.Sorted(slices.Values(labelIDs(real.Items[0].LabelsAfter))); !slices.Equal(got, c.want) {
			t.Errorf("%s: the write left %v, the dry run said %v", c.name, got, c.want)
		}
	}
}

// raeEnding is the fake account's own signature below a body, as Gmail's
// composer writes one that ends a message.
const raeEnding = `<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br>` +
	`<div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><div>Rae Reader<br>Example Org</div></div>`

// stored is a stored message's plain text and HTML version, read by the
// standard library rather than by internal/mime.
func stored(t *testing.T, fake *gmailtest.Server, messageID string) (text, html string) {
	t.Helper()
	raw, ok := fake.Raw(messageID)
	if !ok {
		t.Fatalf("no stored message %s", messageID)
	}
	text, _ = gmailtest.PlainPart(raw)
	html, _ = gmailtest.HTMLPart(raw)
	return text, html
}

// setSignature sets a send-as address's signature, as a person would in
// Gmail's settings: the account's own is the first, the alias the second.
func setSignature(fake *gmailtest.Server, i int, html string) {
	fake.UpdateSettings(func(s *gmailtest.Settings) { s.SendAs[i].Signature = html })
}

// signature puts the signature Gmail holds for the From address below
// the body, as Gmail's composer writes one, for no read beyond the
// sendAs.list create_draft makes anyway; without it a draft is as it was.
func TestCreateDraftSignsWhenAsked(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	const signed = "Hello.\n\n-- \nRae Reader\nExample Org\n"
	added := &tools.DraftSignatureOut{Outcome: "added", Lines: 2}
	cases := []struct {
		name        string
		args        map[string]any
		html, plain string
		sig         *tools.DraftSignatureOut
	}{
		{"not asked", map[string]any{"body": "Hello."}, "<p>Hello.</p>\n", "Hello.", nil},
		{"the default address's", map[string]any{"body": "Hello.", "signature": true}, "<p>Hello.</p>\n" + raeEnding, signed, added},
		{"an address with none", map[string]any{"body": "Hello.", "signature": true, "from": gmailtest.AliasAddress},
			"<p>Hello.</p>\n", "Hello.", &tools.DraftSignatureOut{Outcome: "none_set"}},
		{"in body_html, before its close", map[string]any{"body": "Hello.", "signature": true,
			"body_html": "<html><body><p>Hello.</p></body></html>"},
			"<html><body><p>Hello.</p>" + raeEnding + "</body></html>", signed, added},
	}
	for _, c := range cases {
		fake.ResetAccounting()
		var out tools.DraftWriteOut
		call(t, h, "create_draft", c.args, &out)
		if text, html := stored(t, fake, out.MessageID); text != c.plain || html != c.html {
			t.Errorf("%s: stored %q and %q, want %q and %q", c.name, text, html, c.plain, c.html)
		}
		if !reflect.DeepEqual(out.Signature, c.sig) {
			t.Errorf("%s: signature %+v, want %+v", c.name, out.Signature, c.sig)
		}
		if n := len(fake.CallsOf("gmail.users.settings.sendAs.list")); n != 1 || out.Units != 11 {
			t.Errorf("%s: %d sendAs.list reads and %d units, want 1 and 11", c.name, n, out.Units)
		}
	}

	setSignature(fake, 1, "<p>Rae, projects</p>")
	var alias tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "Hello.", "signature": true, "from": gmailtest.AliasAddress}, &alias)
	if got, want := storedHTML(t, fake, alias.MessageID), "<p>Hello.</p>\n"+
		`<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br>`+
		`<div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><p>Rae, projects</p></div>`; got != want {
		t.Errorf("from the alias: stored HTML %q, want %q", got, want)
	}
}

// Above a quote the signature carries no delimiter, as Gmail writes it
// there, so a reader that drops a signature does not drop the quote.
func TestCreateDraftSignsAboveTheQuote(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	parentID := fake.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[2]
	var out tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"reply_to": parentID, "body": "Thanks.", "quote": true, "signature": true}, &out)
	text, html := stored(t, fake, out.MessageID)
	if !strings.HasPrefix(text, "Thanks.\n\nRae Reader\nExample Org\n\nOn ") || strings.Contains(text, "\n-- \n") {
		t.Errorf("stored text %q, want the body, the signature without a delimiter, then the quote", text)
	}
	if want := "<p>Thanks.</p>\n" + `<div><br clear="all"></div><div>` +
		`<div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><div>Rae Reader<br>Example Org</div></div></div><p>On `; !strings.HasPrefix(html, want) {
		t.Errorf("stored HTML %q, want it to start %q", html, want)
	}
}

// A new body keeps the signature the draft carries; signature: false
// takes it out, true puts the address's current one there for one
// sendAs.list, and plain_only takes it with the HTML version.
func TestUpdateDraftCarriesTheSignature(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var d tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"a@example.com"}, "body": "Hello.", "signature": true}, &d)
	update := func(args map[string]any) tools.DraftWriteOut {
		t.Helper()
		args["draft_id"], args["message_id"] = d.DraftID, d.MessageID
		fake.ResetAccounting()
		var out tools.DraftWriteOut
		call(t, h, "update_draft", args, &out)
		if !out.DryRun {
			d = out
		}
		return out
	}
	steps := []struct {
		name          string
		args          map[string]any
		html, plain   string
		sig           *tools.DraftSignatureOut
		changed       string
		units, sendAs int
	}{
		{"a new body keeps it", map[string]any{"body": "Revised."}, "<p>Revised.</p>\n" + raeEnding,
			"Revised.\n\n-- \nRae Reader\nExample Org\n", &tools.DraftSignatureOut{Outcome: "kept", Lines: 2}, "body,body_html", 35, 0},
		{"false takes it out", map[string]any{"body": "Plain.", "signature": false}, "<p>Plain.</p>\n", "Plain.",
			&tools.DraftSignatureOut{Outcome: "removed"}, "body,body_html,signature", 35, 0},
		{"a dry run saves nothing", map[string]any{"body": "Dry.", "signature": true, "dry_run": true}, "<p>Plain.</p>\n", "Plain.",
			&tools.DraftSignatureOut{Outcome: "added", Lines: 2}, "body,body_html,signature", 21, 1},
		{"true puts it back", map[string]any{"body": "Again.", "signature": true}, "<p>Again.</p>\n" + raeEnding,
			"Again.\n\n-- \nRae Reader\nExample Org\n", &tools.DraftSignatureOut{Outcome: "added", Lines: 2},
			"body,body_html,signature", 36, 1},
		{"plain_only takes it with the HTML", map[string]any{"body": "Bare.", "plain_only": true}, "", "Bare.",
			&tools.DraftSignatureOut{Outcome: "removed"}, "body,body_html,signature", 35, 0},
		{"with HTML, a plain draft takes one", map[string]any{"body": "Back.", "plain_only": false, "signature": true},
			"<p>Back.</p>\n" + raeEnding, "Back.\n\n-- \nRae Reader\nExample Org\n",
			&tools.DraftSignatureOut{Outcome: "added", Lines: 2}, "body,body_html,signature", 36, 1},
	}
	for _, c := range steps {
		out := update(c.args)
		if text, html := stored(t, fake, d.MessageID); text != c.plain || html != c.html {
			t.Errorf("%s: stored %q and %q, want %q and %q", c.name, text, html, c.plain, c.html)
		}
		if !reflect.DeepEqual(out.Signature, c.sig) || strings.Join(out.Changed, ",") != c.changed {
			t.Errorf("%s: signature %+v, changed %v; want %+v, %s", c.name, out.Signature, out.Changed, c.sig, c.changed)
		}
		if n := len(fake.CallsOf("gmail.users.settings.sendAs.list")); n != c.sendAs || out.Units != c.units {
			t.Errorf("%s: %d sendAs.list reads and %d units, want %d and %d", c.name, n, out.Units, c.sendAs, c.units)
		}
	}

	for _, c := range []struct {
		name, gmail string
		want        tools.DraftSignatureOut
		changed     string
	}{
		{"true over another", "<p>Rae</p>", tools.DraftSignatureOut{Outcome: "replaced", Lines: 1}, "body,body_html,signature"},
		{"true over the same", "<p>Rae</p>", tools.DraftSignatureOut{Outcome: "kept", Lines: 1}, "body,body_html"},
		{"true with none set", "", tools.DraftSignatureOut{Outcome: "none_set"}, "body,body_html,signature"},
	} {
		setSignature(fake, 0, c.gmail)
		out := update(map[string]any{"body": "Again.", "signature": true})
		if out.Signature == nil || *out.Signature != c.want || strings.Join(out.Changed, ",") != c.changed {
			t.Errorf("%s: %+v, changed %v; want %+v, %s", c.name, out.Signature, out.Changed, c.want, c.changed)
		}
	}
	setSignature(fake, 0, "<p>Rae</p>")
	update(map[string]any{"body": "Again.", "signature": true})

	var bare, alias tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "List mail.", "plain_only": true}, &bare)
	call(t, h, "create_draft", map[string]any{"body": "From the alias.", "from": gmailtest.AliasAddress}, &alias)
	fake.UpdateSettings(func(s *gmailtest.Settings) { s.SendAs = s.SendAs[:1] })
	for _, c := range []struct {
		name  string
		draft tools.DraftWriteOut
		args  map[string]any
	}{
		{"without a body", d, map[string]any{"subject": "x", "signature": true}},
		{"with plain_only", d, map[string]any{"body": "x", "signature": true, "plain_only": true}},
		{"on a plain draft", bare, map[string]any{"body": "x", "signature": true}},
		{"from an address no longer sent as", alias, map[string]any{"body": "x", "signature": true}},
		{"below one in body_html", d, map[string]any{"body": "x",
			"body_html": `<p>x</p><div class="gmail_signature" data-smartmail="gmail_signature">Rae</div>`}},
	} {
		c.args["draft_id"], c.args["message_id"] = c.draft.DraftID, c.draft.MessageID
		fake.ResetAccounting()
		refused(t, h, "update_draft", c.args, gapi.ClassInvalid)
		if n := writeCalls(fake); n != 0 {
			t.Errorf("%s: %d writes reached Gmail", c.name, n)
		}
	}
}

// The signature goes into the draft and nowhere else: not into a result,
// whose text and structured content say what it shows, and not into a log.
func TestTheSignatureStaysInTheDraft(t *testing.T) {
	var logs bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, fake := connectFake(t, config.Config{}, func(o *gapi.Options) { o.Logger = lg })
	const canary = "CANARY-signature-line"
	setSignature(fake, 0, "<div>"+canary+"</div>")
	results := []*mcp.CallToolResult{h.Call(t, "create_draft", map[string]any{"body": "Hello.", "signature": true})}
	var d tools.DraftWriteOut
	structured(t, results[0], testutil.Text(results[0]), results[0].StructuredContent, &d)
	for _, args := range []map[string]any{{"body": "Kept."}, {"body": "Again.", "signature": true}} {
		args["draft_id"], args["message_id"] = d.DraftID, d.MessageID
		res := h.Call(t, "update_draft", args)
		structured(t, res, testutil.Text(res), res.StructuredContent, &d)
		results = append(results, res)
	}
	if _, got := stored(t, fake, d.MessageID); !strings.Contains(got, canary) {
		t.Fatalf("stored HTML %q does not carry the signature; this test would pass on a server that adds none", got)
	}
	for i, res := range results {
		b, _ := json.Marshal(res.StructuredContent)
		if strings.Contains(testutil.Text(res), canary) || strings.Contains(string(b), canary) {
			t.Errorf("result %d carries the signature:\n%s\n%s", i, testutil.Text(res), b)
		}
	}
	if strings.TrimSpace(logs.String()) == "" {
		t.Fatal("nothing was logged; this test would pass on a client that logs nothing")
	}
	if strings.Contains(logs.String(), canary) {
		t.Errorf("the logs carry the signature:\n%s", linesWith(logs.String(), canary))
	}
}

// A signature's image goes as a part of its own beside the HTML, never
// inside it, since Gmail would move it on save and drop the plain text
// (§18 row 99). A new body keeps the part, and a signature taken out
// takes its part with it.
func TestTheSignaturesImageIsAPartOfItsOwn(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	setSignature(fake, 0, `<div>Rae<img src="data:image/png;base64,UE5HREFUQQ=="></div>`)
	const part = "Content-ID: <sig.2d4566582844690f@signature>"
	check := func(step, messageID string, parts int, html string) {
		t.Helper()
		raw, _ := fake.Raw(messageID)
		text, got := stored(t, fake, messageID)
		if n := strings.Count(string(raw), part); n != parts || got != html || strings.Contains(string(raw), "data:image") {
			t.Errorf("%s: %d image parts, html %q; want %d, %q, and no data URL", step, n, got, parts, html)
		}
		if !strings.HasPrefix(text, "Hello") {
			t.Errorf("%s: plain text %q, want the body", step, text)
		}
	}
	signed := `<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br>` +
		`<div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><div>Rae<img src="cid:sig.2d4566582844690f@signature"></div></div>`
	var d tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "Hello.", "signature": true}, &d)
	check("create_draft", d.MessageID, 1, "<p>Hello.</p>\n"+signed)
	if d.Signature == nil || *d.Signature != (tools.DraftSignatureOut{Outcome: "added", Lines: 1, Images: 1}) {
		t.Errorf("create_draft signature %+v", d.Signature)
	}
	call(t, h, "update_draft", map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID, "body": "Hello again."}, &d)
	check("a new body", d.MessageID, 1, "<p>Hello again.</p>\n"+signed)
	call(t, h, "update_draft", map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID, "body": "Hello again.",
		"signature": true}, &d)
	check("the same signature set again", d.MessageID, 1, "<p>Hello again.</p>\n"+signed)
	if d.Signature == nil || d.Signature.Outcome != "kept" || slices.Contains(d.Changed, "signature") {
		t.Errorf("the same signature set again: %+v, changed %v; want kept, the signature unchanged", d.Signature, d.Changed)
	}
	call(t, h, "update_draft", map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID, "body": "Hello, unsigned.",
		"signature": false}, &d)
	check("signature false", d.MessageID, 0, "<p>Hello, unsigned.</p>\n")
}

// With GMAIL_DRAFT_SIGNATURE on, a new draft is signed when the call
// leaves signature out, and the description says so; a reply or a
// forward is not, and the default never refuses: a body that already
// carries the signature, or plain text alone, goes without it and the
// result says why. true and false still decide.
func TestTheServerSignsNewDraftsWhenSetTo(t *testing.T) {
	h, fake := connectFake(t, config.Config{DraftSignature: true})
	parentID := fake.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[2]
	const signed = "Hello.\n\n-- \nRae Reader\nExample Org\n"
	byDefault := &tools.DraftSignatureOut{Outcome: "added", ByDefault: true, Lines: 2}
	skipped := func(why string) *tools.DraftSignatureOut {
		return &tools.DraftSignatureOut{Outcome: "skipped", ByDefault: true, Reason: why}
	}
	cases := []struct {
		name  string
		args  map[string]any
		plain string
		sig   *tools.DraftSignatureOut
	}{
		{"left out", map[string]any{"body": "Hello."}, signed, byDefault},
		{"false", map[string]any{"body": "Hello.", "signature": false}, "Hello.", nil},
		{"true", map[string]any{"body": "Hello.", "signature": true}, signed, &tools.DraftSignatureOut{Outcome: "added", Lines: 2}},
		{"a reply", map[string]any{"body": "Hello.", "reply_to": parentID}, "Hello.", nil},
		{"a forward", map[string]any{"body": "Hello.", "forward": parentID}, "Hello.", nil},
		{"plain only", map[string]any{"body": "Hello.", "plain_only": true}, "Hello.", skipped("plain_only")},
		{"the body holds it", map[string]any{"body": "Hello.\n\nRae Reader\nExample Org"}, "Hello.\n\nRae Reader\nExample Org",
			skipped("in_body")},
		{"body_html holds one", map[string]any{"body": "Hello.",
			"body_html": `<p>Hello.</p><div data-smartmail="gmail_signature">Rae</div>`}, "Hello.", skipped("in_body_html")},
	}
	for _, c := range cases {
		var out tools.DraftWriteOut
		call(t, h, "create_draft", c.args, &out)
		if text, _ := stored(t, fake, out.MessageID); text != c.plain || !reflect.DeepEqual(out.Signature, c.sig) {
			t.Errorf("%s: stored %q, signature %+v; want %q, %+v", c.name, text, out.Signature, c.plain, c.sig)
		}
	}

	describe := func(h *testutil.Harness) string {
		t.Helper()
		for _, tool := range h.Tools(t) {
			if tool.Name == "create_draft" {
				return tool.Description
			}
		}
		t.Fatal("create_draft is not registered")
		return ""
	}
	off, _ := connectFake(t, config.Config{})
	if on := describe(h); !strings.Contains(on, "signs a new draft") || strings.Contains(describe(off), "signs a new draft") {
		t.Errorf("the description does not follow the setting:\non: %s\noff: %s", on, describe(off))
	}
}

// New body_html would drop a signature the draft's HTML holds and cannot
// carry, which the caller cannot see: refused unless signature says what
// to do (standard §3, never destroy what the caller cannot see).
func TestUpdateDraftWillNotDropASignatureUnsaid(t *testing.T) {
	h, fake := connectFake(t, config.Config{})
	var d tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"body": "Hello.", "body_html": "<p><b>Hello.</b></p>", "signature": true}, &d)
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID, "body": "Again.", "body_html": "<p><b>Again.</b></p>"}
		maps.Copy(a, extra)
		return a
	}
	fake.ResetAccounting()
	refused(t, h, "update_draft", args(nil), gapi.ClassConflict)
	if n := writeCalls(fake); n != 0 {
		t.Errorf("%d writes reached Gmail", n)
	}
	call(t, h, "update_draft", args(map[string]any{"signature": true}), &d)
	if _, html := stored(t, fake, d.MessageID); html != "<p><b>Again.</b></p>"+raeEnding || d.Signature == nil ||
		d.Signature.Outcome != "kept" {
		t.Errorf("signature true: html %q, signature %+v; want Gmail's below it, the same one kept", html, d.Signature)
	}
	call(t, h, "update_draft", args(map[string]any{"signature": false}), &d)
	if _, html := stored(t, fake, d.MessageID); html != "<p><b>Again.</b></p>" || d.Signature == nil ||
		d.Signature.Outcome != "removed" || !slices.Contains(d.Changed, "signature") {
		t.Errorf("signature false: html %q, signature %+v, changed %v; want it removed", html, d.Signature, d.Changed)
	}
}
