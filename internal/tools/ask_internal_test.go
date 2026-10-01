package tools

import (
	"testing"
	"time"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
)

// An answer counts through the last second before its question
// expires, and once: a spent answer stays spent in that second too.
func TestAnAnswerCountsUntilItExpiresAndOnce(t *testing.T) {
	a := newAsking(nil)
	const expires = 1_800_000_000
	state := func(nonce string) string {
		return a.sign(askState{Tool: "send_draft", Args: "args", Nonce: nonce, Expires: expires})
	}
	tests := []struct {
		name   string
		nonce  string
		at     int64
		wantOK bool
	}{
		{"fresh, in its last second", "n1", expires, true},
		{"spent, in its last second", "n1", expires, false},
		{"fresh, a second late", "n2", expires + 1, false},
	}
	for _, tc := range tests {
		_, err := a.redeem(state(tc.nonce), "send_draft", "args", time.Unix(tc.at, 0))
		if tc.wantOK && err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if c, _ := gapi.ClassOf(err); !tc.wantOK && c != gapi.ClassBlocked {
			t.Errorf("%s: err = %v; want [blocked]", tc.name, err)
		}
	}
}
