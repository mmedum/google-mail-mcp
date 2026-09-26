//go:build live

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/scripts/internal/mcpstdio"
	"github.com/mmedum/google-mail-mcp/scripts/internal/transcript"
)

// step is one tool call the driver makes and what it checks.
//
// `gates live-cover` reads this table from the source: the tool literal
// and the keys of the map args builds are the options the step drives.
type step struct {
	name string
	tool string
	// quiet steps print a summary instead of the result. They are the
	// ones whose result is the whole mailbox rather than the run's mail —
	// every label name, the account — which the redactor cannot mask.
	quiet bool
	// args builds the arguments from what the run inserted.
	args func(e *env) map[string]any
	// check judges the result. It returns an error to fail the step.
	check func(e *env, text string) error
}

// A thread or message id in a rendering, 16 hex digits after its noun.
var (
	threadIDIn  = regexp.MustCompile(`thread ([0-9a-f]{16})`)
	messageIDIn = regexp.MustCompile(`message ([0-9a-f]{16})`)
	pageTokenIn = regexp.MustCompile(`page_token=(\S+)`)
)

// steps is every step, in order. Later steps read what earlier ones
// stored on env.
var steps = []step{
	{name: "profile", tool: "get_profile", quiet: true,
		args:  func(*env) map[string]any { return map[string]any{} },
		check: func(_ *env, text string) error { return want(text, "@") }},

	{name: "labels with counts", tool: "list_labels", quiet: true,
		args:  func(*env) map[string]any { return map[string]any{"counts": true} },
		check: func(e *env, text string) error { return want(text, e.seed.label.name) }},

	{name: "search threads in the run label, first page", tool: "search_threads",
		args: func(e *env) map[string]any {
			now := time.Now().UTC()
			return map[string]any{
				"q": e.seed.label.query(""), "labels": []any{e.seed.label.name},
				"after": now.AddDate(0, 0, -1).Format(time.DateOnly), "before": now.Add(time.Hour).Format(time.RFC3339),
				"time_zone": "Europe/Copenhagen", "max": 2, "include_spam_trash": false,
			}
		},
		check: func(e *env, text string) error {
			if err := e.ownsAll(threadIDIn, text, 2); err != nil {
				return err
			}
			m := pageTokenIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("three inserted threads at max 2 gave no next page")
			}
			e.pageToken = m[1]
			return nil
		}},

	{name: "search threads in the run label, second page", tool: "search_threads",
		args: func(e *env) map[string]any {
			return map[string]any{"q": e.seed.label.query(""), "max": 2, "page_token": e.pageToken}
		},
		check: func(e *env, text string) error { return e.ownsAll(threadIDIn, text, 1) }},

	{name: "search messages in the run label, first page", tool: "search_messages",
		args: func(e *env) map[string]any {
			now := time.Now().UTC()
			return map[string]any{
				"q": e.seed.label.query("Synthetic"), "labels": []any{e.seed.label.name},
				"after": now.Add(-24 * time.Hour).Format(time.RFC3339), "before": now.AddDate(0, 0, 1).Format(time.DateOnly),
				"time_zone": "America/Los_Angeles", "max": 2, "include_spam_trash": true,
			}
		},
		check: func(e *env, text string) error {
			if err := e.ownsAll(messageIDIn, text, 2); err != nil {
				return err
			}
			m := pageTokenIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("three inserted messages at max 2 gave no next page")
			}
			e.pageToken = m[1]
			return nil
		}},

	{name: "search messages in the run label, second page", tool: "search_messages",
		args: func(e *env) map[string]any {
			return map[string]any{"q": e.seed.label.query("Synthetic"), "max": 2, "page_token": e.pageToken}
		},
		check: func(e *env, text string) error { return e.ownsAll(messageIDIn, text, 1) }},

	{name: "read an inserted thread", tool: "get_thread",
		args: func(e *env) map[string]any {
			return map[string]any{"thread_id": e.seed.threads[0], "budget_chars": 2000, "show_quoted": true,
				"all_headers": true, "time_zone": "UTC", "cursor": 0}
		},
		check: func(_ *env, text string) error { return want(text, "Synthetic body 1") }},

	{name: "read an inserted message", tool: "get_message",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": e.seed.messages[1], "budget_chars": 4000, "show_quoted": false,
				"all_headers": false, "time_zone": "Europe/Copenhagen", "offset": 0}
		},
		check: func(_ *env, text string) error { return want(text, "Synthetic body 2") }},

	{name: "read an inserted message by its Message-ID", tool: "get_message",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": "rfc822:<" + e.seed.label.name + ".3@livemail.invalid>"}
		},
		check: func(_ *env, text string) error { return want(text, "Synthetic body 3") }},

	{name: "list the run's drafts, first page", tool: "list_drafts",
		args: func(e *env) map[string]any { return map[string]any{"q": "subject:" + e.seed.label.name, "max": 1} },
		check: func(e *env, text string) error {
			m := pageTokenIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("two run drafts at max 1 gave no next page")
			}
			e.pageToken = m[1]
			return want(text, "draft")
		}},

	{name: "list the run's drafts, second page", tool: "list_drafts",
		args: func(e *env) map[string]any {
			return map[string]any{"q": "subject:" + e.seed.label.name, "max": 1, "page_token": e.pageToken}
		},
		check: func(_ *env, text string) error { return want(text, "draft") }},

	{name: "read a run draft", tool: "get_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.seed.drafts[0], "budget_chars": 2000, "show_quoted": true,
				"all_headers": true, "time_zone": "UTC", "offset": 0}
		},
		check: func(_ *env, text string) error { return want(text, "Synthetic draft 1") }},
}

// env is what a step sees.
type env struct {
	session   *mcpstdio.Session
	tr        *transcript.Transcript
	seed      *seeded
	pageToken string
}

func want(text, s string) error {
	if !strings.Contains(text, s) {
		return fmt.Errorf("the result does not contain %q", s)
	}
	return nil
}

// ownsAll requires exactly n distinct ids of one kind in text, every one
// the run's own. Fewer means the server missed inserted mail; one the run
// did not insert means the scoping failed, which is the worse failure.
func (e *env) ownsAll(re *regexp.Regexp, text string, n int) error {
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if !e.seed.owns(m[1]) {
			return fmt.Errorf("%w: the result names an id the run did not insert", errUnscoped)
		}
		seen[m[1]] = true
	}
	if len(seen) != n {
		return fmt.Errorf("the result names %d ids; want %d", len(seen), n)
	}
	return nil
}

// errUnscoped is the guard refusing a read outside the run's own mail.
var errUnscoped = errors.New("refused: the live driver reads only what it inserted")

// guard holds a call to §9.1 before it is sent: a search must carry the
// run's label, a draft listing the run's name, and a read by id must name
// an id the run made.
func (e *env) guard(tool string, args map[string]any) error {
	q, _ := args["q"].(string)
	switch tool {
	case "search_threads", "search_messages":
		if !strings.Contains(q, "label:"+e.seed.label.name) {
			return fmt.Errorf("%w: %s without label:%s in q", errUnscoped, tool, e.seed.label.name)
		}
	case "list_drafts":
		if !strings.Contains(q, e.seed.label.name) {
			return fmt.Errorf("%w: list_drafts without the run's name in q", errUnscoped)
		}
	}
	for _, key := range []string{"thread_id", "message_id", "draft_id"} {
		id, ok := args[key].(string)
		if !ok {
			continue
		}
		if rest, isRFC := strings.CutPrefix(id, "rfc822:"); isRFC {
			if !strings.Contains(rest, e.seed.label.name) {
				return fmt.Errorf("%w: %s by a Message-ID the run did not write", errUnscoped, tool)
			}
			continue
		}
		if !e.seed.owns(id) {
			return fmt.Errorf("%w: %s on an id the run did not make", errUnscoped, tool)
		}
	}
	return nil
}

// runStep calls one step's tool through the guard and checks the result.
func (e *env) runStep(s step) error {
	args := s.args(e)
	if err := e.guard(s.tool, args); err != nil {
		return err
	}
	e.tr.Sayf("=== %s %s ===", s.tool, mcpstdio.Encode(args))
	text, isError, err := e.session.CallTool(s.tool, args)
	if err != nil {
		return err
	}
	if s.quiet {
		e.tr.Sayf("(%d characters; not printed, since this result is the whole mailbox's rather than the run's)", len(text))
	} else {
		e.tr.Say(text)
	}
	if isError {
		return errors.New("the tool refused")
	}
	return s.check(e, text)
}

// spike is one question the documentation does not answer (§15). Each
// states its question and its verdict separately.
type spike struct {
	name     string
	question string
	ask      func(ctx context.Context, box mailbox, s *seeded) (verdict string)
}

// spikes run after the steps, against the run's own mail only.
var spikes = []spike{
	{name: "F (dates)", question: "Which date does messages.insert record with internalDateSource=receivedTime: " +
		"the insert's own time, or the message's Date header?",
		ask: func(ctx context.Context, box mailbox, s *seeded) string {
			ms, err := box.InternalDate(ctx, s.messages[0])
			if err != nil {
				return "could not read the message's internalDate: " + err.Error()
			}
			got := time.UnixMilli(ms).UTC()
			header, _ := time.Parse(time.RFC1123Z, syntheticDateHeader)
			switch {
			case got.Equal(header):
				return fmt.Sprintf("internalDate is %s, the Date header, not the time of the insert", got.Format(time.RFC3339))
			case time.Since(got) < time.Hour:
				return fmt.Sprintf("internalDate is %s, the time of the insert", got.Format(time.RFC3339))
			}
			return fmt.Sprintf("internalDate is %s, neither the Date header nor the insert's time", got.Format(time.RFC3339))
		}},
	{name: "K", question: "Is a history cursor far in the past answered 404, as the sync guide says?",
		ask: func(ctx context.Context, box mailbox, _ *seeded) string {
			status := box.Probe(ctx, http.MethodGet, "history", url.Values{"startHistoryId": {"1"}})
			return fmt.Sprintf("history.list with startHistoryId=1 answered HTTP %d (the guide says 404)", status)
		}},
	{name: "G (positive half)", question: "Does Gmail refuse messages.delete under gmail.modify, " +
		"so only https://mail.google.com/ can delete permanently?",
		ask: func(ctx context.Context, box mailbox, s *seeded) string {
			last := s.messages[len(s.messages)-1]
			status := box.Probe(ctx, http.MethodDelete, "messages/"+url.PathEscape(last), nil)
			if status/100 == 2 {
				// It is gone for good, and it was the run's own synthetic
				// message; cleanup must not trash it again.
				s.messages = s.messages[:len(s.messages)-1]
				return fmt.Sprintf("messages.delete on a run message answered HTTP %d: DELETED. "+
					"The token carries a scope that deletes; check the profile's scopes before trusting §4.6", status)
			}
			return fmt.Sprintf("messages.delete on a run message answered HTTP %d (expected 403)", status)
		}},
}

func runSpikes(ctx context.Context, box mailbox, s *seeded, tr *transcript.Transcript) {
	for _, sp := range spikes {
		tr.Sayf("spike %s — question: %s", sp.name, sp.question)
		tr.Sayf("spike %s — observed: %s", sp.name, sp.ask(ctx, box, s))
	}
}
