package tools

import "time"

// SetAskTTL changes how long a question waits for its answer, for a
// test of an answer that comes too late. It returns the undo.
func SetAskTTL(d time.Duration) func() {
	old := askTTL
	askTTL = d
	return func() { askTTL = old }
}
