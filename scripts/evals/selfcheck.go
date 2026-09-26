//go:build evals

package main

import (
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
const selfCheckFloor = 6

// runSelfCheck scores every canned transcript and compares the verdict
// with the expected one. It also requires each task to have a passing
// and a failing transcript, so a scorer that passes or fails everything
// cannot pass.
func runSelfCheck(w printer) int {
	cases, err := loadCanned()
	if err != nil {
		w.Sayf("self-check: %v", err)
		return 1
	}
	byName := map[string]Task{}
	for _, t := range tasks() {
		byName[t.Name] = t
	}
	bad := 0
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
