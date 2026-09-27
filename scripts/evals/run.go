//go:build evals

package main

import (
	"context"
	"fmt"
	"strings"
)

// outcome is how one trial of one task ended.
type outcome struct {
	// status is ok, FAIL, UNFINISHED or ERROR. UNFINISHED is a run the
	// CLI stopped before its last word, on turns or budget, and ERROR
	// one that never produced a verdict: neither says anything about
	// the tools.
	status  string
	verdict Verdict
	notes   []string
	run     Run
}

// runTask gives one trial its own mailbox, server and conversation.
func runTask(ctx context.Context, out printer, o cliOptions, t Task, verbose bool) outcome {
	w, err := newWorld(t)
	if err != nil {
		return errored(outcome{}, err)
	}
	defer w.Close()
	run, err := askClaude(ctx, o, t.Prompt, w.URL)
	res := outcome{run: run}
	if verbose {
		for _, c := range run.Calls {
			out.Sayf("      > %s %s", c.Tool, argsLine(c.Args))
		}
		for _, r := range run.Refusals {
			out.Sayf("      ! %s", r)
		}
		if run.Final != "" {
			out.Sayf("      | %s", strings.ReplaceAll(run.Final, "\n", "\n      | "))
		}
	}
	if err != nil {
		return errored(res, err)
	}
	if run.Subtype != "success" {
		res.status = "UNFINISHED"
		res.notes = append(res.notes, "the CLI ended the run: "+run.Subtype)
		return res
	}
	res.verdict = Score(t, run.Transcript)
	res.notes = append(res.notes, res.verdict.Reasons...)
	pass := res.verdict.Pass
	if t.EndState != nil {
		if ok, note := t.EndState(w, run.Transcript); !ok {
			pass = false
			res.notes = append(res.notes, note)
		}
	}
	res.status = "FAIL"
	if pass {
		res.status = "ok"
	}
	return res
}

func errored(o outcome, err error) outcome {
	o.status = "ERROR"
	o.notes = append(o.notes, err.Error())
	return o
}

// runAll scores every selected task over trials and prints one line per
// trial and a total. A task passes only when every trial passes: a
// model that follows a planted instruction one time in five follows it.
func runAll(ctx context.Context, out printer, o cliOptions, only string, trials int, verbose bool) int {
	out.Sayf("evals: scoring %s at %s effort through the claude CLI, %d trial(s) per task", o.model, o.effort, trials)
	var cost float64
	var passed, failed, incomplete, followed int
	for _, t := range tasks() {
		if only != "" && !strings.Contains(t.Name, only) {
			continue
		}
		wins, fails := 0, 0
		for i := range trials {
			r := runTask(ctx, out, o, t, verbose)
			cost += r.run.CostUSD
			if r.verdict.FollowedInjection {
				followed++
			}
			switch r.status {
			case "ok":
				wins++
			case "FAIL":
				fails++
			}
			out.Sayf("%-10s %-20s trial %d: %s (%d turns, %d calls, %d refused, $%.2f)", r.status, t.Name, i+1,
				noteLine(r), r.run.Turns, len(r.run.Calls), r.run.Refused, r.run.CostUSD)
			if ctx.Err() != nil {
				return 2
			}
		}
		// A task with a trial that neither passed nor failed is not
		// counted as a verdict on the tools either way.
		switch {
		case wins == trials:
			passed++
		case fails > 0:
			failed++
		default:
			incomplete++
		}
		out.Sayf("%-10s %-20s %d of %d trials passed", "task", t.Name, wins, trials)
	}
	out.Sayf("%d tasks passed, %d failed, %d incomplete; the planted instruction was followed in %d trial(s); $%.2f spent",
		passed, failed, incomplete, followed, cost)
	if passed+failed+incomplete == 0 {
		out.Fail("evals: no task matched -task %q", only)
		return 2
	}
	if failed > 0 {
		out.Sayf("a failure is more often a tool description than a model; rerun it with -v and read what it tried")
		return 1
	}
	if incomplete > 0 {
		out.Sayf("an incomplete task was stopped by the CLI or never ran; its lines say which, and it scored nothing")
		return 2
	}
	return 0
}

func noteLine(o outcome) string {
	if len(o.notes) == 0 {
		return "passed"
	}
	return strings.Join(o.notes, "; ")
}

// argsLine is a call's arguments on one line, for -v.
func argsLine(args map[string]any) string {
	return clip(fmt.Sprint(args), 160)
}
