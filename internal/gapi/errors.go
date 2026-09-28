package gapi

import (
	"errors"
	"fmt"
)

// Class is the closed error vocabulary of docs/architecture.md §6.5.
//
// Closed means both directions, and `scripts/gates classes` asserts
// both: every class the code emits is in Classes, and every class in
// Classes is emitted somewhere.
type Class string

// The twelve classes. The standard names six; the other six are forced
// by this API and each is argued in §6.5.
const (
	// ClassInvalid: malformed or under-specified arguments, or a message
	// larger than Google accepts.
	ClassInvalid Class = "invalid"
	// ClassNotFound: no such message, thread, draft or label.
	ClassNotFound Class = "not_found"
	// ClassAuth: not signed in, the token was revoked or expired, or a
	// scope is missing. The caller runs login.
	ClassAuth Class = "auth"
	// ClassForbidden: signed in, but the account or its organization
	// refuses this.
	ClassForbidden Class = "forbidden"
	// ClassConflict: the mailbox state refuses the operation — a label
	// name already taken, SENT applied by hand.
	ClassConflict Class = "conflict"
	// ClassStale: the draft moved since its witness was read (§4.4).
	// Kept apart from conflict because it asks for a re-read.
	ClassStale Class = "stale"
	// ClassAmbiguous: a label name or rfc822 id matched more than one.
	ClassAmbiguous Class = "ambiguous"
	// ClassBlocked: a guard refused what the API would have allowed.
	ClassBlocked Class = "blocked"
	// ClassRateLimited: a per-user, project or sending limit (§2.2).
	ClassRateLimited Class = "rate_limited"
	// ClassUnavailable: a transient upstream failure.
	ClassUnavailable Class = "unavailable"
	// ClassUnsupported: the API cannot do this (§2).
	ClassUnsupported Class = "unsupported"
	// ClassAmbiguousOutcome: a send, or a filter create, may or may not
	// have happened (§4.3, §7.9). Never retried; the result carries the
	// read that tries to settle it.
	ClassAmbiguousOutcome Class = "ambiguous_outcome"
)

// Classes is the vocabulary, in the order §6.5 tabulates it.
var Classes = []Class{
	ClassInvalid, ClassNotFound, ClassAuth, ClassForbidden,
	ClassConflict, ClassStale, ClassAmbiguous, ClassBlocked,
	ClassRateLimited, ClassUnavailable, ClassUnsupported,
	ClassAmbiguousOutcome,
}

// Valid reports whether c is in the vocabulary.
func (c Class) Valid() bool {
	for _, k := range Classes {
		if k == c {
			return true
		}
	}
	return false
}

// Retryable reports whether a failure of this class may be retried
// without the caller doing anything. stale needs a re-read, and
// ambiguous_outcome needs somebody to look.
func (c Class) Retryable() bool {
	return c == ClassUnavailable || c == ClassRateLimited
}

// Error is a classified failure. It reaches the model as a tool result,
// never as a protocol error.
type Error struct {
	Class Class
	// Message says what to do, not only what happened. It never carries
	// mail content.
	Message string
	// Status is the HTTP status, 0 for a failure that never reached
	// Google.
	Status int
	// Reason is Google's machine reason, kept for classification.
	Reason string
	err    error
}

// Error renders "[class] message".
func (e *Error) Error() string { return fmt.Sprintf("[%s] %s", e.Class, e.Message) }

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.err }

// Errf builds a classified error.
func Errf(c Class, format string, args ...any) *Error {
	return &Error{Class: c, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds a classified error carrying a cause.
func Wrap(c Class, err error, format string, args ...any) *Error {
	return &Error{Class: c, Message: fmt.Sprintf(format, args...), err: err}
}

// ClassOf returns the class of err, and whether it carried one.
func ClassOf(err error) (Class, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Class, true
	}
	return "", false
}
