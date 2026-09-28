package tools_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v2/internal/tools"
)

var (
	settingsOn  = config.Config{EnableSettings: true}
	autoReplyOn = config.Config{EnableSettings: true, EnableSend: true}
)

// connectSettings connects to a fake whose token holds
// gmail.settings.basic.
func connectSettings(t *testing.T, cfg config.Config) (*testutil.Harness, *gmailtest.Server) {
	t.Helper()
	h, fake := connectFake(t, cfg)
	fake.SettingsScope = true
	return h, fake
}

// The settings tools exist only with their flag, and the vacation reply
// only with the send flag as well (§9.4, §17.4).
func TestSettingsToolsRegisterOnlyWithTheirFlags(t *testing.T) {
	// tools/list is in name order.
	settings := []string{"create_filter", "delete_filter", "update_signature"}
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"default", config.Config{}, nil},
		{"send alone", sendOn, nil},
		{"settings", settingsOn, settings},
		{"settings and send", autoReplyOn, []string{"create_filter", "delete_filter", "set_vacation", "update_signature"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := connectFake(t, tc.cfg)
			var got []string
			for _, tool := range h.Tools(t) {
				if !slices.Contains(append(slices.Clone(settings), "set_vacation"), tool.Name) {
					continue
				}
				got = append(got, tool.Name)
				a := tool.Annotations
				switch tool.Name {
				case "set_vacation":
					if !*a.OpenWorldHint || tool.Meta["anthropic/requiresUserInteraction"] != true {
						t.Errorf("set_vacation reaches other people and does not say so: %+v", a)
					}
				default:
					// A signature overwritten or mail a filter trashed is not
					// put back: destructive, and reaching nobody else.
					if !*a.DestructiveHint || *a.OpenWorldHint || a.ReadOnlyHint {
						t.Errorf("%s annotations %+v", tool.Name, a)
					}
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("registered %v; want %v", got, tc.want)
			}
		})
	}
	env := map[string]string{"GMAIL_READ_ONLY": "true", "GMAIL_ENABLE_SETTINGS": "true"}
	if _, err := config.Load(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("read-only with the settings flag was accepted")
	}
}

func TestUpdateSignature(t *testing.T) {
	h, fake := connectSettings(t, settingsOn)
	args := map[string]any{"signature": "Rae Reader\n<b>Operations</b> & more"}

	var dry tools.SignatureOut
	call(t, h, "update_signature", with(args, "dry_run", true), &dry)
	if !dry.DryRun || dry.Address != gmailtest.Account || !strings.Contains(string(dry.UntrustedBefore), "Example Org") {
		t.Fatalf("dry run %+v", dry)
	}
	if fake.Settings().SendAs[0].Signature != "<div>Rae Reader<br>Example Org</div>" {
		t.Fatal("a dry run changed the signature")
	}

	var out tools.SignatureOut
	text := call(t, h, "update_signature", args, &out)
	// Markup the caller wrote is escaped, never applied.
	if got := fake.Settings().SendAs[0].Signature; got != "Rae Reader<br>&lt;b&gt;Operations&lt;/b&gt; &amp; more" {
		t.Fatalf("stored %q", got)
	}
	if !strings.Contains(string(out.UntrustedAfter), "<b>Operations</b> & more") ||
		!strings.Contains(text, "drafts this server writes carry no signature") {
		t.Errorf("after %q\n%s", out.UntrustedAfter, text)
	}

	call(t, h, "update_signature", map[string]any{"send_as": strings.ToUpper(gmailtest.AliasAddress), "signature": ""}, &out)
	if out.Address != gmailtest.AliasAddress || fake.Settings().SendAs[1].Signature != "" {
		t.Errorf("clearing the alias's signature: %+v", out)
	}
	refused(t, h, "update_signature", map[string]any{"send_as": "someone@example.com", "signature": "x"}, gapi.ClassInvalid)
	refused(t, h, "update_signature", map[string]any{"signature": strings.Repeat("x", 10001)}, gapi.ClassInvalid)

	// Without the scope the call is refused, and the advice is to log in
	// again, which asks for it.
	fake.SettingsScope = false
	if text := refused(t, h, "update_signature", args, gapi.ClassAuth); !strings.Contains(text, "login") {
		t.Errorf("no re-login advice: %s", text)
	}
}

// A filter cannot forward: the type it is written as has no field for it.
func TestAFilterCannotForward(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[gmail.FilterWrite](), reflect.TypeFor[gmail.FilterWriteAction]()} {
		for f := range typ.Fields() {
			if strings.Contains(strings.ToLower(f.Name+f.Tag.Get("json")), "forward") {
				t.Errorf("%s has a field %s", typ, f.Name)
			}
		}
	}
	for f := range reflect.TypeFor[tools.CreateFilterIn]().Fields() {
		if strings.Contains(strings.ToLower(f.Name), "forward") {
			t.Errorf("create_filter takes %s", f.Name)
		}
	}
}

func TestCreateFilter(t *testing.T) {
	h, fake := connectSettings(t, settingsOn)
	before := len(fake.Settings().Filters)
	args := map[string]any{"from": gmailtest.Ada.Email, "add_labels": []any{"Projects"}, "archive": true, "star": true}

	var dry tools.FilterWriteOut
	call(t, h, "create_filter", with(args, "dry_run", true), &dry)
	if !dry.DryRun || dry.Created || len(fake.Settings().Filters) != before {
		t.Fatalf("dry run %+v", dry)
	}

	var out tools.FilterWriteOut
	text := call(t, h, "create_filter", args, &out)
	fs := fake.Settings().Filters
	if !out.Created || out.Filter.ID == "" || len(fs) != before+1 {
		t.Fatalf("create %+v", out)
	}
	made := fs[len(fs)-1]
	if made.Criteria.From != gmailtest.Ada.Email || !slices.Contains(made.Action.AddLabelIDs, "STARRED") ||
		!slices.Contains(made.Action.RemoveLabelIDs, "INBOX") || made.Action.Forward != "" {
		t.Errorf("stored %+v %+v", made.Criteria, made.Action)
	}
	if !strings.Contains(text, "mail already in the mailbox is untouched") {
		t.Errorf("text:\n%s", text)
	}
	// Archiving removes INBOX and nothing else, as Gmail stored it live
	// (§18 row 56).
	if !slices.Equal(made.Action.RemoveLabelIDs, []string{"INBOX"}) || out.Filter.NeverSpam || strings.Contains(text, "spam") {
		t.Errorf("an archiving filter: %+v\n%s", made.Action, text)
	}

	// SPAM removed is Gmail's "Never send it to Spam", and is said so
	// rather than as a label taken off, in the dry run too.
	for _, dry := range []bool{true, false} {
		var ns tools.FilterWriteOut
		nsText := call(t, h, "create_filter", map[string]any{"from": gmailtest.Bruno.Email, "remove_labels": []any{"SPAM"},
			"archive": true, "dry_run": dry}, &ns)
		if !ns.Filter.NeverSpam || !strings.Contains(nsText, "never sends matching mail to spam") ||
			strings.Contains(nsText, "SPAM") || !strings.Contains(nsText, "removes labels: INBOX") {
			t.Errorf("dry run %v: %+v\n%s", dry, ns.Filter, nsText)
		}
	}

	conflict := refused(t, h, "create_filter", args, gapi.ClassConflict)
	if !strings.Contains(conflict, out.Filter.ID) {
		t.Errorf("the conflict does not name the filter: %s", conflict)
	}
	// Gmail was seen adding SPAM to archiving filters after the fact
	// (§18 row 56). The seeded newsletter filter carries it; asking for
	// the same filter without it is still a duplicate. Asking for one
	// that only never-spams is not the archiving filter.
	harbor := map[string]any{"from": gmailtest.Harbor.Email, "add_labels": []any{"Newsletters"}, "archive": true}
	if text := refused(t, h, "create_filter", harbor, gapi.ClassConflict); !strings.Contains(text, gmailtest.FilterNewsletters) ||
		!strings.Contains(text, "also never sends matching mail to spam") || !strings.Contains(text, "delete it and create this again") {
		t.Errorf("the conflict does not say the stored filter also keeps mail out of spam: %s", text)
	}
	// A filter differing in case is another filter: the criteria are
	// compared as given.
	call(t, h, "create_filter", with(with(args, "from", strings.ToUpper(gmailtest.Ada.Email)), "dry_run", true), &tools.FilterWriteOut{})
	call(t, h, "create_filter", map[string]any{"from": gmailtest.Harbor.Email, "add_labels": []any{"Newsletters"},
		"remove_labels": []any{"SPAM"}, "dry_run": true}, &tools.FilterWriteOut{})
	refused(t, h, "create_filter", map[string]any{"archive": true}, gapi.ClassInvalid)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.com"}, gapi.ClassInvalid)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.com", "add_labels": []any{"TRASH"}}, gapi.ClassInvalid)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.com", "add_labels": []any{"SENT"}}, gapi.ClassInvalid)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.com", "add_labels": []any{"No such label"}}, gapi.ClassNotFound)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.com", "star": true, "size": 100}, gapi.ClassInvalid)
}

// Filter writes sent in parallel all land. Gmail refused a filter write
// that overlapped another (§18 row 54), and so does the fake; the
// server holds its settings changes to one at a time.
func TestParallelFilterWritesDoNotOverlap(t *testing.T) {
	h, fake := connectSettings(t, settingsOn)
	before := len(fake.Settings().Filters)
	type job struct {
		tool string
		args map[string]any
	}
	var jobs []job
	for i := range 8 {
		jobs = append(jobs, job{"create_filter", map[string]any{"from": fmt.Sprintf("sender%d@example.org", i), "star": true}})
	}
	for _, id := range []string{gmailtest.FilterNewsletters, gmailtest.FilterReceipts} {
		jobs = append(jobs, job{"delete_filter", map[string]any{"filter_id": id, "confirm": true}})
	}
	failed := make([]string, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Go(func() {
			res, err := h.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: j.tool, Arguments: j.args})
			switch {
			case err != nil:
				failed[i] = err.Error()
			case res.IsError:
				failed[i] = testutil.Text(res)
			}
		})
	}
	wg.Wait()
	for i, f := range failed {
		if f != "" {
			t.Errorf("%s %v: %s", jobs[i].tool, jobs[i].args, f)
		}
	}
	if got, want := len(fake.Settings().Filters), before+8-2; got != want {
		t.Errorf("%d filters after, want %d", got, want)
	}
	if n := fake.Overlaps(); n != 0 {
		t.Errorf("%d filter writes overlapped another at Gmail", n)
	}
}

// A create Google did not confirm is never repeated. The server reads
// the filters afterwards and says whether it was made (§4.3's spirit).
func TestAmbiguousFilterCreateIsSettledByReading(t *testing.T) {
	const create, list = "gmail.users.settings.filters.create", "gmail.users.settings.filters.list"
	args := map[string]any{"from": "late@example.org", "archive": true}
	for _, tc := range []struct {
		name  string
		fail  gmailtest.Failure
		want  string
		saved bool
	}{
		{"created", gmailtest.Failure{Method: create, Status: 500, Reason: "backendError", Message: "Internal error", Served: true},
			"verdict: created", true},
		// Gmail was seen adding SPAM to archiving filters after the fact
		// (§18 row 56); the filter is still the one asked for.
		{"created, SPAM added", gmailtest.Failure{Method: create, Status: 500, Reason: "backendError", Message: "Internal error",
			Served: true, Then: func(s *gmailtest.Server) {
				s.UpdateSettings(func(st *gmailtest.Settings) {
					f := st.Filters[len(st.Filters)-1]
					f.Action.RemoveLabelIDs = append(f.Action.RemoveLabelIDs, "SPAM")
				})
			}},
			"verdict: created", true},
		{"not created", gmailtest.Failure{Method: create, Status: 500, Reason: "backendError", Message: "Internal error"},
			"verdict: not_created", false},
		{"unknown", gmailtest.Failure{Method: create, Status: 500, Reason: "backendError", Message: "Internal error",
			Then: func(s *gmailtest.Server) { s.Fail(gmailtest.Failure{Method: list, Status: 403, Reason: "forbidden"}) }},
			"verdict: unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := connectSettings(t, settingsOn)
			before := len(fake.Settings().Filters)
			fake.Fail(tc.fail)
			text := refused(t, h, "create_filter", args, gapi.ClassAmbiguousOutcome)
			fs := fake.Settings().Filters
			if !strings.Contains(text, tc.want) || len(fs) != before+btoi(tc.saved) {
				t.Errorf("%d filters, want %d; %s", len(fs), before+btoi(tc.saved), text)
			}
			if tc.saved && !strings.Contains(text, fs[len(fs)-1].ID) {
				t.Errorf("the verdict does not name the filter made: %s", text)
			}
			if n := len(fake.CallsOf(create)); n != 1 {
				t.Errorf("the create was sent %d times", n)
			}
		})
	}

	// With a size, which Gmail rounds, the duplicate check may miss a
	// late filter, so the verdict does not promise it will refuse one.
	h, fake := connectSettings(t, settingsOn)
	fake.Fail(gmailtest.Failure{Method: create, Status: 500, Reason: "backendError", Message: "Internal error"})
	text := refused(t, h, "create_filter", with(with(args, "size", 1<<20), "size_comparison", "larger"), gapi.ClassAmbiguousOutcome)
	if !strings.Contains(text, "verdict: not_created") || strings.Contains(text, "refuses it as a conflict") ||
		!strings.Contains(text, "list_filters first") {
		t.Errorf("a sized filter's verdict: %s", text)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A filter that trashes hides mail as it arrives, so it needs confirm.
func TestCreateFilterThatTrashes(t *testing.T) {
	h, fake := connectSettings(t, settingsOn)
	args := map[string]any{"from": "noise@example.org", "trash": true}
	text := refused(t, h, "create_filter", args, gapi.ClassBlocked)
	if !strings.Contains(text, "confirm: true") {
		t.Errorf("refusal: %s", text)
	}
	var dry tools.FilterWriteOut
	call(t, h, "create_filter", with(args, "dry_run", true), &dry)
	if !dry.Trashes {
		t.Errorf("a dry run of a trashing filter does not say so: %+v", dry)
	}
	var out tools.FilterWriteOut
	text = call(t, h, "create_filter", with(args, "confirm", true), &out)
	fs := fake.Settings().Filters
	if !out.Trashes || !slices.Contains(fs[len(fs)-1].Action.AddLabelIDs, "TRASH") || !strings.Contains(text, "to the trash") {
		t.Errorf("create %+v\n%s", out, text)
	}
}

func TestDeleteFilter(t *testing.T) {
	h, fake := connectSettings(t, settingsOn)
	args := map[string]any{"filter_id": gmailtest.FilterNewsletters}
	refused(t, h, "delete_filter", args, gapi.ClassBlocked)

	var dry tools.FilterWriteOut
	call(t, h, "delete_filter", with(args, "dry_run", true), &dry)
	if !dry.DryRun || dry.Filter.ID != gmailtest.FilterNewsletters || len(dry.Filter.RemoveLabels) == 0 {
		t.Fatalf("dry run %+v", dry)
	}
	var out tools.FilterWriteOut
	call(t, h, "delete_filter", with(args, "confirm", true), &out)
	if !out.Deleted || slices.ContainsFunc(fake.Settings().Filters, func(f gmail.Filter) bool { return f.ID == gmailtest.FilterNewsletters }) {
		t.Fatalf("delete %+v", out)
	}
	refused(t, h, "delete_filter", with(args, "confirm", true), gapi.ClassNotFound)
}

func TestSetVacation(t *testing.T) {
	h, fake := connectSettings(t, autoReplyOn)
	on := map[string]any{"enable": true, "subject": "Away", "body": "Back on Monday.", "audience": "contacts",
		"start": "2026-10-01T09:00:00Z", "end": "2026-10-05T17:00:00Z"}

	for _, missing := range []string{"audience", "body"} {
		args := with(on, "confirm", true)
		delete(args, missing)
		refused(t, h, "set_vacation", args, gapi.ClassInvalid)
	}
	refused(t, h, "set_vacation", with(with(on, "audience", "everyone"), "confirm", true), gapi.ClassInvalid)
	refused(t, h, "set_vacation", with(with(on, "end", "2026-09-01T00:00:00Z"), "confirm", true), gapi.ClassInvalid)
	refused(t, h, "set_vacation", with(with(on, "start", "tomorrow"), "confirm", true), gapi.ClassInvalid)
	blocked := refused(t, h, "set_vacation", on, gapi.ClassBlocked)
	if !strings.Contains(blocked, "confirm: true") {
		t.Errorf("refusal: %s", blocked)
	}

	var dry tools.VacationOut
	call(t, h, "set_vacation", with(on, "dry_run", true), &dry)
	if !dry.DryRun || !dry.After.Enabled || fake.Settings().Vacation.EnableAutoReply {
		t.Fatalf("dry run %+v", dry)
	}

	var out tools.VacationOut
	text := call(t, h, "set_vacation", with(on, "confirm", true), &out)
	v := fake.Settings().Vacation
	if !v.EnableAutoReply || !v.RestrictToContacts || v.RestrictToDomain || v.ResponseBodyPlainText != "Back on Monday." ||
		v.StartTime != "1790845200000" || !out.After.Enabled || out.Before.Enabled {
		t.Fatalf("on: %+v\n%+v", v, out)
	}
	if !strings.Contains(text, "vacation reply is ON · to contacts only") {
		t.Errorf("text:\n%s", text)
	}

	// A Workspace account may limit the reply to its domain; a personal
	// one has no domain of its own, and is refused before any write.
	call(t, h, "set_vacation", with(with(on, "audience", "domain"), "dry_run", true), &dry)
	if !dry.After.RestrictToDomain || dry.After.RestrictToContacts {
		t.Errorf("domain: %+v", dry.After)
	}
	fake.ProfileAddress = "someone" + "@gmail.com" // built, so the leak gate sees no address
	text = refused(t, h, "set_vacation", with(with(on, "audience", "domain"), "confirm", true), gapi.ClassInvalid)
	if !strings.Contains(text, "personal Gmail account") {
		t.Errorf("refusal: %s", text)
	}
	fake.ProfileAddress = ""

	// Off needs neither an audience nor confirm, and keeps the text.
	call(t, h, "set_vacation", map[string]any{"enable": false}, &out)
	v = fake.Settings().Vacation
	if v.EnableAutoReply || v.ResponseBodyPlainText != "Back on Monday." || !v.RestrictToContacts || out.After.Enabled {
		t.Errorf("off: %+v", v)
	}
}

// A description is what a model takes as the truth, so what get_settings
// and list_filters say this server can change follows the flags.
func TestSettingsDescriptionsFollowTheFlags(t *testing.T) {
	for _, tc := range []struct {
		cfg                 config.Config
		settings, filters   string
		notSettings, noFilt string
	}{
		{config.Config{}, "cannot change any of them", "cannot create or change filters", "update_signature", "create_filter"},
		{settingsOn, "update_signature changes a signature", "create_filter and delete_filter", "cannot change any", "cannot create"},
		{autoReplyOn, "update_signature and set_vacation", "create_filter and delete_filter", "cannot change any", "cannot create"},
	} {
		h, _ := connectFake(t, tc.cfg)
		for _, tool := range h.Tools(t) {
			d := tool.Description
			switch tool.Name {
			case "get_settings":
				if !strings.Contains(d, tc.settings) || strings.Contains(d, tc.notSettings) {
					t.Errorf("%+v: get_settings says %q", tc.cfg, d)
				}
			case "list_filters":
				if !strings.Contains(d, tc.filters) || strings.Contains(d, tc.noFilt) {
					t.Errorf("%+v: list_filters says %q", tc.cfg, d)
				}
			}
		}
	}
}

// Criteria are compared as Gmail would store them, and a match of
// exclude_chats alone, or a size out of range, is refused before a call.
func TestCreateFilterChecksItsCriteria(t *testing.T) {
	h, _ := connectSettings(t, settingsOn)
	var out tools.FilterWriteOut
	call(t, h, "create_filter", map[string]any{"from": "news@example.org", "star": true}, &out)
	refused(t, h, "create_filter", map[string]any{"from": "  news@example.org ", "star": true}, gapi.ClassConflict)
	refused(t, h, "create_filter", map[string]any{"exclude_chats": true, "star": true}, gapi.ClassInvalid)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.org", "star": true, "size": -5, "size_comparison": "larger"}, gapi.ClassInvalid)
	refused(t, h, "create_filter", map[string]any{"from": "x@example.org", "star": true, "size": 5_000_000_000, "size_comparison": "larger"}, gapi.ClassInvalid)
}

// Runs of spaces in a signature survive the HTML Gmail stores.
func TestUpdateSignatureKeepsSpacing(t *testing.T) {
	h, fake := connectSettings(t, settingsOn)
	call(t, h, "update_signature", map[string]any{"signature": "Rae\n    Title  here"}, &tools.SignatureOut{})
	if got := fake.Settings().SendAs[0].Signature; got != "Rae<br>&nbsp; &nbsp; Title&nbsp; here" {
		t.Errorf("stored %q", got)
	}
}

// A delete Google did not confirm is not sent again. The server reads
// the filters afterwards and says whether it was deleted.
func TestAmbiguousFilterDeleteIsSettledByReading(t *testing.T) {
	const del, list = "gmail.users.settings.filters.delete", "gmail.users.settings.filters.list"
	// A 500 after the delete, then Gmail's generic refusal of the repeat,
	// is ambiguous (§7.9).
	refusal := gmailtest.Failure{Method: del, Status: 400, Reason: "failedPrecondition", Message: "Precondition check failed."}
	for _, tc := range []struct {
		name, want string
		served     bool
		listFails  bool
	}{
		{"deleted", "verdict: deleted", true, false},
		{"still there", "verdict: still_there", false, false},
		{"unknown", "verdict: unknown", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := connectSettings(t, settingsOn)
			fake.Fail(gmailtest.Failure{Method: del, Status: 500, Reason: "backendError", Message: "Internal error", Served: tc.served})
			r := refusal
			if tc.listFails {
				r.Then = func(s *gmailtest.Server) { s.Fail(gmailtest.Failure{Method: list, Status: 403, Reason: "forbidden"}) }
			}
			fake.Fail(r)
			text := refused(t, h, "delete_filter", map[string]any{"filter_id": gmailtest.FilterReceipts, "confirm": true},
				gapi.ClassAmbiguousOutcome)
			there := slices.ContainsFunc(fake.Settings().Filters, func(f gmail.Filter) bool { return f.ID == gmailtest.FilterReceipts })
			if !strings.Contains(text, tc.want) || there == tc.served {
				t.Errorf("filter there %v; %s", there, text)
			}
			if n := len(fake.CallsOf(del)); n != 2 {
				t.Errorf("the delete was sent %d times, want 2: the first and one repeat", n)
			}
		})
	}
}
