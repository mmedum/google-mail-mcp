package gapi

import (
	"errors"
	"fmt"
)

// Class is the closed error vocabulary (D-77).
//
// Closed means both directions, and `scripts/gates classes` asserts
// both: every class the code emits is in Classes, and every class in
// Classes is emitted somewhere.
type Class string

// The classes. Each asks the caller for a different next step; a failure
// that needs a step none of them names gets a class of its own.
const (
	// ClassInvalid: malformed or under-specified arguments, such as a
	// write naming no ids or too many (D-44), or a message larger than
	// Google accepts. The caller fixes the arguments.
	ClassInvalid Class = "invalid"
	// ClassNotFound: no such message, thread, draft or label. The caller
	// checks the id.
	ClassNotFound Class = "not_found"
	// ClassAuth: not signed in, the token was revoked or expired (D-01),
	// or a scope is missing (D-73). The caller runs login.
	ClassAuth Class = "auth"
	// ClassForbidden: signed in, but the account or its organization
	// refuses this. The caller asks an administrator.
	ClassForbidden Class = "forbidden"
	// ClassConflict: the mailbox state refuses the operation — a label
	// name already taken, SENT applied by hand. The caller reads and
	// reconsiders.
	ClassConflict Class = "conflict"
	// ClassStale: the draft moved since its witness was read (D-42), or a
	// witness the call carries is not what Gmail holds (D-47). The caller
	// re-reads and retries, which is why it is kept apart from conflict.
	ClassStale Class = "stale"
	// ClassAmbiguous: a label name or rfc822 id matched more than one.
	// The caller passes an id.
	ClassAmbiguous Class = "ambiguous"
	// ClassBlocked: a guard refused what the API would have allowed
	// (D-49), or a write that cannot be undone came without confirm: true
	// (D-66). The caller passes the override, or does not; it passes
	// confirm only when the person asked for that write.
	ClassBlocked Class = "blocked"
	// ClassRateLimited: a per-user, project or sending limit (D-78, D-79).
	// The caller waits as long as the message says. The message tells the
	// sending limit from the request limits: a minute is not a day.
	ClassRateLimited Class = "rate_limited"
	// ClassUnavailable: a transient upstream failure. The caller retries.
	ClassUnavailable Class = "unavailable"
	// ClassUnsupported: the API cannot do this. The caller reads the
	// message, which says what Gmail cannot do.
	ClassUnsupported Class = "unsupported"
	// ClassAmbiguousOutcome: a write that is not safe to repeat failed
	// without a clear answer, so it may or may not have happened. Never
	// retried. A send and a filter write settle it with a read and report
	// the verdict (D-50, D-58); any other write leaves the caller to read
	// before trying again.
	ClassAmbiguousOutcome Class = "ambiguous_outcome"
)

// Classes is the vocabulary, each class once.
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
