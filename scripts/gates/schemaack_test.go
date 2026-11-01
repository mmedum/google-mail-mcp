package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// plain is a released surface: two tools and a resource template.
const plain = `{"version":"v1.0.0","tools":[
	{"name":"modify_labels","annotations":{"destructiveHint":false},
	 "inputSchema":{"type":"object","properties":{"ids":{"type":"string"}}}},
	{"name":"get_message","annotations":{"readOnlyHint":true},
	 "inputSchema":{"type":"object","properties":{"ids":{"type":"string"}}}}],
	"resource_templates":[{"uriTemplate":"gmail://threads/{id}","name":"thread"}]}`

// flipped is plain with modify_labels made destructive, and renamed is
// plain with modify_labels's ids input renamed, which breaks it.
var (
	flipped = strings.Replace(plain, `"destructiveHint":false`, `"destructiveHint":true`, 1)
	renamed = strings.Replace(plain, `"properties":{"ids"`, `"properties":{"message_ids"`, 1)
)

// renameBreak is the break renamed makes of plain.
var renameBreak = []change{{"modify_labels", "modify_labels input: field ids removed", "input ids"}}

// surfaceOf reads a dump the tests write out.
func surfaceOf(t *testing.T, dump string) *schemaDiffSurface {
	t.Helper()
	s, err := schemaDiffDecode([]byte(dump), "the test's dump")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// with is plain with extra keys at its top level.
func with(keys string) string {
	return strings.Replace(plain, `{"version":"v1.0.0",`, `{"version":"v1.0.0",`+keys+`,`, 1)
}

// schema-ack fails a pull request unless its commits name every changed
// item in a footer, and every break of a release in a BREAKING CHANGE:
// footer.
func TestSchemaAckNeedsEveryItemNamed(t *testing.T) {
	relabeled := strings.Replace(plain, `"name":"thread"`, `"name":"a thread"`, 1)
	withoutTemplate := strings.Replace(plain, `{"uriTemplate":"gmail://threads/{id}","name":"thread"}`, ``, 1)
	const (
		wantUnnamed      = "no footer names modify_labels"
		wantUnnamedBreak = "no BREAKING CHANGE: footer names modify_labels"
	)
	for _, tc := range []struct {
		name, base, built, message, wantErr string
	}{
		{"an unchanged surface needs nothing", plain, plain, "Tidy the README", ""},
		{"a change needs a footer", plain, flipped, "Mark modify_labels destructive", wantUnnamed},
		{"a footer naming the tool acknowledges it", plain, flipped, "Mark it\n\nSCHEMA-CHANGE: modify_labels: destructiveHint true", ""},
		{"a footer naming another tool does not", plain, flipped, "Mark it\n\nSCHEMA-CHANGE: get_message: destructiveHint true", wantUnnamed},
		{"a name is a whole token", plain, flipped, "Mark it\n\nSCHEMA-CHANGE: modify_labels_all: destructiveHint true", wantUnnamed},
		{"a continuation line can name it", plain, flipped, "Mark them\n\nSCHEMA-CHANGE: get_message,\n  modify_labels: destructiveHint true", ""},
		{"an empty footer says nothing", plain, flipped, "Mark it\n\nSCHEMA-CHANGE:", wantUnnamed},
		{"the next line is not the footer's text", plain, flipped, "Mark it\n\nSCHEMA-CHANGE:\nCo-Authored-By: modify_labels", wantUnnamed},
		{"a footer must open its line", plain, flipped, "Mark it, see SCHEMA-CHANGE: modify_labels", wantUnnamed},
		{"a line of prose is not a footer", plain, flipped, "Mark it\n\nThe gate wants a\nSCHEMA-CHANGE: modify_labels footer, which this is not.", wantUnnamed},
		{"a subject is not a footer", plain, flipped, "SCHEMA-CHANGE: modify_labels", wantUnnamed},
		{"an indented block is not a footer block", plain, flipped, "Mark it\n\nSCHEMA-CHANGE: modify_labels: hints\n\n    go run ./scripts/gates schema-ack", wantUnnamed},
		{"a footer above the co-author trailer", plain, flipped, "Mark it\n\nSCHEMA-CHANGE: modify_labels: hints\n\nCo-Authored-By: someone <x@example.com>", ""},
		{"a footer above three blank lines", plain, flipped, "Mark it\n\nSCHEMA-CHANGE: modify_labels: hints\n\n\n\nCo-Authored-By: someone <x@example.com>", ""},
		{"a footer beside the co-author trailer", plain, flipped, "Mark it\n\nCo-Authored-By: someone <x@example.com>\nSCHEMA-CHANGE: modify_labels: hints", ""},
		{"a kind is a change to its tool", with(`"kinds":{"modify_labels":"write"}`), with(`"kinds":{"modify_labels":"destructive"}`),
			"Move it behind the destructive switch", wantUnnamed},
		{"a resource template by its URI", plain, relabeled, "Rename it\n\nSCHEMA-CHANGE: gmail://threads/{id}: renamed", ""},
		{"a resource template unnamed", plain, relabeled, "Rename it\n\nSCHEMA-CHANGE: modify_labels: renamed", "no footer names gmail://threads/{id}"},
		{"an instructions change needs a footer", with(`"instructions":{"default":"a"}`), with(`"instructions":{"default":"b"}`),
			"Reword them", "no footer names instructions"},
		{"the instructions by their key", with(`"instructions":{"default":"a"}`), with(`"instructions":{"default":"b"}`),
			"Reword them\n\nSCHEMA-CHANGE: instructions: reworded", ""},
		{"a break named only as a change", plain, renamed, "Rename the field\n\nSCHEMA-CHANGE: modify_labels: ids is message_ids", wantUnnamedBreak},
		{"BREAKING CHANGE: acknowledges a break", plain, renamed, "Rename the field\n\nBREAKING CHANGE: modify_labels: ids is message_ids", ""},
		{"BREAKING-CHANGE: is its synonym", plain, renamed, "Rename the field\n\nBREAKING-CHANGE: modify_labels: ids is message_ids", ""},
		{"SCHEMA-CHANGE: in any case", plain, flipped, "Mark it\n\nSchema-Change: modify_labels: destructiveHint true", ""},
		{"BREAKING CHANGE: only in uppercase", plain, renamed, "Rename the field\n\nBreaking-Change: modify_labels: ids is message_ids", wantUnnamedBreak},
		{"a removed template needs BREAKING CHANGE:", plain, withoutTemplate, "Drop it\n\nSCHEMA-CHANGE: gmail://threads/{id}: gone",
			"no BREAKING CHANGE: footer names gmail://threads/{id}"},
		{"a removed prompt needs BREAKING CHANGE:", with(`"prompts":[{"name":"summarize"}]`), plain,
			"Drop it\n\nSCHEMA-CHANGE: summarize: gone", "no BREAKING CHANGE: footer names summarize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := schemaDiffCompare(surfaceOf(t, tc.base), surfaceOf(t, tc.built))
			err := checkAck(io.Discard, c, []string{"Tidy the docs", tc.message}, "base")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("checkAck = %v, want it to pass", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("checkAck = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// The fix a refusal names is the footer that fixes it: BREAKING CHANGE:
// for an unnamed break, SCHEMA-CHANGE: for the rest, each naming its
// items, and a message that ends with it passes.
func TestARefusalNamesTheFooterThatFixesIt(t *testing.T) {
	both := strings.Replace(renamed, `"name":"thread"`, `"name":"a thread"`, 1)
	for _, tc := range []struct{ name, built, wantFix string }{
		{"a change", flipped, "SCHEMA-CHANGE: modify_labels: what changed"},
		{"a break", renamed, "BREAKING CHANGE: modify_labels: what broke"},
		{"a break and a change", both, "BREAKING CHANGE: modify_labels: what broke\nSCHEMA-CHANGE: gmail://threads/{id}: what changed"},
		{"a break and a change to one tool", strings.Replace(renamed, `"destructiveHint":false`, `"destructiveHint":true`, 1),
			"BREAKING CHANGE: modify_labels: what broke"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := schemaDiffCompare(surfaceOf(t, plain), surfaceOf(t, tc.built))
			err := checkAck(io.Discard, c, []string{"Change it"}, "base")
			if err == nil {
				t.Fatal("checkAck passed with no footer")
			}
			_, fix, _ := strings.Cut(err.Error(), "\n\n")
			if fix != tc.wantFix {
				t.Errorf("the refusal's fix is %q, want %q", fix, tc.wantFix)
			}
			if err := checkAck(io.Discard, c, []string{"Change it", "Name it\n\n" + fix}, "base"); err != nil {
				t.Errorf("checkAck = %v after the fix it named", err)
			}
		})
	}
}

// A name in backticks is taken whole, so a URI with a comma in it can be
// named; split on its comma, it names nothing.
func TestAFooterCanNameAURIWithACommaInBackticks(t *testing.T) {
	const tmpl = "gmail://threads/{id}{?format,page}"
	was := strings.Replace(plain, `"gmail://threads/{id}"`, `"`+tmpl+`"`, 1)
	now := strings.Replace(was, `"name":"thread"`, `"name":"a thread"`, 1)
	c := schemaDiffCompare(surfaceOf(t, was), surfaceOf(t, now))
	if err := checkAck(io.Discard, c, []string{"Rename\n\nSCHEMA-CHANGE: `" + tmpl + "`: renamed"}, "base"); err != nil {
		t.Errorf("checkAck = %v, want the backticked URI to name the template", err)
	}
	if err := checkAck(io.Discard, c, []string{"Rename\n\nSCHEMA-CHANGE: " + tmpl + ": renamed"}, "base"); err == nil {
		t.Error("checkAck passed with the URI split on its comma, which names nothing")
	}
}

// The fix a refusal prints puts a name the footer would split or trim in
// backticks, so the footer it prints is one that passes.
func TestARefusalQuotesANameTheFooterWouldSplit(t *testing.T) {
	for _, uri := range []string{"gmail://threads/{id}{?format,page}", "gmail://threads/{id}."} {
		t.Run(uri, func(t *testing.T) {
			was := strings.Replace(plain, `"gmail://threads/{id}"`, `"`+uri+`"`, 1)
			c := schemaDiffCompare(surfaceOf(t, was), surfaceOf(t, strings.Replace(was, `"name":"thread"`, `"name":"a thread"`, 1)))
			err := checkAck(io.Discard, c, []string{"Rename it"}, "base")
			_, fix, _ := strings.Cut(fmt.Sprint(err), "\n\n")
			if want := "SCHEMA-CHANGE: `" + uri + "`: what changed"; fix != want {
				t.Fatalf("the refusal's fix is %q, want %q", fix, want)
			}
			if err := checkAck(io.Discard, c, []string{"Rename it", "Name it\n\n" + fix}, "base"); err != nil {
				t.Errorf("checkAck = %v after the fix it named", err)
			}
		})
	}
}

// A break of the released surface needs BREAKING CHANGE:; removing a tool
// added since the release breaks no caller, so it is a plain change, and
// the report lists it as one.
func TestABreakIsABreakOfARelease(t *testing.T) {
	release := surfaceOf(t, plain)
	base := surfaceOf(t, strings.Replace(plain, `"tools":[`, `"tools":[{"name":"draft_tool"},`, 1))
	head := surfaceOf(t, renamed)
	c := releaseBreaksOnly(schemaDiffCompare(base, head), schemaDiffCompare(release, head).breaking)
	if !slices.Equal(c.breaking, renameBreak) {
		t.Errorf("breaking = %v, want %v", c.breaking, renameBreak)
	}
	var report strings.Builder
	footer := "Drop and rename\n\nSCHEMA-CHANGE: draft_tool: never released\nBREAKING CHANGE: modify_labels: ids is message_ids"
	if err := checkAck(&report, c, []string{footer}, "base"); err != nil {
		t.Errorf("checkAck = %v, want both acknowledged", err)
	}
	want := "against the merge-base base: 0 added, 2 changed, 1 breaking\n" +
		"  look at this: modify_labels: inputSchema changed\n" +
		"  look at this: tool draft_tool removed\n" +
		"  BREAKING: modify_labels input: field ids removed\n" +
		"the change since base is acknowledged\n"
	if report.String() != want {
		t.Errorf("the report is\n%s\nwant\n%s", report.String(), want)
	}
}

// A tool the base branch already broke since the release, in one field,
// does not make a pull request's break of another field, one no release
// shipped, a break of the release.
func TestABreakOfTheReleaseIsMatchedByItsField(t *testing.T) {
	withInputs := func(props string) *schemaDiffSurface {
		return surfaceOf(t, strings.Replace(plain, `"properties":{"ids":{"type":"string"}}`, `"properties":{`+props+`}`, 1))
	}
	release := withInputs(`"ids":{"type":"string"},"note":{"type":"string"}`)
	base := withInputs(`"ids":{"type":"string"},"draft":{"type":"string"}`)
	head := withInputs(`"ids":{"type":"string"}`)
	c := releaseBreaksOnly(schemaDiffCompare(base, head), schemaDiffCompare(release, head).breaking)
	if len(c.breaking) != 0 {
		t.Errorf("breaking = %v, want draft, which no release shipped, as a change", c.breaking)
	}
}

// A break of the release is matched by its field, not its text. Here the
// release takes a string, the merge-base widened that to an integer as
// well, and the head takes only an integer: the two breaks read
// differently, and both break the same field.
func TestABreakOfTheReleaseIsNotMatchedByItsText(t *testing.T) {
	typed := func(typ string) *schemaDiffSurface {
		return surfaceOf(t, strings.Replace(plain, `"properties":{"ids":{"type":"string"}}`,
			`"properties":{"ids":{"type":`+typ+`}}`, 1))
	}
	release, base, head := typed(`"string"`), typed(`["integer","string"]`), typed(`"integer"`)
	c := releaseBreaksOnly(schemaDiffCompare(base, head), schemaDiffCompare(release, head).breaking)
	if len(c.breaking) != 1 || c.breaking[0].item != "modify_labels" {
		t.Errorf("breaking = %v, want modify_labels's narrowed input kept as a break of the release", c.breaking)
	}
}

// The release is the baseline the base branch records. Before the first
// release there is none, and nothing breaks one.
func TestReleasedBreaksReadTheBaseBranchBaseline(t *testing.T) {
	dir := gitRepo(t, map[string]string{"README.md": "x\n"})
	t.Chdir(dir)
	head := surfaceOf(t, renamed)

	breaks, err := releasedBreaks("HEAD", head)
	if err != nil || len(breaks) != 0 {
		t.Fatalf("releasedBreaks with no baseline = %v, %v; want none", breaks, err)
	}

	put := func(body string) {
		t.Helper()
		writeFiles(t, dir, map[string]string{schemaDiffBaseline: body})
	}
	put(plain)
	gitIn(t, dir, "add", filepath.FromSlash(schemaDiffBaseline))
	gitIn(t, dir, "commit", "-qm", "Release")
	// The working tree's baseline is a branch's, not the release's.
	put(`{"tools":[{"name":"other"}]}`)
	breaks, err = releasedBreaks("HEAD", head)
	if err != nil || !slices.Equal(breaks, renameBreak) {
		t.Errorf("releasedBreaks = %v, %v; want %v", breaks, err, renameBreak)
	}
}

// A gate that could not run says so and exits 2; a refusal exits 1. The
// message is what tells them apart in CI, where `go run` exits 1 for
// both.
func TestSchemaAckTellsARefusalFromAFailure(t *testing.T) {
	refused := checkAck(io.Discard, schemaDiffCompare(surfaceOf(t, plain), surfaceOf(t, flipped)), nil, "base")
	if refused == nil || errors.As(refused, new(couldNotRun)) {
		t.Errorf("a refusal (%v) reads as a gate that could not run", refused)
	}
	t.Chdir(gitRepo(t, map[string]string{"README.md": "x\n"}))
	// git's own words, which a locale would translate.
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANGUAGE", "")
	var stdout, stderr strings.Builder
	if code := run([]string{"schema-ack", "no-such-ref", "HEAD"}, &stdout, &stderr); code != 2 {
		t.Errorf("schema-ack with no base exits %d, want 2: %s", code, stderr.String())
	}
	const want = "gates: schema-ack: could not run: git merge-base no-such-ref HEAD: exit status 128: fatal: Not a valid object name no-such-ref\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}
