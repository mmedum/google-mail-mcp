// Package redact masks what a real mailbox puts in a transcript, for the
// programs under scripts/ that drive the built server against one.
//
// The same input always gets the same placeholder, so a transcript still
// reads as a story: <ID_1> in one result is <ID_1> in the next.
//
// What it hides: Gmail links, addresses (which covers RFC 5322
// Message-IDs, since they are shaped like one), the display names beside
// addresses, message, thread and draft ids, and OAuth tokens and client
// ids. What it cannot hide: subjects, bodies, snippets, attachment names
// and label names. Nothing tells those from prose. The live driver reads
// only messages it wrote itself (docs/architecture.md §9.1), which is
// what keeps them harmless, and Summary says so on every run.
package redact

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	core "github.com/mmedum/google-mail-mcp/v2/internal/redact"
)

// A Redactor replaces account-specific values with stable placeholders.
type Redactor struct {
	// Off returns text unchanged. Only for a terminal nobody else sees.
	Off bool

	seen   map[string]string
	counts map[string]int
}

// pattern is one class of thing to hide, with the name its placeholders
// carry. Order matters: a link goes before the id inside it, and a token
// before the address-shaped text that might follow it.
type pattern struct {
	name string
	re   *regexp.Regexp
}

var patterns = []pattern{
	{"LINK", regexp.MustCompile(`https://mail\.google\.com/[^\s"'<>)\]]+`)},
	// OAuth material: an access token, a refresh token, a client secret
	// and a client id. None should ever reach a transcript; if one does,
	// it goes no further than here.
	// The refresh token is the server's own pattern. The client id is
	// wider than the server's: the server's needs exactly 32 characters so
	// it leaves a Go pseudo-version alone, and requires nothing after it;
	// this one requires the googleusercontent suffix and so can take any
	// length, which catches an id Google ever issues in a new shape.
	{"TOKEN", regexp.MustCompile(`ya29\.[0-9A-Za-z_\-.]+|` + core.RefreshTokenPattern + `|GOCSPX-[0-9A-Za-z_\-]+|` +
		`[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com`)},
	{"EMAIL", regexp.MustCompile(core.AddressPattern)},
	// Message, thread and draft ids, in the server's own shapes. All are
	// opaque and all identify mail.
	{"ID", regexp.MustCompile(core.GmailIDPattern + `|` + core.DraftIDPattern)},
	// A long bare number. Gmail stamps a message the API inserted with
	// "Received: from <number> named unknown by gmailapi.google.com", and
	// the number is the Cloud project the OAuth client belongs to — found
	// by reading a live transcript. Ten digits and up also takes page
	// tokens and epoch seconds, which cost a reader nothing to lose.
	{"NUMBER", regexp.MustCompile(`\b[0-9]{10,}\b`)},
	// A user label's id, Label_ and a counter. It says which labels the
	// account has; a live transcript carried them before this existed.
	{"LABEL", regexp.MustCompile(`\bLabel_[0-9]+\b`)},
	// A filter id in the shape a live run showed Gmail issuing. The
	// positions below catch one in any other shape.
	{"FILTER", regexp.MustCompile(`\bANe1B[0-9A-Za-z_\-]{2,}`)},
}

// filterPositions are where a filter id appears whatever its shape: the
// rendered "filter <id>", where a token with a digit in it is an id and a
// word like "without" is not, and an argument echo's "filter_id":"<id>".
var filterPositions = []*regexp.Regexp{
	regexp.MustCompile(`\bfilter ([0-9A-Za-z_\-]*[0-9][0-9A-Za-z_\-]*)`),
	// Not a placeholder the FILTER pattern already put there.
	regexp.MustCompile(`"filter_id":"([^"<]+)"`),
}

// historyPositions are where a history id appears. It is a plain decimal
// counter, too short for NUMBER and shaped like any other number, so it
// is found by what stands before it: list_changes's "history N",
// "history from N" and "history_id=N", an argument echo's "history_id":"N", and a request's
// startHistoryId=N. A live transcript carried the account's real ones
// before these existed.
var historyPositions = []*regexp.Regexp{
	regexp.MustCompile(`\bhistory (?:from )?(\d+)\b`),
	regexp.MustCompile(`\bhistory_id=(\d+)\b`),
	regexp.MustCompile(`"history_id":"(\d+)"`),
	regexp.MustCompile(`\bstartHistoryId=(\d+)\b`),
}

// dirPositions are where the live driver's own directory appears: a
// saved file's path runs through the system's temporary directory,
// which can name the maintainer's account or machine. What stands before
// the run's folder is masked, the folder itself kept.
var dirPositions = []*regexp.Regexp{
	regexp.MustCompile(`((?:[A-Za-z]:)?[/\\][^\s"<>]*?)[/\\]livemail-[0-9]{8}-[0-9]{6}-[0-9a-f]{6}-`),
}

// personName is the shape of a display name: capitalized words, with
// the usual lowercase particles between them, or a dotted lowercase
// token, which is what Gmail shows when no display name is set. It is
// only ever matched in a position beside an address, never on its own.
const personName = `(?:` + capitalizedName + `|` + dottedName + `)`

const capitalizedName = `(?:\p{Lu}[\p{L}'\-.]+)(?: (?:\p{Lu}[\p{L}'\-.]+|van|von|der|den|de|del|di|du|la|le|bin|al)){0,4}`

const dottedName = `(?:[\p{Ll}\d]+(?:[.\-_][\p{Ll}\d]+)+)`

// personPositions are the places a name sits beside an address once the
// address is a placeholder: `Name <addr>` and `"Name" <addr>`.
var personPositions = []*regexp.Regexp{
	regexp.MustCompile(`(` + personName + `) <<EMAIL_\d+>>`),
	regexp.MustCompile(`"(` + personName + `)" <<EMAIL_\d+>>`),
}

// NewRedactor returns a redactor with an empty memory.
func NewRedactor(off bool) *Redactor {
	return &Redactor{Off: off, seen: map[string]string{}, counts: map[string]int{}}
}

// Do replaces every account-specific value in text.
func (r *Redactor) Do(text string) string {
	if r.Off {
		return text
	}
	for _, p := range patterns {
		text = p.re.ReplaceAllStringFunc(text, func(match string) string {
			return r.placeholder(p.name, match)
		})
	}
	text = r.inPositions(text, "HISTORY", historyPositions)
	text = r.inPositions(text, "FILTER", filterPositions)
	text = r.inPositions(text, "DIR", dirPositions)
	// Names last: one of their positions is defined by an address
	// placeholder.
	return r.inPositions(text, "PERSON", personPositions)
}

// inPositions masks what each position's first group captures, keeping
// the words around it that make it recognizable.
func (r *Redactor) inPositions(text, kind string, positions []*regexp.Regexp) string {
	for _, re := range positions {
		text = re.ReplaceAllStringFunc(text, func(match string) string {
			v := re.FindStringSubmatch(match)[1]
			return strings.Replace(match, v, r.placeholder(kind, v), 1)
		})
	}
	return text
}

func (r *Redactor) placeholder(kind, value string) string {
	if got, ok := r.seen[value]; ok {
		return got
	}
	r.counts[kind]++
	out := "<" + kind + "_" + strconv.Itoa(r.counts[kind]) + ">"
	r.seen[value] = out
	return out
}

// Summary says what was hidden, and what cannot be.
func (r *Redactor) Summary() string {
	const caveat = "Subjects, bodies, snippets, attachment names and label names are never redacted: " +
		"read the transcript before sharing it."
	if r.Off {
		return "redaction off: this transcript carries real ids, links and addresses"
	}
	if len(r.seen) == 0 {
		return "nothing needed redacting. " + caveat
	}
	kinds := make([]string, 0, len(r.counts))
	for k := range r.counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, strconv.Itoa(r.counts[k])+" "+strings.ToLower(k))
	}
	return "redacted: " + strings.Join(parts, ", ") + ".\n" + caveat
}

// ID is the server's own truncation (internal/redact.ID), so a report
// and a log line show the same key for the same id.
func ID(id string) string { return core.ID(id) }
