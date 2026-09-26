//go:build evals

// Command evals scores a model against the tool surface, over the
// in-memory mailbox of gmailtest only.
//
// Phase 0 ships the harness's scorer and -self-check, which scores
// canned transcripts against their expected verdicts with no model, no
// API key and no network. The model-driven run arrives in phase 4.
//
//	go run -tags evals ./scripts/evals -self-check
package main

import (
	"flag"
	"io"
	"os"

	"github.com/mmedum/google-mail-mcp/scripts/internal/redact"
	"github.com/mmedum/google-mail-mcp/scripts/internal/transcript"
)

func main() { os.Exit(run(os.Args[1:], transcript.New(redact.NewRedactor(false)))) }

// printer is how this program writes: the redacting transcript, which
// the transcript gate holds as the only way out.
type printer interface {
	Sayf(format string, args ...any)
	Fail(format string, args ...any)
}

func run(args []string, out printer) int {
	fs := flag.NewFlagSet("evals", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	selfCheck := fs.Bool("self-check", false, "score the canned transcripts against their expected verdicts; no API key")
	if err := fs.Parse(args); err != nil {
		out.Fail("evals: %v; usage: evals -self-check", err)
		return 2
	}
	if *selfCheck {
		return runSelfCheck(out)
	}
	out.Fail("evals: the model-driven run arrives in phase 4; use -self-check")
	return 2
}
