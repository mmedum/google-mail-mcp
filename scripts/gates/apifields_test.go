package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// apiFieldsFixture publishes Label (two fields, one inline object) and
// Profile; internal/gmail models Label.id and Profile.historyId.
func apiFieldsFixture(t *testing.T, edit func(files map[string]string)) string {
	t.Helper()
	surface := discoverySurface{Schemas: []discoverySchema{
		{Name: "Label", Fields: []string{"color", "color.text", "id"}},
		{Name: "Profile", Fields: []string{"historyId"}},
		{Name: "WatchRequest", Fields: []string{"topicName"}},
	}}
	snap, err := json.Marshal(surface)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		discoverySnapshotPath: string(snap),
		apiFieldsRecordPath: "# comment\n" +
			"Label.id\tmodeled\t-\n" +
			"Label.color\tout\tcolors are decoration nobody asked for\n" +
			"Label.color.text\tout\tcolors are decoration nobody asked for\n" +
			"Profile.historyId\tmodeled\t-\n",
		"internal/gmail/gmail.go": "package gmail\n\n" +
			"type Label struct {\n\tID string `json:\"id,omitempty\"`\n\tlocal int\n}\n\n" +
			"type Profile struct {\n\tHistoryID string `json:\"historyId\"`\n\tSkip string `json:\"-\"`\n}\n\n" +
			"type unexported struct{ X int }\n",
		"internal/gmail/gmail_test.go": "package gmail\n\ntype Fake struct{ Y int `json:\"y\"` }\n",
	}
	if edit != nil {
		edit(files)
	}
	return writeTree(t, files)
}

var apiFieldsTestFloor = apiFieldsLimits{required: []string{"Label"}, judged: 4, modeled: 2}

func TestAPIFieldsClean(t *testing.T) {
	var out sink
	if err := apiFieldsCheck(apiFieldsFixture(t, nil), apiFieldsTestFloor, &out); err != nil {
		t.Fatalf("a complete record failed: %v\n%s", err, out.String())
	}
	out.mustSay(t, "api-fields ok: 2 schemas judged, 4 published fields, 2 modeled, 2 out; 2 structs in 1 files")
}

func apiFieldsEdit(path, old, repl string) func(map[string]string) {
	return func(f map[string]string) { f[path] = strings.Replace(f[path], old, repl, 1) }
}

func TestAPIFieldsFailures(t *testing.T) {
	cases := []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{"published field with no row", apiFieldsEdit(apiFieldsRecordPath, "Label.color.text\tout\tcolors are decoration nobody asked for\n", ""),
			"Label.color.text is published and has no row"},
		{"row for an unpublished field", apiFieldsEdit(apiFieldsRecordPath, "# comment\n", "Label.gone\tout\ta reason that is long enough\n"),
			"Label.gone is not a published field"},
		{"row for an unjudged schema", apiFieldsEdit(apiFieldsRecordPath, "# comment\n", "WatchRequest.topicName\tout\ta reason that is long enough\n"),
			"WatchRequest is not a judged schema"},
		{"modeled with no struct field", apiFieldsEdit(apiFieldsRecordPath, "Label.color\tout\tcolors are decoration nobody asked for", "Label.color\tmodeled\t-"),
			`Label.color is recorded as modeled and internal/gmail.Label has no field tagged "color"`},
		{"out and modeled", apiFieldsEdit(apiFieldsRecordPath, "Label.id\tmodeled\t-", "Label.id\tout\tnot needed by any tool at all"),
			"Label.id is recorded as out and internal/gmail models it"},
		{"short reason", apiFieldsEdit(apiFieldsRecordPath, "Label.color\tout\tcolors are decoration nobody asked for", "Label.color\tout\tno"),
			"Label.color is out with a reason of 2 characters"},
		{"unknown verdict", apiFieldsEdit(apiFieldsRecordPath, "Label.id\tmodeled", "Label.id\tmaybe"),
			`verdict "maybe" is not modeled or out`},
		{"struct field Google does not publish", apiFieldsEdit("internal/gmail/gmail.go", "\tlocal int\n", "\tExtra string `json:\"extra\"`\n"),
			"Label.extra: internal/gmail/gmail.go:"},
		{"struct with no schema of its name", apiFieldsEdit("internal/gmail/gmail.go", "type unexported", "type Renamed struct{ X int }\n\ntype unexported"),
			"internal/gmail declares Renamed, and Google publishes no schema of that name"},
		{"a struct makes its schema judged", apiFieldsEdit("internal/gmail/gmail.go", "type unexported", "type WatchRequest struct{}\n\ntype unexported"),
			"WatchRequest.topicName is published and has no row"},
		{"no structs at all", func(f map[string]string) { f["internal/gmail/gmail.go"] = "package gmail\n" },
			""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out sink
			err := apiFieldsCheck(apiFieldsFixture(t, tc.edit), apiFieldsTestFloor, &out)
			if err == nil {
				t.Fatalf("passed:\n%s", out.String())
			}
			if tc.want != "" {
				out.mustSay(t, tc.want)
			}
		})
	}
}

func TestAPIFieldsFloors(t *testing.T) {
	var out sink
	err := apiFieldsCheck(apiFieldsFixture(t, nil), apiFieldsLimits{required: []string{"Label", "Missing"}, judged: 5, modeled: 3}, &out)
	if err == nil {
		t.Fatal("passed below the floors")
	}
	out.mustSay(t, "§8b names Missing and the snapshot publishes no such schema")
	out.mustSay(t, "4 published fields judged, below the floor of 5")
	out.mustSay(t, "2 fields modeled, below the floor of 3")
}

// A struct that declares itself a view of a schema is held to that
// schema's fields: a view of Label may model id, and not a field Label
// does not publish.
func TestAPIFieldsViews(t *testing.T) {
	view := func(field string) func(map[string]string) {
		return apiFieldsEdit("internal/gmail/gmail.go", "type unexported",
			"// LabelPatch is a narrower write.\n//\n// Schema: Label\ntype LabelPatch struct {\n\tX string `json:\""+field+"\"`\n}\n\ntype unexported")
	}
	var out sink
	if err := apiFieldsCheck(apiFieldsFixture(t, view("id")), apiFieldsTestFloor, &out); err != nil {
		t.Fatalf("a view of published fields failed: %v\n%s", err, out.String())
	}
	out = sink{}
	err := apiFieldsCheck(apiFieldsFixture(t, view("notPublished")), apiFieldsTestFloor, &out)
	if err == nil || !strings.Contains(out.String(), "Label.notPublished") {
		t.Errorf("a view modeling an unpublished field passed: %v\n%s", err, out.String())
	}
}
