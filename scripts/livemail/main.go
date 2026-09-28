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
//	go run -tags live ./scripts/livemail -profile NAME [-keep] [-run PATTERN] [-send-to ADDRESS] [-spikes-de] [-spike-h]
//
// Nothing is sent unless -send-to names the maintainer's second address;
// then one send_draft step sends to it, and spikes D and E too with
// -spikes-de, and to nothing else. The server runs with sending and permanent deletion registered;
// the delete steps run only when the profile's login granted
// https://mail.google.com/, and are skipped, saying so, otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	netmail "net/mail"
	"os"
	"regexp"
	"slices"
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
	// sendTo is the one address spikes D and E and the send step may
	// send to: the maintainer's second address, given on the command line
	// and never committed (§14). Without it, nothing is sent.
	sendTo string
	// spikeH floods the run's own message with reads until Gmail rate
	// limits them, for spike H.
	spikeH bool
	// spikesDE sends spikes D and E again; they were answered in phase 2.
	spikesDE bool
	// clean names a stranded run whose drafts and messages to remove,
	// instead of running.
	clean string
}

// runName is the shape newRunLabel gives a run; -clean takes nothing
// else, so it can only ever name a run's own mail.
var runName = regexp.MustCompile(`^livemail-[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

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
	if opts.clean != "" {
		if err := cleanStranded(ctx, box, opts.clean, tr); err != nil {
			tr.Fail("livemail: %v", err)
			return 1
		}
		return 0
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
	fs.StringVar(&o.sendTo, "send-to", "", "the maintainer's second address, which spikes D and E and the send step send to; none sends without it")
	fs.StringVar(&o.clean, "clean", "", "remove the drafts and messages a stranded run left, by its name, and exit")
	fs.BoolVar(&o.spikesDE, "spikes-de", false, "send spikes D and E again, to -send-to; answered in phase 2")
	fs.BoolVar(&o.spikeH, "spike-h", false, "read the run's own message until Gmail rate limits the reads, for spike H")
	if err := fs.Parse(args); err != nil {
		return o, nil, err
	}
	if o.profile == "" {
		return o, nil, errors.New("-profile is required: the driver never guesses which account to write to")
	}
	if o.seed < 3 {
		return o, nil, errors.New("-seed must be at least 3: the steps use three inserted messages")
	}
	if o.clean != "" && !runName.MatchString(o.clean) {
		return o, nil, errors.New("-clean takes a run's name, as livemail-20260927-101500-a1b2c3")
	}
	if o.sendTo != "" {
		if a, err := netmail.ParseAddress(o.sendTo); err != nil || a.Address != o.sendTo {
			return o, nil, errors.New("-send-to must be one bare address")
		}
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

	// The run's own directory for download_attachment and the files the
	// write steps attach, removed after.
	localDir, err := os.MkdirTemp("", r.name+"-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(localDir) }()
	if err := writeLocalFiles(localDir, r); err != nil {
		return err
	}

	// The settings steps change the account's own signature and vacation
	// reply. They run only when the login granted their scope, and what
	// they change is saved first and put back after, before the run's
	// label goes: a filter the run left names it.
	serverEnv := []string{"GMAIL_PROFILE=" + o.profile, "GMAIL_LOCAL_DIR=" + localDir,
		"GMAIL_ENABLE_SEND=true", "GMAIL_ENABLE_DESTRUCTIVE=true"}
	var saved savedSettings
	if box.SettingsScope() {
		var saveErr error
		if saved, saveErr = box.SaveSettings(ctx); saveErr != nil {
			return saveErr
		}
		serverEnv = append(serverEnv, "GMAIL_ENABLE_SETTINGS=true")
		defer func() {
			n, filtersErr := box.DeleteFiltersFrom(ctx, filterSender(seed.label))
			if n > 0 {
				tr.Sayf("cleanup: deleted %d filter(s) a step left", n)
			}
			if restoreErr := errors.Join(filtersErr, box.RestoreSettings(ctx, saved)); restoreErr != nil {
				tr.Fail("restore the signature and the vacation reply: %v", restoreErr)
				if err == nil {
					err = restoreErr
				}
			} else {
				tr.Sayf("restored the signature and the vacation reply as the run found them")
			}
		}()
	}
	session, err := mcpstdio.Start(o.binary, serverEnv...)
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
	e := &env{ctx: ctx, session: session, tr: tr, seed: seed, localDir: localDir, sendTo: o.sendTo, full: box.FullScope(),
		settings: box.SettingsScope(), signatureAddress: saved.address}

	failed := 0
	for _, s := range slices.Concat(steps, writeSteps, gatedSteps, settingsSteps) {
		if !o.run.MatchString(s.name) {
			continue
		}
		if s.needs != nil {
			if why := s.needs(e); why != "" {
				tr.Sayf("skip %s: %s", s.name, why)
				continue
			}
		}
		if stepErr := e.runStep(s); stepErr != nil {
			failed++
			tr.Sayf("FAIL %s: %v", s.name, stepErr)
			continue
		}
		tr.Sayf("ok   %s", s.name)
	}

	runSpikes(ctx, spikeRun{box: box, s: seed, sendTo: o.sendTo, account: e.account, draftSide: e.spikeE, full: e.full,
		sent: e.sent, spikeH: o.spikeH, spikesDE: o.spikesDE}, tr)

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

// cleanStranded removes what a run left when its cleanup failed: its
// drafts, deleted, and its messages, trashed. They are found by the
// run's name, which only the run's own subjects carry. This is the one
// search the driver makes.
func cleanStranded(ctx context.Context, box *restMailbox, name string, tr *transcript.Transcript) error {
	q := `subject:"` + name + `"`
	drafts, err := box.Find(ctx, q, true)
	if err != nil {
		return err
	}
	for _, id := range drafts {
		if err := box.DeleteDraft(ctx, id); err != nil {
			return err
		}
	}
	messages, err := box.Find(ctx, q, false)
	if err != nil {
		return err
	}
	for _, id := range messages {
		if err := box.Trash(ctx, id); err != nil {
			return err
		}
	}
	tr.Sayf("clean %s: deleted %d draft(s), trashed %d message(s)", name, len(drafts), len(messages))
	return nil
}
