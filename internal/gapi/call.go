package gapi

import "net/url"

// Call is one request to the Gmail API.
//
// Every call site builds one as a composite literal whose ID is a
// string literal naming the discovery method, "gmail.users.messages.get".
// `scripts/gates api-coverage` reads those literals from this package's
// syntax tree and holds them against testdata/api-coverage.tsv, so a
// call nobody judged fails the build. Do not compute ID.
type Call struct {
	// ID is the discovery method id, as a string literal.
	ID string
	// Method is the HTTP verb.
	Method string
	// Path is a template under users/me, written in this package and
	// never taken from a caller: "messages/{}/attachments/{}". Each {} is
	// filled, in order, by one element of Args, escaped as a single path
	// segment. The count must match; a mismatch is a programming error
	// the client refuses before sending.
	Path string
	// Args fill Path's placeholders. They are caller-supplied ids.
	Args []string
	// Query is appended as the query string. It may carry a search, so
	// it never reaches a log or an error message.
	Query url.Values
	// Body is marshalled as JSON when not nil.
	Body any
	// Units are not set here: the client charges each call its cost from
	// the quota table keyed by ID (docs/architecture.md §4.9).
	//
	// Repeatable is the reason a POST may be sent twice without applying
	// twice. Empty means the method decides: GET, PUT, PATCH and DELETE
	// repeat; POST does not. Never set on a send.
	Repeatable string
}
