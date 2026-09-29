package tools

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
)

// Asking the person (§4.13). A write that takes confirm is put to the
// person through the client, as an MCP form elicitation, when the client
// declares it can ask. The form has no fields: accepting it is the
// confirmation. The question goes out the multi-round-trip way: the
// first call does every read, stops before the write and returns the
// question with a signed requestState; the call comes back with the
// answer and that state, reads again, and writes only on an accept.
// Clients on protocols before 2026-07-28 get the same through the SDK,
// which asks with elicitation/create and calls the handler again within
// the same request.

// askTTL is how long a question may wait for its answer when its state
// travels through the client, from 2026-07-28. An answer after it is
// refused, and the call is made again to ask again.
var askTTL = 5 * time.Minute

// inProcessTTL bounds a state that never leaves the process, before
// 2026-07-28: the request's own context bounds the wait, and this only
// how long its nonce is remembered.
const inProcessTTL = 24 * time.Hour

// statelessProtocol is the first revision whose client carries the
// requestState: the SDK's own test for asking the multi-round-trip way.
const statelessProtocol = "2026-07-28"

// askKey names the one input request.
const askKey = "confirm"

// emptyForm is the question's form: no fields, so the client's accept
// is the answer. The specification types properties as an open map with
// no minimum (§18 row 64).
var emptyForm = json.RawMessage(`{"type":"object","properties":{}}`)

// asking signs and redeems the requestState of every question this
// process asks. The key is drawn per process, so a state is good only
// in the process that issued it, and each is redeemed at most once.
type asking struct {
	key []byte
	lg  *slog.Logger

	mu   sync.Mutex
	used map[string]time.Time // nonce → when it expires
}

func newAsking(lg *slog.Logger) *asking {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &asking{key: key, lg: lg, used: map[string]time.Time{}}
}

// askState is what a requestState carries. It binds the answer to the
// tool, to the call's arguments — the ids it names and, for a send, the
// draft's message_id witness among them — and to what the question
// binds (render.Question.Bind).
type askState struct {
	Tool     string `json:"t"`
	Args     string `json:"a"`
	Question string `json:"q"`
	Nonce    string `json:"n"`
	Expires  int64  `json:"e"`
}

// sign is st as base64url JSON, a dot, and its HMAC-SHA256.
func (a *asking) sign(st askState) string {
	payload, _ := json.Marshal(st)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(a.mac(payload))
}

func (a *asking) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(payload)
	return m.Sum(nil)
}

// redeem checks a requestState a client echoed and spends it. Every
// refusal is [blocked]: whatever the answer was, nothing is written on
// it.
func (a *asking) redeem(state, tool, args string, now time.Time) (askState, error) {
	var st askState
	enc := base64.RawURLEncoding
	p, s, ok := strings.Cut(state, ".")
	payload, err1 := enc.DecodeString(p)
	sum, err2 := enc.DecodeString(s)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(sum, a.mac(payload)) || json.Unmarshal(payload, &st) != nil {
		return st, gapi.Errf(gapi.ClassBlocked,
			"the call came back with an answer to a question this server did not ask; nothing was written. Call it again without one")
	}
	if st.Tool != tool || st.Args != args {
		return st, gapi.Errf(gapi.ClassBlocked,
			"the answer came back with another call than the one the person was asked about; nothing was written. Call it again to ask again")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for n, exp := range a.used {
		if now.Unix() > exp.Unix() {
			delete(a.used, n)
		}
	}
	if now.Unix() > st.Expires {
		return st, gapi.Errf(gapi.ClassBlocked,
			"the question to the person expired before the answer came back; nothing was written. Call it again to ask again")
	}
	if _, spent := a.used[st.Nonce]; spent {
		return st, gapi.Errf(gapi.ClassBlocked, "that answer was already used once; nothing was written")
	}
	a.used[st.Nonce] = time.Unix(st.Expires, 0)
	return st, nil
}

// person is one call's way to the person: the service asks it before a
// write that takes confirm.
type person struct {
	a       *asking
	tool    string
	in      any
	canAsk  bool
	require bool
	// travels is whether the requestState goes through the client,
	// which is when askTTL applies.
	travels bool
	now     time.Time
	// argSum is the arguments' hash, computed when first needed.
	argSum string

	// answer is set when the call came back accepting a question this
	// process asked, verified.
	answer *answer
	// asked is set when the service asked and the question is to go out.
	asked *render.Question
}

type answer struct {
	question string
	spent    bool
}

// errAsking stops the service before its write while the question goes
// out. It becomes the input request and never reaches a caller.
var errAsking = gapi.Errf(gapi.ClassBlocked, "the person has not answered yet; nothing was written")

// args is the hash of the call's arguments, as the handler decoded them.
func (p *person) args() string {
	if p.argSum == "" {
		raw, _ := json.Marshal(p.in)
		sum := sha256.Sum256(raw)
		p.argSum = hex.EncodeToString(sum[:])
	}
	return p.argSum
}

// personFor sets up one call. A call that carries answers must also
// carry the requestState they belong to, which must be one this process
// issued for this tool and these arguments, unexpired and unspent: a
// client could otherwise answer a question before it was asked.
func (a *asking) personFor(req *mcp.CallToolRequest, tool string, in any, require bool) (*person, error) {
	p := &person{a: a, tool: tool, in: in, require: require, now: time.Now(), travels: true}
	if c := req.ClientCapabilities(); c != nil && c.Elicitation != nil {
		// Form is what an empty elicitation capability declares; only a
		// client that declares URL alone cannot show a form.
		p.canAsk = c.Elicitation.Form != nil || c.Elicitation.URL == nil
	}
	if req.Session != nil {
		if ip := req.Session.InitializeParams(); ip != nil {
			p.travels = ip.ProtocolVersion >= statelessProtocol
		}
	}
	var state string
	var responses mcp.InputResponseMap
	if req.Params != nil {
		state, responses = req.Params.RequestState, req.Params.InputResponses
	}
	switch {
	case state == "" && len(responses) == 0:
		return p, nil
	case state == "":
		return nil, gapi.Errf(gapi.ClassBlocked,
			"the call came with answers to a question this server has not asked; nothing was written. Call it again without them")
	}
	st, err := a.redeem(state, tool, p.args(), p.now)
	if err != nil {
		return nil, err
	}
	action := "none"
	if r, ok := responses[askKey].(*mcp.ElicitResult); ok && r != nil {
		switch r.Action {
		case "accept", "decline", "cancel":
			action = r.Action
		default:
			action = "other"
		}
	}
	a.lg.Info("person_answered", "tool", tool, "answer", action)
	if action != "accept" {
		// Refused here, before any read runs, so the refusal never
		// depends on this round reaching its question again.
		return nil, gapi.Errf(gapi.ClassBlocked, "%s was not confirmed by the person: the client answered %s. Nothing was "+
			"written. Do not call it again unless the person asks for it", tool, action)
	}
	p.answer = &answer{question: st.Question}
	return p, nil
}

// Ask implements service.Asker.
func (p *person) Ask(_ context.Context, q render.Question) error {
	if ans := p.answer; ans != nil {
		if ans.spent {
			return gapi.Errf(gapi.ClassUnavailable, "%s asked the person twice in one call, which is a bug in this server; nothing more was written", p.tool)
		}
		ans.spent = true
		if ans.question != questionSum(q) {
			return gapi.Errf(gapi.ClassBlocked, "what %s would do changed after the person was asked, so what they saw is not "+
				"what would be written; nothing was written. Call it again to ask again", p.tool)
		}
		return nil
	}
	if !p.canAsk {
		if p.require {
			return gapi.Errf(gapi.ClassBlocked, "%s needs the person to confirm it, and this client cannot ask them; "+
				"GMAIL_REQUIRE_PROMPT is set, so nothing was written. Use a client that supports MCP elicitation", p.tool)
		}
		return nil
	}
	p.asked = &q
	return errAsking
}

// inputRequest is the question as the result that asks it.
func (p *person) inputRequest() *mcp.CallToolResult {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	ttl := askTTL
	if !p.travels {
		ttl = inProcessTTL
	}
	st := askState{Tool: p.tool, Args: p.args(), Question: questionSum(*p.asked), Nonce: hex.EncodeToString(nonce),
		Expires: p.now.Add(ttl).Unix()}
	p.a.lg.Info("person_asked", "tool", p.tool)
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{askKey: &mcp.ElicitParams{
			Mode: "form", Message: p.asked.Text, RequestedSchema: emptyForm,
		}},
		RequestState: p.a.sign(st),
	}
}

// questionSum binds a state to what the question binds.
func questionSum(q render.Question) string {
	sum := sha256.Sum256([]byte(q.Bind))
	return hex.EncodeToString(sum[:])
}

// The stages of one tools/call that asked, for AskFailures: what a
// failure to reply means depends on how far the call got.
const (
	stageNone    int32 = iota
	stageWaiting       // the question is out; nothing is written before the answer
	stageWriting       // the answer confirmed the write, which may have happened
	stageWritten       // the handler returned after writing
)

type stageKey struct{}

func setStage(ctx context.Context, s int32) {
	if v, ok := ctx.Value(stageKey{}).(*atomic.Int32); ok {
		v.Store(s)
	}
}

func stageOf(ctx context.Context) int32 {
	if v, ok := ctx.Value(stageKey{}).(*atomic.Int32); ok {
		return v.Load()
	}
	return stageNone
}

// AskFailures is receiving middleware for a tools/call that asked the
// person and then failed as a JSON-RPC error rather than a tool result.
// On protocols before 2026-07-28 the SDK asks with elicitation/create
// itself, and a client that answers with an error, or an answer that
// does not fit the form, fails the whole call that way; that call wrote
// nothing, since its write waits for the answer, and it becomes
// [blocked]. A failure after the answer confirmed the write — the reply
// could not be built or sent, the request was canceled — may follow a
// write, so it becomes [ambiguous_outcome] and is never "nothing was
// written" (§4.3). The client's error text is not repeated.
func AskFailures() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if !ok {
				return next(ctx, method, req)
			}
			stage := &atomic.Int32{}
			res, err := next(context.WithValue(ctx, stageKey{}, stage), method, req)
			if err == nil || stage.Load() == stageNone {
				return res, err
			}
			name := "the call"
			if call.Params != nil && toolName(call.Params.Name) {
				name = call.Params.Name
			}
			var msg string
			switch stage.Load() {
			case stageWaiting:
				msg = "[" + string(gapi.ClassBlocked) + "] " + name + " was not confirmed by the person: the client " +
					"could not put the question to them, or its answer could not be read. Nothing was written"
			case stageWritten:
				msg = "[" + string(gapi.ClassAmbiguousOutcome) + "] the person confirmed " + name + ", and it was written " +
					"(verdict: written), but its result could not be returned. Do not make the call again"
			default:
				msg = "[" + string(gapi.ClassAmbiguousOutcome) + "] the person confirmed " + name + ", and the call ended " +
					"before its result, so the write may have started (verdict: unknown). Do not make the call again; " +
					"read the mailbox to see whether it took effect"
			}
			out := &mcp.CallToolResult{}
			out.SetError(errors.New(msg))
			return out, nil
		}
	}
}

// toolName is true for a name shaped like this server's tools, which a
// message may repeat; anything else came from the client.
func toolName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}
