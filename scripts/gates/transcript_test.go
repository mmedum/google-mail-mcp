package main

import (
	"strings"
	"testing"
)

// transcriptTree is a minimal repository that passes: one driver printing
// through the transcript, and the transcript redacting through one write.
func transcriptTree(overrides map[string]string) map[string]string {
	files := map[string]string{
		"scripts/drv/main.go": `package main

import "example.invalid/scripts/internal/transcript"

func main() { transcript.New().Say("hello") }
`,
		"scripts/internal/redact/redact.go": `package redact

type R struct{}

func (R) Do(s string) string { return s }
`,
		"scripts/internal/transcript/transcript.go": `package transcript

import (
	"fmt"
	"io"
	"os"
)

type T struct{ out, err io.Writer; red interface{ Do(string) string } }

func New() *T { return &T{out: os.Stdout, err: os.Stderr} }

func (t *T) Say(s string) { t.write(t.out, s) }

func (t *T) write(w io.Writer, s string) { _, _ = fmt.Fprintln(w, t.red.Do(s)) }
`,
	}
	for k, v := range overrides {
		if v == "" {
			delete(files, k)
			continue
		}
		files[k] = v
	}
	return files
}

var transcriptTestPackages = []string{"scripts/drv", "scripts/internal/redact"}

func runTranscript(t *testing.T, files map[string]string, packages []string) (string, error) {
	t.Helper()
	root := writeTree(t, files)
	var out sink
	err := transcriptCheck(&out, root, packages, "scripts/internal/transcript")
	return out.String(), err
}

// The floor is one file per package, so the fixture lowers nothing: the
// clean fixture fails only on the floor, which proves the floor bites.
func TestTranscriptFloorOnFiles(t *testing.T) {
	_, err := runTranscript(t, transcriptTree(nil), transcriptTestPackages)
	if err == nil || !strings.Contains(err.Error(), "want at least 5") {
		t.Fatalf("err = %v, want the file floor", err)
	}
}

func TestTranscriptPassesWhenEveryPathRedacts(t *testing.T) {
	files := transcriptTree(nil)
	for _, p := range []string{"a", "b", "c"} {
		files["scripts/drv/"+p+".go"] = "package main\n"
	}
	out, err := runTranscript(t, files, transcriptTestPackages)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "transcript ok (5 files in 2 packages; 2 terminal mentions") {
		t.Errorf("the gate does not say how much it read: %s", out)
	}
}

func TestTranscriptRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override map[string]string
		packages []string
		want     string
	}{
		{"a Println", map[string]string{"scripts/drv/bad.go": "package main\n\nimport \"fmt\"\n\nfunc f() { fmt.Println(1) }\n"},
			nil, "bad.go:5: fmt.Println reaches the terminal"},
		{"os.Stderr handed on", map[string]string{"scripts/drv/bad.go": "package main\n\nimport (\"fmt\"; \"os\")\n\nfunc f() { fmt.Fprintln(os.Stderr, 1) }\n"},
			nil, "os.Stderr reaches the terminal"},
		{"log", map[string]string{"scripts/drv/bad.go": "package main\n\nimport \"log\"\n\nfunc f() { log.Print(1) }\n"},
			nil, "the log package"},
		{"slog", map[string]string{"scripts/drv/bad.go": "package main\n\nimport \"log/slog\"\n\nfunc f() { slog.Info(\"x\") }\n"},
			nil, "the log/slog package"},
		{"the builtin", map[string]string{"scripts/drv/bad.go": "package main\n\nfunc f() { println(1) }\n"},
			nil, "the builtin println"},
		{"an unlisted driver", map[string]string{"scripts/other/main.go": "package main\n\nimport _ \"example.invalid/scripts/internal/mcpstdio\"\n"},
			nil, "scripts/other imports"},
		{"a listed package that is missing", nil,
			append([]string{"scripts/gone"}, transcriptTestPackages...), "scripts/gone is listed and cannot be read"},
		{"an exemption that does not redact", map[string]string{"scripts/internal/transcript/transcript.go": `package transcript

import ("fmt"; "io"; "os")

type T struct{ out io.Writer }

func New() *T { return &T{out: os.Stdout} }

func (t *T) Say(s string) { _, _ = fmt.Fprintln(t.out, s) }
`}, nil, "1 write(s), 0 of them redacting"},
		{"a second write site", map[string]string{"scripts/internal/transcript/extra.go": `package transcript

import ("fmt"; "io")

func (t *T) Raw(w io.Writer, s string) { _, _ = fmt.Fprint(w, t.red.Do(s)) }
`}, nil, "2 write(s), 2 of them redacting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packages := tc.packages
			if packages == nil {
				packages = transcriptTestPackages
			}
			out, err := runTranscript(t, transcriptTree(tc.override), packages)
			if err == nil {
				t.Fatalf("passed:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not say %q:\n%s", tc.want, out)
			}
		})
	}
}
