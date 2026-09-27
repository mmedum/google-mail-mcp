package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// smoke drives the shipped binary over stdio, signed out.
//
// It is the check that the artifact speaks the protocol at all: every
// stdout line a JSON-RPC frame, both protocol revisions answered, the
// surface listed, a tool call failing as a classified tool error rather
// than a crash, and a client that hangs up mid-request ending the
// process with exit 0.
func smoke(out io.Writer, args []string) error {
	bin, cleanup, err := serverBinary(args)
	if err != nil {
		return err
	}
	defer cleanup()
	return smokeRun(out, bin)
}

const (
	// smokeCurrentRevision is the newest revision the initialize
	// handshake negotiates. From 2026-07-28 the handshake is deprecated
	// and the SDK caps it here; that revision is probed through
	// server/discover instead (smokeDiscover).
	smokeCurrentRevision = "2025-11-25"
	// smokePreviousRevision is the older revision a client in the field
	// still sends.
	smokePreviousRevision = "2025-06-18"
	// smokeDiscoverRevision is the revision server/discover must list:
	// the newest the linked SDK supports, which
	// TestSmokeProbesTheSDKsNewestRevision holds, so an SDK bump that
	// adds a revision fails until this gate asks for it.
	smokeDiscoverRevision = "2026-07-28"
	// smokeTimeout bounds one session.
	smokeTimeout = 30 * time.Second
	// smokeProbeTool is registered in every mode and needs a login, so
	// signed out it must answer [auth].
	smokeProbeTool = "get_profile"
)

// smokeSupported is the linked SDK's list, a variable so a test can
// replace it.
var smokeSupported = mcp.SupportedProtocolVersions

// JSON-RPC codes the SDK uses when a connection closes with a request in
// flight. Matched by code, never by message text.
const (
	smokeCodeConnectionClosed = -32004
	smokeCodeRequestCanceled  = -32003
)

// smokeMode is one configuration the server registers a different set
// of tools under, and the kinds of tool it registers there.
type smokeMode struct {
	name string
	// env is what the mode sets beyond the isolated environment.
	env []string
	// localDir sets GMAIL_LOCAL_DIR, which registers the tools that
	// write a file. Read-only mode leaves it unset: annotations cannot
	// tell a tool that writes a file from one that writes the mailbox,
	// so with it set the read-only set would not be derivable.
	localDir bool
	// registers are the kinds the mode registers, as smokeKind names
	// them.
	registers []string
}

// smokeModes are the modes internal/tools/register.go's Kind.allowed
// distinguishes. Each is driven through a whole session and must list
// exactly the tools of its kinds from the dump.
var smokeModes = []smokeMode{
	{name: "default", localDir: true, registers: smokeDefaultKinds},
	{name: "read-only", env: []string{"GMAIL_READ_ONLY=true"}, registers: []string{smokeKindRead}},
	{name: "send", env: []string{"GMAIL_ENABLE_SEND=true"}, localDir: true,
		registers: append(slices.Clone(smokeDefaultKinds), smokeKindSend)},
	{name: "destructive", env: []string{"GMAIL_ENABLE_DESTRUCTIVE=true"}, localDir: true,
		registers: append(slices.Clone(smokeDefaultKinds), smokeKindDestructive)},
	{name: "settings", env: []string{"GMAIL_ENABLE_SETTINGS=true"}, localDir: true,
		registers: append(slices.Clone(smokeDefaultKinds), smokeKindSettings, smokeKindSettingsForGood)},
	{name: "settings and send", env: []string{"GMAIL_ENABLE_SETTINGS=true", "GMAIL_ENABLE_SEND=true"}, localDir: true,
		registers: append(slices.Clone(smokeDefaultKinds), smokeKindSend, smokeKindSettings, smokeKindSettingsForGood,
			smokeKindAutoReply)},
}

// smokeDefaultKinds are the kinds the default mode registers, with
// GMAIL_LOCAL_DIR set.
var smokeDefaultKinds = []string{smokeKindRead, smokeKindReadLocal, smokeKindWrite, smokeKindWriteForGood}

// smokeMinModes is the floor on modes driven: default, read-only, send,
// destructive, settings, and settings with send.
const smokeMinModes = 6

// environ is the mode's environment on top of base.
func (m smokeMode) environ(base []string, localDir string) []string {
	env := append(slices.Clone(base), m.env...)
	if m.localDir {
		env = append(env, "GMAIL_LOCAL_DIR="+localDir)
	}
	return env
}

func smokeRun(out io.Writer, bin string) error {
	if !slices.Contains(smokeSupported(), smokeCurrentRevision) {
		return fmt.Errorf("the linked MCP SDK supports %v, which lacks the current revision %s",
			smokeSupported(), smokeCurrentRevision)
	}
	if len(smokeModes) < smokeMinModes {
		return fmt.Errorf("%d modes in the table, want at least %d", len(smokeModes), smokeMinModes)
	}
	surface, _, err := dumpSchemas(bin)
	if err != nil {
		return err
	}
	kinds, err := smokeDumpKinds(surface)
	if err != nil {
		return err
	}
	env, localDir, cleanupEnv, err := smokeEnv()
	if err != nil {
		return err
	}
	defer cleanupEnv()

	frames := 0
	var counts []string
	for _, mode := range smokeModes {
		tools := 0
		for _, rev := range []string{smokeCurrentRevision, smokePreviousRevision} {
			n, listed, err := smokeSession(bin, mode.environ(env, localDir), rev, surface, mode, kinds)
			if err != nil {
				return fmt.Errorf("%s mode, revision %s: %w", mode.name, rev, err)
			}
			frames += n
			tools = listed
		}
		counts = append(counts, fmt.Sprintf("%s %d", mode.name, tools))
	}
	if err := smokeAbruptDisconnect(bin, env); err != nil {
		return err
	}
	versions, err := smokeDiscover(bin, env)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "smoke ok: %d frames read across %d modes at revisions %s and %s; server/discover "+
		"lists %v; tools registered: %s, of %d in the full surface; a mid-request disconnect exited 0\n",
		frames, len(smokeModes), smokeCurrentRevision, smokePreviousRevision, versions, strings.Join(counts, ", "),
		len(surface.Tools))
	return nil
}

// The kinds a tool's annotations and _meta can say, following
// internal/tools/register.go. Write covers both Write and
// ReadWritesLocally, which carry the same annotations.
const (
	smokeKindRead         = "read"
	smokeKindWrite        = "write"
	smokeKindWriteForGood = "write-for-good"
	smokeKindSend         = "send"
	smokeKindDestructive  = "destructive"
	// The kinds that share another's annotations, and so are read from
	// the dump's kinds rather than from what a client sees.
	smokeKindReadLocal       = "read-writes-locally"
	smokeKindSettings        = "settings"
	smokeKindSettingsForGood = "settings-for-good"
	smokeKindAutoReply       = "auto-reply"
)

// smokeFamily is the annotations each dumped kind must carry, as
// smokeKind names them: a settings write looks like a write for good to
// a client, and the vacation reply like a send.
var smokeFamily = map[string]string{
	smokeKindRead: smokeKindRead, smokeKindReadLocal: smokeKindWrite, smokeKindWrite: smokeKindWrite,
	smokeKindWriteForGood: smokeKindWriteForGood, smokeKindSend: smokeKindSend, smokeKindDestructive: smokeKindDestructive,
	smokeKindSettings: smokeKindWriteForGood, smokeKindSettingsForGood: smokeKindWriteForGood, smokeKindAutoReply: smokeKindSend,
}

// smokeUserInteraction is the _meta key a send or destructive tool
// carries.
const smokeUserInteraction = "anthropic/requiresUserInteraction"

// smokeKind reads a tool's kind from its annotations and _meta, the way
// Kind.annotations and Kind.requiresUserInteraction write them. An
// absent hint takes the MCP default: destructive and open-world. A
// combination no Kind produces is an error.
func smokeKind(name string, annotations, meta json.RawMessage) (string, error) {
	var a struct {
		ReadOnly    *bool `json:"readOnlyHint"`
		Destructive *bool `json:"destructiveHint"`
		OpenWorld   *bool `json:"openWorldHint"`
	}
	if len(annotations) > 0 {
		if err := json.Unmarshal(annotations, &a); err != nil {
			return "", fmt.Errorf("%s: annotations: %w", name, err)
		}
	}
	var m map[string]any
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &m); err != nil {
			return "", fmt.Errorf("%s: _meta: %w", name, err)
		}
	}
	hint := func(v *bool, def bool) bool {
		if v == nil {
			return def
		}
		return *v
	}
	ro, de, ow := hint(a.ReadOnly, false), hint(a.Destructive, true), hint(a.OpenWorld, true)
	ui := m[smokeUserInteraction] == true
	switch {
	case ro && !ui:
		return smokeKindRead, nil
	case !ro && ui && ow && !de:
		return smokeKindSend, nil
	case !ro && ui && de && !ow:
		return smokeKindDestructive, nil
	case !ro && !ui && de && !ow:
		return smokeKindWriteForGood, nil
	case !ro && !ui && !de && !ow:
		return smokeKindWrite, nil
	}
	return "", fmt.Errorf("%s is annotated readOnly=%v destructive=%v openWorld=%v with %s=%v, which no Kind "+
		"in internal/tools produces", name, ro, de, ow, smokeUserInteraction, ui)
}

// smokeDumpKinds is every dumped tool's kind, by name, as the dump
// names it. Each tool's annotations must be the ones its kind carries.
func smokeDumpKinds(surface *schemaDump) (map[string]string, error) {
	kinds := map[string]string{}
	for _, t := range surface.Tools {
		fam, err := smokeKind(t.Name, t.Annotations, t.Meta)
		if err != nil {
			return nil, fmt.Errorf("--dump-schemas: %w", err)
		}
		k := surface.Kinds[t.Name]
		switch want, known := smokeFamily[k]; {
		case k == "":
			return nil, fmt.Errorf("--dump-schemas names no kind for %s", t.Name)
		case !known:
			return nil, fmt.Errorf("--dump-schemas names %s's kind %q, which this gate does not know", t.Name, k)
		case want != fam:
			return nil, fmt.Errorf("%s is a %s tool annotated as a %s tool; its kind carries %s annotations", t.Name, k, fam, want)
		}
		kinds[t.Name] = k
	}
	return kinds, nil
}

// smokeEnv is the parent's environment without any GMAIL_ setting, with
// HOME and the config directory in a fresh temporary directory and a
// profile no keyring holds, so the run can never find a real login.
// localDir is an empty directory a mode may name as GMAIL_LOCAL_DIR.
func smokeEnv() (env []string, localDir string, cleanup func(), err error) {
	home, err := os.MkdirTemp("", "gates-smoke-")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(home) }
	localDir = filepath.Join(home, "local")
	if err := os.Mkdir(localDir, 0o700); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(key, "GMAIL_"),
			key == "HOME", key == "USERPROFILE", key == "XDG_CONFIG_HOME", key == "APPDATA":
			continue
		}
		env = append(env, kv)
	}
	config := filepath.Join(home, "config")
	env = append(env,
		"HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+config, "APPDATA="+config,
		"GMAIL_CONFIG_DIR="+filepath.Join(config, binaryName),
		"GMAIL_PROFILE=smoke-"+filepath.Base(home),
		"GMAIL_LOG_LEVEL=error")
	return env, localDir, cleanup, nil
}

// smokeFrame is one message to send. A request has an id; a
// notification does not.
type smokeFrame struct {
	id     int
	method string
	params any
}

func (f smokeFrame) encode() string {
	m := map[string]any{"jsonrpc": "2.0", "method": f.method}
	if f.id != 0 {
		m["id"] = f.id
	}
	if f.params != nil {
		m["params"] = f.params
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // the frames are built here from literals
	}
	return string(b)
}

// smokeAwaited derives the ids a session waits for from the frames it
// sends, so adding a request cannot leave its answer unread.
func smokeAwaited(frames []smokeFrame) []int {
	var ids []int
	for _, f := range frames {
		if f.id != 0 {
			ids = append(ids, f.id)
		}
	}
	return ids
}

func smokeInit(id int, rev string) smokeFrame {
	return smokeFrame{id: id, method: "initialize", params: map[string]any{
		"protocolVersion": rev,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gates-smoke", "version": "0"},
	}}
}

// smokeReply is one decoded response.
type smokeReply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// smokeSession runs one ordinary session in a mode and checks every
// answer, the tool listing against what the dump says the mode
// registers.
func smokeSession(bin string, env []string, rev string, surface *schemaDump, mode smokeMode,
	kinds map[string]string) (int, int, error) {
	frames := []smokeFrame{
		smokeInit(1, rev),
		{method: "notifications/initialized"},
		{id: 2, method: "tools/list"},
		{id: 3, method: "resources/list"},
		{id: 4, method: "resources/templates/list"},
		{id: 5, method: "tools/call", params: map[string]any{"name": smokeProbeTool, "arguments": map[string]any{}}},
	}
	replies, err := smokeDrive(bin, env, frames, true)
	if err != nil {
		return 0, 0, err
	}
	if err := smokeCheckInit(replies[1], rev); err != nil {
		return 0, 0, err
	}
	tools, err := smokeListedKinds(replies[2])
	if err != nil {
		return 0, 0, err
	}
	if err := smokeCheckTools(tools, kinds, mode); err != nil {
		return 0, 0, err
	}
	if err := smokeCheckResources(replies[3], replies[4], surface); err != nil {
		return 0, 0, err
	}
	if err := smokeCheckSignedOut(replies[5]); err != nil {
		return 0, 0, err
	}
	return len(replies), len(tools), nil
}

func smokeCheckInit(r smokeReply, rev string) error {
	if r.Error != nil {
		return fmt.Errorf("initialize refused: %d", r.Error.Code)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	if res.ProtocolVersion != rev {
		return fmt.Errorf("asked for %s, the server negotiated %q", rev, res.ProtocolVersion)
	}
	return nil
}

// smokeListedKinds is each tool tools/list names, with the kind its
// own annotations and _meta say.
func smokeListedKinds(r smokeReply) (map[string]string, error) {
	if r.Error != nil {
		return nil, fmt.Errorf("tools/list refused: %d", r.Error.Code)
	}
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Annotations json.RawMessage `json:"annotations"`
			Meta        json.RawMessage `json:"_meta"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return nil, fmt.Errorf("tools/list result: %w", err)
	}
	if len(res.Tools) == 0 {
		return nil, errors.New("tools/list answered with no tools")
	}
	listed := map[string]string{}
	for _, t := range res.Tools {
		k, err := smokeKind(t.Name, t.Annotations, t.Meta)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		listed[t.Name] = k
	}
	return listed, nil
}

// smokeCheckTools holds a mode's listing to the dump: every tool listed
// is dumped with the same kind, and the mode lists exactly the dumped
// tools of the kinds it registers. A tool the dump does not carry is one
// the schema gates never saw; one of a kind the mode must not register
// is a leak past the only control there is.
func smokeCheckTools(listed, dumped map[string]string, mode smokeMode) error {
	for _, name := range slices.Sorted(maps.Keys(listed)) {
		want, ok := dumped[name]
		switch {
		case !ok:
			return fmt.Errorf("tools/list names %s, which --dump-schemas does not", name)
		case listed[name] != smokeFamily[want]:
			return fmt.Errorf("tools/list shows %s as a %s tool, --dump-schemas as a %s tool", name, listed[name], want)
		case !slices.Contains(mode.registers, want):
			return fmt.Errorf("%s mode registers %s, a %s tool it must not", mode.name, name, want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(dumped)) {
		if _, ok := listed[name]; !ok && slices.Contains(mode.registers, dumped[name]) {
			return fmt.Errorf("%s mode lacks %s, a %s tool it registers", mode.name, name, dumped[name])
		}
	}
	if _, ok := listed[smokeProbeTool]; !ok {
		return fmt.Errorf("tools/list lacks %s, which every mode registers", smokeProbeTool)
	}
	return nil
}

// smokeCheckResources holds resources/list and resources/templates/list
// to the dump. With none dumped, an empty list or method-not-found are
// both the truth.
func smokeCheckResources(list, templates smokeReply, surface *schemaDump) error {
	want := smokeURIs(surface.Resources, "uri")
	got, err := smokeListed(list, "resources", "uri", len(want))
	if err != nil {
		return fmt.Errorf("resources/list: %w", err)
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("resources/list gives %v, the dump %v", got, want)
	}
	wantT := smokeURIs(surface.ResourceTemplates, "uriTemplate")
	gotT, err := smokeListed(templates, "resourceTemplates", "uriTemplate", len(wantT))
	if err != nil {
		return fmt.Errorf("resources/templates/list: %w", err)
	}
	if !slices.Equal(gotT, wantT) {
		return fmt.Errorf("resources/templates/list gives %v, the dump %v", gotT, wantT)
	}
	return nil
}

func smokeURIs(raw []json.RawMessage, key string) []string {
	out := []string{}
	for _, r := range raw {
		var m map[string]any
		if json.Unmarshal(r, &m) == nil {
			if s, ok := m[key].(string); ok {
				out = append(out, s)
			}
		}
	}
	slices.Sort(out)
	return out
}

func smokeListed(r smokeReply, field, key string, want int) ([]string, error) {
	if r.Error != nil {
		// -32601 is method not found: a server with no resources need
		// not advertise the capability.
		if r.Error.Code == -32601 && want == 0 {
			return []string{}, nil
		}
		return nil, fmt.Errorf("refused: %d", r.Error.Code)
	}
	// Decode only the list: the result carries other members beside it,
	// such as the SDK's ttlMs, whose types are not ours to assume.
	var res map[string]json.RawMessage
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return nil, err
	}
	var items []map[string]any
	if raw, ok := res[field]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
	}
	out := []string{}
	for _, item := range items {
		if s, ok := item[key].(string); ok {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out, nil
}

// smokeCheckSignedOut: signed out, a tool call is a tool result with
// isError and the [auth] class, never a protocol error or a crash.
func smokeCheckSignedOut(r smokeReply) error {
	if r.Error != nil {
		return fmt.Errorf("%s signed out was a protocol error %d; it must be a tool error", smokeProbeTool, r.Error.Code)
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return fmt.Errorf("tools/call result: %w", err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		text.WriteString(c.Text)
	}
	if !res.IsError || !strings.HasPrefix(strings.TrimSpace(text.String()), "[auth]") {
		return fmt.Errorf("%s signed out answered isError=%v %q; want an [auth] tool error",
			smokeProbeTool, res.IsError, smokeClip(text.String()))
	}
	return nil
}

// smokeAbruptDisconnect writes a tool call and closes stdin the moment
// the last byte is written, so the server sees EOF with the request in
// flight. That is how a session ends when a person quits their client,
// and it must exit 0. Any response it still manages to write must be a
// result or one of the two closed-connection codes.
func smokeAbruptDisconnect(bin string, env []string) error {
	frames := []smokeFrame{
		smokeInit(1, smokeCurrentRevision),
		{method: "notifications/initialized"},
		{id: 2, method: "tools/call", params: map[string]any{"name": smokeProbeTool, "arguments": map[string]any{}}},
	}
	replies, err := smokeDrive(bin, env, frames, false)
	if err != nil {
		return fmt.Errorf("a client hanging up mid-request: %w", err)
	}
	for _, r := range replies {
		if r.Error != nil && r.Error.Code != smokeCodeConnectionClosed && r.Error.Code != smokeCodeRequestCanceled {
			return fmt.Errorf("a client hanging up mid-request: response %d ended with error %d; want a result, %d or %d",
				r.ID, r.Error.Code, smokeCodeConnectionClosed, smokeCodeRequestCanceled)
		}
	}
	return nil
}

// smokeReadResult is what smokeRead saw on stdout.
type smokeReadResult struct {
	replies map[int]smokeReply
	err     error
}

// smokeRead reads stdout until it closes, collecting responses by id and
// closing allIn once every pending id has answered. A line that is not
// a JSON-RPC frame is reported on done at once, and the pipe is then
// drained until the process exits.
func smokeRead(stdout io.Reader, pending map[int]bool, allIn chan<- struct{}, done chan<- smokeReadResult) {
	replies := map[int]smokeReply{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	signalled := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var frame struct {
			JSONRPC string           `json:"jsonrpc"`
			ID      *json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.JSONRPC != "2.0" {
			done <- smokeReadResult{err: fmt.Errorf("stdout carried a line that is not JSON-RPC: %q", smokeClip(line))}
			_, _ = io.Copy(io.Discard, stdout) // keep the pipe drained until exit
			return
		}
		if frame.ID == nil {
			continue // a notification or a server request
		}
		var r smokeReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // a request from the server with a string id
		}
		replies[r.ID] = r
		delete(pending, r.ID)
		if len(pending) == 0 && !signalled {
			signalled = true
			close(allIn)
		}
	}
	done <- smokeReadResult{replies: replies, err: sc.Err()}
}

// smokeWriteError explains a failed write to stdin: the server going
// away first, what the reader saw, or the write error itself.
func smokeWriteError(werr error, res smokeReadResult, stderr string) error {
	if errors.Is(werr, syscall.EPIPE) || errors.Is(werr, os.ErrClosed) {
		return fmt.Errorf("the server went away before reading its input: %w; stderr:\n%s", werr, smokeClip(stderr))
	}
	if res.err != nil {
		return res.err
	}
	return fmt.Errorf("write: %w; stderr:\n%s", werr, smokeClip(stderr))
}

// smokeDrive runs the binary, writes the frames and reads stdout.
//
// With wait, stdin stays open until every awaited id has answered, then
// closes: the client hanging up. Without it, stdin closes the moment the
// last frame is written and whatever arrives is returned.
//
// Every stdout line must be a JSON-RPC frame, and the process must exit
// 0 once stdin closes. A write that fails because the server already
// died (EPIPE) reaps the child and reports its stderr, which is where
// the reason is.
func smokeDrive(bin string, env []string, frames []smokeFrame, wait bool) (map[int]smokeReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	var lines []string
	for _, f := range frames {
		lines = append(lines, f.encode())
	}
	awaited := smokeAwaited(frames)

	pending := map[int]bool{}
	for _, id := range awaited {
		pending[id] = true
	}
	allIn := make(chan struct{})
	done := make(chan smokeReadResult, 1)
	go smokeRead(stdout, pending, allIn, done)

	_, werr := io.WriteString(stdin, strings.Join(lines, "\n")+"\n")
	if werr != nil {
		_ = stdin.Close()
		res := <-done
		_ = cmd.Wait()
		return nil, smokeWriteError(werr, res, stderr.String())
	}
	if wait {
		select {
		case <-allIn:
		case res := <-done:
			_ = stdin.Close()
			_ = cmd.Wait()
			if res.err != nil {
				return nil, res.err
			}
			return nil, fmt.Errorf("stdout closed with %d response(s) unanswered; stderr:\n%s",
				len(pending), smokeClip(stderr.String()))
		case <-ctx.Done():
			_ = stdin.Close()
			_ = cmd.Wait()
			return nil, fmt.Errorf("timed out waiting for responses; stderr:\n%s", smokeClip(stderr.String()))
		}
	}
	if err := stdin.Close(); err != nil {
		return nil, err
	}
	res := <-done
	waitErr := cmd.Wait()
	if res.err != nil {
		return nil, res.err
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("the server did not exit within %s of stdin closing", smokeTimeout)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("the server exited (%w) after the client hung up; a disconnect must be exit 0; stderr:\n%s",
			waitErr, smokeClip(stderr.String()))
	}
	if wait {
		for _, id := range awaited {
			if _, ok := res.replies[id]; !ok {
				return nil, fmt.Errorf("no response to id %d", id)
			}
		}
	}
	return res.replies, nil
}

func smokeClip(s string) string {
	const limit = 2000
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// smokeDiscover asks server/discover, the 2026-07-28 replacement for
// the initialize handshake, which revisions the server speaks, and
// requires the current one among them. The request carries the new
// protocol's _meta: without it the SDK answers method-not-found, a
// clean refusal that looks nothing like a missing feature, which is why
// the list is read back rather than the call merely made.
func smokeDiscover(bin string, env []string) ([]string, error) {
	frames := []smokeFrame{{id: 1, method: "server/discover", params: map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    smokeDiscoverRevision,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}}}
	replies, err := smokeDrive(bin, env, frames, true)
	if err != nil {
		return nil, fmt.Errorf("server/discover: %w", err)
	}
	r, ok := replies[1]
	if !ok {
		return nil, fmt.Errorf("server/discover: no reply")
	}
	if r.Error != nil {
		return nil, fmt.Errorf("server/discover was refused: %d %s", r.Error.Code, r.Error.Message)
	}
	var res struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return nil, fmt.Errorf("server/discover: %w", err)
	}
	if !slices.Contains(res.SupportedVersions, smokeDiscoverRevision) {
		return nil, fmt.Errorf("server/discover lists %v, which lacks the current revision %s",
			res.SupportedVersions, smokeDiscoverRevision)
	}
	return res.SupportedVersions, nil
}
