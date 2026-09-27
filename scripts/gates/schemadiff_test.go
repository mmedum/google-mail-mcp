package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// schemaDiffFixture is a surface as large as the floor. edit changes the
// map before it is encoded.
func schemaDiffFixture(t *testing.T, edit func(d map[string]any)) []byte {
	t.Helper()
	var tools []any
	for i := range schemaDiffMinTools {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("tool_%d", i),
			"description": "reads",
			"annotations": map[string]any{"readOnlyHint": true},
			"inputSchema": map[string]any{"type": "object", "required": []any{"id"},
				"properties": map[string]any{
					"id":   map[string]any{"type": "string"},
					"opts": map[string]any{"type": "object", "properties": map[string]any{"deep": map[string]any{}}},
				}},
			"outputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"items": map[string]any{"type": "array", "items": map[string]any{"type": "object",
					"properties": map[string]any{"message_id": map[string]any{}}}},
			}},
		})
	}
	d := map[string]any{"server": "s", "tools": tools,
		"resources":          []any{map[string]any{"uri": "gmail://labels"}},
		"resource_templates": []any{map[string]any{"uriTemplate": "gmail://threads/{id}"}}}
	if edit != nil {
		edit(d)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func schemaDiffTool0(d map[string]any) map[string]any {
	return d["tools"].([]any)[0].(map[string]any)
}

func TestSchemaDiffSameSurfacePasses(t *testing.T) {
	var out sink
	if err := schemaDiffCompare(&out, schemaDiffFixture(t, nil), schemaDiffFixture(t, nil)); err != nil {
		t.Fatal(err)
	}
	out.mustSay(t, fmt.Sprintf("baseline %d tools, 1 resources, 1 templates; built %d tools", schemaDiffMinTools, schemaDiffMinTools))
}

func TestSchemaDiffAdditionsPassAndAreReported(t *testing.T) {
	now := schemaDiffFixture(t, func(d map[string]any) {
		d["tools"] = append(d["tools"].([]any), map[string]any{"name": "tool_new"})
		in := schemaDiffTool0(d)["inputSchema"].(map[string]any)
		in["properties"].(map[string]any)["extra"] = map[string]any{"type": "string"}
		schemaDiffTool0(d)["annotations"] = map[string]any{"readOnlyHint": false}
	})
	var out sink
	if err := schemaDiffCompare(&out, schemaDiffFixture(t, nil), now); err != nil {
		t.Fatalf("an addition failed: %v", err)
	}
	out.mustSay(t, "added: tool tool_new")
	out.mustSay(t, "look at this: tool_0: annotations changed")
}

func TestSchemaDiffFailsEachBreakingChange(t *testing.T) {
	cases := map[string]struct {
		edit func(d map[string]any)
		want string
	}{
		"tool removed": {func(d map[string]any) {
			d["tools"] = append(d["tools"].([]any)[1:], map[string]any{"name": "tool_x"})
		}, "tool tool_0 removed"},
		"input field lost": {func(d map[string]any) {
			delete(schemaDiffTool0(d)["inputSchema"].(map[string]any)["properties"].(map[string]any), "id")
		}, "field id removed"},
		"nested input field lost": {func(d map[string]any) {
			p := schemaDiffTool0(d)["inputSchema"].(map[string]any)["properties"].(map[string]any)
			p["opts"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}, "field opts.deep removed"},
		"new required input": {func(d map[string]any) {
			schemaDiffTool0(d)["inputSchema"].(map[string]any)["required"] = []any{"id", "opts"}
		}, "field opts newly required"},
		"output field lost inside an array": {func(d map[string]any) {
			schemaDiffTool0(d)["outputSchema"] = map[string]any{"type": "object", "properties": map[string]any{
				"items": map[string]any{"type": "array", "items": map[string]any{"type": "object",
					"properties": map[string]any{}}}}}
		}, "field items.message_id removed"},
		"output schema lost": {func(d map[string]any) {
			delete(schemaDiffTool0(d), "outputSchema")
		}, "output schema removed"},
		"resource removed": {func(d map[string]any) { d["resources"] = []any{} }, "resource gmail://labels removed"},
		"template removed": {func(d map[string]any) { d["resource_templates"] = []any{} },
			"resource template gmail://threads/{id} removed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out sink
			err := schemaDiffCompare(&out, schemaDiffFixture(t, nil), schemaDiffFixture(t, tc.edit))
			if err == nil {
				t.Fatalf("passed: %s", out.String())
			}
			out.mustSay(t, tc.want)
		})
	}
}

func TestSchemaDiffFloor(t *testing.T) {
	small := schemaDiffFixture(t, func(d map[string]any) { d["tools"] = d["tools"].([]any)[:3] })
	err := schemaDiffCompare(&sink{}, schemaDiffFixture(t, nil), small)
	if err == nil || !strings.Contains(err.Error(), "below the floor") {
		t.Errorf("got %v", err)
	}
}

func TestBaselineNormalizeSorts(t *testing.T) {
	raw := schemaDiffFixture(t, func(d map[string]any) {
		tools := d["tools"].([]any)
		tools[0], tools[7] = tools[7], tools[0]
	})
	norm, n, err := baselineNormalize(raw)
	if err != nil || n != schemaDiffMinTools {
		t.Fatalf("%d, %v", n, err)
	}
	if strings.Index(string(norm), `"tool_0"`) > strings.Index(string(norm), `"tool_7"`) {
		t.Error("tools not sorted by name")
	}
	again, _, _ := baselineNormalize(norm)
	if string(again) != string(norm) {
		t.Error("normalizing twice changed the bytes")
	}
}
