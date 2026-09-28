// Package render turns the model into the text a person and a model
// read.
//
// Two rules run through everything here (docs/architecture.md §4.1,
// §4.8):
//
//   - Mail content is data. Every field a sender wrote — subject, names,
//     addresses, body, snippet, attachment names, parameters — is printed
//     inside a delimited block that names its origin, closed by a
//     boundary token drawn per call so the content cannot close the block
//     itself. What the server says about the content (hidden text
//     removed, links whose text names another site, what was collapsed or
//     cut) is printed outside the blocks, as statements of fact. Nothing
//     the server writes is phrased as an instruction taken from the mail.
//   - A read never silently returns less than it found. Every read has
//     a budget in characters, states it, and names each omission with a
//     way to continue.
//
// The first rule is held by types, not by care: the writer's only way
// to put text outside a block is say, which takes a constant phrase and
// parts, and every part constructor takes a value a sender cannot shape
// (see writer.go).
package render

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// DefaultBudget is the character budget of a read when none is given.
// About 6,000 tokens: a thread read leaves room in a client capped at
// 25,000 (§3.8).
const DefaultBudget = 24000

// MinBudget keeps a tiny budget from producing a read with no content.
const MinBudget = 2000

// TokenSource draws a boundary token. Tests inject a fixed one.
type TokenSource func() string

// RandomToken is 16 random hex digits.
func RandomToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Options shape a read.
type Options struct {
	// Budget in characters; 0 means DefaultBudget.
	Budget int
	// Tokens draws the boundary token; nil means RandomToken.
	Tokens TokenSource
	// Location renders dates; nil means UTC.
	Location *time.Location
	// AllHeaders shows every header rather than the fixed set (§7.2).
	AllHeaders bool
	// ShowQuoted keeps quoted text and signatures instead of collapsing
	// them.
	ShowQuoted bool
	// Cursor skips this many of a thread's newest messages.
	Cursor int
	// Offset starts a message's body at this character.
	Offset int
	// RowChars, for a listing, is what each row adds to the reply outside
	// the text when shown in full: its structured entry, as JSON with its
	// comma, one per row in order. When set, the budget covers the whole
	// reply but for the slim rows (§4.8): the text twice, since the reply
	// carries it in content and again in untrusted_text, the rows the
	// text shows, and the ids it leaves out.
	RowChars []int
	// Fixed, with RowChars, is what the reply carries whatever the text
	// shows: rows kept whole, as a listing of filters or changes keeps
	// every one. It is spent before the first row.
	Fixed int
}

func (o Options) budget() int {
	if o.Budget <= 0 {
		return DefaultBudget
	}
	return max(o.Budget, MinBudget)
}

func (o Options) loc() *time.Location {
	if o.Location == nil {
		return time.UTC
	}
	return o.Location
}

// Result is a rendered read and what it left for later.
type Result struct {
	Text string
	// Token is the boundary token the blocks use.
	Token  string
	Budget int
	// Truncated is set when anything found was left out.
	Truncated bool
	// NextCursor continues a thread; 0 when nothing is left.
	NextCursor int
	// NextOffset continues a cut body; 0 when the body was whole.
	NextOffset int
	// Omitted lists the ids of messages or rows not shown.
	Omitted []string
	// Shown is how many of a listing's rows the text shows; the rest
	// are the slim ones.
	Shown int
}

// maxTokenDraws bounds the redraws when content contains the token.
const maxTokenDraws = 8

// render runs fn with a fresh token, drawing again while any untrusted
// text contains the token — so the content can never hold the line that
// closes its own block.
func render(o Options, fn func(w *writer) Result) Result {
	src := o.Tokens
	if src == nil {
		src = RandomToken
	}
	var res Result
	for i := range 2 * maxTokenDraws {
		if i == maxTokenDraws {
			// The injected source keeps colliding; fall back to random.
			src = RandomToken
		}
		w := &writer{tok: src(), loc: o.loc()}
		res = fn(w)
		res.Text, res.Token = w.text(), w.tok
		if !w.leaks() {
			break
		}
	}
	return res
}

// plain renders text with no blocks, for results that hold nothing a
// sender wrote.
func plain(fn func(w *writer)) string {
	w := &writer{loc: time.UTC}
	fn(w)
	return w.text()
}
