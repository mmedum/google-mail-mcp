package livecover

import (
	"fmt"
	"strings"
)

// A Recorder remembers what a run actually sent.
//
// Reading the source is not the same as watching the run: a step can sit
// in a slice nobody passes on, or behind a condition that was false, and
// the source still says the option is driven. So the driver records every
// call where calls go out, and compares the record with what the source
// claims.
type Recorder struct {
	sent map[string]map[string]bool
	// called counts invocations per tool. It is not derivable from sent:
	// get_profile takes no options, so a run that called it records an
	// empty set.
	called map[string]int
}

// NewRecorder returns a recorder with nothing in it.
func NewRecorder() *Recorder {
	return &Recorder{sent: map[string]map[string]bool{}, called: map[string]int{}}
}

func (r *Recorder) calls() int {
	n := 0
	for _, count := range r.called {
		n += count
	}
	return n
}

// Sent records one tool call with the arguments actually handed to the
// server.
func (r *Recorder) Sent(tool string, args map[string]any) {
	r.called[tool]++
	if r.sent[tool] == nil {
		r.sent[tool] = map[string]bool{}
	}
	for name := range args {
		r.sent[tool][name] = true
	}
}

// Report says what this run drove, and what the source claims it sends
// that this run did not.
//
// published is the surface the server registered for this run, which is
// the fair denominator: a run without the send or destructive flags
// registers none of those tools. believed is what FromSource read.
func (r *Recorder) Report(published map[string][]string, believed map[string]map[string]bool) string {
	var b strings.Builder
	total, driven := 0, 0
	var untouched []string
	for _, tool := range Sorted(published) {
		if r.called[tool] == 0 {
			untouched = append(untouched, tool)
		}
		for _, option := range published[tool] {
			total++
			if r.sent[tool][option] {
				driven++
			}
		}
	}
	fmt.Fprintf(&b, "live cover: this run sent %d of %d options across %d registered tools, in %d calls",
		driven, total, len(published), r.calls())

	var ghosts []string
	for _, tool := range Sorted(believed) {
		if _, registered := published[tool]; !registered || r.called[tool] == 0 {
			continue // a tool never called is named once, below
		}
		for _, option := range Sorted(believed[tool]) {
			if !r.sent[tool][option] {
				ghosts = append(ghosts, tool+"."+option)
			}
		}
	}
	if len(ghosts) > 0 {
		fmt.Fprintf(&b, "\n\n!! %d option(s) the driver's source says it sends were NOT sent by this run.\n"+
			"   Expected for a step filtered out by -run; otherwise it is a step that exists and does not\n"+
			"   run, which `gates live-cover` reads as coverage because the words are there:\n   %s",
			len(ghosts), strings.Join(ghosts, ", "))
	}
	if len(untouched) > 0 {
		fmt.Fprintf(&b, "\n(%d registered tool(s) were not called at all this run: %s.)",
			len(untouched), strings.Join(untouched, ", "))
	}
	return b.String()
}
