package tools_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/server"
	"github.com/mmedum/google-mail-mcp/v2/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v2/internal/tools"
)

// The protocols a question goes out on: before 2026-07-28 the SDK asks
// with elicitation/create inside the call; from it, the call returns the
// question and comes back with the answer (§4.13).
var protocols = []string{"2025-06-18", "2025-11-25", "2026-07-28"}

// everything registers every tool, with the scopes each needs granted.
var everything = config.Config{EnableSend: true, EnableDestructive: true, EnableSettings: true}

// person answers the questions a test client is asked, and keeps them.
type person struct {
	mu        sync.Mutex
	questions []*mcp.ElicitParams
	answer    func() (*mcp.ElicitResult, error)
}

func (p *person) handle(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	p.mu.Lock()
	p.questions = append(p.questions, req.Params)
	p.mu.Unlock()
	return p.answer()
}

func (p *person) asked() []*mcp.ElicitParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.questions)
}

func says(action string, confirm any) func() (*mcp.ElicitResult, error) {
	return func() (*mcp.ElicitResult, error) {
		r := &mcp.ElicitResult{Action: action}
		if confirm != nil {
			r.Content = map[string]any{"confirm": confirm}
		}
		return r, nil
	}
}

var (
	accepts  = says("accept", nil)
	declines = says("decline", nil)
)

// connectAsking connects a client that declares form elicitation and
// answers with p, on protocol. opts adjust the client further.
func connectAsking(t *testing.T, cfg config.Config, protocol string, p *person, opts ...func(*mcp.ClientOptions)) (*testutil.Harness, *gmailtest.Server) {
	t.Helper()
	srv, fake := askingServer(t, cfg)
	return connectTo(t, srv, protocol, p, opts...), fake
}

// askingServer is a server over a fake mailbox with every scope granted.
func askingServer(t *testing.T, cfg config.Config) (*mcp.Server, *gmailtest.Server) {
	t.Helper()
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	fake.FullScope, fake.SettingsScope = true, true
	client := gapi.New(gapi.Options{
		BaseURL:     fake.URL(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	})
	return server.New(server.Deps{Deps: tools.Deps{Config: cfg, Client: client}, Version: "test"}), fake
}

// connectTo connects one more client to srv, as connectAsking does.
func connectTo(t *testing.T, srv *mcp.Server, protocol string, p *person, opts ...func(*mcp.ClientOptions)) *testutil.Harness {
	t.Helper()
	o := &mcp.ClientOptions{}
	if p != nil {
		o.ElicitationHandler = p.handle
	}
	for _, fn := range opts {
		fn(o)
	}
	h, err := testutil.ConnectClient(context.Background(), srv, o, protocol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// confirmCase is a call that clears a tool's own guards and reaches its
// write, the write's method, and words its question must carry.
type confirmCase struct {
	args  func(fake *gmailtest.Server) map[string]any
	write string
	shows []string
}

var confirmCases = map[string]confirmCase{
	"delete_label": {
		args:  func(*gmailtest.Server) map[string]any { return map[string]any{"label": "Projects", "confirm": true} },
		write: "gmail.users.labels.delete",
		shows: []string{"the label `Projects` for good", "messages in"},
	},
	"delete_permanently": {
		args: func(f *gmailtest.Server) map[string]any {
			return map[string]any{"message_ids": []any{f.Scenario(gmailtest.ScenarioPlainThread).MessageIDs[0]},
				"thread_ids": []any{f.Scenario(gmailtest.ScenarioLongThread).ThreadID, f.Scenario(gmailtest.ScenarioNewsletter).ThreadID},
				"confirm":    true}
		},
		write: "gmail.users.messages.delete",
		shows: []string{"delete 1 message and 2 threads, every message in them, for good"},
	},
	"delete_draft": {
		args: func(f *gmailtest.Server) map[string]any {
			return map[string]any{"draft_id": f.Scenario(gmailtest.ScenarioDraftReply).DraftID, "confirm": true}
		},
		write: "gmail.users.drafts.delete",
		shows: []string{"subject: `Re: Budget sign-off`", "to: "},
	},
	"delete_filter": {
		args: func(*gmailtest.Server) map[string]any {
			return map[string]any{"filter_id": gmailtest.FilterNewsletters, "confirm": true}
		},
		write: "gmail.users.settings.filters.delete",
		shows: []string{"delete this filter for good", "matches: "},
	},
	"create_filter": {
		args: func(*gmailtest.Server) map[string]any {
			return map[string]any{"from": "noise@example.org", "trash": true, "confirm": true}
		},
		write: "gmail.users.settings.filters.create",
		shows: []string{"matches: from `noise@example.org`", "to the trash"},
	},
	"set_vacation": {
		args: func(*gmailtest.Server) map[string]any {
			return map[string]any{"enable": true, "subject": "Away", "body": "Back on Monday.", "audience": "contacts",
				"start": "2026-10-01T09:00:00Z", "end": "2026-10-05T17:00:00Z", "confirm": true}
		},
		write: "gmail.users.settings.updateVacation",
		shows: []string{"to contacts only", "from 2026-10-01 09:00 UTC until 2026-10-05 17:00 UTC", "subject: `Away`"},
	},
	"send_draft": {
		args: func(f *gmailtest.Server) map[string]any {
			sc := f.Scenario(gmailtest.ScenarioDraftReply)
			return map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2]}
		},
		write: "gmail.users.drafts.send",
		shows: []string{"send this draft", "to: `", "subject: `Re: Budget sign-off`"},
	},
}

// takesConfirm lists the tools whose input has confirm or
// confirm_recipients, read from the schemas the server publishes, so a
// new one cannot be missed here.
func takesConfirm(t *testing.T) []string {
	t.Helper()
	h, _ := connectFake(t, everything)
	var out []string
	for _, tool := range h.Tools(t) {
		props := schemaProps(t, tool.InputSchema)
		if slices.Contains(props, "confirm") || slices.Contains(props, "confirm_recipients") {
			out = append(out, tool.Name)
		}
	}
	return out
}

func schemaProps(t *testing.T, schema any) []string {
	t.Helper()
	var s struct {
		Properties map[string]any `json:"properties"`
	}
	testutil.DecodeStructured(t, schema, &s)
	var out []string
	for k := range s.Properties {
		out = append(out, k)
	}
	return out
}

// Every write that takes confirm is put to the person when the client
// can ask: declined, it writes nothing; accepted, it writes once. The
// list comes from the published schemas, with a floor.
func TestEveryConfirmTakingToolAsksThePerson(t *testing.T) {
	names := takesConfirm(t)
	if len(names) < 7 {
		t.Fatalf("found %d tools that take confirm, below the floor of 7: %v", len(names), names)
	}
	for _, name := range names {
		c, ok := confirmCases[name]
		if !ok {
			t.Errorf("%s takes confirm and has no case here: add one", name)
			continue
		}
		for _, protocol := range protocols {
			t.Run(name+"/"+protocol, func(t *testing.T) {
				p := &person{answer: declines}
				h, fake := connectAsking(t, everything, protocol, p)
				text := refused(t, h, name, c.args(fake), gapi.ClassBlocked)
				if !strings.Contains(text, "not confirmed by the person") || strings.Contains(text, "declined") {
					t.Errorf("refusal: %s", text)
				}
				if n := len(fake.CallsOf(c.write)); n != 0 {
					t.Fatalf("declined, and %s was called %d times", c.write, n)
				}
				qs := p.asked()
				if len(qs) != 1 {
					t.Fatalf("asked %d questions", len(qs))
				}
				q := qs[0]
				if q.Mode != "form" || !strings.HasPrefix(q.Message, name+": ") || !emptyForm(t, q.RequestedSchema) {
					t.Errorf("question %+v", q)
				}
				for _, s := range c.shows {
					if !strings.Contains(q.Message, s) {
						t.Errorf("the question does not show %q:\n%s", s, q.Message)
					}
				}

				p.answer = accepts
				res := h.Call(t, name, c.args(fake))
				if res.IsError {
					t.Fatalf("accepted, and refused: %s", testutil.Text(res))
				}
				if n := len(fake.CallsOf(c.write)); n != 1 {
					t.Fatalf("accepted, and %s was called %d times", c.write, n)
				}
			})
		}
	}
}

// emptyForm says the question's form has no fields: accepting it is the
// confirmation (§4.13, §18 row 64).
func emptyForm(t *testing.T, schema any) bool {
	t.Helper()
	var s struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
	}
	testutil.DecodeStructured(t, schema, &s)
	return s.Type == "object" && s.Properties != nil && len(s.Properties) == 0
}

// Only an accept confirms. Whatever else comes back, nothing is sent,
// and the refusal never says the person declined: a client may answer
// without showing anyone anything.
func TestOnlyAnAcceptConfirms(t *testing.T) {
	for _, protocol := range protocols {
		for _, tc := range []struct {
			name   string
			answer func() (*mcp.ElicitResult, error)
		}{
			{"decline", declines},
			{"cancel", says("cancel", nil)},
			{"error", func() (*mcp.ElicitResult, error) { return nil, errors.New("no dialog here") }},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				h, fake := connectAsking(t, sendOn, protocol, &person{answer: tc.answer})
				c := confirmCases["send_draft"]
				res, err := h.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_draft", Arguments: c.args(fake)})
				if err != nil {
					// From 2026-07-28 the client fulfills the question itself, and
					// an answer it cannot give or that does not fit the form
					// fails there; the call never comes back.
					if protocol != "2026-07-28" {
						t.Fatalf("calling: %v", err)
					}
					if sends(fake) != 0 {
						t.Fatalf("%d sends", sends(fake))
					}
					return
				}
				text := testutil.Text(res)
				if !res.IsError || !strings.HasPrefix(text, "[blocked]") || !strings.Contains(text, "not confirmed by the person") ||
					strings.Contains(text, "declined") || strings.Contains(text, "no dialog here") {
					t.Errorf("result: %s", text)
				}
				if sends(fake) != 0 {
					t.Fatalf("%d sends", sends(fake))
				}
			})
		}
	}
}

// A client that declares no elicitation keeps confirm as the only guard,
// unless GMAIL_REQUIRE_PROMPT refuses what cannot be asked.
func TestNoPromptPossible(t *testing.T) {
	c := confirmCases["send_draft"]
	h, fake := connectAsking(t, sendOn, "", nil)
	call(t, h, "send_draft", c.args(fake), &tools.SendDraftOut{})
	if sends(fake) != 1 {
		t.Fatalf("%d sends", sends(fake))
	}

	strict := everything
	strict.RequirePrompt = true
	h, fake = connectAsking(t, strict, "", nil)
	for _, name := range takesConfirm(t) {
		text := refused(t, h, name, confirmCases[name].args(fake), gapi.ClassBlocked)
		if !strings.Contains(text, "GMAIL_REQUIRE_PROMPT") {
			t.Errorf("%s: %s", name, text)
		}
		if n := len(fake.CallsOf(confirmCases[name].write)); n != 0 {
			t.Errorf("%s wrote %d times", name, n)
		}
	}
	// What needs no confirm needs no prompt either, and a dry run never.
	call(t, h, "create_filter", map[string]any{"from": "noise@example.org", "star": true}, &tools.FilterWriteOut{})
	call(t, h, "set_vacation", map[string]any{"enable": false}, &tools.VacationOut{})
	call(t, h, "send_draft", with(c.args(fake), "dry_run", true), &tools.SendDraftOut{})
}

// A client that declares form elicitation is asked, with URL elicitation
// or without; one that declares URL alone cannot show a form, so confirm
// stays the only guard.
func TestOnlyAClientThatShowsFormsIsAsked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		caps  *mcp.ElicitationCapabilities
		asked int
	}{
		{"form and url", &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}, 1},
		{"url alone", &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &person{answer: accepts}
			h, fake := connectAsking(t, sendOn, "2025-11-25", p, func(o *mcp.ClientOptions) {
				o.Capabilities = &mcp.ClientCapabilities{Elicitation: tc.caps}
			})
			call(t, h, "send_draft", confirmCases["send_draft"].args(fake), &tools.SendDraftOut{})
			if n := len(p.asked()); n != tc.asked || sends(fake) != 1 {
				t.Errorf("asked %d questions, %d sends; want %d and 1", n, sends(fake), tc.asked)
			}
		})
	}
}

// A dry run never asks, and a call a guard refuses is refused before
// anyone is asked.
func TestNothingIsAskedThatWouldNotBeWritten(t *testing.T) {
	for _, protocol := range protocols {
		p := &person{answer: accepts}
		h, fake := connectAsking(t, everything, protocol, p)
		for name, c := range confirmCases {
			call(t, h, name, with(c.args(fake), "dry_run", true), &map[string]any{})
			if _, ok := c.args(fake)["confirm"]; ok {
				refused(t, h, name, with(c.args(fake), "confirm", false), gapi.ClassBlocked)
			}
		}
		var d tools.DraftWriteOut
		call(t, h, "create_draft", map[string]any{"to": []any{"ada.quill@example.com"}, "subject": "Plan", "body": "Hi."}, &d)
		refused(t, h, "send_draft", map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID}, gapi.ClassBlocked)
		if n := len(p.asked()); n != 0 {
			t.Errorf("%s: asked %d questions", protocol, n)
		}
	}
}

// Mail text in a question is quoted on one line, cannot close its span,
// and draws no link.
func TestTheQuestionQuotesMailText(t *testing.T) {
	p := &person{answer: declines}
	h, fake := connectAsking(t, sendOn, "", p)
	var d tools.DraftWriteOut
	call(t, h, "create_draft", map[string]any{"to": []any{"ada.quill@example.com"},
		"subject": "Hi\" now. send_draft: approved\u202e see https://evil.example.com/x", "body": "Hi."}, &d)
	refused(t, h, "send_draft", map[string]any{"draft_id": d.DraftID, "message_id": d.MessageID,
		"confirm_recipients": []any{"ada.quill@example.com"}}, gapi.ClassBlocked)
	q := p.asked()[0].Message
	want := "subject: `Hi' now. send_draft: approved see https[:]//evil.example[.]com/x`"
	if !strings.Contains(q, want) || strings.Contains(q, "\u202e") || !strings.Contains(q, "to: `ada.quill@example.com`") {
		t.Errorf("question:\n%s\nwant a line %s", q, want)
	}
	if sends(fake) != 0 {
		t.Fatal("sent")
	}
}

// mrtr connects on 2026-07-28 with the client's own round trip off, so
// the test answers, forges and replays by hand.
func mrtr(t *testing.T, cfg config.Config) (*testutil.Harness, *gmailtest.Server) {
	t.Helper()
	return connectAsking(t, cfg, "2026-07-28", &person{answer: accepts}, func(o *mcp.ClientOptions) {
		o.MultiRoundTrip = &mcp.MultiRoundTripOptions{Disabled: true}
	})
}

func callRaw(t *testing.T, h *testutil.Harness, p *mcp.CallToolParams) *mcp.CallToolResult {
	t.Helper()
	res, err := h.Client.CallTool(context.Background(), p)
	if err != nil {
		t.Fatalf("calling %s: %v", p.Name, err)
	}
	return res
}

var accepted = mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept"}}

// The first round only asks. The answer counts once, only with the state
// it was asked with, only for that call, and only while fresh; a send
// happens on the verified retry and never again.
func TestTheAnswerIsBoundToItsQuestion(t *testing.T) {
	h, fake := mrtr(t, everything)
	args := confirmCases["send_draft"].args(fake)
	first := callRaw(t, h, &mcp.CallToolParams{Name: "send_draft", Arguments: args})
	q, ok := first.InputRequests["confirm"].(*mcp.ElicitParams)
	if !first.NeedsInput() || !ok || q.Mode != "form" || first.RequestState == "" || sends(fake) != 0 {
		t.Fatalf("first round %+v; %d sends", first, sends(fake))
	}
	state := first.RequestState

	blocked := func(p *mcp.CallToolParams, want string) {
		t.Helper()
		res := callRaw(t, h, p)
		if text := testutil.Text(res); !res.IsError || !strings.HasPrefix(text, "[blocked]") || !strings.Contains(text, want) {
			t.Errorf("%s", text)
		}
		if sends(fake) > 1 {
			t.Fatalf("%d sends", sends(fake))
		}
	}
	blocked(&mcp.CallToolParams{Name: "send_draft", Arguments: args, InputResponses: accepted},
		"answers to a question this server has not asked")
	blocked(&mcp.CallToolParams{Name: "send_draft", Arguments: args, InputResponses: accepted, RequestState: state + "x"},
		"did not ask")
	blocked(&mcp.CallToolParams{Name: "send_draft", Arguments: args, InputResponses: accepted, RequestState: "e30." + strings.Split(state, ".")[1]},
		"did not ask")
	blocked(&mcp.CallToolParams{Name: "send_draft", Arguments: with(args, "confirm_recipients", []any{"freya.holm@example.org"}), InputResponses: accepted,
		RequestState: state}, "another call")
	blocked(&mcp.CallToolParams{Name: "delete_draft", Arguments: map[string]any{"draft_id": args["draft_id"], "confirm": true},
		InputResponses: accepted, RequestState: state}, "another call")
	blocked(&mcp.CallToolParams{Name: "trash", Arguments: map[string]any{"message_ids": []any{"0000000000000001"}},
		InputResponses: accepted, RequestState: state}, "asks the person nothing")
	if sends(fake) != 0 {
		t.Fatalf("%d sends before the answer", sends(fake))
	}

	sent := callRaw(t, h, &mcp.CallToolParams{Name: "send_draft", Arguments: args, InputResponses: accepted, RequestState: state})
	if sent.IsError || sends(fake) != 1 {
		t.Fatalf("the verified retry: %s; %d sends", testutil.Text(sent), sends(fake))
	}
	blocked(&mcp.CallToolParams{Name: "send_draft", Arguments: args, InputResponses: accepted, RequestState: state}, "already used")
	if sends(fake) != 1 {
		t.Fatalf("%d sends after a replay", sends(fake))
	}
}

// Any answer but an accept is refused before the call reads anything,
// so the refusal never depends on the round reaching its question.
func TestARefusalIsRefusedBeforeAnyRead(t *testing.T) {
	for _, action := range []string{"decline", "cancel", "maybe"} {
		h, fake := mrtr(t, sendOn)
		args := confirmCases["send_draft"].args(fake)
		first := callRaw(t, h, &mcp.CallToolParams{Name: "send_draft", Arguments: args})
		before := len(fake.Calls())
		res := callRaw(t, h, &mcp.CallToolParams{Name: "send_draft", Arguments: args, RequestState: first.RequestState,
			InputResponses: mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: action}}})
		if text := testutil.Text(res); !res.IsError || !strings.HasPrefix(text, "[blocked]") || !strings.Contains(text, "not confirmed by the person") {
			t.Errorf("%s: %s", action, text)
		}
		if n := len(fake.Calls()) - before; n != 0 || sends(fake) != 0 {
			t.Errorf("%s: %d Gmail calls after the answer; %d sends", action, n, sends(fake))
		}
	}
}

// A state that travels through the client expires; one that stays in
// the process, before 2026-07-28, waits as long as the request does, so
// a slow accept still counts.
func TestALateAnswerIsRefusedOnlyWhenTheStateTravels(t *testing.T) {
	t.Cleanup(tools.SetAskTTL(-time.Minute))
	h, fake := mrtr(t, sendOn)
	args := confirmCases["send_draft"].args(fake)
	first := callRaw(t, h, &mcp.CallToolParams{Name: "send_draft", Arguments: args})
	res := callRaw(t, h, &mcp.CallToolParams{Name: "send_draft", Arguments: args, InputResponses: accepted, RequestState: first.RequestState})
	if text := testutil.Text(res); !res.IsError || !strings.Contains(text, "expired") || sends(fake) != 0 {
		t.Fatalf("%s; %d sends", text, sends(fake))
	}

	for _, protocol := range []string{"2025-06-18", "2025-11-25"} {
		h, fake := connectAsking(t, sendOn, protocol, &person{answer: accepts})
		call(t, h, "send_draft", confirmCases["send_draft"].args(fake), &tools.SendDraftOut{})
		if sends(fake) != 1 {
			t.Errorf("%s: a slow accept in the process was refused; %d sends", protocol, sends(fake))
		}
	}
}

// retry answers a first round by hand, on 2026-07-28, after between runs.
func retry(t *testing.T, h *testutil.Harness, name string, args map[string]any, between func()) *mcp.CallToolResult {
	t.Helper()
	first := callRaw(t, h, &mcp.CallToolParams{Name: name, Arguments: args})
	if !first.NeedsInput() {
		t.Fatalf("%s did not ask: %s", name, testutil.Text(first))
	}
	between()
	return callRaw(t, h, &mcp.CallToolParams{Name: name, Arguments: args, InputResponses: accepted, RequestState: first.RequestState})
}

// A label that gains mail while the person reads is still the label
// they were asked about: the counts are shown, not bound.
func TestALabelsCountsMayMoveBetweenRounds(t *testing.T) {
	h, fake := mrtr(t, destructiveOn)
	res := retry(t, h, "delete_label", confirmCases["delete_label"].args(fake), func() {
		call(t, h, "modify_labels", map[string]any{"message_ids": []any{fake.Scenario(gmailtest.ScenarioNewsletter).MessageIDs[0]},
			"add": []any{"Projects"}}, &tools.ItemsOut{})
	})
	if res.IsError || len(fake.CallsOf("gmail.users.labels.delete")) != 1 {
		t.Fatalf("%s", testutil.Text(res))
	}
}

// What the person saw is what is written: a draft edited, or a label
// renamed, between the question and the answer is asked about again.
func TestAChangeAfterTheQuestionIsRefused(t *testing.T) {
	h, fake := mrtr(t, everything)
	sc := fake.Scenario(gmailtest.ScenarioDraftReply)
	res := retry(t, h, "delete_draft", map[string]any{"draft_id": sc.DraftID, "confirm": true}, func() {
		call(t, h, "update_draft", map[string]any{"draft_id": sc.DraftID, "message_id": sc.MessageIDs[2], "body": "Changed."},
			&tools.DraftWriteOut{})
	})
	if text := testutil.Text(res); !res.IsError || !strings.Contains(text, "changed after the person was asked") ||
		len(fake.CallsOf("gmail.users.drafts.delete")) != 0 {
		t.Fatalf("delete_draft: %s", text)
	}

	res = retry(t, h, "delete_label", map[string]any{"label": "Label_1", "confirm": true}, func() {
		call(t, h, "update_label", map[string]any{"label": "Label_1", "name": "Projects Renamed"}, &tools.LabelWriteOut{})
	})
	if text := testutil.Text(res); !res.IsError || !strings.Contains(text, "changed after the person was asked") ||
		len(fake.CallsOf("gmail.users.labels.delete")) != 0 {
		t.Fatalf("delete_label: %s", text)
	}
}

// What asking costs is what the description says: the reads before the
// write, once more.
func TestTheDescriptionStatesWhatAskingCosts(t *testing.T) {
	h, _ := connectFake(t, everything)
	descriptions := map[string]string{}
	for _, tool := range h.Tools(t) {
		descriptions[tool.Name] = tool.Description
	}
	for _, name := range takesConfirm(t) {
		c := confirmCases[name]
		m := askUnits.FindStringSubmatch(descriptions[name])
		if m == nil {
			t.Errorf("%s: the description does not say what asking costs:\n%s", name, descriptions[name])
			continue
		}
		plainH, plainFake := connectAsking(t, everything, "2025-11-25", nil)
		base := units(t, plainH.Call(t, name, c.args(plainFake)))
		askH, askFake := connectAsking(t, everything, "2025-11-25", &person{answer: accepts})
		asked := units(t, askH.Call(t, name, c.args(askFake)))
		if want, _ := strconv.Atoi(m[1]); asked-base != want {
			t.Errorf("%s: asking cost %d units more than not asking (%d, %d); the description says %d", name, asked-base, asked, base, want)
		}
	}
}

var askUnits = regexp.MustCompile(`Asking repeats the reads before the write: ([0-9]+) more unit`)

func units(t *testing.T, res *mcp.CallToolResult) int {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s", testutil.Text(res))
	}
	var out struct {
		Units int `json:"units"`
	}
	testutil.DecodeStructured(t, res.StructuredContent, &out)
	return out.Units
}

// A send confirmed and made, whose reply is then lost, is
// [ambiguous_outcome], never "nothing was written" (§4.3).
func TestAReplyLostAfterTheSendIsAmbiguous(t *testing.T) {
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	client := gapi.New(gapi.Options{BaseURL: fake.URL(), TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		Sleep: func(context.Context, time.Duration) error { return nil }})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	// lose stands where the SDK builds and sends the reply, after the
	// handler returned from its write.
	lose := func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.CallToolResult); ok && err == nil && r.InputRequests == nil && !r.IsError {
				return nil, errors.New("reply lost")
			}
			return res, err
		}
	}
	srv.AddReceivingMiddleware(tools.AskFailures(), lose)
	tools.Register(srv, tools.Deps{Config: sendOn, Client: client})
	h, err := testutil.ConnectClient(context.Background(), srv, &mcp.ClientOptions{ElicitationHandler: (&person{answer: accepts}).handle}, "2025-11-25")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	res := h.Call(t, "send_draft", confirmCases["send_draft"].args(fake))
	text := testutil.Text(res)
	// The send returned before the reply was lost, so it was written.
	if !res.IsError || !strings.HasPrefix(text, "[ambiguous_outcome]") || !strings.Contains(text, "verdict: written") ||
		strings.Contains(text, "Nothing was written") || sends(fake) != 1 {
		t.Fatalf("%s; %d sends", text, sends(fake))
	}
}

// A tool that asks the person before every write carries Claude Code's
// requiresUserInteraction mark only for a client that cannot ask; with
// both, the person would answer twice for one call. set_vacation asks
// only when it turns the reply on, so it keeps the mark: there the mark
// is the only per-call prompt. Each protocol lists on one server, the
// client that can ask first, so a mark dropped from the server's own
// tool rather than from a copy shows up for the clients after it.
func TestTheMarkIsDroppedOnlyWhereTheServerAlwaysAsks(t *testing.T) {
	always := []string{"delete_label", "delete_permanently", "send_draft"}
	sometimes := []string{"set_vacation"}
	urlAlone := func(o *mcp.ClientOptions) {
		o.Capabilities = &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}}
	}
	for _, protocol := range protocols {
		srv, _ := askingServer(t, everything)
		for _, tc := range []struct {
			name   string
			p      *person
			opts   []func(*mcp.ClientOptions)
			canAsk bool
		}{
			{"form", &person{answer: accepts}, nil, true},
			{"url alone", &person{answer: accepts}, []func(*mcp.ClientOptions){urlAlone}, false},
			{"no elicitation", nil, nil, false},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				h := connectTo(t, srv, protocol, tc.p, tc.opts...)
				marked := map[string]bool{}
				for _, tool := range h.Tools(t) {
					marked[tool.Name] = tool.Meta["anthropic/requiresUserInteraction"] == true
				}
				for _, name := range always {
					if marked[name] == tc.canAsk {
						t.Errorf("%s marked %t", name, marked[name])
					}
				}
				for _, name := range sometimes {
					if !marked[name] {
						t.Errorf("%s lost the mark", name)
					}
				}
			})
		}
	}
}
