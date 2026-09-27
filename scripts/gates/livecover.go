package main

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/mmedum/google-mail-mcp/scripts/internal/livecover"
)

// liveCover holds the live driver to the whole tool surface, per option.
//
// Every tool the server publishes must be called by some step, and every
// option of it sent by some step, or be waived in testdata/live-waivers.tsv
// with a verdict and a reason. A waiver whose option a step now sends
// fails, so the record cannot outlive the gap it excused.
//
// The gate reads the driver's source, so a step that exists and never runs
// reads as coverage. The driver's recorder is the other half: it prints,
// at the end of every run, what the source claims and the run did not
// send.
func liveCover(out io.Writer, args []string) error {
	bin, cleanup, err := serverBinary(args)
	if err != nil {
		return err
	}
	defer cleanup()
	dump, _, err := dumpSchemas(bin)
	if err != nil {
		return err
	}
	return liveCoverCheck(out, dump, liveCoverDriver, liveCoverWaivers)
}

const (
	liveCoverDriver  = "scripts/livemail"
	liveCoverWaivers = "testdata/live-waivers.tsv"
	// liveCoverMinTools is the phase 0 read surface; fewer means the
	// gate is not looking at the whole dump.
	liveCoverMinTools = 8
	// liveCoverMaxUndriven is the ceiling on rows that park work a step
	// could do. It only ever goes down.
	liveCoverMaxUndriven = 3
	// liveCoverMinReason is the shortest reason that can say anything.
	liveCoverMinReason = 12
)

// liveCoverWaiver is one row: a tool or tool.option the driver does not
// drive, and why.
type liveCoverWaiver struct {
	verdict string
	line    int
}

func liveCoverCheck(out io.Writer, dump *schemaDump, driverDir, waiversPath string) error {
	known := liveCoverOptions(dump)
	if len(known) < liveCoverMinTools {
		return fmt.Errorf("the dump carries %d tools, want at least %d; this gate is not looking at the "+
			"whole surface", len(known), liveCoverMinTools)
	}
	sent, err := livecover.FromSource(driverDir, known)
	if err != nil {
		return err
	}
	waivers, problems := liveCoverRead(waiversPath)

	total, driven, excused := 0, 0, 0
	for _, tool := range livecover.Sorted(known) {
		_, called := sent[tool]
		toolWaiver, toolWaived := waivers[tool]
		switch {
		case called && toolWaived:
			problems = append(problems, fmt.Sprintf("%s:%d: %s is waived as %s and a step calls it; delete "+
				"the row", waiversPath, toolWaiver.line, tool, toolWaiver.verdict))
		case !called && !toolWaived:
			problems = append(problems, fmt.Sprintf("%s is a tool no step in %s calls, and %s does not say why",
				tool, driverDir, waiversPath))
		}
		for _, option := range known[tool] {
			total++
			name := tool + "." + option
			w, listed := waivers[name]
			switch {
			case sent[tool][option] && listed:
				problems = append(problems, fmt.Sprintf("%s:%d: %s is waived as %s and a step sends it; "+
					"delete the row", waiversPath, w.line, name, w.verdict))
				driven++
			case sent[tool][option]:
				driven++
			case listed || toolWaived:
				excused++
			default:
				problems = append(problems, fmt.Sprintf("%s is an option no step sends, and %s does not say "+
					"why: drive it, or waive it as undrivable (what blocks it) or undriven (what closing it "+
					"takes)", name, waiversPath))
			}
		}
	}
	problems = append(problems, liveCoverStale(waivers, known, waiversPath)...)
	undriven := 0
	for _, w := range waivers {
		if w.verdict == "undriven" {
			undriven++
		}
	}
	if undriven > liveCoverMaxUndriven {
		problems = append(problems, fmt.Sprintf("%d rows are waived as undriven and the ceiling is %d: an "+
			"undriven row is parked work, so the number may not grow", undriven, liveCoverMaxUndriven))
	}
	sort.Strings(problems)
	if err := problemsError(out, "the live driver's coverage", problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "live cover ok (%d of %d options driven across %d tools; %d waived, %d rows "+
		"undriven of a ceiling of %d)\n", driven, total, len(known), excused, undriven, liveCoverMaxUndriven)
	return nil
}

// liveCoverOptions is each tool in the dump with its sorted option names.
func liveCoverOptions(dump *schemaDump) map[string][]string {
	known := map[string][]string{}
	for _, t := range dump.Tools {
		var options []string
		if t.InputSchema != nil {
			for name := range t.InputSchema.Properties {
				options = append(options, name)
			}
		}
		sort.Strings(options)
		known[t.Name] = options
	}
	return known
}

// liveCoverStale reports waiver rows for a tool or option that no longer
// exists: such a row reads as a decision about today's surface.
func liveCoverStale(waivers map[string]liveCoverWaiver, known map[string][]string, waiversPath string) []string {
	var problems []string
	for _, name := range livecover.Sorted(waivers) {
		tool, option, hasOption := strings.Cut(name, ".")
		options, ok := known[tool]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s:%d: %s names a tool that does not exist",
				waiversPath, waivers[name].line, name))
		case hasOption && !slices.Contains(options, option):
			problems = append(problems, fmt.Sprintf("%s:%d: %s names an option %s does not have",
				waiversPath, waivers[name].line, name, tool))
		}
	}
	return problems
}

// liveCoverRead reads the waivers: key, verdict, reason.
func liveCoverRead(path string) (map[string]liveCoverWaiver, []string) {
	rows, problems := readTSV(path, 3)
	out := map[string]liveCoverWaiver{}
	for _, row := range rows {
		name, verdict, reason := row.fields[0], row.fields[1], strings.TrimSpace(row.fields[2])
		switch {
		case verdict != "undrivable" && verdict != "undriven":
			problems = append(problems, fmt.Sprintf("%s:%d: %s has verdict %q; it must be undrivable or undriven",
				path, row.line, name, verdict))
		case len(reason) < liveCoverMinReason:
			problems = append(problems, fmt.Sprintf("%s:%d: %s carries no reason worth the name (%q)",
				path, row.line, name, reason))
		default:
			out[name] = liveCoverWaiver{verdict: verdict, line: row.line}
		}
	}
	return out, problems
}
