// Command gates runs this repository's own checks and release helpers.
//
// It is Go rather than shell so that the code holding the gates shut is
// itself held to them: it compiles, it is vetted, it is linted, and it
// has tests. `make check` also runs on Windows in CI, where bash is a
// dependency rather than a given. goreleaser builds only ./cmd/..., so
// none of this ships.
//
//	go run ./scripts/gates help
//	go run ./scripts/gates <name> [args]
package main

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// command is one thing this program does.
type command struct {
	// run receives the arguments after the command's name. It reports
	// through out and returns an error to fail.
	run func(out io.Writer, args []string) error
	// minArgs and maxArgs bound the arguments after the name.
	minArgs, maxArgs int
	// args is how the arguments are spelled in the usage text.
	args string
	doc  string
	// runsIn says which pipeline has to run this command. The parity
	// gate reads it.
	runsIn where
}

// where is the pipeline a command belongs to.
type where int

const (
	// manual is a maintainer convenience; nothing has to run it.
	//
	// It starts at one so that the zero value is no pipeline at all: an
	// entry added without runsIn would otherwise read as manual, and a
	// gate wired into the Makefile but not declared here would leave
	// parity green. TestEveryCommandDeclaresWhereItRuns refuses it.
	manual where = iota + 1
	// inCheck has to run in both `make check` and the CI workflow.
	inCheck
	// inRelease has to run in the release pipeline: the goreleaser
	// config or the release and publish workflows.
	inRelease
	// inCI has to run in the CI workflow and not in `make check`,
	// because it needs what only a pull request has: its base commit.
	inCI
)

func (w where) String() string {
	switch w {
	case manual:
		return "manual"
	case inCheck:
		return "check"
	case inRelease:
		return "release"
	case inCI:
		return "ci"
	}
	return fmt.Sprintf("where(%d)", int(w))
}

// commands is the one list of what this program does. The dispatch, the
// usage text, the parity gate and the staleness gate all read it, so the
// only way to add a command is to add it here.
//
// Filled in init because parity reads the list and the list names
// parity, which Go sees as an initialization cycle.
var commands map[string]command

func init() {
	commands = map[string]command{
		// Code.
		"coverage": {run: coverage, maxArgs: 1, args: "[PROFILE]", runsIn: inCheck,
			doc: "statement coverage per package, scored on its own files"},
		"classes": {run: classes, runsIn: inCheck,
			doc: "the error vocabulary the code emits is the one §6.5 declares, both ways"},
		"outcomes": {run: outcomes, runsIn: inCheck,
			doc: "every write's result states its outcome on every branch"},

		// Confidentiality.
		"leaks": {run: leaksCommand, maxArgs: 1, args: "[history]", runsIn: inCheck,
			doc: "nothing from a real mailbox or account is in the tree; with history, in any commit, message or tag"},
		"transcript": {run: transcript, runsIn: inCheck,
			doc: "the drivers reach a terminal only through the redacting writer"},
		"live-cover": {run: liveCover, maxArgs: 1, args: "[BINARY]", runsIn: inCheck,
			doc: "every tool option is driven live or waived with a reason"},

		// The API.
		"api-coverage": {run: apiCoverage, runsIn: inCheck,
			doc: "every published method used, planned or written off on purpose"},
		"api-fields": {run: apiFields, runsIn: inCheck,
			doc: "every published field modeled or left out on purpose"},
		"api-diff": {run: apiDiff, runsIn: manual,
			doc: "refetch the discovery document into testdata/api-surface.json (needs the network)"},

		// The tool surface.
		"smoke": {run: smoke, maxArgs: 1, args: "[BINARY]", runsIn: inCheck,
			doc: "the built server over stdio, signed out, at two protocol revisions"},
		"schema-diff": {run: schemaDiff, maxArgs: 1, args: "[BINARY]", runsIn: inCheck,
			doc: "the tool and resource surface against the committed baseline"},
		"schema-baseline": {run: schemaBaseline, maxArgs: 1, args: "[BINARY]", runsIn: manual,
			doc: "write the committed baseline from the built binary"},

		// Documents.
		"staleness": {run: staleness, maxArgs: 1, args: "[BINARY]", runsIn: inCheck,
			doc: "the docs held to what the code defines"},
		"checklist": {run: checklist, runsIn: inCheck,
			doc: "CLAUDE.md's definition of done against the Makefile's check: prerequisites"},
		"changelog": {run: changelog, minArgs: 2, maxArgs: 2, args: "BASE HEAD", runsIn: inCI,
			doc: "a pull request adds a CHANGELOG entry unless it is a release cut"},
		"changelog-links": {run: changelogLinks, runsIn: inCheck,
			doc: "every CHANGELOG version heading has its link reference"},

		// The pipeline.
		"pins": {run: pins, runsIn: inCheck,
			doc: "every action and tool held to one exact version, the same everywhere"},
		"parity": {run: parity, runsIn: inCheck,
			doc: "`make check` and CI run the same set"},
		"precommit": {run: precommit, runsIn: manual,
			doc: "the fast gates, run by .githooks/pre-commit"},
		"install-hooks": {run: installHooks, runsIn: manual,
			doc: "point git at .githooks"},
		"evals-check": {run: evalsCheck, runsIn: inCheck,
			doc: "the evals harness scores its canned transcripts, no API key"},

		// Release.
		"release": {run: releaseGate, runsIn: inCheck,
			doc: "goreleaser's config held against the release workflow"},
		"release-notes": {run: releaseNotes, minArgs: 1, maxArgs: 2, args: "VERSION [CHANGELOG]", runsIn: inRelease,
			doc: "one version's CHANGELOG section, verbatim, which is the release note"},
		"mcpb": {run: mcpb, runsIn: inCheck,
			doc: "the committed bundle manifest against its vendored schema and the staging table"},
		"mcpb-pack": {run: mcpbPack, minArgs: 3, maxArgs: 3, args: "DIST VERSION OUT", runsIn: inRelease,
			doc: "pack the Claude Desktop bundle from the binaries goreleaser built"},
		"server-json": {run: serverJSON, maxArgs: 2, args: "[TAG CHECKSUMS]", runsIn: inCheck,
			doc: "the MCP registry entry against the vendored schema and the registry's rules; " +
				"with TAG CHECKSUMS, print the entry to publish"},
		"schema-refetch": {run: schemaRefetch, runsIn: manual,
			doc: "compare the vendored schemas with what upstream serves (needs the network); writes nothing"},
	}
}

// leaksCommand scans the tree, or with "history" every commit, commit
// message and tag. One command, because the Makefile and CI spell the
// history scan as `leaks history`.
func leaksCommand(out io.Writer, args []string) error {
	if len(args) == 0 {
		return leaks(out, nil)
	}
	if args[0] != "history" {
		return fmt.Errorf("unknown mode %q; the only mode is history", args[0])
	}
	return leaksHistory(out, nil)
}

// commandsRunningIn are the commands one pipeline has to run, sorted.
func commandsRunningIn(w where) []string {
	var out []string
	for name, c := range commands {
		if c.runsIn == w {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}
	c, ok := commands[args[0]]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "gates: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	rest := args[1:]
	if len(rest) < c.minArgs || len(rest) > c.maxArgs {
		_, _ = fmt.Fprintf(stderr, "gates: %s takes %s\n", args[0], cmp.Or(c.args, "no arguments"))
		return 2
	}
	if err := c.run(stdout, rest); err != nil {
		_, _ = fmt.Fprintf(stderr, "gates: %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// usage is generated from the command list, so it cannot fall behind it.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, "gates — this repository's own checks\n\nUsage: go run ./scripts/gates <command> [args]\n\n")
	names := slices.Sorted(maps.Keys(commands))
	width := 0
	for _, name := range names {
		width = max(width, len(strings.TrimSpace(name+" "+commands[name].args)))
	}
	for _, name := range names {
		c := commands[name]
		_, _ = fmt.Fprintf(w, "  %-*s  %-7s  %s\n", width, strings.TrimSpace(name+" "+c.args), c.runsIn, c.doc)
	}
}
