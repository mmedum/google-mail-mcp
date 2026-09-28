package service

import (
	"context"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
)

// Asker puts a question to the person using the server before a write
// that takes confirm (§4.13). Ask returns nil when the write may go
// ahead, and an error to return in its place otherwise; the tools
// layer's register installs one per call, for the kinds that ask.
type Asker interface {
	Ask(ctx context.Context, q render.Question) error
}

type askerKey struct{}

// WithAsker returns a context whose writes that take confirm are put to
// a.
func WithAsker(ctx context.Context, a Asker) context.Context {
	return context.WithValue(ctx, askerKey{}, a)
}

// ask is the last step before a write that takes confirm, after every
// read and every other guard, so the question shows what the write
// would do and nothing is asked that a guard would refuse anyway. A dry
// run asks nothing. A write reached with no asker is refused: only a
// tool registered for it may make one.
func ask(ctx context.Context, q render.Question) error {
	if gapi.WritesForbidden(ctx) {
		return nil
	}
	a, ok := ctx.Value(askerKey{}).(Asker)
	if !ok {
		return gapi.Errf(gapi.ClassUnavailable, "this write has no way to ask the person, which is a bug in this server; nothing was written")
	}
	return a.Ask(ctx, q)
}
