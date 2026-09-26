package mime

import (
	"strings"
	"testing"
	"time"
)

// finishesWithin fails when fn runs longer than d. The bound is far
// above what a linear pass needs, even under -race, and far below what
// a quadratic one takes on the same input.
func finishesWithin(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not finish within %v", d)
	}
}

func TestStripInvisibleLongRun(t *testing.T) {
	for name, in := range map[string]string{
		"zero-width spaces": "a" + strings.Repeat("\u200B", 300_000) + "b",
		"joiners":           "é" + strings.Repeat("\u200D", 300_000) + "a",
	} {
		t.Run(name, func(t *testing.T) {
			finishesWithin(t, 5*time.Second, func() {
				out, n := StripInvisible(in)
				if n != 300_000 || strings.ContainsRune(out, 0x200B) {
					t.Errorf("stripped %d, left %d bytes", n, len(out))
				}
			})
		})
	}
}

func TestFindSpansManySpans(t *testing.T) {
	for name, in := range map[string]string{
		"alternating quotes":  "reply\n" + strings.Repeat("> q\ntext\n", 100_000),
		"attributed quotes":   "reply\n" + strings.Repeat("On Monday Ada wrote:\n> q\n\n", 20_000),
		"leading blank lines": strings.Repeat("\n", 200_000) + "text\n> q\n",
		"all quote, no reply": strings.Repeat("On Monday Ada wrote:\n> q\n\n", 20_000),
	} {
		t.Run(name, func(t *testing.T) {
			finishesWithin(t, 10*time.Second, func() { FindSpans(in) })
		})
	}
}
