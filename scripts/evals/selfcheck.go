//go:build evals

package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"slices"
)

//go:embed testdata/*.json
var canned embed.FS

// cannedCase is a transcript with the verdict it must get.
type cannedCase struct {
	Transcript
	Want struct {
		Pass              bool `json:"pass"`
		FollowedInjection bool `json:"followed_injection"`
	} `json:"want"`
}

// selfCheckFloor is the fewest canned transcripts a self-check may
// score. The evals-check gate reads the count this prints.
const selfCheckFloor = 12

// minOffered is the fewest tools a task's model may be offered: the
// default surface is well over it, and a server that registered nothing
// would otherwise score as a model that did nothing.
const minOffered = 15

// runSelfCheck runs everything but the model. Every canned transcript
// must get its expected verdict, every task needs a passing and a
// failing one, and every task must FAIL on a mailbox nobody touched: a
// scorer that passes a run that did nothing is scoring nothing.
func runSelfCheck(w printer) int {
	cases, err := loadCanned()
	if err != nil {
		w.Sayf("self-check: %v", err)
		return 1
	}
	ctx := context.Background()
	bad := 0
	byName := map[string]Task{}
	for _, t := range tasks() {
		if _, twice := byName[t.Name]; twice {
			w.Sayf("FAIL  task %s is named twice", t.Name)
			bad++
		}
		byName[t.Name] = t
		if err := checkUntouched(ctx, t); err != nil {
			w.Sayf("FAIL  task %s: %v", t.Name, err)
			bad++
			continue
		}
		w.Sayf("ok    task %s fails on an untouched mailbox", t.Name)
	}
	outcomes := map[string][]bool{}
	for _, c := range cases {
		t, ok := byName[c.Task]
		if !ok {
			w.Sayf("FAIL  %s: no task named %q", c.name, c.Task)
			bad++
			continue
		}
		v := Score(t, c.Transcript)
		outcomes[t.Name] = append(outcomes[t.Name], v.Pass)
		if v.Pass != c.Want.Pass || v.FollowedInjection != c.Want.FollowedInjection {
			w.Sayf("FAIL  %s: scored pass=%v injection=%v, want pass=%v injection=%v %v",
				c.name, v.Pass, v.FollowedInjection, c.Want.Pass, c.Want.FollowedInjection, v.Reasons)
			bad++
			continue
		}
		w.Sayf("ok    %s", c.name)
	}
	for _, t := range tasks() {
		o := outcomes[t.Name]
		if !slices.Contains(o, true) || !slices.Contains(o, false) {
			w.Sayf("FAIL  task %s needs a passing and a failing transcript; it has %v", t.Name, o)
			bad++
		}
	}
	w.Sayf("self-check: %d transcripts scored, %d tasks, %d wrong", len(cases), len(tasks()), bad)
	if len(cases) < selfCheckFloor {
		w.Sayf("self-check: fewer than %d transcripts; the harness is not reading them", selfCheckFloor)
		return 1
	}
	if bad > 0 {
		return 1
	}
	return 0
}

// checkUntouched builds the task's world and scores a run that made no
// call and said nothing. It must fail, on the trace or on the mailbox,
// and an end-state scorer must fail on its own. The tool list must be
// the one the task's flags register.
func checkUntouched(ctx context.Context, t Task) error {
	if t.Name == "" || t.Prompt == "" || t.Why == "" || t.MaxCalls == 0 {
		return fmt.Errorf("needs a name, a prompt, a why and a call cap")
	}
	if len(t.MustCall) == 0 && t.Answer == nil && t.EndState == nil && t.Injection == nil {
		return fmt.Errorf("has no scorer")
	}
	if inj := t.Injection; inj != nil && (len(inj.Tools) == 0 || inj.Marker == "") {
		// An empty marker is in every argument, and no tools catch nothing.
		return fmt.Errorf("its injection needs the tools it asks for and a marker")
	}
	w, err := newWorld(ctx, t)
	if err != nil {
		return err
	}
	defer w.Close()
	offered, err := w.offered(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(offered))
	for _, d := range offered {
		names = append(names, d.Name)
	}
	switch {
	case len(offered) < minOffered:
		return fmt.Errorf("the model would be offered %d tools", len(offered))
	case slices.Contains(names, "send_draft") != t.Send:
		return fmt.Errorf("send_draft offered=%v, but the task's Send is %v", !t.Send, t.Send)
	case slices.Contains(names, "delete_permanently"):
		return fmt.Errorf("the destructive tools are registered; no task turns them on")
	case w.Instructions == "":
		return fmt.Errorf("the server gave no instructions")
	}
	if t.EndState != nil {
		if ok, note := t.EndState(w); ok {
			return fmt.Errorf("its end state passes a mailbox nobody touched: %s", note)
		}
	} else if Score(t, Transcript{Task: t.Name}).Pass {
		return fmt.Errorf("an empty run passes")
	}
	return nil
}

type namedCase struct {
	cannedCase
	name string
}

func loadCanned() ([]namedCase, error) {
	entries, err := canned.ReadDir("testdata")
	if err != nil {
		return nil, err
	}
	var out []namedCase
	for _, e := range entries {
		b, err := canned.ReadFile(path.Join("testdata", e.Name()))
		if err != nil {
			return nil, err
		}
		var c cannedCase
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, namedCase{c, e.Name()})
	}
	return out, nil
}
