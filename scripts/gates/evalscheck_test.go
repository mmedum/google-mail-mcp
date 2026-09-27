package main

import (
	"errors"
	"strings"
	"testing"
)

func TestEvalsCheckJudge(t *testing.T) {
	var out sink
	if err := evalsCheckJudge(&out, []byte("ok a\nself-check: 15 transcripts scored, 6 tasks, 0 wrong\n"), nil); err != nil {
		t.Fatal(err)
	}
	out.mustSay(t, "15 transcripts over 6 tasks")

	cases := map[string]struct {
		output string
		err    error
		want   string
	}{
		"non-zero exit":  {"self-check: 15 transcripts scored, 6 tasks, 1 wrong\n", errors.New("exit status 1"), "failed"},
		"wrong verdicts": {"self-check: 15 transcripts scored, 6 tasks, 2 wrong\n", nil, "2 canned transcript(s)"},
		"below floor":    {"self-check: 11 transcripts scored, 6 tasks, 0 wrong\n", nil, "below the floor"},
		"no summary":     {"all fine\n", nil, "no summary line"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := evalsCheckJudge(&sink{}, []byte(tc.output), tc.err)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}
