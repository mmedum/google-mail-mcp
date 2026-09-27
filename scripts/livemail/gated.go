//go:build live

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// The gated tools' steps and phase 3's spikes. The server runs with
// sending and permanent deletion registered. Every send_draft step but
// one works on a draft addressed to reserved domains and is refused or a
// dry run; the one that sends needs -send-to, and its draft goes to that
// address alone. The delete steps touch only the run's own messages and
// labels, and a permanent delete needs a login that granted
// https://mail.google.com/.

var (
	sentDraftIn    = regexp.MustCompile(`sent draft (\S+) · sent message ([0-9a-f]{16}) · thread ([0-9a-f]{16})`)
	draftRFC822In  = regexp.MustCompile(`(?m)^Message-ID: (<[^<>\s]+>)$`)
	deletedLabelIn = regexp.MustCompile(`deleted label .* · id (\S+);`)
)

// needSendTo keeps a step that sends to runs given -send-to.
func needSendTo(e *env) string {
	if e.sendTo == "" {
		return "it sends mail, and -send-to was not given"
	}
	return ""
}

// needFull keeps a permanent delete to profiles whose login granted the
// one scope Gmail accepts for it.
func needFull(e *env) string {
	if !e.full {
		return "the profile's login did not grant https://mail.google.com/; log in with GMAIL_ENABLE_DESTRUCTIVE=true to drive it"
	}
	return ""
}

var gatedSteps = []step{
	{name: "send a draft, dry run", tool: "send_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.draftID, "message_id": e.draftMessage, "dry_run": true}
		},
		check: func(_ *env, text string) error {
			if err := want(text, "it starts a new conversation"); err != nil {
				return err
			}
			return want(text, "not confirmed: to[0], bcc[0]")
		}},

	// The parent was received: its sender is in the thread, and the To
	// its sender wrote is not (§17.8).
	{name: "a reply-all clears the sender only, dry run", tool: "send_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.replyAll, "message_id": e.replyAllMessage, "dry_run": true}
		},
		check: func(_ *env, text string) error {
			if err := want(text, "recipients: 2 · 1 in the thread · 0 confirmed"); err != nil {
				return err
			}
			return want(text, "not confirmed: to[1]")
		}},

	{name: "a send with unconfirmed recipients is refused", tool: "send_draft", refuses: "blocked",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.draftID, "message_id": e.draftMessage}
		},
		check: func(_ *env, text string) error { return want(text, "name each address in confirm_recipients") }},

	{name: "a send against an old message id is refused", tool: "send_draft", refuses: "stale",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.draftID, "message_id": e.staleMessage}
		},
		check: func(_ *env, text string) error { return want(text, "changed since it was read") }},

	{name: "compose a draft to -send-to", tool: "create_draft", needs: needSendTo,
		args: func(e *env) map[string]any {
			return map[string]any{"to": []any{e.sendTo}, "subject": e.seed.label.name + " send_draft",
				"body": "A synthetic message, sent by the live driver through send_draft.\n"}
		},
		check: func(e *env, text string) error {
			var err error
			e.sent.draftID, e.sent.draftMessage, err = e.composed(text)
			return err
		}},

	{name: "send the draft to -send-to", tool: "send_draft", needs: needSendTo,
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.sent.draftID, "message_id": e.sent.draftMessage,
				"confirm_recipients": []any{e.sendTo}}
		},
		check: func(e *env, text string) error {
			m := sentDraftIn.FindStringSubmatch(text)
			if m == nil || m[1] != e.sent.draftID {
				return errors.New("the result names no sent draft")
			}
			// Sent: the draft is gone, and cleanup trashes the sent copy.
			e.seed.forget(m[1])
			e.sent.sentMessage = m[2]
			e.seed.messages = append(e.seed.messages, m[2])
			if r := draftRFC822In.FindStringSubmatch(text); r != nil {
				e.sent.rfc822 = r[1]
			}
			return want(text, "recipients: 1 · 0 in the thread · 1 confirmed")
		}},

	{name: "a label delete without confirm is refused", tool: "delete_label", refuses: "blocked",
		args:  func(e *env) map[string]any { return map[string]any{"label": e.seed.label.name + "-renamed"} },
		check: func(_ *env, text string) error { return want(text, "confirm: true") }},

	{name: "delete a label, dry run", tool: "delete_label",
		args: func(e *env) map[string]any {
			return map[string]any{"label": e.seed.label.name + "-renamed", "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "would delete label") }},

	{name: "delete a label", tool: "delete_label",
		args: func(e *env) map[string]any {
			return map[string]any{"label": e.seed.label.name + "-renamed", "confirm": true}
		},
		check: func(e *env, text string) error {
			m := deletedLabelIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("the result names no deleted label")
			}
			e.seed.forgetLabel(m[1])
			return want(text, "The mail itself is untouched.")
		}},

	{name: "a permanent delete without confirm is refused", tool: "delete_permanently", refuses: "blocked",
		args:  func(e *env) map[string]any { return map[string]any{"message_ids": []any{e.seed.messages[2]}} },
		check: func(_ *env, text string) error { return want(text, "cannot be undone") }},

	{name: "delete permanently, dry run", tool: "delete_permanently",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[2]}, "thread_ids": []any{e.seed.threads[1]}, "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "2 would be deleted for good · 0 failed") }},

	{name: "delete permanently", tool: "delete_permanently", needs: needFull,
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[2]}, "thread_ids": []any{e.seed.threads[1]}, "confirm": true}
		},
		check: func(e *env, text string) error {
			if err := want(text, "2 deleted for good · 0 failed"); err != nil {
				return err
			}
			// Each inserted message is its own thread, so the thread held
			// the second message alone.
			e.seed.deleted(e.seed.messages[2], e.seed.messages[1])
			return nil
		}},
}

// sendSpikes are phase 3's questions (§15). B and C read what the send
// step left; H reads the run's own message until Gmail pushes back.
var sendSpikes = []spike{
	{name: "C", question: "What does drafts.send leave behind: does the sent message keep the draft's message id, " +
		"and does drafts.get answer 404 afterwards?",
		ask: func(ctx context.Context, x spikeRun) string {
			if x.sent.sentMessage == "" {
				return "not run: the send step did not send (it needs -send-to)"
			}
			same := "a new id, not the draft's message id"
			if x.sent.sentMessage == x.sent.draftMessage {
				same = "the draft's message id"
			}
			r := x.box.Probe(ctx, http.MethodGet, "drafts/"+url.PathEscape(x.sent.draftID), url.Values{"format": {"minimal"}}, nil)
			return fmt.Sprintf("the sent message has %s; drafts.get on the sent draft answered HTTP %d, reason %q",
				same, r.status, r.reason)
		}},
	{name: "B (drafts.send)", question: "Does drafts.send keep the Message-ID the draft carried, which §4.3 searches SENT " +
		"for after an ambiguous send?",
		ask: func(ctx context.Context, x spikeRun) string {
			switch {
			case x.sent.sentMessage == "":
				return "not run: the send step did not send (it needs -send-to)"
			case x.sent.rfc822 == "":
				return "the send_draft result named no Message-ID for the draft"
			}
			got, err := x.box.Header(ctx, x.sent.sentMessage, "Message-ID")
			if err != nil {
				return "the sent message's Message-ID could not be read: " + err.Error()
			}
			kept := "kept"
			if got != x.sent.rfc822 {
				kept = "replaced"
			}
			return "drafts.send " + kept + " the draft's Message-ID on the sent message. The received copy is in the " +
				"second mailbox, under \"" + x.s.label.name + " send_draft\"."
		}},
	{name: "H", question: "What does a per-user rate limit look like: its status, its reason, and whether Retry-After " +
		"comes with it?",
		ask: spikeH},
}

// spikeH reads the run's first message from several workers until Gmail
// answers anything but success, or a bound is reached, and records that
// answer whole. Reads of the run's own message spend quota and change
// nothing. The driver's own calls wait out the refusal, so the cleanup
// after it still runs.
func spikeH(ctx context.Context, x spikeRun) string {
	if !x.spikeH {
		return "not run: it spends a minute's quota, and -spike-h was not given"
	}
	const workers, bound = 32, 2000
	path, q := "messages/"+url.PathEscape(x.s.messages[0]), url.Values{"format": {"minimal"}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var (
		mu    sync.Mutex
		calls int
		hit   *probeResult
		wg    sync.WaitGroup
	)
	for range workers {
		wg.Go(func() {
			for {
				mu.Lock()
				if hit != nil || calls >= bound || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				calls++
				mu.Unlock()
				r := x.box.Probe(ctx, http.MethodGet, path, q, nil)
				if r.status/100 != 2 {
					mu.Lock()
					if hit == nil {
						hit = &r
					}
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	if hit == nil {
		return fmt.Sprintf("no refusal in %d reads (%d units)", calls, calls*20)
	}
	retry := "no Retry-After header"
	if hit.retryAfter != "" {
		retry = "Retry-After " + hit.retryAfter
	}
	return fmt.Sprintf("after %d reads (%d units): HTTP %d, reason %q, message %q, %s",
		calls, calls*20, hit.status, hit.reason, hit.message, retry)
}
