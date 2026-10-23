package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v3/internal/config"
	"github.com/mmedum/google-mail-mcp/v3/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v3/internal/server/testutil"
)

type probeIn struct {
	ID     string `json:"id" jsonschema:"an id"`
	DryRun bool   `json:"dry_run,omitempty"`
	Fail   string `json:"fail,omitempty"`
}

type probeOut struct {
	Seen      string    `json:"seen"`
	DryRun    bool      `json:"dry_run"`
	WrittenAt time.Time `json:"written_at"`
}

func (o probeOut) Render() string { return "rendered: " + o.Seen }

// probe answers with what it saw, and fails on request.
func probe(ctx context.Context, in probeIn) (probeOut, error) {
	switch in.Fail {
	case "classified":
		return probeOut{}, gapi.Errf(gapi.ClassNotFound, "no message with that id")
	case "plain":
		return probeOut{}, errors.New("something broke")
	}
	return probeOut{Seen: in.ID, DryRun: gapi.WritesForbidden(ctx)}, nil
}

// allKinds is every Kind, for the tests that probe each one.
var allKinds = []Kind{Read, ReadWritesLocally, Write, WriteForGood, Send, Destructive, Settings, SettingsForGood, AutoReply}

// confirmingProbeIn is probeIn with confirm, which a kind that cannot be
// undone must take.
type confirmingProbeIn struct {
	ID      string `json:"id" jsonschema:"an id"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Fail    string `json:"fail,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// registerProbes registers one probe per Kind, named after the kind.
func registerProbes(s *mcp.Server, d Deps) {
	for _, k := range allKinds {
		sp := Spec{Name: strings.ReplaceAll(k.String(), "-", "_"), Description: "probe", Kind: k}
		if !k.confirmsEveryCall() {
			register(s, d, sp, probe)
			continue
		}
		register(s, d, sp, func(ctx context.Context, in confirmingProbeIn) (probeOut, error) {
			return probe(ctx, probeIn{ID: in.ID, DryRun: in.DryRun, Fail: in.Fail})
		})
	}
}

func surface(t *testing.T, cfg config.Config) []string {
	t.Helper()
	h := testutil.Connect(t, func(s *mcp.Server) { registerProbes(s, Deps{Config: cfg}) })
	var names []string
	for _, tool := range h.Tools(t) {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// §9.4 and §8: what each configuration registers.
func TestRegistrationGates(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"default", config.Config{}, []string{"read", "write", "write_for_good"}},
		{"local dir", config.Config{LocalDir: "/x"}, []string{"read", "read_writes_locally", "write", "write_for_good"}},
		{"read-only", config.Config{ReadOnly: true}, []string{"read"}},
		{"read-only with local dir", config.Config{ReadOnly: true, LocalDir: "/x"}, []string{"read", "read_writes_locally"}},
		{"send", config.Config{EnableSend: true}, []string{"read", "send", "write", "write_for_good"}},
		{"destructive", config.Config{EnableDestructive: true}, []string{"destructive", "read", "write", "write_for_good"}},
		{"settings", config.Config{EnableSettings: true}, []string{"read", "settings", "write", "write_for_good"}},
		{"settings and destructive", config.Config{EnableSettings: true, EnableDestructive: true},
			[]string{"destructive", "read", "settings", "settings_for_good", "write", "write_for_good"}},
		{"settings and send", config.Config{EnableSettings: true, EnableSend: true},
			[]string{"auto_reply", "read", "send", "settings", "write", "write_for_good"}},
		// Config refuses this pair; the gate still holds if it is ever built.
		{"read-only beats enable", config.Config{ReadOnly: true, EnableSend: true, EnableDestructive: true, EnableSettings: true},
			[]string{"read"}},
		{"full surface", FullSurface(config.Config{ReadOnly: true}), []string{"auto_reply", "destructive", "read",
			"read_writes_locally", "send", "settings", "settings_for_good", "write", "write_for_good"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := surface(t, tt.cfg); !slices.Equal(got, tt.want) {
				t.Errorf("registered %v, want %v", got, tt.want)
			}
		})
	}
	if Kind(99).allowed(FullSurface(config.Config{})) {
		t.Error("an unknown kind registers")
	}
}

// The annotations decide each client's prompt (§4.13): claude.ai and
// Codex prompt by destructiveHint, VS Code on every tool without
// readOnlyHint, and Claude Code on every call of a tool with the mark.
func TestAnnotationsAndMetaComeFromKind(t *testing.T) {
	h := testutil.Connect(t, func(s *mcp.Server) { registerProbes(s, Deps{Config: FullSurface(config.Config{})}) })
	byName := map[string]*mcp.Tool{}
	for _, tool := range h.Tools(t) {
		byName[tool.Name] = tool
	}
	type want struct {
		readOnly, idempotent, destructive, openWorld, interaction bool
	}
	wants := map[string]want{
		"read":                {readOnly: true, idempotent: true},
		"read_writes_locally": {},
		"write":               {},
		"write_for_good":      {destructive: true, idempotent: true},
		"send":                {destructive: true, openWorld: true},
		"destructive":         {destructive: true, idempotent: true, interaction: true},
		"settings":            {destructive: true, idempotent: true},
		"settings_for_good":   {destructive: true, idempotent: true},
		"auto_reply":          {destructive: true, openWorld: true},
	}
	if len(wants) != len(allKinds) {
		t.Fatalf("%d kinds have a want, of %d", len(wants), len(allKinds))
	}
	for name, w := range wants {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("%s not registered", name)
		}
		a := tool.Annotations
		if a.ReadOnlyHint != w.readOnly || a.IdempotentHint != w.idempotent {
			t.Errorf("%s: readOnly %t idempotent %t", name, a.ReadOnlyHint, a.IdempotentHint)
		}
		if (a.DestructiveHint == nil) != w.readOnly || (a.DestructiveHint != nil && *a.DestructiveHint != w.destructive) {
			t.Errorf("%s: destructiveHint %s, want %t", name, hint(a.DestructiveHint), w.destructive)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint != w.openWorld {
			t.Errorf("%s: openWorld %v, want %t", name, a.OpenWorldHint, w.openWorld)
		}
		_, has := tool.Meta["anthropic/requiresUserInteraction"]
		if has != w.interaction {
			t.Errorf("%s: requiresUserInteraction present %t", name, has)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", name)
		}
	}
	for _, k := range allKinds {
		if strings.HasPrefix(k.String(), "kind(") {
			t.Errorf("kind %d has no name", int(k))
		}
	}
	if Kind(42).String() != "kind(42)" {
		t.Error("unknown kind name")
	}
}

func TestOutputSchemaMarksTimesAsDateTime(t *testing.T) {
	b, err := json.Marshal(outputSchema[probeOut]())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"format":"date-time"`) {
		t.Errorf("schema has no date-time: %s", b)
	}
}

func TestSchemaForPanicsOnAnUnrepresentableType(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	_ = outputSchema[chan int]()
}

// Both halves: structured content under the schema, and the rendering
// in content — never the JSON twice.
func TestAReplyCarriesBothHalves(t *testing.T) {
	h := testutil.Connect(t, func(s *mcp.Server) { registerProbes(s, Deps{Config: config.Config{}}) })
	var out probeOut
	res := h.CallInto(t, "read", map[string]any{"id": "m1"}, &out)
	if res.IsError {
		t.Fatalf("error: %s", testutil.Text(res))
	}
	if out.Seen != "m1" {
		t.Errorf("structured = %+v", out)
	}
	if got := testutil.Text(res); got != "rendered: m1" {
		t.Errorf("content = %q", got)
	}
}

// dryFirstIn has dry_run as its first field, at index 0.
type dryFirstIn struct {
	DryRun bool   `json:"dry_run,omitempty"`
	ID     string `json:"id"`
}

func TestDryRunEntersTheNoWritesContext(t *testing.T) {
	h := testutil.Connect(t, func(s *mcp.Server) {
		d := Deps{Config: config.Config{}}
		registerProbes(s, d)
		register(s, d, Spec{Name: "write_dry_first", Description: "probe", Kind: Write},
			func(ctx context.Context, in dryFirstIn) (probeOut, error) {
				return probeOut{Seen: in.ID, DryRun: gapi.WritesForbidden(ctx)}, nil
			})
	})
	for _, tool := range []string{"write", "write_dry_first"} {
		for _, dry := range []bool{true, false} {
			var out probeOut
			h.CallInto(t, tool, map[string]any{"id": "m1", "dry_run": dry}, &out)
			if out.DryRun != dry {
				t.Errorf("%s dry_run %t: handler saw %t", tool, dry, out.DryRun)
			}
		}
	}
	type noDry struct {
		DryRun string `json:"dry_run"`
	}
	if dryRunField[noDry]() != -1 || dryRunField[int]() != -1 {
		t.Error("a non-bool dry_run or a non-struct input was taken for the flag")
	}
}

func TestErrorsAreClassedToolErrors(t *testing.T) {
	h := testutil.Connect(t, func(s *mcp.Server) { registerProbes(s, Deps{Config: config.Config{}}) })
	res := h.Call(t, "read", map[string]any{"id": "m1", "fail": "classified"})
	if !res.IsError || testutil.Text(res) != "[not_found] no message with that id" {
		t.Errorf("classified: %v %q", res.IsError, testutil.Text(res))
	}
	res = h.Call(t, "read", map[string]any{"id": "m1", "fail": "plain"})
	if !res.IsError || testutil.Text(res) != "[unavailable] something broke" {
		t.Errorf("plain: %v %q", res.IsError, testutil.Text(res))
	}
}

func TestRegisterWithNoToolsYet(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "x", Version: "0"}, nil)
	Register(s, Deps{})
}

// Names is exactly what tools/list returns, in order, under each gate.
func TestNamesIsWhatToolsListReturns(t *testing.T) {
	for _, cfg := range []config.Config{{}, {ReadOnly: true}, FullSurface(config.Config{})} {
		var listed []string
		h := testutil.Connect(t, func(s *mcp.Server) { Register(s, Deps{Config: cfg}) })
		for tool, err := range h.Client.Tools(context.Background(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			listed = append(listed, tool.Name)
		}
		names := Names(Deps{Config: cfg})
		slices.Sort(listed)
		sorted := slices.Sorted(slices.Values(names))
		if len(names) == 0 || !slices.Equal(sorted, listed) {
			t.Errorf("%+v: Names = %v, tools/list = %v", cfg, names, listed)
		}
	}
}

// hint is an annotation's value as text, "unset" for none.
func hint(b *bool) string {
	if b == nil {
		return "unset"
	}
	return strconv.FormatBool(*b)
}

type confirmIn struct {
	Confirm bool `json:"confirm,omitempty"`
}

// registers reports whether register accepted a tool of kind k taking
// In, or panicked.
func registers[In any](k Kind) (ok bool) {
	defer func() { ok = recover() == nil }()
	register(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), Deps{Config: FullSurface(config.Config{})},
		Spec{Name: "probe", Description: "probe", Kind: k},
		func(context.Context, In) (probeOut, error) { return probeOut{}, nil })
	return true
}

// confirm says a write cannot be undone (§4.13), so a tool that takes it
// is annotated destructive, and a client that prompts by annotation
// prompts for it.
func TestAToolThatTakesConfirmIsAnnotatedDestructive(t *testing.T) {
	for _, k := range allKinds {
		want := k != Read && k != ReadWritesLocally && k != Write
		if got := registers[confirmIn](k); got != want {
			t.Errorf("%s with confirm: registered %t, want %t", k, got, want)
		}
	}
}

// Every delete, and every kind that cannot be undone, takes confirm: a
// witness that the caller meant it (§4.13).
func TestAKindThatCannotBeUndoneTakesConfirm(t *testing.T) {
	for _, k := range allKinds {
		want := k != WriteForGood && k != Destructive && k != SettingsForGood
		if got := registers[probeIn](k); got != want {
			t.Errorf("%s without confirm: registered %t, want %t", k, got, want)
		}
	}
}
