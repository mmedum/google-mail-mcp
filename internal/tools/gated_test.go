package tools_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/tools"
)

var (
	sendOn        = config.Config{EnableSend: true}
	destructiveOn = config.Config{EnableDestructive: true}
)

// The gated tools exist only under their flag, and read-only mode wins
// over both (§9.4).
func TestGatedToolsRegisterOnlyWithTheirFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"default", config.Config{}, nil},
		{"send", sendOn, []string{"send_draft"}},
		{"destructive", destructiveOn, []string{"delete_permanently", "delete_label"}},
		{"both", config.Config{EnableSend: true, EnableDestructive: true}, []string{"send_draft", "delete_permanently", "delete_label"}},
		{"read-only wins", config.Config{ReadOnly: true, EnableSend: true, EnableDestructive: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := connectFake(t, tc.cfg)
			var got []string
			for _, tool := range h.Tools(t) {
				switch tool.Name {
				case "send_draft":
					if !*tool.Annotations.OpenWorldHint || *tool.Annotations.DestructiveHint {
						t.Errorf("send_draft annotations %+v", tool.Annotations)
					}
				case "delete_permanently", "delete_label":
					if !*tool.Annotations.DestructiveHint || *tool.Annotations.OpenWorldHint {
						t.Errorf("%s annotations %+v", tool.Name, tool.Annotations)
					}
				default:
					continue
				}
				if tool.Meta["anthropic/requiresUserInteraction"] != true {
					t.Errorf("%s does not ask for a person", tool.Name)
				}
				got = append(got, tool.Name)
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Errorf("gated tools %v; want %v", got, tc.want)
			}
		})
	}
}

func sends(fake *gmailtest.Server) int { return len(callsOf(fake, "gmail.users.drafts.send")) }

func callsOf(fake *gmailtest.Server, method string) []gmailtest.Call {
	var out []gmailtest.Call
	for _, c := range fake.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// A reply to a thread goes to its participants without confirming them.
func TestSendDraftReply(t *testing.T) {
	h, fake := connectFake(t, sendOn)
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	args := map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2]}

	var dry tools.SendDraftOut
	text := call(t, h, "send_draft", with(args, "dry_run", true), &dry)
	if !dry.DryRun || dry.Sent || dry.Answers != 2 || dry.Unconfirmed != 0 || len(dry.Recipients) != 1 ||
		!dry.Recipients[0].Participant || sends(fake) != 0 {
		t.Fatalf("dry run %+v", dry)
	}
	if !strings.Contains(text, "answers a thread of 2 messages") || strings.Contains(outside(text), "freya") {
		t.Errorf("dry run text:\n%s", text)
	}

	refused(t, h, "send_draft", with(args, "message_id", sc.MessageIDs[1]), gapi.ClassStale)

	fake.ResetAccounting()
	var out tools.SendDraftOut
	text = call(t, h, "send_draft", args, &out)
	if !out.Sent || out.SentMessageID == "" || out.SentThreadID != sc.ThreadID || !slices.Contains(labelIDs(out.SentLabels), "SENT") {
		t.Fatalf("send %+v", out)
	}
	if slices.Contains(fake.DraftIDs(), sc.DraftID) || sends(fake) != 1 || out.Units != 20+40+100 {
		t.Fatalf("draft still there, %d sends, %d units", sends(fake), out.Units)
	}
	if !strings.Contains(text, "sent draft "+sc.DraftID) {
		t.Errorf("text:\n%s", text)
	}
	refused(t, h, "send_draft", args, gapi.ClassNotFound)
}

// A new conversation reaches nobody the caller did not write out (§4.2).
func TestSendDraftRecipientGuard(t *testing.T) {
	h, fake := connectFake(t, sendOn)
	var d tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"Ada Quill <ada.quill@example.com>"},
		"cc": []any{"bruno.fennick@example.org"}, "subject": "Plan", "body": "Hello."}, &d)
	args := map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID}

	text := refused(t, h, "send_draft", args, gapi.ClassBlocked)
	if !strings.Contains(text, "2 of draft "+d.DraftID+"'s recipients start a new conversation") ||
		!strings.Contains(text, "to[0], cc[0]") || strings.Contains(text, "example") {
		t.Errorf("refusal: %s", text)
	}
	var dry tools.SendDraftOut
	text = call(t, h, "send_draft", with(args, "dry_run", true), &dry)
	if dry.Unconfirmed != 2 || dry.Answers != 0 || !strings.Contains(text, "not confirmed: to[0], cc[0]") {
		t.Fatalf("dry %+v\n%s", dry, text)
	}

	refused(t, h, "send_draft", with(args, "confirm_recipients", []any{"ada.quill@example.com"}), gapi.ClassBlocked)
	text = refused(t, h, "send_draft", with(args, "confirm_recipients",
		[]any{"ada.quill@example.com", "bruno.fennick@example.org", "chiara@example.com"}), gapi.ClassInvalid)
	if !strings.Contains(text, "does not send to") {
		t.Errorf("an extra confirmation: %s", text)
	}
	refused(t, h, "send_draft", with(args, "confirm_recipients", []any{"not an address"}), gapi.ClassInvalid)
	if sends(fake) != 0 {
		t.Fatalf("%d sends before the guard cleared", sends(fake))
	}

	var out tools.SendDraftOut
	call(t, h, "send_draft", with(args, "confirm_recipients", []any{"Ada.Quill@example.com", "Bruno <bruno.fennick@example.org>"}), &out)
	if !out.Sent || out.Unconfirmed != 0 || !out.Recipients[1].Confirmed || out.Recipients[1].Position != 0 || sends(fake) != 1 {
		t.Fatalf("send %+v", out)
	}
}

func TestSendDraftRefusesWhatCannotGo(t *testing.T) {
	h, fake := connectFake(t, sendOn)
	var empty tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"subject": "No one", "body": "x"}, &empty)
	refused(t, h, "send_draft", map[string]any{"draft_id": empty.DraftID, "message_id": empty.MessageID}, gapi.ClassInvalid)

	many := make([]any, 51)
	for i := range many {
		many[i] = "r" + strings.Repeat("x", i) + "@example.com"
	}
	var crowd tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": many, "body": "x"}, &crowd)
	text := refused(t, h, "send_draft", map[string]any{"draft_id": crowd.DraftID, "message_id": crowd.MessageID,
		"dry_run": true, "confirm_recipients": many}, gapi.ClassBlocked)
	if !strings.Contains(text, "51 recipients, over the 50") {
		t.Errorf("refusal: %s", text)
	}
	refused(t, h, "send_draft", map[string]any{"draft_id": crowd.DraftID, "message_id": " "}, gapi.ClassInvalid)
	refused(t, h, "send_draft", map[string]any{"draft_id": "", "message_id": crowd.MessageID}, gapi.ClassInvalid)
	if sends(fake) != 0 {
		t.Fatalf("%d sends", sends(fake))
	}
}

// A send Gmail does not confirm is never repeated, and the reads after
// it say what happened (§4.3).
func TestSendDraftAmbiguousOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures []gmailtest.Failure
		verdict  string
		gone     bool
	}{
		{"sent, answer lost", []gmailtest.Failure{{Method: "gmail.users.drafts.send", Status: 500, Reason: "backendError", Served: true}},
			"verdict: sent", true},
		{"reset after acting", []gmailtest.Failure{{Method: "gmail.users.drafts.send", Reset: true, Served: true}},
			"verdict: sent", true},
		{"reset before acting", []gmailtest.Failure{{Method: "gmail.users.drafts.send", Reset: true}},
			"verdict: not_sent", false},
		{"the thread read fails", []gmailtest.Failure{{Method: "gmail.users.drafts.send", Status: 502, Served: true,
			Then: func(s *gmailtest.Server) {
				s.Fail(gmailtest.Failure{Method: "gmail.users.threads.get", Status: 400, Reason: "invalidArgument"})
			}}}, "verdict: unknown", true},
		{"the draft read fails", []gmailtest.Failure{{Method: "gmail.users.drafts.send", Status: 500,
			Then: func(s *gmailtest.Server) {
				s.Fail(gmailtest.Failure{Method: "gmail.users.drafts.get", Status: 403, Reason: "forbidden"})
			}}}, "verdict: unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := connectFake(t, sendOn)
			sc := fake.Scenario(gmailtest.ScenarioDraftReply)
			args := map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2]}
			// The failures meet the send and the reads after it, not the
			// reads before.
			call(t, h, "send_draft", with(args, "dry_run", true), &tools.SendDraftOut{})
			for _, f := range tc.failures {
				fake.Fail(f)
			}
			text := refused(t, h, "send_draft", args, gapi.ClassAmbiguousOutcome)
			if !strings.Contains(text, tc.verdict) || !strings.Contains(text, "was not repeated") {
				t.Errorf("text: %s", text)
			}
			if sends(fake) != 1 {
				t.Errorf("%d sends; a send is never repeated", sends(fake))
			}
			if gone := !slices.Contains(fake.DraftIDs(), sc.DraftID); gone != tc.gone {
				t.Errorf("draft gone %v; want %v", gone, tc.gone)
			}
		})
	}
}

func TestSendDraftSendingLimit(t *testing.T) {
	h, fake := connectFake(t, sendOn)
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.drafts.send", Status: 429, Times: 5})
	text := refused(t, h, "send_draft", map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2]}, gapi.ClassRateLimited)
	if !strings.Contains(text, "sending limit") || sends(fake) != 1 {
		t.Errorf("%d sends: %s", sends(fake), text)
	}
}

func TestDeletePermanently(t *testing.T) {
	h, fake := connectFake(t, destructiveOn)
	fake.FullScope = true
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	long := fake.Scenario(gmailtest.ScenarioLongThread)
	draft := fake.Scenario(gmailtest.ScenarioDraftReply)
	args := map[string]any{"message_ids": []any{plain.MessageIDs[0], draft.MessageIDs[2], "00000000000fffff"},
		"thread_ids": []any{long.ThreadID}}

	refused(t, h, "delete_permanently", args, gapi.ClassBlocked)
	var dry tools.ItemsOut
	text := call(t, h, "delete_permanently", with(args, "dry_run", true), &dry)
	if dry.Items[0].Outcome != "would_change" || len(callsOf(fake, "gmail.users.messages.delete")) != 0 ||
		!strings.Contains(text, "2 would be deleted for good · 2 failed") {
		t.Fatalf("dry run %+v\n%s", dry.Items, text)
	}

	fake.ResetAccounting()
	var out tools.ItemsOut
	text = call(t, h, "delete_permanently", with(args, "confirm", true), &out)
	want := []string{"changed", "failed", "failed", "changed"}
	for i, it := range out.Items {
		if it.Outcome != want[i] {
			t.Errorf("item %d: %s %s; want %s", i, it.Outcome, it.Error, want[i])
		}
	}
	if !strings.Contains(out.Items[1].Error, "delete_draft") || !strings.HasPrefix(out.Items[2].Error, "[not_found]") {
		t.Errorf("errors %q %q", out.Items[1].Error, out.Items[2].Error)
	}
	if _, ok := fake.Message(plain.MessageIDs[0], "minimal"); ok {
		t.Error("the message is still there")
	}
	if _, ok := fake.Message(long.MessageIDs[0], "minimal"); ok {
		t.Error("the thread's first message is still there")
	}
	if !strings.Contains(text, "deleted for good · labels it had:") || out.Units != 1+30+20+20+60 {
		t.Errorf("%d units:\n%s", out.Units, text)
	}
}

// Without https://mail.google.com/, Gmail refuses; each item says to
// log in again (spike G).
func TestDeletePermanentlyNeedsTheFullScope(t *testing.T) {
	h, fake := connectFake(t, destructiveOn)
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	var out tools.ItemsOut
	call(t, h, "delete_permanently", map[string]any{"message_ids": []any{plain.MessageIDs[1]}, "confirm": true}, &out)
	if out.Failed != 1 || !strings.HasPrefix(out.Items[0].Error, "[auth]") || !strings.Contains(out.Items[0].Error, "login") {
		t.Fatalf("items %+v", out.Items)
	}
	if _, ok := fake.Message(plain.MessageIDs[1], "minimal"); !ok {
		t.Error("the message is gone")
	}
}

func TestDeletePermanentlyGoneBetweenReadAndDelete(t *testing.T) {
	h, fake := connectFake(t, destructiveOn)
	fake.FullScope = true
	plain := fake.Scenario(gmailtest.ScenarioPlainThread)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.messages.delete", Status: 404, Reason: "notFound"})
	var out tools.ItemsOut
	call(t, h, "delete_permanently", map[string]any{"message_ids": []any{plain.MessageIDs[1]}, "confirm": true}, &out)
	if out.Changed != 1 {
		t.Fatalf("items %+v", out.Items)
	}
}

func TestDeleteLabel(t *testing.T) {
	h, fake := connectFake(t, destructiveOn)
	refused(t, h, "delete_label", map[string]any{"label": "Projects"}, gapi.ClassBlocked)
	refused(t, h, "delete_label", map[string]any{"label": "INBOX", "confirm": true}, gapi.ClassInvalid)
	refused(t, h, "delete_label", map[string]any{"label": "No such label", "confirm": true}, gapi.ClassNotFound)

	var dry tools.LabelDeleteOut
	text := call(t, h, "delete_label", map[string]any{"label": "projects", "dry_run": true}, &dry)
	if !dry.DryRun || dry.Deleted || dry.Label.ID != "Label_1" || dry.Messages == 0 || !strings.Contains(text, "would delete label Projects") {
		t.Fatalf("dry %+v\n%s", dry, text)
	}
	var out tools.LabelDeleteOut
	text = call(t, h, "delete_label", map[string]any{"label": "Projects", "confirm": true}, &out)
	if !out.Deleted || out.Gone || out.Messages != dry.Messages || out.Units != 7 {
		t.Fatalf("delete %+v", out)
	}
	for _, l := range fake.Labels() {
		if l.ID == "Label_1" {
			t.Fatal("the label is still there")
		}
	}
	if !strings.Contains(text, "The mail itself is untouched.") {
		t.Errorf("text:\n%s", text)
	}
}

func TestDeleteLabelGoneBetweenReadAndDelete(t *testing.T) {
	h, fake := connectFake(t, destructiveOn)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.labels.delete", Status: 404, Reason: "notFound"})
	var out tools.LabelDeleteOut
	text := call(t, h, "delete_label", map[string]any{"label": "Projects", "confirm": true}, &out)
	if !out.Gone || !strings.Contains(text, "is gone") {
		t.Fatalf("out %+v\n%s", out, text)
	}
}

// with is args with one more key.
func with(args map[string]any, key string, v any) map[string]any {
	out := maps.Clone(args)
	out[key] = v
	return out
}

// rawDraft saves a draft of raw bytes, as another client would.
func rawDraft(t *testing.T, fake *gmailtest.Server, raw string) (draftID, messageID string) {
	t.Helper()
	c := gapi.New(gapi.Options{BaseURL: fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"})})
	d, err := c.CreateDraft(context.Background(), "", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return d.ID, d.Message.ID
}

// A recipient header the server cannot read whole would let Gmail send
// to addresses the guard never saw.
func TestSendDraftRefusesRecipientsItCannotReadWhole(t *testing.T) {
	h, fake := connectFake(t, sendOn)
	for name, headers := range map[string]string{
		"repeated": "To: ada.quill@example.com\r\nCc: bruno.fennick@example.org\r\nCc: chiara@example.com\r\n",
		"lenient":  "To: ada.quill@example.com, \"odd\" <bruno@example.org\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			id, msg := rawDraft(t, fake, "From: reader@example.com\r\n"+headers+"Subject: x\r\n\r\nbody\r\n")
			for _, dry := range []bool{true, false} {
				refused(t, h, "send_draft", map[string]any{"draft_id": id, "message_id": msg, "dry_run": dry,
					"confirm_recipients": []any{"ada.quill@example.com"}}, gapi.ClassBlocked)
			}
		})
	}
	if sends(fake) != 0 {
		t.Fatalf("%d sends", sends(fake))
	}
}

// A send refused as unavailable may have gone out; it is settled, not
// retried, and the witness is read without the spaces around it.
func TestSendDraftUnavailableIsSettled(t *testing.T) {
	h, fake := connectFake(t, sendOn)
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	fake.Fail(gmailtest.Failure{Method: "gmail.users.drafts.send", Status: 503, Reason: "backendError", Served: true, Times: 5})
	text := refused(t, h, "send_draft", map[string]any{"draft_id": sc.DraftID, "message_id": " " + sc.MessageIDs[2] + " "},
		gapi.ClassAmbiguousOutcome)
	if !strings.Contains(text, "verdict: sent") || sends(fake) != 1 {
		t.Errorf("%d sends: %s", sends(fake), text)
	}
}

func TestDeletePermanentlyRefusesAThreadHoldingADraft(t *testing.T) {
	h, fake := connectFake(t, destructiveOn)
	fake.FullScope = true
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	var out tools.ItemsOut
	call(t, h, "delete_permanently", map[string]any{"thread_ids": []any{sc.ThreadID}, "confirm": true}, &out)
	if out.Failed != 1 || !strings.Contains(out.Items[0].Error, "delete_draft") || !slices.Contains(fake.DraftIDs(), sc.DraftID) {
		t.Fatalf("items %+v", out.Items)
	}
}
