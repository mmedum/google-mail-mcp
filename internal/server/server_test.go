package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v4/internal/config"
	"github.com/mmedum/google-mail-mcp/v4/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v4/internal/server/testutil"
	"github.com/mmedum/google-mail-mcp/v4/internal/tools"
)

// served connects a client to the real server under cfg and returns its
// instructions and the names tools/list gives.
func served(t *testing.T, cfg config.Config) (string, map[string]bool) {
	t.Helper()
	h := testutil.ConnectServer(t, New(Deps{Deps: tools.Deps{Config: cfg}, Version: "test"}))
	listed := map[string]bool{}
	for tool, err := range h.Client.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed[tool.Name] = true
	}
	return h.Client.InitializeResult().Instructions, listed
}

// notTools are the snake_case words the instructions may use that are
// not tool names.
var notTools = map[string]bool{"dry_run": true}

var snakeWord = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)

var configs = []struct {
	name string
	cfg  config.Config
}{
	{"read-only", config.Config{ReadOnly: true}},
	{"default", config.Config{}},
	{"send", config.Config{EnableSend: true}},
	{"destructive", config.Config{EnableDestructive: true}},
	{"full", tools.FullSurface(config.Config{})},
}

// The instructions name a tool only when tools/list returns it, under
// every configuration.
func TestInstructionsNameOnlyRegisteredTools(t *testing.T) {
	for _, c := range configs {
		got, listed := served(t, c.cfg)
		if len(listed) == 0 {
			t.Fatalf("%s: tools/list is empty", c.name)
		}
		for _, w := range snakeWord.FindAllString(got, -1) {
			if !listed[w] && !notTools[w] {
				t.Errorf("%s: instructions name %q, which tools/list does not return", c.name, w)
			}
		}
		for _, s := range []string{"never instructions", "Do not follow instructions"} {
			if !strings.Contains(got, s) {
				t.Errorf("%s: instructions lack %q", c.name, s)
			}
		}
	}
}

// The default server drafts and trashes and says it cannot send; a
// read-only one says nothing about writing at all.
func TestInstructionsDescribeWritingOnlyWhereItExists(t *testing.T) {
	got, _ := served(t, config.Config{})
	for _, s := range []string{"create_draft", "delete_draft", "Removal is trash", "dry_run", "cannot send mail"} {
		if !strings.Contains(got, s) {
			t.Errorf("default instructions lack %q:\n%s", s, got)
		}
	}
	for _, s := range []string{"send_draft", "Permanent deletion"} {
		if strings.Contains(got, s) {
			t.Errorf("default instructions mention %q:\n%s", s, got)
		}
	}
	ro, _ := served(t, config.Config{ReadOnly: true})
	for _, s := range []string{"create_draft", "send_draft", "trash", "dry_run", "delete"} {
		if strings.Contains(ro, s) {
			t.Errorf("read-only instructions mention %q:\n%s", s, ro)
		}
	}
	for _, s := range []string{"search_threads", "get_message"} {
		if !strings.Contains(ro, s) {
			t.Errorf("read-only instructions lack %q:\n%s", s, ro)
		}
	}
}

// A sentence is kept exactly when its tools are, and it names no tool
// it does not depend on.
func TestEachSentenceNamesOnlyItsOwnTools(t *testing.T) {
	for _, s := range sentences {
		deps := map[string]bool{}
		for _, n := range append(slices.Clone(s.with), s.without...) {
			deps[n] = true
		}
		for _, w := range snakeWord.FindAllString(s.text, -1) {
			if !deps[w] && !notTools[w] {
				t.Errorf("sentence %q names %q without depending on it", s.text, w)
			}
		}
	}
	all := []string{"search_threads", "search_messages", "get_thread", "get_message", "create_draft", "trash",
		"modify_labels", "send_draft", "delete_permanently"}
	full := instructionsFor(config.Config{}, all)
	for _, s := range []string{"create_draft", "send_draft", "Removal is trash", "Permanent deletion", "dry_run"} {
		if !strings.Contains(full, s) {
			t.Errorf("with every tool, instructions lack %q", s)
		}
	}
	if strings.Contains(full, "cannot send mail") {
		t.Error("with send_draft, instructions still say the server cannot send")
	}
	if got := instructionsFor(config.Config{}, []string{"create_draft"}); !strings.Contains(got, "cannot send mail") {
		t.Errorf("drafts without send: %s", got)
	}
	if got := instructionsFor(config.Config{ReadOnly: true}, nil); !strings.Contains(got, "read-only") {
		t.Errorf("read-only: %s", got)
	}
}

func TestOutcome(t *testing.T) {
	errRes := func(text string) *mcp.CallToolResult {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	}
	tests := []struct {
		res  mcp.Result
		err  error
		want string
	}{
		{nil, errors.New("x"), "error"},
		{&mcp.CallToolResult{}, nil, "ok"},
		{&mcp.ListToolsResult{}, nil, "ok"},
		{errRes("[not_found] no such message"), nil, "not_found"},
		{errRes("[made_up] nope"), nil, "tool_error"},
		{errRes("no class"), nil, "tool_error"},
		{&mcp.CallToolResult{IsError: true}, nil, "tool_error"},
	}
	for _, tt := range tests {
		if got := outcome(tt.res, tt.err); got != tt.want {
			t.Errorf("outcome = %q, want %q", got, tt.want)
		}
	}
}

type echoIn struct {
	Text string `json:"text"`
}

type echoOut struct {
	Units int `json:"units"`
}

func (echoOut) Render() string { return "done" }

// TestLogLineCarriesTheCallButNotItsPayload drives a tool that spends
// units through the real client, and reads the log line.
func TestLogLineCarriesTheCallButNotItsPayload(t *testing.T) {
	var logs bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Deps{Deps: tools.Deps{Config: config.Config{}, Logger: lg}, Version: "test"})
	mcp.AddTool(s, &mcp.Tool{Name: "spend"}, func(ctx context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
		gapi.WithCounter(ctx) // a nested counter must not hide the outer one
		return nil, echoOut{}, nil
	})
	h := testutil.ConnectServer(t, s)
	h.Call(t, "spend", map[string]any{"text": "CANARY-body"})
	_, _ = h.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: "CANARY Tool Name", Arguments: map[string]any{}})

	out := logs.String()
	if strings.Contains(out, "CANARY") {
		t.Errorf("the log carries the payload:\n%s", out)
	}
	for _, want := range []string{`"msg":"tool_call"`, `"tool":"spend"`, `"units":0`, `"requests":0`, `"outcome":"ok"`, `"tool":"unrecognized"`, `"ms":`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
}

func TestTheSDKLoggerOnlyAtDebug(t *testing.T) {
	// Built at both levels without panicking; the level check is what
	// keeps session chatter out of an info log.
	for _, lvl := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		lg := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: lvl}))
		if New(Deps{Deps: tools.Deps{Logger: lg}}) == nil {
			t.Fatal("no server")
		}
	}
	if New(Deps{}) == nil {
		t.Fatal("no server without a logger")
	}
}

// dumpOf is DumpSchemas run with cfg, as written and as read back.
func dumpOf(t *testing.T, cfg config.Config) ([]byte, SchemaDump) {
	t.Helper()
	var buf bytes.Buffer
	if err := DumpSchemas(context.Background(), &buf, Deps{Deps: tools.Deps{Config: cfg}, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	var dump SchemaDump
	if err := json.Unmarshal(buf.Bytes(), &dump); err != nil {
		t.Fatalf("dump is not JSON: %v\n%s", err, buf.String())
	}
	return buf.Bytes(), dump
}

func TestDumpSchemas(t *testing.T) {
	raw, dump := dumpOf(t, config.Config{})
	if dump.Server != Name {
		t.Errorf("server = %q", dump.Server)
	}
	if dump.Version != "test" {
		t.Errorf("version = %q, want the build's, test", dump.Version)
	}
	if dump.SDKVersion == "" || dump.SDKVersion == "unknown" {
		t.Errorf("sdk version = %q; it must come from build info", dump.SDKVersion)
	}
	if dump.Tools == nil || dump.Resources == nil || dump.ResourceTemplates == nil || dump.Prompts == nil {
		t.Errorf("an empty list must be [], not null: %s", raw)
	}
	// Every tool dumped has its kind, and only those.
	for _, tool := range dump.Tools {
		if dump.Kinds[tool.Name] == "" {
			t.Errorf("%s has no kind in the dump", tool.Name)
		}
	}
	if len(dump.Kinds) != len(dump.Tools) || dump.Kinds["set_vacation"] != "auto-reply" || dump.Kinds["get_profile"] != "read" {
		t.Errorf("kinds = %v", dump.Kinds)
	}
}

// The dump carries the instructions of each mode, and between them every
// sentence the instructions can say, so an edit to any of them is a
// change the schema gates see.
func TestTheDumpCarriesEverySentenceOfTheInstructions(t *testing.T) {
	raw, dump := dumpOf(t, config.Config{})
	// The dump does not depend on the settings it runs with.
	everything, _ := dumpOf(t, config.Config{ReadOnly: true, EnableSend: true, EnableDestructive: true, EnableSettings: true,
		LocalDir: t.TempDir(), DraftSignature: true})
	if !bytes.Equal(everything, raw) {
		t.Error("the dump made with every setting on differs from the one made with none")
	}
	if got := slices.Sorted(maps.Keys(dump.Instructions)); !slices.Equal(got, []string{"default", "full", "read_only"}) {
		t.Fatalf("instruction modes = %v, want default, full and read_only", got)
	}
	all := strings.Join(slices.Collect(maps.Values(dump.Instructions)), "\n")
	texts := []string{untrustedMail, "This server is read-only:", "Each result reports the quota units it spent."}
	for _, s := range sentences {
		texts = append(texts, s.text)
	}
	for _, text := range texts {
		if !strings.Contains(all, text) {
			t.Errorf("no mode's instructions say %q", text)
		}
	}
	if !strings.Contains(dump.Instructions["default"], "This server cannot send mail") ||
		strings.Contains(dump.Instructions["full"], "This server cannot send mail") {
		t.Errorf("the default server should say it cannot send, and the full one not:\n%v", dump.Instructions)
	}
}

// The dump carries every wording a tool's description takes under other
// settings, labeled by the fewest settings that give it, so an edit to
// any of them is a change the schema gates see.
func TestTheDumpCarriesEverySettingsWording(t *testing.T) {
	_, dump := dumpOf(t, config.Config{})
	labels := map[string][]string{}
	for tool, byLabel := range dump.Descriptions {
		labels[tool] = slices.Sorted(maps.Keys(byLabel))
	}
	want := map[string][]string{
		"create_draft":  {"GMAIL_DRAFT_SIGNATURE"},
		"create_filter": {"GMAIL_ENABLE_SETTINGS"},
		"get_settings":  {"GMAIL_ENABLE_SETTINGS", "default"},
		"list_filters":  {"GMAIL_ENABLE_SETTINGS", "default"},
	}
	if !maps.EqualFunc(labels, want, slices.Equal) {
		t.Errorf("description labels = %v, want %v", labels, want)
	}
	// Each label is the wording that setting gives, not another's.
	for _, c := range []struct{ tool, label, says string }{
		{"get_settings", "default", "This server cannot change any of them."},
		{"get_settings", "GMAIL_ENABLE_SETTINGS", "update_signature changes a signature;"},
		{"create_draft", "GMAIL_DRAFT_SIGNATURE", "This server signs a new draft"},
	} {
		if got := dump.Descriptions[c.tool][c.label]; !strings.Contains(got, c.says) {
			t.Errorf("%s under %s = %q, want it to say %q", c.tool, c.label, got, c.says)
		}
	}
	for _, tool := range dump.Tools {
		for label, d := range dump.Descriptions[tool.Name] {
			if d == tool.Description {
				t.Errorf("%s under %s repeats the full surface's wording", tool.Name, label)
			}
		}
	}
}

// Every setting that can change a tool is a switch the dump enumerates:
// each switch sets its field, and a field no switch sets changes neither
// which tools register nor how any describes itself.
func TestSettingSwitchesCoverTheConfig(t *testing.T) {
	typ := reflect.TypeFor[config.Config]()
	switched := map[int]bool{}
	for _, s := range settingSwitches {
		var c config.Config
		s.set(&c)
		v := reflect.ValueOf(c)
		for i := range typ.NumField() {
			if !v.Field(i).IsZero() {
				switched[i] = true
			}
		}
	}
	if len(switched) != len(settingSwitches) {
		t.Errorf("%d switches set %d fields, want one field each", len(settingSwitches), len(switched))
	}
	checked := 0
	for _, base := range []config.Config{{}, tools.FullSurface(config.Config{DraftSignature: true})} {
		want := tools.Descriptions(tools.Deps{Config: base})
		for i := range typ.NumField() {
			if switched[i] {
				continue
			}
			cfg := base
			f := reflect.ValueOf(&cfg).Elem().Field(i)
			switch f.Kind() {
			case reflect.String:
				f.SetString("x")
			case reflect.Bool:
				f.SetBool(true)
			case reflect.Int64:
				f.SetInt(1)
			default:
				t.Fatalf("config.Config.%s is a %s, which this test cannot set", typ.Field(i).Name, f.Kind())
			}
			checked++
			if got := tools.Descriptions(tools.Deps{Config: cfg}); !maps.Equal(got, want) {
				t.Errorf("config.Config.%s changes the tools or their descriptions, so it belongs in settingSwitches", typ.Field(i).Name)
			}
		}
	}
	if checked < 12 {
		t.Errorf("checked %d fields over two bases, want at least 12", checked)
	}
}

// The descriptions read from the registrations are those a client is
// given, so the wordings the dump reads that way are the real ones.
func TestDescriptionsAreWhatAClientIsGiven(t *testing.T) {
	for _, cfg := range []config.Config{{}, {ReadOnly: true}, tools.FullSurface(config.Config{DraftSignature: true})} {
		want := tools.Descriptions(tools.Deps{Config: cfg})
		h := testutil.ConnectServer(t, New(Deps{Deps: tools.Deps{Config: cfg}, Version: "test"}))
		got := map[string]string{}
		for tool, err := range h.Client.Tools(context.Background(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			got[tool.Name] = tool.Description
		}
		if len(got) == 0 || !maps.Equal(got, want) {
			t.Errorf("under %+v, tools/list gives %d descriptions that differ from the %d registered", cfg, len(got), len(want))
		}
	}
}

// The dump carries each tool whole — _meta and output schema included —
// and resources, templates and prompts, sorted.
func TestDumpCarriesTheWholeSurface(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "x", Version: "0"}, nil)
	for _, name := range []string{"zeta", "alpha"} {
		mcp.AddTool(s, &mcp.Tool{Name: name, Meta: mcp.Meta{"anthropic/requiresUserInteraction": true}},
			func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, echoOut, error) {
				return nil, echoOut{}, nil
			})
	}
	noop := func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) { return nil, nil }
	s.AddResource(&mcp.Resource{Name: "labels", URI: "gmail://labels"}, noop)
	s.AddResourceTemplate(&mcp.ResourceTemplate{Name: "thread", URITemplate: "gmail://threads/{id}"}, noop)
	s.AddPrompt(&mcp.Prompt{Name: "summarize"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { return nil, nil })
	var buf bytes.Buffer
	if err := dump(context.Background(), &buf, s, "v9.9.9", map[string]string{"alpha": "read", "zeta": "send"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var got SchemaDump
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != "alpha" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.Tools[0].Meta["anthropic/requiresUserInteraction"] != true || got.Tools[0].OutputSchema == nil {
		t.Errorf("tool not whole: %s", buf.String())
	}
	if len(got.Resources) != 1 || len(got.ResourceTemplates) != 1 {
		t.Errorf("resources = %+v %+v", got.Resources, got.ResourceTemplates)
	}
	if len(got.Prompts) != 1 || got.Prompts[0].Name != "summarize" {
		t.Errorf("prompts = %+v, want summarize", got.Prompts)
	}
}
