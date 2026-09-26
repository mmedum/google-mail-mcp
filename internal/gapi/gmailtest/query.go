package gmailtest

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// term is one search term: an operator and its value, or a free word
// when op is "".
type term struct {
	neg     bool
	op, val string
}

// operators the fake understands. Anything else with a colon is a word.
var operators = map[string]bool{
	"from": true, "to": true, "cc": true, "subject": true, "label": true, "is": true,
	"has": true, "in": true, "after": true, "before": true, "rfc822msgid": true,
}

// parseQuery splits q into terms, honoring double quotes.
func parseQuery(q string) []term {
	var out []term
	i := 0
	for i < len(q) {
		for i < len(q) && q[i] == ' ' {
			i++
		}
		if i >= len(q) {
			break
		}
		start := i
		inQuote := false
		for i < len(q) && (inQuote || q[i] != ' ') {
			if q[i] == '"' {
				inQuote = !inQuote
			}
			i++
		}
		tok := q[start:i]
		t := term{}
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			t.neg = true
			tok = tok[1:]
		}
		if op, val, ok := strings.Cut(tok, ":"); ok && operators[strings.ToLower(op)] {
			t.op, t.val = strings.ToLower(op), val
		} else {
			t.val = tok
		}
		t.val = strings.ToLower(strings.Trim(t.val, `"`))
		out = append(out, t)
	}
	return out
}

// reachesSpamTrash reports whether the query asks for spam or trash
// explicitly, which Gmail honors without includeSpamTrash.
func reachesSpamTrash(terms []term) bool {
	for _, t := range terms {
		if t.neg {
			continue
		}
		if (t.op == "in" || t.op == "label") && (t.val == "spam" || t.val == "trash" || t.val == "anywhere") {
			return true
		}
	}
	return false
}

// visible applies the spam and trash rule.
func visible(m *message, includeSpamTrash bool, terms []term, labelIDs []string) bool {
	if includeSpamTrash || reachesSpamTrash(terms) || slices.Contains(labelIDs, "SPAM") || slices.Contains(labelIDs, "TRASH") {
		return true
	}
	return !slices.Contains(m.labels, "SPAM") && !slices.Contains(m.labels, "TRASH")
}

func (s *Server) matches(m *message, terms []term, labelIDs []string) bool {
	for _, l := range labelIDs {
		if !slices.Contains(m.labels, l) {
			return false
		}
	}
	for _, t := range terms {
		if s.matchTerm(m, t) == t.neg {
			return false
		}
	}
	return true
}

func (s *Server) matchTerm(m *message, t term) bool {
	has := func(l string) bool { return slices.Contains(m.labels, l) }
	switch t.op {
	case "from":
		return strings.Contains(m.from, t.val)
	case "to", "cc":
		return strings.Contains(m.to, t.val)
	case "subject":
		return strings.Contains(m.subject, t.val)
	case "label":
		return s.hasLabelNamed(m, t.val)
	case "is":
		switch t.val {
		case "unread":
			return has("UNREAD")
		case "read":
			return !has("UNREAD")
		case "starred":
			return has("STARRED")
		case "important":
			return has("IMPORTANT")
		}
		return false
	case "has":
		return t.val == "attachment" && m.hasAttachment
	case "in":
		switch t.val {
		case "anywhere":
			return true
		case "drafts", "draft":
			return has("DRAFT")
		}
		return has(strings.ToUpper(t.val))
	case "after", "before":
		at, ok := parseWhen(t.val)
		if !ok {
			return false
		}
		if t.op == "after" {
			return m.internalDate.After(at)
		}
		return m.internalDate.Before(at)
	case "rfc822msgid":
		return strings.Trim(strings.ToLower(m.rfc822ID), "<>") == strings.Trim(t.val, "<>")
	}
	return strings.Contains(m.subject+" "+m.text+" "+m.from+" "+m.to, t.val)
}

// hasLabelNamed matches a label by id or by name, with Gmail's search
// spelling of a name: lowercase, spaces and slashes as dashes.
func (s *Server) hasLabelNamed(m *message, v string) bool {
	norm := func(x string) string {
		return strings.NewReplacer(" ", "-", "/", "-").Replace(strings.ToLower(x))
	}
	for _, id := range m.labels {
		if strings.EqualFold(id, v) {
			return true
		}
		if l, ok := s.labels[id]; ok && norm(l.Name) == norm(v) {
			return true
		}
	}
	return false
}

// parseWhen reads an epoch in seconds, or yyyy/mm/dd as UTC midnight.
// (Gmail reads a date as Pacific midnight, §2.9; the fake does not model
// that, and the server sends epochs.)
func parseWhen(v string) (time.Time, bool) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), true
	}
	if t, err := time.Parse("2006/01/02", v); err == nil {
		return t, true
	}
	return time.Time{}, false
}
