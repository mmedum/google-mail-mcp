package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v2/internal/config"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/service"
)

// Kind is what a tool does to the world. One field decides a tool's
// annotations, whether it is registered under the configuration, its
// _meta, and nothing else may decide those (CLAUDE.md rule 11).
type Kind int

// Tool kinds.
const (
	// Read changes nothing. Registered in every mode.
	Read Kind = iota
	// ReadWritesLocally reads mail and writes a file into GMAIL_LOCAL_DIR
	// — download_attachment. Registered only when that directory is set,
	// read-only mode included, since read-only is about the mailbox.
	ReadWritesLocally
	// Write changes the mailbox and reaches nobody else: drafts, labels,
	// trash. Not registered in read-only mode.
	Write
	// WriteForGood is a Write that cannot be undone: delete_draft, since
	// Gmail deletes a draft rather than trashing it. Registered as Write
	// is (§17.1), and annotated destructive, because a client that runs
	// non-destructive tools unasked must not run this one on that
	// strength. Its call still takes confirm: true.
	WriteForGood
	// Send delivers mail to other people. Registered only with
	// GMAIL_ENABLE_SEND (§4.2).
	Send
	// Destructive cannot be undone. Registered only with
	// GMAIL_ENABLE_DESTRUCTIVE (§4.6), and each call still takes
	// confirm: true.
	Destructive
	// Settings changes how the account behaves from now on — a
	// signature, a filter — and reaches nobody else. Registered only
	// with GMAIL_ENABLE_SETTINGS (§7.9), which also requests the one
	// scope these methods accept. Annotated destructive: a signature
	// overwritten or mail a filter trashed is not put back by this
	// server.
	Settings
	// SettingsForGood is a Settings write that cannot be undone:
	// delete_filter. Its call takes confirm: true.
	SettingsForGood
	// AutoReply answers other people's mail automatically: the vacation
	// reply. It is sending by another name, so it needs both
	// GMAIL_ENABLE_SETTINGS and GMAIL_ENABLE_SEND (§17.4).
	AutoReply
)

// String names the kind, for tests and the logging harness.
func (k Kind) String() string {
	switch k {
	case Read:
		return "read"
	case ReadWritesLocally:
		return "read-writes-locally"
	case Write:
		return "write"
	case WriteForGood:
		return "write-for-good"
	case Send:
		return "send"
	case Destructive:
		return "destructive"
	case Settings:
		return "settings"
	case SettingsForGood:
		return "settings-for-good"
	case AutoReply:
		return "auto-reply"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// allowed applies the registration gates. They are the control; the
// annotations below are hints a client may ignore.
func (k Kind) allowed(cfg config.Config) bool {
	switch k {
	case Read:
		return true
	case ReadWritesLocally:
		return cfg.LocalDir != ""
	case Write, WriteForGood:
		return !cfg.ReadOnly
	case Send:
		return cfg.EnableSend && !cfg.ReadOnly
	case Destructive:
		return cfg.EnableDestructive && !cfg.ReadOnly
	case Settings, SettingsForGood:
		return cfg.EnableSettings && !cfg.ReadOnly
	case AutoReply:
		return cfg.EnableSettings && cfg.EnableSend && !cfg.ReadOnly
	}
	return false
}

// annotations says what a client shows for this kind. openWorldHint is
// true only for Send and AutoReply: the kinds whose effect reaches
// another person.
func (k Kind) annotations() *mcp.ToolAnnotations {
	no, yes := ptr(false), ptr(true)
	switch k {
	case Read:
		return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: no}
	case ReadWritesLocally:
		// Not read-only: it creates a file. Not destructive: it never
		// overwrites one.
		return &mcp.ToolAnnotations{DestructiveHint: no, OpenWorldHint: no}
	case Send, AutoReply:
		return &mcp.ToolAnnotations{DestructiveHint: no, OpenWorldHint: yes}
	case Destructive, WriteForGood, Settings, SettingsForGood:
		// Deleting what is already gone changes nothing more.
		return &mcp.ToolAnnotations{DestructiveHint: yes, IdempotentHint: true, OpenWorldHint: no}
	default:
		return &mcp.ToolAnnotations{DestructiveHint: no, OpenWorldHint: no}
	}
}

// requiresUserInteraction asks a client to put a person in front of the
// call. A signal, not a control: a host in an auto-approve mode runs the
// tool anyway, which is why Send and Destructive are unregistered by
// default rather than relying on this. For a client that can ask, the
// asksEveryCall kinds go without it (interactionHint).
func (k Kind) requiresUserInteraction() bool { return k == Send || k == Destructive || k == AutoReply }

// asksPerson says a kind's writes that take confirm are also put to the
// person, when the client can ask (§4.13). Every kind but Read and Write
// has one; register refuses a tool that takes confirm under any other.
func (k Kind) asksPerson() bool {
	switch k {
	case WriteForGood, Send, Destructive, Settings, SettingsForGood, AutoReply:
		return true
	}
	return false
}

// asksEveryCall says a kind's tools put every write to the person, when
// the client can ask, so the requiresUserInteraction mark is dropped for
// such a client (interactionHint). AutoReply is left out: set_vacation
// asks only when it turns the reply on. register refuses a tool of these
// kinds that does not take confirm, since one would never ask.
func (k Kind) asksEveryCall() bool { return k == Send || k == Destructive }

// Renderer is the readable half of a reply. Every output type has one,
// so a tool cannot be added without it: a client may show only content
// or only structuredContent, and both must carry the substance, never
// as the same bytes (CLAUDE.md rule 12).
type Renderer interface {
	Render() string
}

// Spec is one tool's registration, apart from its handler.
type Spec struct {
	Name        string
	Description string
	Kind        Kind
	// OmittedIDs, when set, is what omitted_ids means for this tool, in
	// place of Rendered's general description.
	OmittedIDs string
	// AskUnits is what asking the person adds to a tool that takes
	// confirm, starting with the number of units: the retry repeats the
	// reads before the write (§4.13). Required for such a tool.
	AskUnits string
}

// Handler is a tool's work: validated input in, a rendered result or a
// classified error out.
type Handler[In any, Out Renderer] func(ctx context.Context, in In) (Out, error)

// register adds one tool, or leaves it out when the configuration says
// so. Every tool in this package goes through here.
func register[In any, Out Renderer](s *mcp.Server, d Deps, sp Spec, h Handler[In, Out]) {
	if !sp.Kind.allowed(d.Config) {
		return
	}
	if takesConfirm[In]() && (!sp.Kind.asksPerson() || sp.AskUnits == "") {
		panic("tools: " + sp.Name + " takes confirm, and its kind " + sp.Kind.String() + " does not ask the person, " +
			"or its Spec does not say what asking costs")
	}
	if sp.Kind.asksEveryCall() && !takesConfirm[In]() {
		panic("tools: " + sp.Name + " is " + sp.Kind.String() + ", which asks the person before every write, and " +
			"takes no confirm, so it would never ask")
	}
	if d.registered != nil {
		*d.registered = append(*d.registered, sp.Name)
	}
	if d.kinds != nil {
		d.kinds[sp.Name] = sp.Kind.String()
	}
	if d.namesOnly {
		return
	}
	out := outputSchema[Out]()
	if sp.OmittedIDs != "" {
		describeOmitted(out, sp.OmittedIDs)
	}
	description := sp.Description
	if takesConfirm[In]() {
		description += personNote + " Asking repeats the reads before the write: " + sp.AskUnits + "."
	}
	tool := &mcp.Tool{
		Name:         sp.Name,
		Description:  description,
		Annotations:  sp.Kind.annotations(),
		InputSchema:  inputSchema[In](),
		OutputSchema: out,
	}
	if sp.Kind.requiresUserInteraction() {
		tool.Meta = mcp.Meta{interactionKey: true}
	}
	var a *asking
	if sp.Kind.asksPerson() {
		a = d.asking
		if sp.Kind.asksEveryCall() {
			a.always[sp.Name] = true
		}
	}
	mcp.AddTool(s, tool, wrap(h, dryRunField[In](), sp.Name, a, d.Config.RequirePrompt))
}

// personNote ends the description of every tool that takes confirm,
// since each of them also asks the person (§4.13).
const personNote = " When the client can ask, the server also asks the person before it writes; a call they do not " +
	"confirm is [blocked], writes nothing, and is not to be made again unless they ask."

// describeOmitted sets the description of s's omitted_ids, on a copy of
// the property so no other schema shares the change.
func describeOmitted(s *jsonschema.Schema, description string) {
	p, ok := s.Properties["omitted_ids"]
	if !ok {
		panic("tools: OmittedIDs set on a tool whose output has no omitted_ids")
	}
	c := *p
	c.Description = description
	s.Properties["omitted_ids"] = &c
}

// costed is an output that reports the quota its call spent. An embedded
// Cost implements it, through its pointer.
type costed interface{ setUnits(units int) }

func (c *Cost) setUnits(units int) { c.Units = units }

// wrap is what every handler gets without asking: a dry run that cannot
// write, the person asked before a write that takes confirm when a is
// set (§4.13), an error rendered "[class] message" as a tool error, the
// units spent filled into an output that embeds Cost, and Content set
// from Render so the SDK does not fill it with the JSON of the output.
func wrap[In any, Out Renderer](h Handler[In, Out], dryRun int, name string, a *asking, requirePrompt bool) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		if dryRun >= 0 && reflect.ValueOf(in).Field(dryRun).Bool() {
			ctx = gapi.WithoutWrites(ctx)
		}
		var p *person
		if a != nil {
			var err error
			if p, err = a.personFor(req, name, in, requirePrompt); err != nil {
				return nil, zero, fail(err)
			}
			ctx = service.WithAsker(ctx, p)
			if p.answer != nil {
				// The person accepted: from here the write may happen whether
				// or not this round reaches its question again, so a failure
				// to reply is no longer "nothing was written".
				setStage(ctx, stageWriting)
			}
		} else if req.Params != nil && (req.Params.RequestState != "" || len(req.Params.InputResponses) > 0) {
			return nil, zero, fail(gapi.Errf(gapi.ClassBlocked,
				"%s asks the person nothing, and the call came with an answer; nothing was done. Call it again without one", name))
		}
		out, err := h(ctx, in)
		if p != nil && p.asked != nil {
			// The service stopped before its write; the question goes out.
			setStage(ctx, stageWaiting)
			return p.inputRequest(), zero, nil
		}
		if err == nil && p != nil && p.answer != nil && p.answer.spent {
			setStage(ctx, stageWritten)
		}
		if err != nil {
			return nil, zero, fail(err)
		}
		if c, ok := any(&out).(costed); ok {
			c.setUnits(gapi.UnitsSpent(ctx))
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: out.Render()}},
		}, out, nil
	}
}

// fail turns an error into the "[class] message" tool error of §6.5.
// An unclassified error is a bug in this server; it still arrives with
// a class, so the vocabulary stays closed from the caller's side.
func fail(err error) error {
	var e *gapi.Error
	if errors.As(err, &e) {
		return errors.New(e.Error())
	}
	return fmt.Errorf("[%s] %s", gapi.ClassUnavailable, err.Error())
}

// takesConfirm reports whether In has confirm or confirm_recipients: the
// guards a model sets, which a tool that has one backs with the person.
func takesConfirm[In any]() bool {
	t := reflect.TypeFor[In]()
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "confirm" || name == "confirm_recipients" {
			return true
		}
	}
	return false
}

// dryRunField is the index of the input's dry_run bool, or -1. Found by
// reflection so that a tool offering dry_run cannot also have to
// remember to honor it.
func dryRunField[In any]() int {
	t := reflect.TypeFor[In]()
	if t.Kind() != reflect.Struct {
		return -1
	}
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "dry_run" && t.Field(i).Type.Kind() == reflect.Bool {
			return i
		}
	}
	return -1
}

// timeSchema is what a time.Time becomes in a schema. jsonschema-go
// infers a plain string, and the format is what tells a client it is a
// timestamp.
var timeSchema = &jsonschema.ForOptions{
	TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[time.Time](): {Type: "string", Format: "date-time"},
	},
}

// outputSchema and inputSchema build a tool's schemas with timestamps
// annotated. They panic on failure because only a type this server
// declares can fail, which is a build error found at start.
func outputSchema[T any]() *jsonschema.Schema { return schemaFor[T]("output") }

func inputSchema[T any]() *jsonschema.Schema { return schemaFor[T]("input") }

func schemaFor[T any](which string) *jsonschema.Schema {
	s, err := jsonschema.For[T](timeSchema)
	if err != nil {
		panic("tools: " + which + " schema for " + reflect.TypeFor[T]().String() + ": " + err.Error())
	}
	return s
}

func ptr[T any](v T) *T { return &v }
