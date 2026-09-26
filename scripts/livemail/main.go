//go:build live

// Command livemail drives the built server against a real mailbox, to
// check that Gmail agrees with it.
//
// It reads only what it wrote (docs/architecture.md §9.1). A run creates
// a label named for itself, inserts its own synthetic messages under it
// with messages.insert, constrains every read to that label, and at the
// end trashes what it inserted and deletes the label, unless -keep is
// set. Everything it prints goes through the redacting transcript, which
// `gates transcript` holds.
//
//	go run -tags live ./scripts/livemail -profile NAME [-keep] [-run PATTERN]
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/mmedum/google-mail-mcp/scripts/internal/livecover"
	"github.com/mmedum/google-mail-mcp/scripts/internal/mcpstdio"
	"github.com/mmedum/google-mail-mcp/scripts/internal/redact"
	"github.com/mmedum/google-mail-mcp/scripts/internal/transcript"
)

// driverDir is where this source lives, read back at the end of a run to
// compare what the steps claim with what was sent.
const driverDir = "scripts/livemail"

// options are the command line.
type options struct {
	binary  string
	profile string
	keep    bool
	run     *regexp.Regexp
	raw     bool
	seed    int
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	opts, tr, err := parseOptions(args)
	if err != nil {
		transcript.New(redact.NewRedactor(false)).Fail("livemail: %v", err)
		return 2
	}
	ctx := context.Background()
	box, err := openMailbox(ctx, opts.profile)
	if err != nil {
		tr.Fail("livemail: %v", err)
		return 1
	}
	if err := drive(ctx, opts, tr, box); err != nil {
		tr.Fail("livemail: %v", err)
		tr.Say(tr.Summary())
		return 1
	}
	tr.Say(tr.Summary())
	return 0
}

// parseOptions reads the flags. Flag output is discarded and errors are
// returned, so nothing reaches a terminal outside the transcript.
func parseOptions(args []string) (options, *transcript.Transcript, error) {
	fs := flag.NewFlagSet("livemail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	var pattern string
	fs.StringVar(&o.binary, "binary", "./google-mail-mcp", "the built server")
	fs.StringVar(&o.profile, "profile", "", "the signed-in profile to drive (required)")
	fs.BoolVar(&o.keep, "keep", false, "leave the run's label and messages in the mailbox")
	fs.StringVar(&pattern, "run", "", "only steps whose name matches this regular expression")
	fs.BoolVar(&o.raw, "raw", false, "turn redaction off, for a terminal nobody else sees")
	fs.IntVar(&o.seed, "seed", 3, "how many synthetic messages to insert")
	if err := fs.Parse(args); err != nil {
		return o, nil, err
	}
	if o.profile == "" {
		return o, nil, errors.New("-profile is required: the driver never guesses which account to write to")
	}
	if o.seed < 1 {
		return o, nil, errors.New("-seed must be at least 1")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return o, nil, err
	}
	o.run = re
	return o, transcript.New(redact.NewRedactor(o.raw)), nil
}

// drive is one run: seed, step, report, clean up.
func drive(ctx context.Context, o options, tr *transcript.Transcript, box mailbox) (err error) {
	r := newRunLabel(time.Now())
	tr.Sayf("run %s", r.name)

	seed, err := seedMailbox(ctx, box, r, o.seed)
	if err != nil {
		return err
	}
	defer func() {
		if o.keep {
			tr.Sayf("-keep: leaving label %s and %d message(s)", r.name, len(seed.messages))
			return
		}
		if cleanErr := cleanUp(ctx, box, seed); cleanErr != nil {
			tr.Fail("cleanup: %v", cleanErr)
			if err == nil {
				err = cleanErr
			}
		}
	}()

	session, err := mcpstdio.Start(o.binary, "GMAIL_PROFILE="+o.profile)
	if err != nil {
		return err
	}
	defer session.Close()
	protocol, tools, err := session.Initialize("livemail")
	if err != nil {
		return err
	}
	tr.Sayf("protocol %s, %d tools", protocol, len(tools))

	rec := livecover.NewRecorder()
	session.OnCall(rec.Sent)
	e := &env{session: session, tr: tr, seed: seed}

	failed := 0
	for _, s := range steps {
		if !o.run.MatchString(s.name) {
			continue
		}
		if stepErr := e.runStep(s); stepErr != nil {
			failed++
			tr.Sayf("FAIL %s: %v", s.name, stepErr)
			continue
		}
		tr.Sayf("ok   %s", s.name)
	}

	runSpikes(ctx, box, seed, tr)

	believed, srcErr := livecover.FromSource(driverDir, session.Options())
	if srcErr != nil {
		believed = nil
		tr.Sayf("(the driver's source was not found from here, so the report cannot compare: %v)", srcErr)
	}
	tr.Say(rec.Report(session.Options(), believed))
	if failed > 0 {
		return errors.New(plural(failed, "step") + " failed")
	}
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
