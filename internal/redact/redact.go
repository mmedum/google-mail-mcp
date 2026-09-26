// Package redact masks what this server prints for a person and must
// not survive being pasted somewhere else.
//
// `status` and `doctor` print for a human, and the bug form asks for
// their output. Two things reach it by routes nobody chose: the Cloud
// console names the client-secret file after the OAuth client id, so
// printing the path prints the id; and Google's error text names the
// account it refused. Both are masked here, in one place, so a print
// added later is safe without its author knowing the rule.
package redact

import (
	"regexp"
	"strings"
)

// clientID is the shape Google issues for an OAuth client: a project
// number, a hyphen and exactly 32 lowercase characters. The fixed length
// is what keeps a Go pseudo-version out of it — "20260925101500-3f2a9c1b7d4e"
// is digits, a hyphen and twelve hex characters, and a looser pattern
// masked the binary's own --version.
var clientID = regexp.MustCompile(`[0-9]{6,}-[a-z0-9]{32}(\.apps\.googleusercontent\.com)?\b`)

// clientSecretName is the file name the console gives the download. Only
// the id span is replaced, because a second download becomes
// "… (1).json" and people rename them.
var clientSecretName = regexp.MustCompile(`client_secret_[0-9]+-[A-Za-z0-9_-]+`)

// Path masks a client id anywhere in a path. The directory is kept:
// "it looked in the wrong place" is most of what a first-run report is
// about, and a directory identifies nobody.
func Path(p string) string {
	p = clientSecretName.ReplaceAllString(p, "client_secret_<id>")
	return ClientID(p)
}

// ClientID masks an OAuth client id wherever it appears in free text.
func ClientID(s string) string { return clientID.ReplaceAllString(s, "<client-id>") }

// address matches an email address anywhere in free text.
var address = regexp.MustCompile(AddressPattern)

// Email keeps enough of an address for its owner to recognize it and not
// enough for anyone else to use it. The domain goes too: for somebody
// else's address it names their organization.
func Email(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || domain == "" {
		return mask(addr)
	}
	tld := ""
	if i := strings.LastIndexByte(domain, '.'); i >= 0 {
		tld, domain = domain[i:], domain[:i]
	}
	return mask(local) + "@" + mask(domain) + tld
}

// Account is the signed-in account with its local part removed and its
// domain kept. The domain is the half a diagnosis uses — a consumer
// account and a Workspace one have different sending limits and consent
// rules — and the local part answers nothing.
func Account(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || domain == "" {
		return Email(addr)
	}
	return "…@" + domain
}

// Accounts masks every address in text this server did not write, such
// as a 403 that names the account it refused.
func Accounts(s string) string { return address.ReplaceAllStringFunc(s, Account) }

// Addresses masks every address in text with Email, domain included.
// It is for text that can name somebody other than the account, such as
// Google's error message, where a correspondent's domain names their
// organization.
func Addresses(s string) string { return address.ReplaceAllStringFunc(s, Email) }

// The shapes Line removes other than client ids, as pattern
// text so the transcript redactor under scripts/ matches the same ones.
// Each is anchored on a shape the server's own output cannot take by
// accident.
const (
	// RefreshTokenPattern is a refresh token's literal prefix and body.
	RefreshTokenPattern = `\b1//[0-9A-Za-z_\-]{10,}` //nolint:gosec // a pattern that finds tokens, not one
	// GmailIDPattern is a message or thread id: 16 lowercase hex digits.
	GmailIDPattern = `\b[0-9a-f]{16}\b`
	// DraftIDPattern is a draft id: "r" and a signed decimal.
	DraftIDPattern = `\br-?[0-9]{8,}\b`
	// AddressPattern is an email address in free text. The local part
	// takes "=" and "$", which bounce and forwarding addresses use to
	// carry another address inside their own, and "…", so an address
	// already masked here is matched whole.
	AddressPattern = `[A-Za-z0-9._%+\-=$…]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`
)

var (
	refreshToken = regexp.MustCompile(RefreshTokenPattern)
	gmailID      = regexp.MustCompile(GmailIDPattern)
	draftID      = regexp.MustCompile(DraftIDPattern)
)

// Line masks everything in one line that identifies a person, an
// account or a message, for text assembled by somebody else — an API
// response, an error, a rendered result — whose values are not known in
// advance.
func Line(s string) string {
	s = refreshToken.ReplaceAllString(s, "<token>")
	s = Addresses(s)
	s = ClientID(s)
	s = gmailID.ReplaceAllString(s, "<id>")
	return draftID.ReplaceAllString(s, "<id>")
}

// Text is what every stream of the command line passes through: the
// addresses in it reduced to their domain and any client id masked.
func Text(s string) string { return ClientID(Path(Accounts(s))) }

// ID truncates an identifier to a correlation key that cannot be looked
// up or pasted into a URL: its first six characters. The logging rule
// (§9.2) allows that much and no more.
func ID(id string) string {
	r := []rune(id)
	if len(r) <= 6 {
		return "[id]"
	}
	return string(r[:6]) + "…"
}

// mask keeps the first rune and replaces the rest with an ellipsis, so
// the result cannot be mistaken for a short address.
func mask(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return ""
	}
	return string(r[0]) + "…"
}
