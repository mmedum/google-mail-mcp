// Package transcript is how a program that drives a real mailbox prints:
// through one function, which redacts.
//
// `gates transcript` forbids fmt.Print*, os.Stdout, os.Stderr, log and
// log/slog in every such program, which leaves write below as the one
// place a line reaches a terminal. So a print that bypasses the redactor
// is not something to remember; the gate refuses it.
package transcript

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mmedum/google-mail-mcp/scripts/internal/redact"
)

// A Transcript is the only way its program writes anything.
type Transcript struct {
	red *redact.Redactor
	out io.Writer
	err io.Writer
}

// New writes the run to the terminal.
func New(red *redact.Redactor) *Transcript {
	return &Transcript{red: red, out: os.Stdout, err: os.Stderr}
}

// NewTo writes somewhere else, which is how a test reads what a run would
// have printed.
func NewTo(red *redact.Redactor, out, err io.Writer) *Transcript {
	return &Transcript{red: red, out: out, err: err}
}

// Say prints one line.
func (t *Transcript) Say(text string) { t.write(t.out, text) }

// Sayf prints one formatted line. The whole formatted line is redacted,
// not only the arguments somebody remembered to wrap.
func (t *Transcript) Sayf(format string, args ...any) { t.write(t.out, fmt.Sprintf(format, args...)) }

// Fail prints the run's own failure, to stderr.
func (t *Transcript) Fail(format string, args ...any) { t.write(t.err, fmt.Sprintf(format, args...)) }

// write is the one place these programs reach a terminal. Every line
// ends in exactly one newline.
func (t *Transcript) write(w io.Writer, text string) {
	_, _ = fmt.Fprintln(w, t.red.Do(strings.TrimRight(text, "\n")))
}

// Summary says what was hidden, and what could not be.
func (t *Transcript) Summary() string { return t.red.Summary() }
