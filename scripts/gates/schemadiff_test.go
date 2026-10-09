package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// schemaDiffFixture is a surface as large as the floor, stamped v1.0.0.
// edit changes the map before it is encoded.
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
					"max":  map[string]any{"type": "integer"},
					"opts": map[string]any{"type": "object", "properties": map[string]any{"deep": map[string]any{}}},
				}},
			"outputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"subject": map[string]any{"type": "string"},
				"items": map[string]any{"type": "array", "items": map[string]any{"type": "object",
					"properties": map[string]any{"message_id": map[string]any{"type": "string"}}}},
			}},
		})
	}
	d := map[string]any{"server": "s", "version": "v1.0.0", "sdk_version": "v1.8.0", "tools": tools,
		"kinds":              map[string]any{"tool_0": "read"},
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

func schemaDiffProps(d map[string]any, schema string) map[string]any {
	return schemaDiffTool0(d)[schema].(map[string]any)["properties"].(map[string]any)
}

// A CHANGELOG whose newest release is 1.0.0, with something unreleased.
const schemaDiffChangelog = "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- A thing.\n\n" +
	"## [1.0.0] - 2026-10-01\n\n### Added\n\n- The first.\n\n## [0.9.0] - 2026-09-01\n\n" +
	"[Unreleased]: https://example.test/compare/v1.0.0...HEAD\n[1.0.0]: https://example.test/v1.0.0\n"

// The same CHANGELOG with nothing under [Unreleased]: the build is 1.0.0.
var schemaDiffReleased = strings.Replace(schemaDiffChangelog, "### Added\n\n- A thing.\n\n", "", 1)

func TestSchemaDiffSameSurfacePasses(t *testing.T) {
	var out sink
	if err := schemaDiffCheck(&out, schemaDiffChangelog, schemaDiffFixture(t, nil), schemaDiffFixture(t, nil)); err != nil {
		t.Fatal(err)
	}
	out.mustSay(t, fmt.Sprintf("baseline (v1.0.0) %d tools, 1 resources, 1 templates; built %d tools", schemaDiffMinTools, schemaDiffMinTools))
}

func TestSchemaDiffAdditionsPassAndAreReported(t *testing.T) {
	now := schemaDiffFixture(t, func(d map[string]any) {
		d["tools"] = append(d["tools"].([]any), map[string]any{"name": "tool_new"})
		schemaDiffProps(d, "inputSchema")["extra"] = map[string]any{"type": "string"}
		schemaDiffProps(d, "outputSchema")["extra"] = map[string]any{"type": "string"}
		schemaDiffTool0(d)["annotations"] = map[string]any{"readOnlyHint": false}
		d["kinds"] = map[string]any{"tool_0": "write"}
	})
	var out sink
	if err := schemaDiffCheck(&out, schemaDiffChangelog, schemaDiffFixture(t, nil), now); err != nil {
		t.Fatalf("an addition failed: %v", err)
	}
	out.mustSay(t, "added: tool tool_new")
	out.mustSay(t, "look at this: tool_0: annotations changed")
	out.mustSay(t, `look at this: tool_0: kind changed from "read" to "write"`)
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
			delete(schemaDiffProps(d, "inputSchema"), "id")
		}, "tool_0 input: field id removed"},
		"nested input field lost": {func(d map[string]any) {
			schemaDiffProps(d, "inputSchema")["opts"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}, "tool_0 input: field opts.deep removed"},
		"new required input": {func(d map[string]any) {
			schemaDiffTool0(d)["inputSchema"].(map[string]any)["required"] = []any{"id", "opts"}
		}, "tool_0 input: field opts newly required"},
		"output field lost inside an array": {func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["items"] = map[string]any{"type": "array",
				"items": map[string]any{"type": "object", "properties": map[string]any{}}}
		}, "tool_0 output: field items[].message_id removed"},
		"output list loses its elements' schema": {func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["items"] = map[string]any{"type": "array"}
		}, "tool_0 output: field items[] removed"},
		"output schema lost": {func(d map[string]any) {
			delete(schemaDiffTool0(d), "outputSchema")
		}, "tool_0: output schema removed"},
		"input narrowed": {func(d map[string]any) {
			schemaDiffProps(d, "inputSchema")["id"] = map[string]any{"type": "integer"}
		}, `tool_0 input: field id changed type from "string" to "integer"`},
		"output may now be null": {func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["subject"] = map[string]any{"type": []any{"null", "string"}}
		}, `tool_0 output: field subject changed type from "string" to ["null","string"]`},
		"output list element retyped": {func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["items"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		}, `tool_0 output: field items[] changed type from "object" to "string"`},
		"output field inside a list retyped": {func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["items"] = map[string]any{"type": "array", "items": map[string]any{"type": "object",
				"properties": map[string]any{"message_id": map[string]any{"type": "integer"}}}}
		}, `tool_0 output: field items[].message_id changed type from "string" to "integer"`},
		"output loses its type": {func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["subject"] = map[string]any{}
		}, `tool_0 output: field subject changed type from "string" to any`},
		"resource removed": {func(d map[string]any) { d["resources"] = []any{} }, "resource gmail://labels removed"},
		"template removed": {func(d map[string]any) { d["resource_templates"] = []any{} },
			"resource template gmail://threads/{id} removed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out sink
			err := schemaDiffCheck(&out, schemaDiffChangelog, schemaDiffFixture(t, nil), schemaDiffFixture(t, tc.edit))
			if err == nil || !strings.Contains(err.Error(), "breaking change(s) against "+schemaDiffBaseline) {
				t.Fatalf("got %v: %s", err, out.String())
			}
			out.mustSay(t, "BREAKING: "+tc.want)
		})
	}
}

// A type change breaks a caller only in one direction: an input that
// takes more, or an output that returns less, breaks nobody.
func TestSchemaDiffTypeChangesThatBreakNobody(t *testing.T) {
	cases := map[string]func(d map[string]any){
		"input may now be null": func(d map[string]any) {
			schemaDiffProps(d, "inputSchema")["id"] = map[string]any{"type": []any{"null", "string"}}
		},
		"input integer now a number": func(d map[string]any) {
			schemaDiffProps(d, "inputSchema")["max"] = map[string]any{"type": "number"}
		},
		"input takes any type": func(d map[string]any) {
			schemaDiffProps(d, "inputSchema")["id"] = map[string]any{}
		},
		"output no longer null": func(d map[string]any) {
			schemaDiffProps(d, "outputSchema")["subject"] = map[string]any{"type": "string"}
		},
	}
	nullable := func(d map[string]any) {
		schemaDiffProps(d, "outputSchema")["subject"] = map[string]any{"type": []any{"null", "string"}}
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			base := schemaDiffFixture(t, nil)
			if name == "output no longer null" {
				base = schemaDiffFixture(t, nullable)
			}
			var out sink
			if err := schemaDiffCheck(&out, schemaDiffChangelog, base, schemaDiffFixture(t, edit)); err != nil {
				t.Fatalf("%v: %s", err, out.String())
			}
			out.mustSay(t, "look at this: tool_0: ")
		})
	}
}

func TestSchemaDiffFloor(t *testing.T) {
	small := schemaDiffFixture(t, func(d map[string]any) { d["tools"] = d["tools"].([]any)[:3] })
	err := schemaDiffCheck(&sink{}, schemaDiffChangelog, schemaDiffFixture(t, nil), small)
	if err == nil || !strings.Contains(err.Error(), "below the floor") {
		t.Errorf("got %v", err)
	}
}

// The baseline must be the newest release's surface: an older one
// protects an older surface, so what shipped since could be dropped.
func TestSchemaDiffBaselineMustBeTheNewestRelease(t *testing.T) {
	older := schemaDiffFixture(t, func(d map[string]any) { d["version"] = "v0.9.0" })
	err := schemaDiffCheck(&sink{}, schemaDiffChangelog, older, schemaDiffFixture(t, nil))
	want := `the baseline is the "v0.9.0" surface, but CHANGELOG.md's newest release is v1.0.0; ` +
		"record it in that release's commit with `make schema-baseline VERSION=v1.0.0`"
	if err == nil || err.Error() != want {
		t.Errorf("got  %v\nwant %s", err, want)
	}
	unstamped := schemaDiffFixture(t, func(d map[string]any) { delete(d, "version") })
	if err := schemaDiffCheck(&sink{}, schemaDiffChangelog, unstamped, schemaDiffFixture(t, nil)); err == nil ||
		!strings.Contains(err.Error(), `the baseline is the "" surface`) {
		t.Errorf("a baseline with no version passed: %v", err)
	}
	// Before the first release there is no version to hold it to.
	if err := schemaDiffCheck(&sink{}, "# Changelog\n\n## [Unreleased]\n\n- First.\n", older, schemaDiffFixture(t, nil)); err != nil {
		t.Errorf("before the first release: %v", err)
	}
}

// With nothing under [Unreleased] the build is the newest release, so
// its surface is the baseline's exactly.
func TestSchemaDiffNothingUnreleasedMeansTheSameSurface(t *testing.T) {
	addsATool := schemaDiffFixture(t, func(d map[string]any) {
		d["tools"] = append(d["tools"].([]any), map[string]any{"name": "tool_new"})
	})
	err := schemaDiffCheck(&sink{}, schemaDiffReleased, schemaDiffFixture(t, nil), addsATool)
	want := "nothing is under [Unreleased], so this build is v1.0.0, and its surface differs from the baseline; " +
		"in v1.0.0's release commit, run `make schema-baseline VERSION=v1.0.0`, otherwise say what changed under [Unreleased]"
	if err == nil || err.Error() != want {
		t.Errorf("got  %v\nwant %s", err, want)
	}
	newWords := schemaDiffFixture(t, func(d map[string]any) { schemaDiffTool0(d)["description"] = "reads, now better" })
	if err := schemaDiffCheck(&sink{}, schemaDiffReleased, schemaDiffFixture(t, nil), newWords); err == nil {
		t.Error("a changed description passed with nothing unreleased")
	}
	// Other stamps, and the lists in another order, are the same surface.
	restamped := schemaDiffFixture(t, func(d map[string]any) {
		d["version"], d["sdk_version"] = "dev", "v1.9.0"
		tools := d["tools"].([]any)
		tools[0], tools[5] = tools[5], tools[0]
	})
	if err := schemaDiffCheck(&sink{}, schemaDiffReleased, schemaDiffFixture(t, nil), restamped); err != nil {
		t.Errorf("the same surface under other stamps failed: %v", err)
	}
	// With work under [Unreleased], the addition is that work.
	if err := schemaDiffCheck(&sink{}, schemaDiffChangelog, schemaDiffFixture(t, nil), addsATool); err != nil {
		t.Errorf("an addition under unreleased work failed: %v", err)
	}
}

func TestSchemaBaselineRecord(t *testing.T) {
	stamp := func(v string, edit func(d map[string]any)) func(d map[string]any) {
		return func(d map[string]any) {
			d["version"] = v
			if edit != nil {
				edit(d)
			}
		}
	}
	addTool := func(d map[string]any) {
		d["tools"] = append(d["tools"].([]any), map[string]any{"name": "tool_new"})
	}
	dropTool := func(d map[string]any) { d["tools"] = d["tools"].([]any)[1:] }
	addAndDrop := func(d map[string]any) { addTool(d); dropTool(d) }
	const v2 = "# Changelog\n\n## [Unreleased]\n\n## [2.0.0] - 2026-10-09\n\n- Gone.\n\n## [1.0.0] - 2026-10-01\n"
	cases := []struct {
		name, changelog string
		prev            []byte
		build           func(d map[string]any)
		want            string // the error, or "" to record
	}{
		{name: "a build that adds a tool", changelog: strings.Replace(schemaDiffReleased, "1.0.0", "1.1.0", 1),
			prev: schemaDiffFixture(t, nil), build: stamp("v1.1.0", addTool)},
		{name: "the first baseline", changelog: schemaDiffReleased, build: stamp("v1.0.0", nil)},
		{name: "a break in a new major version", changelog: v2,
			prev: schemaDiffFixture(t, nil), build: stamp("v2.0.0", addAndDrop)},
		{name: "a break in a minor version", changelog: strings.Replace(schemaDiffReleased, "1.0.0", "1.1.0", 1),
			prev: schemaDiffFixture(t, nil), build: stamp("v1.1.0", addAndDrop),
			want: `v1.1.0 breaks a caller of "v1.0.0" in 1 way(s) above, and is not a new major version; ` +
				schemaDiffBaseline + " is unchanged"},
		{name: "a second run that drops a tool new in this release", changelog: strings.Replace(schemaDiffReleased, "1.0.0", "1.1.0", 1),
			prev: schemaDiffFixture(t, stamp("v1.1.0", addTool)), build: stamp("v1.1.0", nil),
			want: `v1.1.0 breaks a caller of "v1.1.0" in 1 way(s) above, and is not a new major version; ` +
				schemaDiffBaseline + " is unchanged. It already holds v1.1.0 from an earlier run, so a tool new in v1.1.0 " +
				"counts as released; restore the last release's baseline from its tag, then run this again"},
		{name: "a build of another version", changelog: schemaDiffReleased,
			prev: schemaDiffFixture(t, nil), build: stamp("dev", nil),
			want: "the build is stamped \"dev\", but the release being cut is v1.0.0; run `make schema-baseline VERSION=v1.0.0`"},
		{name: "no release yet", changelog: "# Changelog\n\n## [Unreleased]\n",
			build: stamp("dev", nil), want: "CHANGELOG.md names no release yet, so there is no surface to record"},
		{name: "a surface below the floor", changelog: schemaDiffReleased,
			build: stamp("v1.0.0", func(d map[string]any) { d["tools"] = d["tools"].([]any)[:3] }),
			want:  fmt.Sprintf("the built surface has 3 tools, below the floor of %d; not writing it", schemaDiffMinTools)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := schemaDiffFixture(t, tc.build)
			norm, _, err := schemaBaselineRecord(&sink{}, tc.changelog, tc.prev, raw)
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("got  %v\nwant %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !schemaDiffSame(norm, raw) || !strings.Contains(string(norm), "\n  \"version\": \"v") {
				t.Errorf("recorded something other than the build:\n%s", norm)
			}
		})
	}
}

func TestSchemaBaselineVersion(t *testing.T) {
	for changelog, want := range map[string]string{
		schemaDiffChangelog: "v1.0.0",
		schemaDiffReleased:  "v1.0.0",
		"# Changelog\n\n## [Unreleased]\n\n## [1.1.0] - 2026-10-10\n\n- x\n\n## [1.0.0] - 2026-10-01\n": "v1.1.0",
		"# Changelog\n\n## [Unreleased]\n\n- First.\n":                                                  "",
	} {
		if got := schemaBaselineVersion(changelog); got != want {
			t.Errorf("schemaBaselineVersion(%q) = %q, want %q", changelog, got, want)
		}
	}
}

func TestSchemaNewMajor(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"v2.1.0", "v3.0.0", true},
		{"v2.1.0", "v2.2.0", false},
		{"v2.1.0", "v2.1.0", false},
		{"v3.0.0", "v2.9.0", false},
		{"", "v3.0.0", false},
		{"dev", "v3.0.0", false},
	} {
		if got := schemaNewMajor(c.from, c.to); got != c.want {
			t.Errorf("schemaNewMajor(%q, %q) = %t, want %t", c.from, c.to, got, c.want)
		}
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
