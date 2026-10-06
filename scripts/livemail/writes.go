//go:build live

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
)

// The write steps. Everything they touch is the run's own: the messages
// it inserted, the drafts it and these steps made, the labels named for
// it. A draft's recipients are at reserved domains, and a draft is never
// sent, so nothing here reaches a person. The guard holds all of it.

var (
	createdDraftIn = regexp.MustCompile(`created draft (\S+) · message ([0-9a-f]{16}) · thread ([0-9a-f]{16})`)
	updatedDraftIn = regexp.MustCompile(`updated draft (\S+) · message ([0-9a-f]{16}), was ([0-9a-f]{16})`)
	createdLabelIn = regexp.MustCompile(`created label .* · id (\S+)`)
)

// fourScripts is text in Latin with diacritics, Cyrillic, Greek and Han,
// for spike E's question on the draft side: does what internal/mime
// builds come back from Gmail as it went in?
const fourScripts = "Grüße · Отчёт · Καλημέρα · 会議の案内"

// The files the write steps attach, written into the run's own
// GMAIL_LOCAL_DIR before the server starts.
func attachName(r runLabel) string { return r.name + "-Отчёт-報告.txt" }
func secondName(r runLabel) string { return r.name + "-second.txt" }
func bigName(r runLabel) string    { return r.name + "-large.bin" }

// bigSize is over the 5 MB above which a draft goes as a media upload.
const bigSize = 6 << 20

// writeLocalFiles puts the files the steps attach into dir.
func writeLocalFiles(dir string, r runLabel) error {
	for name, content := range map[string][]byte{
		attachName(r): []byte("Synthetic attachment in " + fourScripts + ", run " + r.name + ".\n"),
		secondName(r): []byte("A second synthetic file, run " + r.name + ".\n"),
		bigName(r):    make([]byte, bigSize),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Reserved-domain recipients: a draft is never sent, and these could not
// be delivered if one were.
const (
	rcptTo  = "Synthetic Reader <reader@example.org>"
	rcptCc  = "Synthetic Copy <copy@example.com>"
	rcptBcc = "Synthetic Blind <blind@example.invalid>"
)

// plainUpdated checks a plain_only step: the draft's new message is
// kept for the next step, and the HTML version moved with the body.
func plainUpdated(e *env, text string) error {
	var err error
	if _, e.plainMessage, _, err = e.updated(text); err != nil {
		return err
	}
	return want(text, "changed: body, body_html")
}

// updated reads an updated draft's ids from a result and makes its new
// message the run's own, so the guard allows it.
func (e *env) updated(text string) (draftID, messageID, was string, err error) {
	m := updatedDraftIn.FindStringSubmatch(text)
	if m == nil {
		return "", "", "", errors.New("the result names no updated draft")
	}
	e.seed.draftMessages = append(e.seed.draftMessages, m[2])
	return m[1], m[2], m[3], nil
}

// composed reads a created draft's ids from a result and makes them the
// run's own, so the guard allows them and cleanup deletes the draft.
func (e *env) composed(text string) (draftID, messageID string, err error) {
	m := createdDraftIn.FindStringSubmatch(text)
	if m == nil {
		return "", "", errors.New("the result names no created draft")
	}
	e.seed.drafts = append(e.seed.drafts, m[1])
	e.seed.draftMessages = append(e.seed.draftMessages, m[2])
	return m[1], m[2], nil
}

var writeSteps = []step{
	{name: "compose a draft, dry run", tool: "create_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"to": []any{rcptTo}, "subject": e.seed.label.name + " dry", "body": "Nothing is saved.",
				"dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "would create a draft") }},

	{name: "compose a draft in four scripts", tool: "create_draft",
		args: func(e *env) map[string]any {
			return map[string]any{
				"to": []any{rcptTo}, "cc": []any{rcptCc}, "bcc": []any{rcptBcc}, "from": e.account,
				"subject": e.seed.label.name + " " + fourScripts, "body": "Body: " + fourScripts + "\n",
				"body_html": "<p>Body: " + fourScripts + "</p>", "attachments": []any{attachName(e.seed.label)},
			}
		},
		check: func(e *env, text string) error {
			var err error
			e.draftID, e.draftMessage, err = e.composed(text)
			if err != nil {
				return err
			}
			return want(text, "update_draft needs draft_id")
		}},

	{name: "read the composed draft back", tool: "get_draft",
		args: func(e *env) map[string]any { return map[string]any{"draft_id": e.draftID, "all_headers": true} },
		check: func(e *env, text string) error {
			for _, s := range []string{e.seed.label.name + " " + fourScripts, "Body: " + fourScripts, attachName(e.seed.label),
				"Synthetic Reader", "Synthetic Blind"} {
				if err := want(text, s); err != nil {
					e.spikeE = "the composed draft did not read back whole: " + err.Error()
					return err
				}
			}
			e.spikeE = "a draft built by internal/mime read back from Gmail with its subject, body, display names and attachment name in four scripts intact"
			return nil
		}},

	{name: "update the draft, dry run", tool: "update_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.draftID, "message_id": e.draftMessage, "body": "Changed.",
				"body_html": "<p>Changed.</p>", "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "would update draft") }},

	{name: "update the draft", tool: "update_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.draftID, "message_id": e.draftMessage,
				"to": []any{rcptCc}, "cc": []any{}, "bcc": []any{rcptBcc}, "subject": e.seed.label.name + " revised",
				"body": "Revised.", "body_html": "<p>Revised.</p>",
				"add_attachments": []any{secondName(e.seed.label)}, "remove_attachments": []any{"1"}}
		},
		check: func(e *env, text string) error {
			_, msg, was, err := e.updated(text)
			if err != nil {
				return err
			}
			if msg == was {
				return errors.New("the draft holds the same message id after an update")
			}
			e.staleMessage, e.draftMessage = was, msg
			return want(text, "changed: to, cc, bcc, subject, body, body_html, attachments")
		}},

	{name: "an update against the old message id is refused", tool: "update_draft", refuses: "stale",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.draftID, "message_id": e.staleMessage, "subject": "not saved"}
		},
		check: func(_ *env, text string) error { return want(text, "changed since it was read") }},

	{name: "compose a plain-text-only draft", tool: "create_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"to": []any{rcptTo}, "subject": e.seed.label.name + " plain",
				"body": longParagraph + "\n", "plain_only": true}
		},
		check: func(e *env, text string) error {
			var err error
			e.plainDraft, e.plainMessage, err = e.composed(text)
			return err
		}},

	{name: "give the plain draft the HTML version", tool: "update_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.plainDraft, "message_id": e.plainMessage,
				"body": longParagraph + "\nWith HTML, and a link: https://example.com/a?b=1&c=2.\n", "plain_only": false}
		},
		check: plainUpdated},

	// Dropping the HTML version needs EditRaw to find the one Gmail
	// stored, link included, to be the one made from the text (§7.4).
	{name: "make the draft plain again", tool: "update_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.plainDraft, "message_id": e.plainMessage,
				"body": longParagraph + "\n", "plain_only": true}
		},
		check: plainUpdated},

	{name: "labeling a draft is refused for that item", tool: "modify_labels",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.draftMessage}, "add": []any{"STARRED"}}
		},
		check: func(_ *env, text string) error { return want(text, "[unsupported]") }},

	{name: "reply to an inserted message", tool: "create_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"reply_to": e.seed.messages[1], "body": "A synthetic reply."}
		},
		check: func(e *env, text string) error {
			var err error
			if e.replyDraft, _, err = e.composed(text); err != nil {
				return err
			}
			return want(text, "Gmail filed the draft in the parent's thread.")
		}},

	{name: "reply to all in an inserted thread", tool: "create_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"reply_to_thread": e.seed.threads[2], "reply_all": true, "body": "A synthetic reply to all."}
		},
		check: func(e *env, text string) error {
			var err error
			if e.replyAll, e.replyAllMessage, err = e.composed(text); err != nil {
				return err
			}
			if err := want(text, "Gmail filed the draft in the parent's thread."); err != nil {
				return err
			}
			return want(text, "(reply_all)")
		}},

	{name: "a large draft goes as an upload", tool: "create_draft",
		args: func(e *env) map[string]any {
			return map[string]any{"subject": e.seed.label.name + " large", "body": "Six megabytes attached.",
				"attachments": []any{bigName(e.seed.label)}}
		},
		check: func(e *env, text string) error {
			if _, _, err := e.composed(text); err != nil {
				return err
			}
			return want(text, "sent as a media upload")
		}},

	{name: "a delete without confirm is refused", tool: "delete_draft", refuses: "blocked",
		args:  func(e *env) map[string]any { return map[string]any{"draft_id": e.replyDraft} },
		check: func(_ *env, text string) error { return want(text, "confirm: true") }},

	{name: "delete a draft, dry run", tool: "delete_draft",
		args:  func(e *env) map[string]any { return map[string]any{"draft_id": e.replyDraft, "dry_run": true} },
		check: func(_ *env, text string) error { return want(text, "would delete draft") }},

	{name: "a delete the person does not confirm is refused", tool: "delete_draft", refuses: "blocked", declines: true,
		args: func(e *env) map[string]any { return map[string]any{"draft_id": e.replyDraft, "confirm": true} },
		check: func(e *env, text string) error {
			if err := want(text, "not confirmed by the person: the client answered decline"); err != nil {
				return err
			}
			q, err := e.asked()
			if err != nil {
				return err
			}
			return want(q, "delete_draft: delete this draft for good?")
		}},

	{name: "delete a draft", tool: "delete_draft",
		args: func(e *env) map[string]any { return map[string]any{"draft_id": e.replyDraft, "confirm": true} },
		check: func(e *env, text string) error {
			if err := want(text, "permanently."); err != nil {
				return err
			}
			if _, err := e.asked(); err != nil {
				return err
			}
			e.seed.forget(e.replyDraft)
			return nil
		}},

	{name: "star and mark important, dry run", tool: "modify_labels",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[0]}, "thread_ids": []any{e.seed.threads[1]},
				"add": []any{"STARRED", "IMPORTANT"}, "dry_run": true}
		},
		check: func(e *env, text string) error { return e.predict(text, "2 would change") }},

	{name: "star and mark important", tool: "modify_labels",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[0]}, "thread_ids": []any{e.seed.threads[1]},
				"add": []any{"STARRED", "IMPORTANT"}}
		},
		check: func(e *env, text string) error {
			if err := want(text, "which is: star, mark important"); err != nil {
				return err
			}
			if err := want(text, "2 changed · 0 unchanged · 0 failed"); err != nil {
				return err
			}
			return e.asPredicted()
		}},

	{name: "unstar", tool: "modify_labels",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[0]}, "remove": []any{"STARRED"}}
		},
		check: func(_ *env, text string) error { return want(text, "1 changed") }},

	{name: "trash, dry run", tool: "trash",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[2]}, "thread_ids": []any{e.seed.threads[1]}, "dry_run": true}
		},
		check: func(e *env, text string) error { return e.predict(text, "2 would change") }},

	{name: "trash", tool: "trash",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[2]}, "thread_ids": []any{e.seed.threads[1]}}
		},
		check: func(e *env, text string) error {
			if err := want(text, "2 changed · 0 unchanged · 0 failed"); err != nil {
				return err
			}
			return e.asPredicted()
		}},

	{name: "trash again leaves it where it is", tool: "trash",
		args:  func(e *env) map[string]any { return map[string]any{"message_ids": []any{e.seed.messages[2]}} },
		check: func(_ *env, text string) error { return want(text, "already in the trash") }},

	{name: "restore, dry run", tool: "restore",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[2]}, "thread_ids": []any{e.seed.threads[1]}, "dry_run": true}
		},
		check: func(e *env, text string) error { return e.predict(text, "2 would change") }},

	{name: "restore", tool: "restore",
		args: func(e *env) map[string]any {
			return map[string]any{"message_ids": []any{e.seed.messages[2]}, "thread_ids": []any{e.seed.threads[1]}}
		},
		check: func(e *env, text string) error {
			if err := want(text, "2 changed · 0 unchanged · 0 failed"); err != nil {
				return err
			}
			return e.asPredicted()
		}},

	{name: "create a label, dry run", tool: "create_label",
		args: func(e *env) map[string]any {
			return map[string]any{"name": e.seed.label.name + "-made", "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "the name is free") }},

	{name: "create a label", tool: "create_label",
		args: func(e *env) map[string]any {
			return map[string]any{"name": e.seed.label.name + "-made", "in_label_list": "hide", "in_message_list": "hide",
				"text_color": "#ffffff", "background_color": "#16a766"}
		},
		check: func(e *env, text string) error {
			m := createdLabelIn.FindStringSubmatch(text)
			if m == nil {
				return errors.New("the result names no created label")
			}
			e.seed.extraLabels = append(e.seed.extraLabels, m[1])
			return want(text, "color #ffffff on #16a766")
		}},

	{name: "a name taken in another case is refused", tool: "create_label", refuses: "conflict",
		args: func(e *env) map[string]any {
			return map[string]any{"name": strings.ToUpper(e.seed.label.name + "-made")}
		},
		check: func(_ *env, text string) error { return want(text, "already has that name") }},

	{name: "update the label, dry run", tool: "update_label",
		args: func(e *env) map[string]any {
			return map[string]any{"label": e.seed.label.name + "-made", "name": e.seed.label.name + "-renamed", "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "would update label") }},

	{name: "update the label", tool: "update_label",
		args: func(e *env) map[string]any {
			return map[string]any{"label": e.seed.label.name + "-made", "name": e.seed.label.name + "-renamed",
				"in_label_list": "show_if_unread", "in_message_list": "show", "text_color": "#000000", "background_color": "#fad165"}
		},
		check: func(e *env, text string) error {
			return want(text, "after:  "+e.seed.label.name+"-renamed · in label list: show_if_unread · in message list: show · color #000000 on #fad165")
		}},
}

// writeTools are the tools guardWrite holds, and writeKeys every argument
// it knows. A write with an argument it has no rule for is refused, so a
// new input is guarded before it is driven, not after.
var (
	writeTools = map[string]bool{"create_draft": true, "update_draft": true, "delete_draft": true, "modify_labels": true,
		"trash": true, "restore": true, "create_label": true, "update_label": true, "send_draft": true,
		"delete_permanently": true, "delete_label": true}
	writeKeys = map[string]bool{
		// Checked below or by guard.
		"message_ids": true, "thread_ids": true, "to": true, "cc": true, "bcc": true, "from": true,
		"attachments": true, "add_attachments": true, "name": true, "label": true,
		"draft_id": true, "message_id": true, "reply_to": true, "reply_to_thread": true, "confirm_recipients": true,
		// Carry no id, address or file: text, flags, part ids of the run's
		// own drafts, and the system labels STARRED and IMPORTANT.
		"dry_run": true, "confirm": true, "reply_all": true, "subject": true, "body": true, "body_html": true, "plain_only": true,
		"remove_attachments": true, "add": true, "remove": true, "in_label_list": true, "in_message_list": true,
		"text_color": true, "background_color": true,
	}
)

// guardWrite holds a write step to the run's own mail, drafts, labels and
// files, and a draft's addresses to reserved domains.
func (e *env) guardWrite(tool string, args map[string]any) error {
	if !writeTools[tool] {
		return nil
	}
	for key := range args {
		if !writeKeys[key] {
			return fmt.Errorf("%w: %s on %s has no rule in the guard", errUnscoped, key, tool)
		}
	}
	for _, key := range []string{"message_ids", "thread_ids"} {
		ids, _ := args[key].([]any)
		for _, v := range ids {
			if id, _ := v.(string); !e.seed.owns(id) {
				return fmt.Errorf("%w: %s names an id the run did not make", errUnscoped, key)
			}
		}
	}
	for _, key := range []string{"to", "cc", "bcc"} {
		list, _ := args[key].([]any)
		for _, v := range list {
			if a, _ := v.(string); !reservedAddress(a) && !e.isSendTo(a) {
				return fmt.Errorf("%w: %s on %s is outside the reserved domains and is not -send-to", errUnscoped, key, tool)
			}
		}
	}
	// A send reaches only -send-to: the only address a step may vouch for.
	confirm, _ := args["confirm_recipients"].([]any)
	for _, v := range confirm {
		if a, _ := v.(string); !e.isSendTo(a) {
			return fmt.Errorf("%w: confirm_recipients names an address that is not -send-to", errUnscoped)
		}
	}
	if from, ok := args["from"].(string); ok && from != e.account {
		return fmt.Errorf("%w: from is not the signed-in account", errUnscoped)
	}
	for _, key := range []string{"attachments", "add_attachments"} {
		names, _ := args[key].([]any)
		for _, v := range names {
			if n, _ := v.(string); !strings.HasPrefix(n, e.seed.label.name) {
				return fmt.Errorf("%w: %s names a file the run did not write", errUnscoped, key)
			}
		}
	}
	for _, key := range []string{"name", "label"} {
		if tool != "create_label" && tool != "update_label" && tool != "delete_label" {
			break
		}
		if v, ok := args[key].(string); ok && !strings.HasPrefix(strings.ToLower(v), strings.ToLower(e.seed.label.name)) {
			return fmt.Errorf("%w: %s on %s is not the run's", errUnscoped, key, tool)
		}
	}
	return nil
}

// isSendTo reports whether an entry is exactly -send-to, when one was
// given.
func (e *env) isSendTo(entry string) bool { return e.sendTo != "" && entry == e.sendTo }

// reservedAddress holds an entry to addresses at example.com, example.org
// or .invalid, which RFC 2606 keeps from ever being delivered. It reads
// the entry with the parser the server uses, so the two agree on what it
// names.
func reservedAddress(entry string) bool {
	list, strict := mime.ParseAddressList(entry)
	if !strict || len(list) == 0 {
		return false
	}
	for _, a := range list {
		_, domain, _ := strings.Cut(strings.ToLower(a.Email), "@")
		if domain != "example.com" && domain != "example.org" && !strings.HasSuffix(domain, ".invalid") {
			return false
		}
	}
	return true
}

// writeSpikes are phase 2's questions (§15). A writes only a draft of the
// run's own. D and E send, and only to -send-to.
var writeSpikes = []spike{
	{name: "thread ids", question: "Is a new thread's id its first message's id? If so, one reply field cannot take both (§4.5).",
		ask: func(_ context.Context, x spikeRun) string {
			same := 0
			for i := range x.s.threads {
				if i < len(x.s.messages) && x.s.threads[i] == x.s.messages[i] {
					same++
				}
			}
			return fmt.Sprintf("%d of %d inserted messages, each starting its own thread, carry their own id as the thread id",
				same, len(x.s.threads))
		}},
	{name: "A", question: "Does drafts.update give the draft a new message id each time, and does drafts.get then name it?",
		ask: func(ctx context.Context, x spikeRun) string {
			raw := syntheticDraft(x.s.label, 9)
			id, err := x.box.CreateDraft(ctx, raw)
			if err != nil {
				return "could not create the spike's draft: " + err.Error()
			}
			x.s.drafts = append(x.s.drafts, id)
			var ids []string
			for step := range 3 {
				if step > 0 {
					if _, err := x.box.UpdateDraft(ctx, id, raw); err != nil {
						return "an update failed: " + err.Error()
					}
				}
				got, err := x.box.DraftMessageID(ctx, id)
				if err != nil {
					return "a read failed: " + err.Error()
				}
				ids = append(ids, got)
			}
			distinct := map[string]bool{}
			for _, i := range ids {
				distinct[i] = true
			}
			return fmt.Sprintf("the draft held %d distinct message ids across a create and two updates of the same bytes "+
				"(3 means each save gives a new id, so the message id is a witness)", len(distinct))
		}},
	{name: "D", question: "Which of threadId, In-Reply-To/References and a matching subject does Gmail need to thread a reply, " +
		"for the sender?",
		ask: spikeD},
	{name: "E", question: "Do a subject, display names, a body and an attachment name in four scripts, built by internal/mime, " +
		"come back from Gmail as they went in?",
		ask: spikeE},
	{name: "M (stored)", question: "Does Gmail keep a plain-text line past 78 characters whole in a draft, with no HTML part " +
		"and with the one made from its text, and keep that HTML as it was?",
		ask: spikeMStored},
}

// sendable reports why a sending spike cannot run, or "".
func (x spikeRun) sendable() string {
	switch {
	case !x.spikesDE:
		return "not run: answered 2026-09-26 (§15); -spikes-de sends it again"
	case x.sendTo == "":
		return "not run: it sends mail, and -send-to was not given"
	case x.account == "":
		return "not run: the account's address was not read"
	case strings.EqualFold(x.sendTo, x.account):
		return "not run: -send-to is the signed-in account; it must be a second address"
	}
	return ""
}

// send sends one message the spike built, to -send-to alone, and files
// it under the run's label so cleanup trashes the sent copy.
func (x spikeRun) send(ctx context.Context, o mime.Outgoing, threadID string) (id, thread string, err error) {
	if id, thread, err = x.box.Send(ctx, o, x.sendTo, threadID); err != nil {
		return "", "", err
	}
	x.s.messages = append(x.s.messages, id)
	return id, thread, x.box.Label(ctx, id, x.s.labelID)
}

// spikeD sends an original and four replies to it: one with all three of
// §2.5's conditions, and one without each. Which replies Gmail files in
// the original's thread is the sender's half of the answer; the second
// mailbox, read by the maintainer, is the receiver's.
func spikeD(ctx context.Context, x spikeRun) string {
	if why := x.sendable(); why != "" {
		return why
	}
	subject := x.s.label.name + " spike D"
	orig := mime.Outgoing{To: []mime.Address{{Name: "Spike D"}}, Subject: subject, Text: "The original.\n",
		MessageID: mime.NewMessageID(x.account)}
	origID, origThread, err := x.send(ctx, orig, "")
	if err != nil {
		return "the original was not sent: " + err.Error()
	}
	sentID, err := x.box.Header(ctx, origID, "Message-ID")
	if err != nil || sentID == "" {
		return "the original's Message-ID could not be read back"
	}
	kept := "kept"
	if sentID != orig.MessageID {
		kept = "replaced"
	}
	var out []string
	for _, v := range []struct{ name, drop string }{
		{"all three", ""}, {"without threadId", "thread"}, {"without In-Reply-To and References", "headers"},
		{"with another subject", "subject"},
	} {
		r := mime.Outgoing{To: []mime.Address{{Name: "Spike D"}}, Subject: "Re: " + subject, Text: "Reply " + v.name + ".\n",
			MessageID: mime.NewMessageID(x.account), InReplyTo: sentID, References: []string{sentID}}
		thread := origThread
		switch v.drop {
		case "thread":
			thread = ""
		case "headers":
			r.InReplyTo, r.References = "", nil
		case "subject":
			r.Subject = x.s.label.name + " spike D, another subject"
		}
		_, got, err := x.send(ctx, r, thread)
		switch {
		case err != nil:
			out = append(out, v.name+": not sent ("+err.Error()+")")
		case got == origThread:
			out = append(out, v.name+": in the original's thread")
		default:
			out = append(out, v.name+": a new thread")
		}
	}
	return "sender side, five messages sent to -send-to: " + strings.Join(out, "; ") +
		". Gmail " + kept + " the client's Message-ID on the sent original. The receiver side is in the second mailbox, under \"" +
		subject + "\"."
}

// spikeE sends one message whose subject, recipient name, body and
// attachment name are in four scripts, reads back the sent copy's bytes
// and parses them.
func spikeE(ctx context.Context, x spikeRun) string {
	draft := x.draftSide
	if draft == "" {
		draft = "the draft steps did not run"
	}
	if why := x.sendable(); why != "" {
		return "draft side: " + draft + ". Send side " + why
	}
	o := mime.Outgoing{To: []mime.Address{{Name: "Zoë Ångström · Иван · Ελένη · 山田 花子"}},
		Subject: x.s.label.name + " spike E " + fourScripts, Text: "Body: " + fourScripts + "\n",
		MessageID:   mime.NewMessageID(x.account),
		Attachments: []mime.OutAttachment{{Filename: "Отчёт-報告-Καλημέρα-Grüße.txt", MediaType: "text/plain", Content: []byte(fourScripts)}}}
	id, _, err := x.send(ctx, o, "")
	if err != nil {
		return "draft side: " + draft + ". Send side: not sent (" + err.Error() + ")"
	}
	_, raw, err := x.box.SentCopy(ctx, id)
	if err != nil {
		return "draft side: " + draft + ". Send side: the sent copy could not be read (" + err.Error() + ")"
	}
	m := mime.ParseRaw(raw)
	var bad []string
	if m.Subject != o.Subject {
		bad = append(bad, "the subject")
	}
	if len(m.To) != 1 || m.To[0].Name != o.To[0].Name {
		bad = append(bad, "the recipient's name")
	}
	if !strings.Contains(m.Body.Text, fourScripts) {
		bad = append(bad, "the body")
	}
	if len(m.Attachments) != 1 || m.Attachments[0].DeclaredName != o.Attachments[0].Filename {
		bad = append(bad, "the attachment name")
	}
	verdict := "every field came back as sent"
	if len(bad) > 0 {
		verdict = "changed: " + strings.Join(bad, ", ")
	}
	return "draft side: " + draft + ". Send side, one message sent to -send-to and its sent copy read back: " + verdict +
		". How a non-Gmail receiver shows it is in the second mailbox, under \"" + x.s.label.name + " spike E\"."
}

// longParagraph is one paragraph on one line, past the 78 characters
// beyond which Gmail's web composer wraps a draft that has no HTML part
// (§18 row 73).
const longParagraph = "This paragraph is a single line of plain words, written by the live driver to see whether Gmail " +
	"keeps a line this long whole when it stores a draft and when it sends one, rather than wrapping it at seventy columns."

// spikeMStored saves two drafts and reads their bytes back: one with no
// HTML part, as this server wrote every draft until it made HTML from the
// text, and one with the HTML version made from its text. Whether Gmail's web composer
// wraps a draft when a person sends it cannot be driven from here (§15).
func spikeMStored(ctx context.Context, x spikeRun) string {
	text := longParagraph + "\n"
	var out []string
	for _, v := range []struct{ name, html string }{
		{"with no HTML part", ""}, {"with the HTML made from its text", mime.HTMLFromText(text)},
	} {
		o := mime.Outgoing{Subject: x.s.label.name + " spike M", Text: text, HTML: v.html, MessageID: mime.NewMessageID(x.account)}
		raw, err := mime.Build(o)
		if err != nil {
			return "not built: " + err.Error()
		}
		id, err := x.box.CreateDraft(ctx, raw)
		if err != nil {
			out = append(out, "a draft "+v.name+" was not saved: "+err.Error())
			continue
		}
		x.s.drafts = append(x.s.drafts, id) // cleanup deletes it
		msg, err := x.box.DraftMessageID(ctx, id)
		if err != nil {
			out = append(out, "a draft "+v.name+": its message could not be found ("+err.Error()+")")
			continue
		}
		out = append(out, "a draft "+v.name+", read back raw, "+readBack(ctx, x, msg))
	}
	return strings.Join(out, "; ")
}

// readBack reads a message the run made and says whether its plain text
// holds longParagraph as one line, and whether it carries the HTML
// version made from that text.
func readBack(ctx context.Context, x spikeRun, messageID string) string {
	_, raw, err := x.box.SentCopy(ctx, messageID)
	if err != nil {
		return "could not be read (" + err.Error() + ")"
	}
	m := mime.ParseRaw(raw)
	line := fmt.Sprintf("keeps the %d-character line whole", len(longParagraph))
	if !slices.Contains(strings.Split(m.Body.Text, "\n"), longParagraph) {
		line = fmt.Sprintf("does not hold the %d-character line whole", len(longParagraph))
	}
	switch htm, ok := gmailtest.HTMLPart(raw); {
	case !ok:
		return line + ", with no HTML part"
	case strings.TrimSpace(htm) == strings.TrimSpace(mime.HTMLFromText(m.Body.Text)):
		return line + ", beside the HTML made from it"
	}
	return line + ", beside HTML not made from it"
}
