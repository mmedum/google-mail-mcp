package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The zero value of where is no pipeline. An entry without runsIn would
// otherwise read as something, and parity would pass over it.
func TestEveryCommandDeclaresWhereItRuns(t *testing.T) {
	for name, c := range commands {
		switch c.runsIn {
		case manual, inCheck, inRelease, inCI:
		default:
			t.Errorf("%s declares no pipeline (runsIn %v)", name, c.runsIn)
		}
		if c.run == nil {
			t.Errorf("%s has no run function", name)
		}
		if strings.TrimSpace(c.doc) == "" {
			t.Errorf("%s has no doc line", name)
		}
		if c.maxArgs < c.minArgs {
			t.Errorf("%s: maxArgs %d below minArgs %d", name, c.maxArgs, c.minArgs)
		}
		if c.maxArgs > 0 && c.args == "" {
			t.Errorf("%s takes arguments and does not spell them for the usage text", name)
		}
	}
}

// A floor, so the registry cannot be emptied and still pass the test
// above. The number is the §5a gate list plus the helpers beside it.
func TestRegistryFloor(t *testing.T) {
	if n := len(commands); n < 27 {
		t.Errorf("the registry has %d commands, want at least 27", n)
	}
	if n := len(commandsRunningIn(inCheck)); n < 19 {
		t.Errorf("%d commands run in check, want at least 19", n)
	}
	for _, w := range []where{manual, inRelease, inCI} {
		if len(commandsRunningIn(w)) == 0 {
			t.Errorf("no command runs in %v", w)
		}
	}
}

func TestUsageNamesEveryCommand(t *testing.T) {
	var out sink
	if code := run([]string{"help"}, &out, &sink{}); code != 0 {
		t.Fatalf("help exited %d", code)
	}
	for name := range commands {
		out.mustSay(t, "  "+name)
	}
}

func TestRunRefusesUnknownAndMisArity(t *testing.T) {
	var out, errs sink
	if code := run(nil, &out, &errs); code != 2 {
		t.Errorf("no arguments exited %d, want 2", code)
	}
	if code := run([]string{"no-such-gate"}, &out, &errs); code != 2 {
		t.Errorf("an unknown command exited %d, want 2", code)
	}
	errs.mustSay(t, `unknown command "no-such-gate"`)
	if code := run([]string{"classes", "extra"}, &out, &errs); code != 2 {
		t.Errorf("classes with an argument exited %d, want 2", code)
	}
	if code := run([]string{"changelog"}, &out, &errs); code != 2 {
		t.Errorf("changelog without its arguments exited %d, want 2", code)
	}
}

func TestWriteFileAtomicLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := writeFileAtomic(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("read %q, %v; want two", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("%d entries left in the directory, want only the file", len(entries))
	}
}

func TestReadTSV(t *testing.T) {
	root := writeTree(t, map[string]string{"r.tsv": "# comment\n\na\tone\nb\t\nb\ttwo\nc\n"})
	rows, problems := readTSV(filepath.Join(root, "r.tsv"), 2)
	if len(rows) != 3 {
		t.Errorf("%d rows, want 3 (a, b and the duplicate b)", len(rows))
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"b is listed twice", "want 2 tab-separated columns, got 1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q do not say %q", joined, want)
		}
	}
}

func TestParseDumpRefusesAnEmptySurface(t *testing.T) {
	if _, err := parseDump([]byte(`{"tools":[]}`), "x"); err == nil {
		t.Error("an empty surface was accepted")
	}
	d, err := parseDump([]byte(`{"server":"s","tools":[{"name":"get_profile","inputSchema":{"type":"object"}}],
		"resources":[{"uri":"gmail://labels"}],"resource_templates":[]}`), "x")
	if err != nil || d.Tools[0].Name != "get_profile" || len(d.Resources) != 1 {
		t.Errorf("parseDump = %+v, %v", d, err)
	}
}
