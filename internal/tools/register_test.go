package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/config"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/server/testutil"
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

var kinds = []Kind{Read, ReadWritesLocally, Write, WriteForGood, Send, Destructive}

// registerProbes registers one probe per Kind, named after the kind.
func registerProbes(s *mcp.Server, d Deps) {
	for _, k := range kinds {
		register(s, d, Spec{Name: strings.ReplaceAll(k.String(), "-", "_"), Description: "probe", Kind: k}, probe)
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
		// Config refuses this pair; the gate still holds if it is ever built.
		{"read-only beats enable", config.Config{ReadOnly: true, EnableSend: true, EnableDestructive: true}, []string{"read"}},
		{"full surface", FullSurface(config.Config{ReadOnly: true}), []string{"destructive", "read", "read_writes_locally", "send", "write",
			"write_for_good"}},
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
		"send":                {openWorld: true, interaction: true},
		"destructive":         {destructive: true, idempotent: true, interaction: true},
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
		if a.DestructiveHint != nil && *a.DestructiveHint != w.destructive {
			t.Errorf("%s: destructive %t", name, *a.DestructiveHint)
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
	for _, k := range kinds {
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

func TestDryRunEntersTheNoWritesContext(t *testing.T) {
	h := testutil.Connect(t, func(s *mcp.Server) { registerProbes(s, Deps{Config: config.Config{}}) })
	for _, dry := range []bool{true, false} {
		var out probeOut
		h.CallInto(t, "write", map[string]any{"id": "m1", "dry_run": dry}, &out)
		if out.DryRun != dry {
			t.Errorf("dry_run %t: handler saw %t", dry, out.DryRun)
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

type confirmIn struct {
	Confirm bool `json:"confirm,omitempty"`
}

// A tool that takes confirm is registered only under a kind that also
// asks the person (§4.13), so a new one cannot rest on the model alone.
func TestATakingConfirmToolMustAskThePerson(t *testing.T) {
	all := []Kind{Read, ReadWritesLocally, Write, WriteForGood, Send, Destructive, Settings, SettingsForGood, AutoReply}
	cfg := FullSurface(config.Config{})
	for _, k := range all {
		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			register(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), Deps{Config: cfg},
				Spec{Name: "confirming", Description: "probe", Kind: k},
				func(context.Context, confirmIn) (probeOut, error) { return probeOut{}, nil })
			return false
		}()
		if panicked == k.asksPerson() {
			t.Errorf("%s: panicked %v, asks the person %v", k, panicked, k.asksPerson())
		}
	}
}
