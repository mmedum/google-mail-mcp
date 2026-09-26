package mime

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isInvisible reports whether r is a character a reader does not see
// but a model reads: zero-width characters, bidi controls, the word
// joiner family, the byte-order mark, soft hyphen and fillers, and the
// Unicode tag and supplementary variation-selector blocks, which carry
// text no font draws (§4.1.2).
func isInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F: // zero-width space, joiners, LRM, RLM
		return true
	case r >= 0x202A && r <= 0x202E: // bidi embeddings and overrides
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner, invisible operators
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r == 0xFEFF, r == 0x00AD, r == 0x034F, r == 0x061C, r == 0x180E:
		return true
	case r == 0x115F, r == 0x1160, r == 0x3164, r == 0xFFA0: // Hangul fillers
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tags
		return true
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement
		return true
	}
	return false
}

// keepJoiner reports whether a zero-width joiner or non-joiner between
// prev and next is doing its visible job — joining an emoji sequence or
// shaping Persian or an Indic script — rather than hiding a word. It is
// kept only between two non-ASCII, non-space characters.
func keepJoiner(r, prev, next rune) bool {
	if r != 0x200C && r != 0x200D {
		return false
	}
	ok := func(c rune) bool { return c > 0x7F && !unicode.IsSpace(c) && !isInvisible(c) }
	return ok(prev) && ok(next)
}

// StripInvisible removes invisible characters from s and returns how
// many it removed. It is linear in s: the visible character after a run
// of invisibles is found once per run, not once per character.
func StripInvisible(s string) (string, int) {
	if !hasInvisible(s) {
		return s, 0
	}
	var b strings.Builder
	n := 0
	prev := rune(0)
	// next is the first visible rune at or after nextAt; valid for any
	// position up to nextAt, since everything before it is invisible.
	next, nextAt := rune(0), -1
	for i, r := range s {
		if !isInvisible(r) {
			b.WriteRune(r)
			prev = r
			continue
		}
		if r == 0x200C || r == 0x200D {
			if after := i + utf8.RuneLen(r); after > nextAt {
				next, nextAt = nextVisible(s, after)
			}
			if keepJoiner(r, prev, next) {
				b.WriteRune(r)
				continue
			}
		}
		n++
	}
	return b.String(), n
}

// nextVisible returns the first visible rune in s at or after byte
// offset from, and its offset; 0 and len(s) when there is none.
func nextVisible(s string, from int) (rune, int) {
	for i, r := range s[from:] {
		if !isInvisible(r) {
			return r, from + i
		}
	}
	return 0, len(s)
}

func hasInvisible(s string) bool {
	for _, r := range s {
		if isInvisible(r) {
			return true
		}
	}
	return false
}
