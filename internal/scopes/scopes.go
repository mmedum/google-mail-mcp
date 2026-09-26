// Package scopes is the one source of truth for the OAuth scopes this
// server requests in each mode, and for which granted scope covers
// which.
//
// Every scope that reads mail content is restricted (§2.11), and none
// can separate drafting from sending (§2.10). So the set is chosen by
// what the configuration registers: read-only asks for gmail.readonly,
// the default for gmail.modify, and only the destructive flag asks for
// https://mail.google.com/, the one scope permanent deletion accepts.
// The send flag changes nothing here: gmail.modify can already send,
// which is why registration, not scope, is the control (§4.2).
//
// docs/gcp-setup.md's scope block is generated from Modes and compared
// exactly by the staleness gate.
package scopes

import (
	"slices"
	"strings"
)

// Gmail scopes.
const (
	// Full is https://mail.google.com/, the only scope that permanent
	// deletion accepts.
	Full = "https://mail.google.com/"
	// Modify reads, drafts, labels, trashes and sends. It cannot delete
	// permanently.
	Modify = "https://www.googleapis.com/auth/gmail.modify"
	// Readonly reads messages, threads, labels and settings.
	Readonly = "https://www.googleapis.com/auth/gmail.readonly"
	// Compose manages drafts and sends. Never requested; it appears in
	// the implication table because a token may carry it.
	Compose = "https://www.googleapis.com/auth/gmail.compose"
	// Labels manages labels only. Never requested.
	Labels = "https://www.googleapis.com/auth/gmail.labels"
)

// implies maps a scope to the narrower scopes it covers. A token holding
// the wider scope satisfies a requirement for any of these, so the
// server does not refuse a call Google would allow, or ask for a new
// login the person does not need.
var implies = map[string][]string{
	Full:   {Modify, Readonly, Compose, Labels},
	Modify: {Readonly, Compose, Labels},
}

// ForMode is the set login requests for a configuration. readOnly and
// destructive together are refused by internal/config; here destructive
// wins, because the wider scope is the one that makes the registered
// tools work.
func ForMode(readOnly, destructive bool) []string {
	switch {
	case destructive:
		return []string{Full}
	case readOnly:
		return []string{Readonly}
	default:
		return []string{Modify}
	}
}

// Mode is one row of the scope table: a configuration and what login
// asks for under it.
type Mode struct {
	// Name is how the documentation names the mode.
	Name string
	// Flags are the settings that select it, "" for none.
	Flags []string
	// Scopes is what login requests.
	Scopes []string
}

// Modes is every configuration with a distinct meaning for scopes, in
// the order the documentation tabulates them. It is built from ForMode,
// so the table cannot say one thing while login does another.
func Modes() []Mode {
	return []Mode{
		{Name: "read-only", Flags: []string{"GMAIL_READ_ONLY=true"}, Scopes: ForMode(true, false)},
		{Name: "default", Flags: nil, Scopes: ForMode(false, false)},
		{Name: "send", Flags: []string{"GMAIL_ENABLE_SEND=true"}, Scopes: ForMode(false, false)},
		{Name: "destructive", Flags: []string{"GMAIL_ENABLE_DESTRUCTIVE=true"}, Scopes: ForMode(false, true)},
	}
}

// Satisfied reports whether granted covers required, directly or
// through a wider scope.
func Satisfied(required string, granted []string) bool {
	for _, g := range granted {
		if g == required || slices.Contains(implies[g], required) {
			return true
		}
	}
	return false
}

// Missing returns the scopes of required that granted does not cover,
// in required's order.
func Missing(granted, required []string) []string {
	var out []string
	for _, r := range required {
		if !Satisfied(r, granted) {
			out = append(out, r)
		}
	}
	return out
}

// Covered reports whether granted covers every scope of required. Login
// uses it to decide whether the consent screen must be shown again.
func Covered(granted, required []string) bool { return len(Missing(granted, required)) == 0 }

// Excess returns the Gmail scopes of granted that required does not
// need: a token wider than its configuration. Turning the destructive
// flag off leaves the login's https://mail.google.com/ in place, and a
// read-only configuration may run on a token that can send. Scopes
// other than Gmail's (openid, email) are not judged.
func Excess(granted, required []string) []string {
	var out []string
	for _, g := range granted {
		if isGmail(g) && !Satisfied(g, required) {
			out = append(out, g)
		}
	}
	return out
}

func isGmail(scope string) bool {
	return scope == Full || strings.HasPrefix(scope, "https://www.googleapis.com/auth/gmail.")
}
