//go:build evals

// Command evals scores whether a model reading only this server's tool
// descriptions can do a task through them, and whether mail written to
// steer it does (docs/architecture.md §13).
//
// It runs the real server, over an in-memory transport, against the
// in-memory mailbox of gmailtest only: the score is about the model and
// the tool surface, and nothing here reads a real mailbox or sends mail.
// It costs money and is not deterministic, so it is run by hand:
//
//	ANTHROPIC_API_KEY=... make evals
//
// -self-check runs everything but the model, with no key and no network,
// and is in `make check`.
package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"

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

const usageLine = "usage: evals [-self-check] [-model M] [-task NAME] [-trials N] [-max-turns N] [-v]"

func run(args []string, out printer) int {
	fs := flag.NewFlagSet("evals", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	selfCheck := fs.Bool("self-check", false, "score the canned transcripts and every task's untouched mailbox; no API key")
	model := fs.String("model", defaultModel, "the model to score")
	only := fs.String("task", "", "run only the tasks whose name contains this")
	trials := fs.Int("trials", 1, "run each task this many times; a task passes only if every trial does")
	maxTurns := fs.Int("max-turns", 12, "stop a trial after this many model turns, as UNFINISHED")
	verbose := fs.Bool("v", false, "print every tool call and the model's own words")
	if err := fs.Parse(args); err != nil {
		out.Fail("evals: %v; %s", err, usageLine)
		return 2
	}
	if fs.NArg() > 0 || *trials < 1 || *maxTurns < 1 {
		out.Fail("evals: %s", usageLine)
		return 2
	}
	if *selfCheck {
		return runSelfCheck(out)
	}
	c, err := newClaude(*model)
	if err != nil {
		out.Fail("evals: %v", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runAll(ctx, out, c, *only, *trials, *maxTurns, *verbose)
}
