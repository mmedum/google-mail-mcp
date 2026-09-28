//go:build evals

// Command evals scores whether a model reading only this server's tool
// descriptions can do a task through them, and whether mail written to
// steer it does (docs/architecture.md §13).
//
// It serves the real server against the in-memory mailbox of gmailtest
// only, and gives each task to a model through `claude -p`, signed in as
// whoever runs it: the score is about the model and the tool surface,
// and nothing here reads a real mailbox or sends mail. It costs money
// and is not deterministic, so it is run by hand:
//
//	make evals EVAL_ARGS='-trials 3'
//
// -self-check runs everything but the model, with no CLI and no
// network, and is in `make check`.
package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"

	"github.com/mmedum/google-mail-mcp/v2/scripts/internal/redact"
	"github.com/mmedum/google-mail-mcp/v2/scripts/internal/transcript"
)

func main() { os.Exit(run(os.Args[1:], transcript.New(redact.NewRedactor(false)))) }

// printer is how this program writes: the redacting transcript, which
// the transcript gate holds as the only way out.
type printer interface {
	Sayf(format string, args ...any)
	Fail(format string, args ...any)
}

const (
	usageLine = "usage: evals [-self-check] [-model M] [-effort E] [-budget USD] [-task NAME] [-trials N] [-v]"
	// defaultModel is set rather than left to the CLI's default, so a
	// score does not move because the default did; so is the effort.
	defaultModel  = "claude-opus-5-5"
	defaultEffort = "high"
)

func run(args []string, out printer) int {
	fs := flag.NewFlagSet("evals", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	selfCheck := fs.Bool("self-check", false, "score the canned transcripts and every task's untouched mailbox; no model")
	model := fs.String("model", defaultModel, "the model to score, as `claude --model` takes it")
	effort := fs.String("effort", defaultEffort, "the effort level, as `claude --effort` takes it")
	budget := fs.Float64("budget", 1.0, "the most one trial may spend, in US dollars")
	only := fs.String("task", "", "run only the tasks whose name contains this")
	trials := fs.Int("trials", 1, "run each task this many times; a task passes only if every trial does")
	verbose := fs.Bool("v", false, "print every tool call and the model's answer")
	if err := fs.Parse(args); err != nil {
		out.Fail("evals: %v; %s", err, usageLine)
		return 2
	}
	if fs.NArg() > 0 || *trials < 1 || *budget <= 0 {
		out.Fail("evals: %s", usageLine)
		return 2
	}
	if *selfCheck {
		return runSelfCheck(out)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runAll(ctx, out, cliOptions{model: *model, effort: *effort, budget: *budget}, *only, *trials, *verbose)
}
