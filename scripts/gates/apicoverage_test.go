package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	apiCoverageTestReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	apiCoverageTestSharing  = "https://www.googleapis.com/auth/gmail.settings.sharing"
)

// apiCoverageFixture is six methods: x0 used with a client call, x1 and
// x2 planned for phase 0 and 1, x3 to x5 out, x5 reachable by no scope
// the server requests.
func apiCoverageFixture(t *testing.T, edit func(files map[string]string)) string {
	t.Helper()
	surface := discoverySurface{Revision: "20260101"}
	for i := range 6 {
		scope := apiCoverageTestReadonly
		if i == 5 {
			scope = apiCoverageTestSharing
		}
		surface.Methods = append(surface.Methods, discoveryMethod{
			ID: fmt.Sprintf("gmail.users.x%d.get", i), Verb: "GET",
			Path: fmt.Sprintf("gmail/v1/users/{userId}/x%d/{id}", i), Scopes: []string{scope},
		})
	}
	snap, err := json.Marshal(surface)
	if err != nil {
		t.Fatal(err)
	}
	out := "Written off: a reason long enough to read"
	files := map[string]string{
		discoverySnapshotPath: string(snap),
		apiCoverageRecordPath: "# comment\n" +
			"gmail.users.x0.get\tused\t-\tUsed: `get_x`\n" +
			"gmail.users.x1.get\tplanned\t0\tUsed: `list_x`\n" +
			"gmail.users.x2.get\tplanned\t1\tGated: `delete_x`\n" +
			"gmail.users.x3.get\tout\t-\t" + out + "\n" +
			"gmail.users.x4.get\tout\t-\tDeferred (§17): a reason long enough\n" +
			"gmail.users.x5.get\tout\t-\t" + out + "\n",
		"internal/scopes/scopes.go": "package scopes\n\nconst Readonly = \"" + apiCoverageTestReadonly + "\"\n",
		"internal/gapi/client.go": "package gapi\n\nimport \"net/http\"\n\nfunc get(id string) Call {\n" +
			"\treturn Call{ID: \"gmail.users.x0.get\", Method: http.MethodGet, Path: \"x0/{}\", Args: []string{id}}\n}\n",
		"internal/gapi/client_test.go": "package gapi\n\nvar _ = Call{ID: \"gmail.users.nope.get\"}\n",
	}
	if edit != nil {
		edit(files)
	}
	return writeTree(t, files)
}

var apiCoverageTestFloor = apiCoverageLimits{methods: 6, inScope: 3, calls: 1}

func TestAPICoverageClean(t *testing.T) {
	var out sink
	if err := apiCoverageCheck(apiCoverageFixture(t, nil), apiCoverageTestFloor, &out); err != nil {
		t.Fatalf("a complete record failed: %v\n%s", err, out.String())
	}
	out.mustSay(t, "6 methods (revision 20260101), 1 used, 2 planned (phase 0: 1, phase 1: 1), 3 out "+
		"(1 accept no scope this server requests); 1 client calls in 2 files; current phase 0")
}

func apiCoverageReplace(old, repl string) func(map[string]string) {
	return func(f map[string]string) {
		f[apiCoverageRecordPath] = strings.Replace(f[apiCoverageRecordPath], old, repl, 1)
	}
}

func apiCoverageClient(body string) func(map[string]string) {
	return func(f map[string]string) {
		f["internal/gapi/extra.go"] = "package gapi\n\nimport \"net/http\"\n\nvar _ = http.MethodGet\n\nfunc extra() Call {\n\treturn " + body + "\n}\n"
	}
}

func TestAPICoverageFailures(t *testing.T) {
	cases := []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{"published method with no row", apiCoverageReplace("gmail.users.x4.get\tout\t-\tDeferred (§17): a reason long enough\n", ""),
			"gmail.users.x4.get (GET gmail/v1/users/{userId}/x4/{id}) has no row"},
		{"row for a method that is gone", apiCoverageReplace("# comment\n", "gmail.users.gone.get\tout\t-\tWritten off: long enough reason here\n"),
			"gmail.users.gone.get is not a published method"},
		{"used with no call", apiCoverageReplace("x1.get\tplanned\t0\tUsed", "x1.get\tused\t-\tUsed"),
			"gmail.users.x1.get is used and no gapi.Call names it"},
		{"planned with a call", apiCoverageClient(`Call{ID: "gmail.users.x1.get", Method: "GET", Path: "x1/{}"}`),
			"gmail.users.x1.get is planned and internal/gapi/extra.go"},
		{"call with no used row", apiCoverageClient(`Call{ID: "gmail.users.x3.get", Method: "GET", Path: "x3/{}"}`),
			`gmail.users.x3.get is called and its row says "out"`},
		{"call to an unpublished method", apiCoverageClient(`Call{ID: "gmail.users.nope.get", Method: "GET", Path: "nope"}`),
			`gapi.Call names "gmail.users.nope.get", which is not a published method`},
		{"call with the wrong verb", apiCoverageClient(`Call{ID: "gmail.users.x0.get", Method: http.MethodPost, Path: "x0/{}"}`),
			"gmail.users.x0.get is GET in the snapshot and the call sends POST"},
		{"call with the wrong path", apiCoverageClient(`Call{ID: "gmail.users.x0.get", Method: "GET", Path: "x0"}`),
			`gmail.users.x0.get's path is "x0/{}" under users/me and the call writes "x0"`},
		{"computed id", apiCoverageClient(`Call{ID: "gmail.users." + "x0.get", Method: "GET", Path: "x0/{}"}`),
			"ID is not a string literal"},
		{"planned phase has begun", func(f map[string]string) {
			f["CHANGELOG.md"] = "## [Unreleased]\n\n## [0.1.0] - 2026-01-01\n"
		}, "gmail.users.x1.get was planned for phase 0 and the CHANGELOG says phase 1 has begun"},
		{"planned without a phase", apiCoverageReplace("planned\t1\t", "planned\t-\t"),
			`a planned row names its phase, 0 to 4; got "-"`},
		{"out with a short reason", apiCoverageReplace("Deferred (§17): a reason long enough", "Deferred"),
			"gmail.users.x4.get is out with a reason of 8 characters"},
		{"verdict contradicting the state", apiCoverageReplace("out\t-\tDeferred", "out\t-\tUsed, deferred"),
			"a out row's verdict starts with one of Written off, Deferred"},
		{"unknown state", apiCoverageReplace("x3.get\tout", "x3.get\tmaybe"), `state "maybe" is not one of`},
		{"in scope with no requestable scope", apiCoverageReplace("x5.get\tout\t-\tWritten off: a reason long enough to read",
			"x5.get\tplanned\t2\tUsed: `share_x`"), "gmail.users.x5.get accepts none of the scopes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out sink
			if err := apiCoverageCheck(apiCoverageFixture(t, tc.edit), apiCoverageTestFloor, &out); err == nil {
				t.Fatalf("passed:\n%s", out.String())
			}
			out.mustSay(t, tc.want)
		})
	}
}

func TestAPICoverageFloors(t *testing.T) {
	var out sink
	err := apiCoverageCheck(apiCoverageFixture(t, nil), apiCoverageLimits{methods: 6, inScope: 4, calls: 2}, &out)
	if err == nil {
		t.Fatal("passed below the floors")
	}
	out.mustSay(t, "3 methods used or planned, below the floor of 4")
	out.mustSay(t, "1 client calls, below the floor of 2")
	if err := apiCoverageCheck(apiCoverageFixture(t, nil), apiCoverageFloors, &sink{}); err == nil ||
		!strings.Contains(err.Error(), "below the floor of 79") {
		t.Errorf("a six-method snapshot passed the real floor: %v", err)
	}
}

func TestAPICoveragePhase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CHANGELOG.md")
	if got := apiCoveragePhase(path); got != 0 {
		t.Errorf("no CHANGELOG: phase %d, want 0", got)
	}
	for body, want := range map[string]int{
		"## [Unreleased]\n":                               0,
		"## [Unreleased]\n\n## [0.1.0] - x\n":             1,
		"## [0.4.2] - x\n\n## [0.1.0] - x\n":              4,
		"## [Unreleased]\n\n## [1.0.0] - x\n## [0.4.0]\n": 5,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := apiCoveragePhase(path); got != want {
			t.Errorf("%q: phase %d, want %d", body, got, want)
		}
	}
}

func TestDiscoveryClientPath(t *testing.T) {
	for in, want := range map[string]string{
		"gmail/v1/users/{userId}/profile":                               "profile",
		"gmail/v1/users/{userId}/messages/{messageId}/attachments/{id}": "messages/{}/attachments/{}",
		"gmail/v1/users/{userId}/settings/sendAs/{sendAsEmail}/verify":  "settings/sendAs/{}/verify",
	} {
		if got := discoveryClientPath(in); got != want {
			t.Errorf("discoveryClientPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The committed record against the committed snapshot, with no code:
// every method has exactly one row and nothing is used without a call.
func TestAPICoverageCommittedRecord(t *testing.T) {
	root := filepath.Join("..", "..")
	surface, err := discoveryLoad(filepath.Join(root, discoverySnapshotPath))
	if err != nil {
		t.Skipf("no snapshot: %v", err)
	}
	rows, problems := readTSV(filepath.Join(root, apiCoverageRecordPath), 4)
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
	if len(rows) != len(surface.Methods) {
		t.Errorf("%d rows for %d methods", len(rows), len(surface.Methods))
	}
}
