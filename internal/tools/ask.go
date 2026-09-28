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

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/render"
)

// Asking the person (§4.13). A write that takes confirm is put to the
// person through the client, as an MCP form elicitation, when the client
// declares it can ask. The question goes out the multi-round-trip way:
// the first call does every read, stops before the write and returns the
// question with a signed requestState; the call comes back with the
// answer and that state, reads again, and writes only on an accept that
// ticked the box. Clients on protocols before 2026-07-28 get the same
// through the SDK, which asks with elicitation/create and calls the
// handler again in the same request.

// askTTL is how long a question may wait for its answer. An answer
// after it is refused, and the call is made again to ask again.
var askTTL = 5 * time.Minute

// askKey names the one input request and the one field it asks for.
const askKey = "confirm"

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
// draft's message_id witness among them — and to the question shown.
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
	args    string
	canAsk  bool
	require bool
	now     time.Time

	// answer is set when the call came back with a question this
	// process asked, verified.
	answer *answer
	// asked is set when the service asked and the question is to go out.
	asked *render.Question
}

type answer struct {
	question string
	action   string // accept, decline, cancel, or none
	ticked   bool
	spent    bool
}

// errAsking stops the service before its write while the question goes
// out. It becomes the input request and never reaches a caller.
var errAsking = gapi.Errf(gapi.ClassBlocked, "the person has not answered yet; nothing was written")

// personFor sets up one call. A call that carries answers must also
// carry the requestState they belong to, which must be one this process
// issued for this tool and these arguments, unexpired and unspent: a
// client could otherwise answer a question before it was asked.
func (a *asking) personFor(req *mcp.CallToolRequest, tool string, in any, require bool) (*person, error) {
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	p := &person{a: a, tool: tool, args: hex.EncodeToString(sum[:]), require: require, now: time.Now()}
	if c := req.ClientCapabilities(); c != nil && c.Elicitation != nil {
		// Form is what an empty elicitation capability declares; only a
		// client that declares URL alone cannot show a form.
		p.canAsk = c.Elicitation.Form != nil || c.Elicitation.URL == nil
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
	st, err := a.redeem(state, tool, p.args, p.now)
	if err != nil {
		return nil, err
	}
	p.answer = &answer{question: st.Question, action: "none"}
	if r, ok := responses[askKey].(*mcp.ElicitResult); ok && r != nil {
		switch r.Action {
		case "accept", "decline", "cancel":
			p.answer.action = r.Action
		default:
			p.answer.action = "other"
		}
		ticked, _ := r.Content[askKey].(bool)
		p.answer.ticked = r.Action == "accept" && ticked
	}
	a.lg.Info("person_answered", "tool", tool, "answer", p.answer.action, "confirmed", p.answer.ticked)
	return p, nil
}

// Ask implements service.Asker.
func (p *person) Ask(_ context.Context, q render.Question) error {
	if ans := p.answer; ans != nil {
		switch {
		case ans.spent:
			return gapi.Errf(gapi.ClassUnavailable, "%s asked the person twice in one call, which is a bug in this server; nothing more was written", p.tool)
		case ans.question != questionSum(q):
			ans.spent = true
			return gapi.Errf(gapi.ClassBlocked, "what %s would do changed after the person was asked, so what they saw is not "+
				"what would be written; nothing was written. Call it again to ask again", p.tool)
		case !ans.ticked:
			ans.spent = true
			return gapi.Errf(gapi.ClassBlocked, "%s was not confirmed by the person: the client answered %s%s. Nothing was "+
				"written. Do not call it again unless the person asks for it", p.tool, ans.action, unticked(ans.action))
		}
		ans.spent = true
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

// unticked says why an accept did not count.
func unticked(action string) string {
	if action == "accept" {
		return " without ticking the box"
	}
	return ""
}

// inputRequest is the question as the result that asks it.
func (p *person) inputRequest() *mcp.CallToolResult {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	st := askState{Tool: p.tool, Args: p.args, Question: questionSum(*p.asked), Nonce: hex.EncodeToString(nonce),
		Expires: p.now.Add(askTTL).Unix()}
	p.a.lg.Info("person_asked", "tool", p.tool)
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{askKey: &mcp.ElicitParams{
			Mode:    "form",
			Message: p.asked.Text,
			RequestedSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{askKey: {
					Type: "boolean", Title: p.asked.Title, Default: json.RawMessage("false"),
				}},
				Required: []string{askKey},
			},
		}},
		RequestState: p.a.sign(st),
	}
}

// questionSum binds a state to the question's words.
func questionSum(q render.Question) string {
	sum := sha256.Sum256([]byte(q.Title + "\x00" + q.Text))
	return hex.EncodeToString(sum[:])
}

type askedKey struct{}

// markAsked records on the request's context that a question went out.
func markAsked(ctx context.Context) {
	if f, ok := ctx.Value(askedKey{}).(*atomic.Bool); ok {
		f.Store(true)
	}
}

// AskFailures is receiving middleware that makes a question the client
// failed to answer a [blocked] tool result. On protocols before
// 2026-07-28 the SDK asks with elicitation/create itself, and a client
// that answers with an error, or an answer that does not fit the form,
// fails the whole tools/call as a JSON-RPC error; the call wrote nothing,
// since its write waits for the answer, and the caller is told so in the
// vocabulary of §6.5. The client's error text is not repeated.
func AskFailures() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if _, ok := req.(*mcp.CallToolRequest); !ok {
				return next(ctx, method, req)
			}
			asked := &atomic.Bool{}
			res, err := next(context.WithValue(ctx, askedKey{}, asked), method, req)
			if err == nil || !asked.Load() {
				return res, err
			}
			out := &mcp.CallToolResult{}
			out.SetError(errors.New("[" + string(gapi.ClassBlocked) + "] the call was not confirmed by the person: the " +
				"client could not put the question to them, or its answer could not be read. Nothing was written"))
			return out, nil
		}
	}
}
