package main

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

// classesFixtureNames are the fixture's classes; the last is planned.
var classesFixtureNames = []string{"invalid", "not_found", "auth", "stale"}

// classesFixture is a module with four classes: three emitted from
// three files, one planned. edit changes it before it is written.
func classesFixture(t *testing.T, edit func(files map[string]string)) string {
	t.Helper()
	var consts, list, table strings.Builder
	for _, v := range classesFixtureNames {
		name := "Class" + strings.ReplaceAll(v, "_", "")
		fmt.Fprintf(&consts, "\t%s Class = %q\n", name, v)
		fmt.Fprintf(&list, "%s, ", name)
		fmt.Fprintf(&table, "| `%s` | means | do |\n", v)
	}
	files := map[string]string{
		"internal/gapi/errors.go": "package gapi\n\ntype Class string\n\nconst (\n" + consts.String() + ")\n\n" +
			"var Classes = []Class{" + list.String() + "}\n\n" +
			"func (c Class) Retryable() bool { return c == Classstale || c == Classinvalid }\n\n" +
			"type Error struct{ Class Class }\n",
		"internal/gapi/a.go":         "package gapi\n\nfunc a() *Error { return &Error{Class: Classinvalid} }\n",
		"internal/service/b.go":      "package service\n\nimport \"x/gapi\"\n\nfunc b() gapi.Class { return gapi.Classnotfound }\n",
		"internal/service/c.go":      "package service\n\nimport \"x/gapi\"\n\nfunc c() any { return gapi.Classauth }\n",
		"internal/service/c_test.go": "package service\n\nimport \"x/gapi\"\n\nvar _ = gapi.Classstale\n",
		"scripts/x/x.go":             "package x\n\nimport \"x/gapi\"\n\nvar _ = gapi.Class(\"made_up\")\n",
		"docs/architecture.md":       "# A\n\n### 6.5 Error classes\n\n| Class | Means | Do |\n|---|---|---|\n" + table.String() + "\n## 7\n\n| `nope` | x | y |\n",
	}
	if edit != nil {
		edit(files)
	}
	return writeTree(t, files)
}

var classesFixturePlanned = map[string]classesPlan{"stale": {2, "the draft witness arrives in phase 2"}}

var classesFixtureFloor = classesLimits{declared: 4, files: 4}

func TestClassesClean(t *testing.T) {
	var out sink
	if err := classesCheck(classesFixture(t, nil), classesFixturePlanned, classesFixtureFloor, &out); err != nil {
		t.Fatalf("a closed vocabulary failed: %v\n%s", err, out.String())
	}
	out.mustSay(t, "classes ok: 4 declared, 3 emitted, 1 planned, in 4 files")
}

// Each way the vocabulary opens, one at a time.
func TestClassesFailures(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(map[string]string)
		planned map[string]classesPlan
		want    string
	}{
		{"planned class now emitted", func(f map[string]string) {
			f["internal/service/d.go"] = "package service\n\nimport \"x/gapi\"\n\nvar d = func() any { return gapi.Classstale }\n"
		}, nil, `"stale" is planned and internal/service/d.go emits it`},
		{"declared, neither emitted nor planned", nil, map[string]classesPlan{},
			`"stale" is declared and nothing emits it`},
		{"planned but not declared", nil, map[string]classesPlan{
			"stale": {2, "r"}, "ghost": {3, "reason"}}, `"ghost" is planned and not declared`},
		{"planned phase has begun", func(f map[string]string) {
			f["CHANGELOG.md"] = "# Changelog\n\n## [Unreleased]\n\n## [0.3.0] - 2026-01-01\n"
		}, nil, `"stale" was planned for phase 2 and phase 3 has begun`},
		{"missing from §6.5", func(f map[string]string) {
			f["docs/architecture.md"] = strings.Replace(f["docs/architecture.md"], "| `auth` | means | do |\n", "", 1)
		}, nil, `"auth" is declared and §6.5 does not tabulate it`},
		{"extra in §6.5", func(f map[string]string) {
			f["docs/architecture.md"] = strings.Replace(f["docs/architecture.md"], "\n## 7", "| `gone` | x | y |\n\n## 7", 1)
		}, nil, `§6.5 tabulates "gone" and the code declares no such class`},
		{"listed twice, not adjacent", func(f map[string]string) {
			f["internal/gapi/errors.go"] = strings.Replace(f["internal/gapi/errors.go"],
				"var Classes = []Class{", "var Classes = []Class{Classauth, ", 1)
		}, nil, `Classes names "auth" 2 times`},
		{"missing from Classes", func(f map[string]string) {
			f["internal/gapi/errors.go"] = strings.Replace(f["internal/gapi/errors.go"], "Classnotfound, ", "", 1)
		}, nil, `"not_found" is declared and Classes does not list it`},
		{"a literal class", func(f map[string]string) {
			f["internal/service/e.go"] = "package service\n\nimport \"x/gapi\"\n\nvar e = func() any { return gapi.Class(\"made_up\") }\n"
		}, nil, `internal/service/e.go emits "made_up", which is not a declared class`},
		{"a literal in an Error", func(f map[string]string) {
			f["internal/gapi/a.go"] = "package gapi\n\nfunc a() *Error { return &Error{Class: \"oops\"} }\nvar _ = Classinvalid\n"
		}, nil, `emits "oops", which is not a declared class`},
		{"a comparison is not emission", func(f map[string]string) {
			f["internal/service/b.go"] = "package service\n\nimport \"x/gapi\"\n\nfunc b(c gapi.Class) bool { return c == gapi.Classnotfound }\n"
		}, nil, `"not_found" is declared and nothing emits it`},
		{"a case label is not emission", func(f map[string]string) {
			f["internal/service/b.go"] = "package service\n\nimport \"x/gapi\"\n\nfunc b(c gapi.Class) {\n\tswitch c {\n\tcase gapi.Classnotfound:\n\t}\n}\n"
		}, nil, `"not_found" is declared and nothing emits it`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planned := tc.planned
			if planned == nil {
				planned = maps.Clone(classesFixturePlanned)
			}
			var out sink
			err := classesCheck(classesFixture(t, tc.edit), planned, classesFixtureFloor, &out)
			if err == nil {
				t.Fatalf("passed:\n%s", out.String())
			}
			out.mustSay(t, tc.want)
		})
	}
}

// The real floors refuse a module this small: "looked at nothing" cannot
// pass for "found nothing".
func TestClassesFloors(t *testing.T) {
	var out sink
	if err := classesCheck(classesFixture(t, nil), classesFixturePlanned, classesFloors, &out); err == nil {
		t.Fatal("the fixture passed the real floors")
	}
	out.mustSay(t, "4 classes declared, below the floor of 12")
	out.mustSay(t, "read 4 Go files, below the floor of 5")
}

// The committed plan names only declared classes with a reason; the
// gate's own run checks the rest against the code.
func TestClassesPlannedIsArgued(t *testing.T) {
	for v, p := range classesPlanned {
		if len(p.reason) < 20 || p.phase < 0 || p.phase > 4 {
			t.Errorf("planned %q: phase %d, reason %q", v, p.phase, p.reason)
		}
	}
}
