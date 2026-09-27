package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
)

// evalsCheckFloor is the fewest canned transcripts the harness must
// score. A self-check that scores nothing passes as easily as one that
// scores everything right.
const evalsCheckFloor = 12

// evalsCheckCommand runs the harness's self-check; a variable so a test
// can replace it.
var evalsCheckCommand = func() ([]byte, error) {
	cmd := exec.Command("go", "run", "-tags", "evals", "./scripts/evals", "-self-check")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

var evalsCheckSummary = regexp.MustCompile(`(?m)^self-check: (\d+) transcripts scored, (\d+) tasks, (\d+) wrong$`)

// evalsCheck runs the evals harness with no model: every canned
// transcript must get its expected verdict.
func evalsCheck(out io.Writer, _ []string) error {
	b, err := evalsCheckCommand()
	return evalsCheckJudge(out, b, err)
}

func evalsCheckJudge(out io.Writer, b []byte, runErr error) error {
	m := evalsCheckSummary.FindSubmatch(b)
	if runErr != nil || m == nil {
		_, _ = out.Write(b)
		if runErr == nil {
			return fmt.Errorf("the self-check printed no summary line")
		}
		return fmt.Errorf("the evals self-check failed: %w", runErr)
	}
	scored, _ := strconv.Atoi(string(m[1]))
	wrong, _ := strconv.Atoi(string(m[3]))
	if wrong != 0 {
		_, _ = out.Write(b)
		return fmt.Errorf("%d canned transcript(s) scored wrong", wrong)
	}
	if scored < evalsCheckFloor {
		return fmt.Errorf("the self-check scored %d transcripts, below the floor of %d", scored, evalsCheckFloor)
	}
	_, _ = fmt.Fprintf(out, "evals-check ok: %d transcripts over %s tasks scored as expected\n", scored, m[2])
	return nil
}
