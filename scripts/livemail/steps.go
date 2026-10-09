//go:build live

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/v2/scripts/internal/mcpstdio"
	"github.com/mmedum/google-mail-mcp/v2/scripts/internal/transcript"
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
	// refuses is the class a step expects the tool to refuse with; the
	// step fails if the call succeeds or refuses with another.
	refuses string
	// args builds the arguments from what the run inserted.
	args func(e *env) map[string]any
	// check judges the result. It returns an error to fail the step.
	check func(e *env, text string) error
	// needs says why the step cannot run in this run, or "": a send
	// needs -send-to, a permanent delete a profile that can delete.
	needs func(e *env) string
	// declines answers the step's question to the person with decline
	// rather than accept.
	declines bool
}

// scriptedPerson answers the questions the server puts to the person,
// for the maintainer running the driver (§4.13): accept, unless a step
// declines. It prints each question.
type scriptedPerson struct {
	tr       *transcript.Transcript
	declines bool
	// asked is the questions put during the current step.
	asked []string
}

func (p *scriptedPerson) answer(message string) string {
	p.asked = append(p.asked, message)
	p.tr.Say("--- question put to the person ---\n" + strings.TrimRight(message, "\n"))
	if p.declines {
		p.tr.Say("--- answered: decline ---")
		return "decline"
	}
	p.tr.Say("--- answered: accept ---")
	return "accept"
}

// asked is the one question put during the step, or an error.
func (e *env) asked() (string, error) {
	if e.person == nil || len(e.person.asked) != 1 {
		return "", errors.New("the server did not put exactly one question to the person")
	}
	return e.person.asked[0], nil
}

// A thread or message id in a rendering, 16 hex digits after its noun.
var (
	threadIDIn  = regexp.MustCompile(`thread ([0-9a-f]{16})`)
	messageIDIn = regexp.MustCompile(`message ([0-9a-f]{16})`)
	pageTokenIn = regexp.MustCompile(`page_token=(\S+)`)
	addedIn     = regexp.MustCompile(`message ([0-9a-f]{16}) added`)
	accountIn   = regexp.MustCompile(`(?m)^account: (\S+@\S+)$`)
)

// steps is every step, in order. Later steps read what earlier ones
// stored on env.
var steps = []step{
	{name: "profile", tool: "get_profile", quiet: true,
		args: func(*env) map[string]any { return map[string]any{} },
		check: func(e *env, text string) error {
			m := accountIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("the result names no account")
			}
			e.account = m[1]
			return nil
		}},

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

	// The second message offers to unsubscribe. Its row reads both
	// headers, which the listing's headers-only read must name.
	{name: "a row reads the List-Unsubscribe the driver wrote", tool: "search_messages",
		args: func(e *env) map[string]any { return map[string]any{"q": e.seed.label.query("Synthetic")} },
		check: func(e *env, text string) error {
			row := regexp.MustCompile(`(?m)^message ` + e.seed.messages[1] + ` · .*$`).FindString(text)
			if !strings.HasSuffix(row, " · unsubscribe: web (one-click, as the sender declares), mail") {
				return fmt.Errorf("the row does not say how to unsubscribe: %q", row)
			}
			return e.unsubscribeIs(e.seed.messages[1], syntheticUnsubscribeURL(e.seed.label), syntheticUnsubscribeMail)
		}},

	// The first message carries a calendar part. Its entry gives the
	// event's UID and times. Gmail may keep the part inline or apart; one
	// apart costs one more read, so the units say which it did.
	{name: "an attachment entry reads the invitation the driver wrote", tool: "get_message",
		args: func(e *env) map[string]any { return map[string]any{"message_id": e.seed.messages[0]} },
		check: func(e *env, text string) error {
			if err := want(text, "uid "+syntheticInviteUID(e.seed.label)); err != nil {
				return err
			}
			return e.invitationIs("2", map[string]any{
				"method": "PUBLISH", "untrusted_uid": syntheticInviteUID(e.seed.label), "sequence": float64(1),
				"untrusted_summary": "Synthetic event, written by the live driver", "untrusted_organizer": "sender-1@example.com",
				"untrusted_start": "2026-09-24T12:00:00+02:00", "untrusted_end": "2026-09-24T13:00:00+02:00",
				"untrusted_time_zone": "Europe/Copenhagen", "all_day": false, "events": float64(1),
			})
		}},

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

	{name: "changes since before the run, in the run label, first page", tool: "list_changes",
		args: func(e *env) map[string]any {
			return map[string]any{"history_id": e.seed.historyStart, "label": e.seed.label.name,
				"kinds": []any{"added"}, "max": 2}
		},
		check: func(e *env, text string) error {
			if err := e.ownsAll(addedIn, text, 2); err != nil {
				return err
			}
			m := pageTokenIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("three inserts at max 2 gave no next page")
			}
			e.pageToken = m[1]
			return nil
		}},

	{name: "changes since before the run, in the run label, second page", tool: "list_changes",
		args: func(e *env) map[string]any {
			return map[string]any{"history_id": e.seed.historyStart, "label": e.seed.label.name,
				"kinds": []any{"added"}, "max": 2, "page_token": e.pageToken}
		},
		check: func(e *env, text string) error {
			if err := e.ownsAll(addedIn, text, 1); err != nil {
				return err
			}
			return want(text, "complete: yes")
		}},

	{name: "changes from an expired cursor", tool: "list_changes",
		args: func(e *env) map[string]any {
			return map[string]any{"history_id": "1", "label": e.seed.label.name}
		},
		check: func(_ *env, text string) error { return want(text, "cursor expired") }},

	{name: "settings", tool: "get_settings", quiet: true,
		args:  func(*env) map[string]any { return map[string]any{} },
		check: func(_ *env, text string) error { return want(text, "forwarding") }},

	{name: "filters", tool: "list_filters", quiet: true,
		args:  func(*env) map[string]any { return map[string]any{} },
		check: func(_ *env, text string) error { return want(text, "forwarding mail out of the account") }},

	{name: "download the run's attachment", tool: "download_attachment",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": e.seed.messages[0], "part_id": "1"}
		},
		check: func(e *env, text string) error { return e.checkDownload(text, syntheticAttachmentName) }},

	{name: "download it again, by its Message-ID", tool: "download_attachment",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": "rfc822:<" + e.seed.label.name + ".1@livemail.invalid>", "part_id": "1"}
		},
		check: func(e *env, text string) error {
			if err := want(text, "a number was added"); err != nil {
				return err
			}
			return e.checkDownload(text, "livemail-synthetic-1.txt")
		}},

	// The first message carries a text file and a calendar part, neither
	// inline, so both are saved; the text file under a third name.
	{name: "save every attachment of the run's first message", tool: "download_attachments",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": e.seed.messages[0], "include_inline": true}
		},
		check: func(e *env, text string) error {
			if err := want(text, "saved 2, skipped 0, failed 0"); err != nil {
				return err
			}
			return e.checkDownload(text, "livemail-synthetic-2.txt")
		}},

	// The third message carries an attached message. If Gmail serves it
	// as its own parts, the part fails as [unsupported] (§18 row 83).
	{name: "save the attached message by its part id", tool: "download_attachments",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": e.seed.messages[2], "part_ids": []any{"1"}}
		},
		check: func(e *env, text string) error {
			if err := want(text, "saved 1, skipped 0, failed 0"); err != nil {
				return err
			}
			got, err := os.ReadFile(filepath.Join(e.localDir, syntheticAttachedName))
			if err != nil {
				return fmt.Errorf("the attached message is not where the result says: %w", err)
			}
			if !bytes.Contains(got, []byte(syntheticAttachedBody(e.seed.label))) {
				return fmt.Errorf("the saved message, %d bytes, does not hold the attached message's text", len(got))
			}
			return nil
		}},

	{name: "read the run's text attachment", tool: "read_attachment",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": e.seed.messages[0], "part_id": "1", "budget_chars": 2000, "offset": 0}
		},
		check: func(e *env, text string) error {
			if e.structured["read_as"] != "text" {
				return fmt.Errorf("read as %v; want text", e.structured["read_as"])
			}
			return want(text, strings.TrimSpace(string(syntheticAttachment(e.seed.label))))
		}},

	// If Gmail serves the attached message as its own parts, the read is
	// refused as [unsupported] (§18 row 83).
	{name: "read the attached message", tool: "read_attachment",
		args: func(e *env) map[string]any {
			return map[string]any{"message_id": e.seed.messages[2], "part_id": "1", "show_quoted": true}
		},
		check: func(e *env, text string) error {
			msg, _ := e.structured["message"].(map[string]any)
			if e.structured["read_as"] != "message" || msg["untrusted_subject"] != e.seed.label.name+" attached message" {
				return fmt.Errorf("read as %v, message %v; want the attached message", e.structured["read_as"], msg)
			}
			return want(text, syntheticAttachedBody(e.seed.label))
		}},
}

// checkDownload holds a download to what the run inserted: the file is in
// the run's directory under the expected name, and its bytes and the hash
// the result states are the attachment's.
func (e *env) checkDownload(text, name string) error {
	content := syntheticAttachment(e.seed.label)
	sum := sha256.Sum256(content)
	if err := want(text, "sha256: "+hex.EncodeToString(sum[:])); err != nil {
		return err
	}
	got, err := os.ReadFile(filepath.Join(e.localDir, name))
	if err != nil {
		return fmt.Errorf("the file is not where the result says: %w", err)
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("the file holds %d bytes that are not the attachment's %d", len(got), len(content))
	}
	return nil
}

// env is what a step sees.
type env struct {
	session   *mcpstdio.Session
	tr        *transcript.Transcript
	seed      *seeded
	pageToken string
	// localDir is the run's GMAIL_LOCAL_DIR, a directory of its own.
	localDir string
	// account is the signed-in address, from get_profile.
	account string
	// draftID and draftMessage are the draft the write steps compose
	// and update; staleMessage is its message before the last update.
	draftID, draftMessage, staleMessage string
	// plainDraft and plainMessage are the draft the plain_only steps
	// make plain, give the HTML version and make plain again.
	plainDraft, plainMessage string
	// replyDraft is the reply the delete steps delete.
	replyDraft string
	// replyAll and replyAllMessage are the reply-all draft the recipient
	// guard's dry run reads.
	replyAll, replyAllMessage string
	// forwardDraft and forwardMessage are the forward the forward steps
	// make, read back and delete.
	forwardDraft, forwardMessage string
	// quoteDraft is the quoting reply the quote steps make, read back
	// and delete.
	quoteDraft string
	// spikeE is the draft side of spike E, from reading a draft back.
	spikeE string
	// sendTo is -send-to, the one address a step may send to; full is
	// whether the profile holds https://mail.google.com/.
	sendTo string
	full   bool
	// settings is whether the profile holds gmail.settings.basic, which
	// the settings steps need; filters are the ones they created.
	settings   bool
	filters    []string
	filterArgs map[string]any
	// archiveFilter is the archiving filter a step made, which a later
	// step looks for SPAM on (§18 row 56).
	archiveFilter string
	// structured is the last step's structuredContent, for a check that
	// needs a row the text may have left out.
	structured map[string]any
	// ctx bounds the waits a step makes.
	ctx context.Context
	// signatureAddress is the default send-as address, whose signature
	// the run saved and changes; no other one is touched.
	signatureAddress string
	// sent is what the send step read, for spikes B and C.
	sent sentDraft
	// person answers the server's questions to the person.
	person *scriptedPerson
	// predicted is each item's labels after, as the last dry run said,
	// for the write that follows to match (§18 row 70).
	predicted map[string][]string
}

// unsubscribeIs holds a row of the last listing to one web address,
// one-click, and one mail address.
func (e *env) unsubscribeIs(id, url, mailto string) error {
	rows, _ := e.structured["messages"].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["id"] != id {
			continue
		}
		u, _ := row["unsubscribe"].(map[string]any)
		urls, _ := u["untrusted_urls"].([]any)
		mails, _ := u["untrusted_mailto"].([]any)
		if u["one_click"] != true || !slices.Equal(urls, []any{url}) || !slices.Equal(mails, []any{mailto}) {
			return fmt.Errorf("the row's unsubscribe is %v; want %s, one-click, and %s", u, url, mailto)
		}
		return nil
	}
	return fmt.Errorf("no row for message %s", id)
}

// invitationIs holds the invitation on one attachment entry of the last
// get_message to want, field by field, and its units to a read with the
// part inline (21) or one fetched apart (41).
func (e *env) invitationIs(partID string, want map[string]any) error {
	if u := e.structured["units"]; u != float64(21) && u != float64(41) {
		return fmt.Errorf("the read spent %v units; want 21, or 41 with the calendar part fetched", u)
	}
	msg, _ := e.structured["message"].(map[string]any)
	atts, _ := msg["attachments"].([]any)
	for _, a := range atts {
		att, _ := a.(map[string]any)
		if att["part_id"] != partID {
			continue
		}
		inv, _ := att["invitation"].(map[string]any)
		for k, v := range want {
			if inv[k] != v {
				return fmt.Errorf("the invitation's %s is %v; want %v", k, inv[k], v)
			}
		}
		return nil
	}
	return fmt.Errorf("no attachment entry for part %s", partID)
}

// itemsAfter is each item's labels_after in the last multi-id write.
func (e *env) itemsAfter() map[string][]string {
	out := map[string][]string{}
	items, _ := e.structured["items"].([]any)
	for _, it := range items {
		row, _ := it.(map[string]any)
		id, _ := row["id"].(string)
		labels, _ := row["labels_after"].([]any)
		ids := []string{}
		for _, l := range labels {
			ref, _ := l.(map[string]any)
			if s, ok := ref["id"].(string); ok {
				ids = append(ids, s)
			}
		}
		slices.Sort(ids)
		out[id] = ids
	}
	return out
}

// predict records a dry run's labels after, once it says what it would do.
func (e *env) predict(text, would string) error {
	if err := want(text, would); err != nil {
		return err
	}
	e.predicted = e.itemsAfter()
	return nil
}

// asPredicted holds a write's labels after to what its dry run said.
func (e *env) asPredicted() error {
	got := e.itemsAfter()
	for id, said := range e.predicted {
		if !slices.Equal(got[id], said) {
			return fmt.Errorf("item %s: the dry run said its labels would be %v, the write left %v", id, said, got[id])
		}
	}
	e.predicted = nil
	return nil
}

// sentDraft is a draft the run sent through send_draft: the draft, the
// message it held and its Message-ID, and the message Gmail filed in SENT.
type sentDraft struct {
	draftID, draftMessage, rfc822, sentMessage string
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
	case "list_changes":
		// Changes are ids, not mail, but they are the whole mailbox's ids:
		// the run's label keeps them to its own.
		if label, _ := args["label"].(string); label != e.seed.label.name {
			return fmt.Errorf("%w: list_changes without label %s", errUnscoped, e.seed.label.name)
		}
	}
	if err := e.guardWrite(tool, args); err != nil {
		return err
	}
	if err := e.guardSettings(tool, args); err != nil {
		return err
	}
	for _, key := range []string{"thread_id", "message_id", "draft_id", "reply_to", "reply_to_thread", "forward"} {
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
	if e.person != nil {
		e.person.asked, e.person.declines = nil, s.declines
	}
	text, structured, isError, err := e.session.CallToolStructured(s.tool, args)
	if err != nil {
		return err
	}
	e.structured = structured
	switch {
	case s.quiet && isError:
		// A refusal is the server's "[class] message", which carries no
		// setting's text, and is what a failed quiet step needs read.
		e.tr.Say(text)
	case s.quiet:
		e.tr.Sayf("(%d characters; not printed, since this result is the whole mailbox's rather than the run's)", len(text))
	default:
		e.tr.Say(text)
	}
	switch {
	case isError && s.refuses == "":
		return errors.New("the tool refused")
	case isError && !strings.HasPrefix(text, "["+s.refuses+"]"):
		return fmt.Errorf("the tool refused, but not with [%s]", s.refuses)
	case !isError && s.refuses != "":
		return fmt.Errorf("the call succeeded; it should have been refused with [%s]", s.refuses)
	}
	return s.check(e, text)
}

// spike is one question the documentation does not answer (§15). Each
// states its question and its verdict separately.
type spike struct {
	name     string
	question string
	ask      func(ctx context.Context, x spikeRun) (verdict string)
}

// spikeRun is what a spike sees: the mailbox, the run's own mail, and
// for the two that send, the address the maintainer passed and the
// account sending.
type spikeRun struct {
	box     mailbox
	s       *seeded
	sendTo  string
	account string
	// draftSide is what the write steps found for spike E without sending.
	draftSide string
	// full is whether the profile holds https://mail.google.com/, under
	// which spike G's question cannot be asked.
	full bool
	// sent is the draft a step sent, for spikes B and C.
	sent sentDraft
	// spikeH floods the run's own message with reads for spike H, only
	// when -spike-h is given.
	spikeH bool
	// spikeL sends filter writes at once for spike L, only when -spike-l
	// is given.
	spikeL bool
	// spikesDE lets spikes D and E send again.
	spikesDE bool
}

// spikes run after the steps, against the run's own mail only.
var spikes = []spike{
	{name: "F (dates)", question: "Which date does messages.insert record with internalDateSource=receivedTime: " +
		"the insert's own time, or the message's Date header?",
		ask: func(ctx context.Context, x spikeRun) string {
			ms, err := x.box.InternalDate(ctx, x.s.messages[0])
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
	{name: "K", question: "Is a history cursor far in the past answered 404, as the sync guide says, and in what body?",
		ask: func(ctx context.Context, x spikeRun) string {
			r := x.box.Probe(ctx, http.MethodGet, "history", url.Values{"startHistoryId": {"1"}}, nil)
			return fmt.Sprintf("history.list with startHistoryId=1 answered HTTP %d, reason %q, message %q (the guide says 404)",
				r.status, r.reason, r.message)
		}},
	{name: "I", question: "Can two labels differ only in case, and what does creating a label named like a system label return?",
		ask: func(ctx context.Context, x spikeRun) string {
			box, s := x.box, x.s
			names := []string{s.label.name + "-Case", s.label.name + "-case", "INBOX", "Inbox"}
			out := make([]string, 0, len(names))
			for _, name := range names {
				r := box.Probe(ctx, http.MethodPost, "labels", nil,
					map[string]string{"name": name, "labelListVisibility": "labelHide", "messageListVisibility": "hide"})
				if r.id != "" {
					// Whatever a spike creates, cleanup deletes.
					s.extraLabels = append(s.extraLabels, r.id)
				}
				shown := name
				if strings.HasPrefix(name, s.label.name) {
					shown = "<run>" + strings.TrimPrefix(name, s.label.name)
				}
				out = append(out, fmt.Sprintf("%s: HTTP %d reason %q message %q created %v", shown, r.status, r.reason,
					r.message, r.id != ""))
			}
			return strings.Join(out, "; ")
		}},
	{name: "G (positive half)", question: "Does Gmail refuse messages.delete under gmail.modify, " +
		"so only https://mail.google.com/ can delete permanently?",
		ask: func(ctx context.Context, x spikeRun) string {
			if x.full {
				return "not run: the profile holds https://mail.google.com/, which the destructive steps need, " +
					"so a delete would succeed. Asked under gmail.modify on 2026-09-26 (§15)"
			}
			s := x.s
			last := s.messages[len(s.messages)-1]
			status := x.box.Probe(ctx, http.MethodDelete, "messages/"+url.PathEscape(last), nil, nil).status
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

func runSpikes(ctx context.Context, x spikeRun, tr *transcript.Transcript) {
	for _, sp := range slices.Concat(spikes, writeSpikes, sendSpikes, settingsSpikes) {
		tr.Sayf("spike %s — question: %s", sp.name, sp.question)
		tr.Sayf("spike %s — observed: %s", sp.name, sp.ask(ctx, x))
	}
}
