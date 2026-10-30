//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mmedum/google-mail-mcp/v3/scripts/internal/transcript"
)

// The settings steps, run only when the profile's login granted
// gmail.settings.basic. drive saves the default address's signature and
// the vacation reply before them, to a file as well, and restores both
// after, exactly, with its own client: update_signature takes plain text
// and could not put an HTML signature back. Filters match only the run's
// own sender address, which no mail comes from, and are deleted by the
// steps or by cleanup.

// needSettings keeps a settings step to profiles whose login granted the
// one scope Gmail accepts for it.
func needSettings(e *env) string {
	if !e.settings {
		return "the profile's login did not grant gmail.settings.basic; log in with GMAIL_ENABLE_SETTINGS=true to drive it"
	}
	return ""
}

// needSignature keeps a signature step to a run that can put the
// account's own signature back: Gmail took it back unchanged before any
// step changed it.
func needSignature(e *env) string {
	if why := needSettings(e); why != "" {
		return why
	}
	return e.signatureSkip
}

// needRunSignature keeps a step that puts the address's signature in a
// draft to a run whose own signature is in place: after a failed set,
// the draft would carry the account's real one, and the transcript
// with it.
func needRunSignature(e *env) string {
	if why := needSignature(e); why != "" {
		return why
	}
	if !e.signatureSet {
		return "the run's own signature is not in place, so a draft would carry the account's"
	}
	return ""
}

// keptSettings are the account's own settings as the run found them, and
// where a copy of them waits until they are put back.
type keptSettings struct {
	saved savedSettings
	// path is the copy: a file only the account can read.
	path string
	// signature is whether the restore writes the signature: the run
	// drives a step that changes it, and Gmail took it back unchanged.
	signature bool
	// signatureSkip is why the signature steps may not run, or "".
	signatureSkip string
}

// keepSettings reads the settings the steps change and, before anything
// changes them, writes them to a file in dir named for the run, which
// only the account can read. It prints the file's path, never what it
// holds. If the restore fails, the settings are still there to put back
// by hand.
//
// When the run drives a signature step, it then writes the signature back
// as it was. The restore writes the same thing, so a signature Gmail
// refuses, or stores changed, could not be put back once a step cleared
// it: the signature steps are skipped, saying why.
func keepSettings(ctx context.Context, box mailbox, tr *transcript.Transcript, dir string, r runLabel,
	signatureSteps bool) (keptSettings, error) {
	saved, err := box.ReadSettings(ctx)
	if err != nil {
		return keptSettings{}, err
	}
	k := keptSettings{saved: saved}
	if k.path, err = writeSettingsCopy(dir, r, saved); err != nil {
		return keptSettings{}, fmt.Errorf("keep a copy of the settings before the run changes them: %w", err)
	}
	tr.Sayf("saved the signature and the vacation reply, as the run found them, to %s in the system's temporary "+
		"directory; the file is deleted once both are put back and read back", k.path)
	if !signatureSteps {
		return k, nil
	}
	stored, err := box.WriteSignature(ctx, saved.address, saved.signature)
	switch {
	case err != nil:
		k.signatureSkip = fmt.Sprintf("Gmail refused the account's own signature written back unchanged (%v), "+
			"so the run could not put it back", err)
	case stored != saved.signature:
		k.signatureSkip = "Gmail stored the account's own signature changed when it was written back unchanged, " +
			"so the run could not put it back"
	default:
		k.signature = true
	}
	return k, nil
}

// drivesSignature reports whether the run drives a signature step, which
// is the only reason to write the signature at all.
func drivesSignature(run *regexp.Regexp) bool {
	return slices.ContainsFunc(settingsSteps, func(s step) bool {
		return s.tool == "update_signature" && run.MatchString(s.name)
	})
}

// settingsCopy is what the copy holds: the default address, its
// signature's HTML and the vacation resource, each as Gmail gave it.
type settingsCopy struct {
	Address   string          `json:"send_as_address"`
	Signature string          `json:"signature_html"`
	Vacation  json.RawMessage `json:"vacation"`
}

// writeSettingsCopy writes s to a new file in dir named for the run, with
// mode 0600. It refuses a file that is already there.
func writeSettingsCopy(dir string, r runLabel, s savedSettings) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settingsCopy{Address: s.address, Signature: s.signature, Vacation: s.vacation}); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.name+"-settings.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(b.Bytes())
	if err = errors.Join(err, f.Close()); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	return path, nil
}

// restore puts back what keepSettings saved and reads both back. Only a
// restore that reads back as the run found them deletes the copy; any
// other outcome fails, naming it, so the settings can be put back by hand.
func (k keptSettings) restore(ctx context.Context, box mailbox) error {
	var errs []error
	if k.signature {
		if _, err := box.WriteSignature(ctx, k.saved.address, k.saved.signature); err != nil {
			errs = append(errs, fmt.Errorf("put the signature back: %w", err))
		}
	}
	if err := box.WriteVacation(ctx, k.saved.vacation); err != nil {
		errs = append(errs, fmt.Errorf("put the vacation reply back: %w", err))
	}
	now, err := box.ReadSettings(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("read the settings back: %w", err))
	} else {
		if now.address != k.saved.address || now.signature != k.saved.signature {
			errs = append(errs, errors.New("the signature reads back other than the run found it"))
		}
		if !sameJSON(now.vacation, k.saved.vacation) {
			errs = append(errs, errors.New("the vacation reply reads back other than the run found it"))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w\n!! the signature and the vacation reply, as the run found them, are in %s in the "+
			"system's temporary directory: put them back by hand, then delete the file", errors.Join(errs...), k.path)
	}
	if err := os.Remove(k.path); err != nil {
		return fmt.Errorf("both read back as the run found them, but the copy in %s could not be deleted: %w", k.path, err)
	}
	return nil
}

// sameJSON reports whether two JSON documents hold the same values,
// whatever their key order and spacing.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// filterSender is the run's own sender address for its filters: at a
// domain that never resolves, so they can match nothing that arrives.
func filterSender(r runLabel) string { return r.name + "@filters.invalid" }

// needFilter keeps a delete step to a run that made a filter to delete.
func needFilter(e *env) string {
	if why := needSettings(e); why != "" {
		return why
	}
	if len(e.filters) == 0 {
		return "no filter was created"
	}
	return ""
}

// recordFilter keeps the id of a filter a step created, and its criteria
// as the result gives them, which delete_filter takes as a witness.
func recordFilter(e *env, text string) error {
	m := filterIDIn.FindStringSubmatch(text)
	if m == nil {
		return errors.New("the result names no filter")
	}
	f, _ := e.structured["filter"].(map[string]any)
	criteria, ok := f["criteria"].(map[string]any)
	if !ok {
		return errors.New("the result gives no criteria for the filter")
	}
	if e.filterCriteria == nil {
		e.filterCriteria = map[string]any{}
	}
	e.filters = append(e.filters, m[1])
	e.filterCriteria[m[1]] = criteria
	return nil
}

// deletedFirst checks a delete of the oldest filter left, and forgets it.
func deletedFirst(e *env, text string) error {
	if err := want(text, "deleted filter"); err != nil {
		return err
	}
	e.filters = e.filters[1:]
	return nil
}

var filterIDIn = regexp.MustCompile(`created filter (\S+)`)

// vacationStart is far enough ahead that the reply can answer nobody
// before the run turns it off again.
func vacationStart() time.Time { return time.Now().UTC().Add(300 * 24 * time.Hour).Truncate(time.Hour) }

var settingsSteps = []step{
	// A result that can carry the account's own signature or vacation
	// text is quiet: not printed. Every signature step's "before" can be
	// the real one, and the vacation reply turned off shows its text, the
	// account's own if an earlier step did not run.
	{name: "set the signature, dry run", tool: "update_signature", quiet: true, needs: needSignature,
		args: func(e *env) map[string]any {
			return map[string]any{"signature": "Synthetic signature for " + e.seed.label.name, "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "dry run: would set the signature of") }},

	{name: "set the signature", tool: "update_signature", quiet: true, needs: needSignature,
		args: func(e *env) map[string]any {
			return map[string]any{"send_as": e.signatureAddress, "signature": "Synthetic signature for " + e.seed.label.name + "\nline two"}
		},
		check: func(e *env, text string) error {
			if err := want(text, "set the signature of"); err != nil {
				return err
			}
			if err := want(text, "Synthetic signature for "+e.seed.label.name+"\nline two"); err != nil {
				return err
			}
			e.signatureSet = true
			return nil
		}},

	// The signature is the one the run just set, so the draft that takes
	// it holds only what the run wrote. Gmail must keep the markup around
	// it as written, or the update cannot find it to carry over (§7.4).
	{name: "a draft takes the address's signature", tool: "create_draft", needs: needRunSignature,
		args: func(e *env) map[string]any {
			// No from: the draft goes from the default address, whose
			// signature the run set.
			return map[string]any{"to": []any{rcptTo}, "subject": e.seed.label.name + " signed", "body": "A signed draft.",
				"signature": true}
		},
		check: func(e *env, text string) error {
			var err error
			if e.signedDraft, e.signedMessage, err = e.composed(text); err != nil {
				return err
			}
			return want(text, "signature: added below the body, the one Gmail holds for the From address: 2 lines.")
		}},

	{name: "read the signed draft back", tool: "get_draft", needs: needRunSignature,
		args: func(e *env) map[string]any { return map[string]any{"draft_id": e.signedDraft, "show_quoted": true} },
		check: func(e *env, text string) error {
			return want(text, "A signed draft.\n\n-- \nSynthetic signature for "+e.seed.label.name+"\nline two")
		}},

	{name: "a new body keeps the signature", tool: "update_draft", needs: needRunSignature,
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.signedDraft, "message_id": e.signedMessage, "body": "A revised signed draft."}
		},
		check: func(e *env, text string) error {
			var err error
			if _, e.signedMessage, _, err = e.updated(text); err != nil {
				return err
			}
			return want(text, "signature: the draft's own, kept below the new body: 2 lines.")
		}},

	{name: "signature false takes it out", tool: "update_draft", needs: needRunSignature,
		args: func(e *env) map[string]any {
			return map[string]any{"draft_id": e.signedDraft, "message_id": e.signedMessage, "body": "An unsigned draft.",
				"signature": false}
		},
		check: func(e *env, text string) error {
			var err error
			if _, e.signedMessage, _, err = e.updated(text); err != nil {
				return err
			}
			return want(text, "signature: removed from the draft.")
		}},

	deleteStepDraft("delete the signed draft", func(e *env) (string, string) { return e.signedDraft, e.signedMessage }).
		needing(needRunSignature),

	{name: "clear the signature", tool: "update_signature", quiet: true, needs: needSignature,
		args: func(*env) map[string]any { return map[string]any{"signature": ""} },
		check: func(e *env, text string) error {
			e.signatureSet = false
			return want(text, "signature after: none")
		}},

	// The filter has every criterion and the common actions, and matches
	// only mail from the run's own sender, which never comes. The dry run
	// keeps it, less dry_run, for the steps that make it and repeat it.
	{name: "create a filter, dry run", tool: "create_filter", needs: needSettings,
		args: func(e *env) map[string]any {
			args := map[string]any{
				"from": filterSender(e.seed.label), "to": e.account, "subject": e.seed.label.name, "query": "synthetic",
				"negated_query": "unrelated", "has_attachment": true, "exclude_chats": true,
				"size": 1 << 20, "size_comparison": "smaller",
				"add_labels": []any{e.seed.label.name}, "remove_labels": []any{"IMPORTANT"}, "archive": true, "star": true,
				"mark_read": true,
			}
			e.filterArgs = maps.Clone(args)
			args["dry_run"] = true
			return args
		},
		check: func(_ *env, text string) error {
			return want(text, "dry run: would create a filter; no filter does this already.")
		}},

	{name: "create a filter", tool: "create_filter", needs: needSettings,
		args: func(e *env) map[string]any { return e.filterArgs },
		check: func(e *env, text string) error {
			if err := recordFilter(e, text); err != nil {
				return err
			}
			return want(text, "mail already in the mailbox is untouched")
		}},

	// A filter that trashes is its own tool, behind the destructive flag
	// too, which the run turns on (§7.9).
	{name: "a trashing filter without confirm is refused", tool: "create_trash_filter", refuses: "blocked", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "trash"}
		},
		check: func(_ *env, text string) error { return want(text, "confirm: true") }},

	// The dry run carries every criterion, as create_filter's does; the
	// filter it would make is never made.
	{name: "create a trashing filter, dry run", tool: "create_trash_filter", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "to": e.account, "subject": e.seed.label.name,
				"query": "trash", "negated_query": "unrelated", "has_attachment": true, "exclude_chats": true,
				"size": 1 << 20, "size_comparison": "smaller", "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "moves matching mail to the trash") }},

	{name: "create a trashing filter", tool: "create_trash_filter", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "trash", "confirm": true}
		},
		check: func(e *env, text string) error {
			if err := recordFilter(e, text); err != nil {
				return err
			}
			return want(text, "moves matching mail to the trash")
		}},

	// Gmail keeps a second identical filter, so the server's check is the
	// only one. This filter has no size, which Gmail rounds (§18 row 52).
	{name: "an identical filter is refused", tool: "create_trash_filter", refuses: "conflict", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "trash", "confirm": true}
		},
		check: func(_ *env, text string) error { return want(text, "already does this") }},

	// An archiving filter with no size, read back at once with the labels
	// sent and no more, and found as a duplicate when asked for again.
	// Gmail was seen adding SPAM to such filters later (§18 row 56).
	{name: "create an archiving filter", tool: "create_filter", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "archive", "archive": true}
		},
		check: func(e *env, text string) error {
			if err := recordFilter(e, text); err != nil {
				return err
			}
			e.archiveFilter = e.filters[len(e.filters)-1]
			return want(text, "removes labels: INBOX")
		}},

	{name: "the same archiving filter is refused", tool: "create_filter", refuses: "conflict", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "archive", "archive": true}
		},
		check: func(_ *env, text string) error { return want(text, "already does this") }},

	// SPAM removed is Gmail's "Never send it to Spam", said in words.
	{name: "create a filter that keeps mail out of spam", tool: "create_filter", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "nospam", "remove_labels": []any{"SPAM"}}
		},
		check: func(e *env, text string) error {
			if err := recordFilter(e, text); err != nil {
				return err
			}
			if strings.Contains(text, "SPAM") {
				return errors.New("the result names SPAM as a label taken off")
			}
			return want(text, "never sends matching mail to spam")
		}},

	// Report only: after another filter write and a pause, whether Gmail
	// has added SPAM to the run's archiving filter (§18 row 56). The list
	// is the whole account's, so only that one fact is printed. It is read
	// from the structured rows, which carry every filter.
	{name: "does the archiving filter list SPAM later", tool: "list_filters", quiet: true, needs: needSettings,
		args: func(e *env) map[string]any {
			if e.ctx != nil { // nil in the driver's own tests
				pause(e.ctx, 15*time.Second)
			}
			return map[string]any{}
		},
		check: func(e *env, _ string) error {
			e.tr.Sayf("report: %s", spamLater(e))
			return nil
		}},

	{name: "a filter delete without confirm is refused", tool: "delete_filter", refuses: "blocked", needs: needFilter,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": e.filterCriteria[e.filters[0]]}
		},
		check: func(_ *env, text string) error { return want(text, "confirm: true") }},

	{name: "delete a filter, dry run", tool: "delete_filter", needs: needFilter,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": e.filterCriteria[e.filters[0]], "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "dry run: would delete filter") }},

	// The criteria say which filter goes, and are the filter's.
	{name: "a filter delete with other criteria is refused", tool: "delete_filter", refuses: "stale", needs: needFilter,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": map[string]any{"from": filterSender(e.seed.label)},
				"confirm": true}
		},
		check: func(e *env, text string) error {
			return want(text, "criteria are not filter "+e.filters[0]+"'s; nothing was deleted")
		}},

	{name: "delete the filter", tool: "delete_filter", needs: needFilter, check: deletedFirst,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": e.filterCriteria[e.filters[0]], "confirm": true}
		}},

	{name: "delete the trashing filter", tool: "delete_filter", needs: needFilter, check: deletedFirst,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": e.filterCriteria[e.filters[0]], "confirm": true}
		}},

	{name: "delete the archiving filter", tool: "delete_filter", needs: needFilter, check: deletedFirst,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": e.filterCriteria[e.filters[0]], "confirm": true}
		}},

	{name: "delete the filter that keeps mail out of spam", tool: "delete_filter", needs: needFilter, check: deletedFirst,
		args: func(e *env) map[string]any {
			return map[string]any{"filter_id": e.filters[0], "criteria": e.filterCriteria[e.filters[0]], "confirm": true}
		}},

	{name: "a vacation reply without an audience is refused", tool: "set_vacation", refuses: "invalid", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"enable": true, "body": "Synthetic reply for " + e.seed.label.name, "confirm": true,
				"start": vacationStart().Format(time.RFC3339)}
		},
		check: func(_ *env, text string) error { return want(text, "audience is required") }},

	{name: "turn the vacation reply on, dry run", tool: "set_vacation", needs: needSettings,
		args: func(e *env) map[string]any {
			start := vacationStart()
			return map[string]any{"enable": true, "subject": "Synthetic " + e.seed.label.name,
				"body": "Synthetic reply for " + e.seed.label.name + ".", "audience": "contacts", "dry_run": true,
				"start": start.Format(time.RFC3339), "end": start.Add(24 * time.Hour).Format(time.RFC3339)}
		},
		check: func(_ *env, text string) error { return want(text, "dry run: would turn the vacation reply on") }},

	{name: "turn the vacation reply on, months ahead", tool: "set_vacation", needs: needSettings,
		args: func(e *env) map[string]any {
			start := vacationStart()
			return map[string]any{"enable": true, "subject": "Synthetic " + e.seed.label.name,
				"body": "Synthetic reply for " + e.seed.label.name + ".", "audience": "contacts", "confirm": true,
				"start": start.Format(time.RFC3339), "end": start.Add(24 * time.Hour).Format(time.RFC3339)}
		},
		check: func(e *env, text string) error {
			if err := want(text, "vacation reply is ON · to contacts only · from "); err != nil {
				return err
			}
			return want(text, "Synthetic reply for "+e.seed.label.name)
		}},

	{name: "turn the vacation reply off", tool: "set_vacation", quiet: true, needs: needSettings,
		args:  func(*env) map[string]any { return map[string]any{"enable": false} },
		check: func(_ *env, text string) error { return want(text, "vacation reply is off; its text is kept.") }},
}

// settingsTools are the tools guardSettings holds, and settingsKeys every
// argument it knows, as writeKeys are for the other writes.
var (
	settingsTools = map[string]bool{"update_signature": true, "create_filter": true, "create_trash_filter": true,
		"delete_filter": true, "set_vacation": true}
	settingsKeys = map[string]bool{
		// Checked below.
		"send_as": true, "signature": true, "from": true, "to": true, "add_labels": true, "filter_id": true,
		"start": true, "body": true, "subject": true,
		// Flags, and criteria that name nothing of the account's; criteria
		// on a delete are a witness, compared and never written.
		"dry_run": true, "confirm": true, "enable": true, "audience": true, "end": true, "query": true, "criteria": true,
		"negated_query": true, "has_attachment": true, "exclude_chats": true, "size": true, "size_comparison": true,
		"remove_labels": true, "archive": true, "mark_read": true, "star": true,
	}
)

// minVacationLead is how far ahead a vacation reply the run turns on must
// start, so it can answer nobody before it is turned off.
const minVacationLead = 200 * 24 * time.Hour

// guardSettings holds a settings step to the run's own: a signature it
// wrote, on the account's own address; filters matching only its own
// sender, and deleted only when it made them; a vacation reply that
// starts months ahead.
func (e *env) guardSettings(tool string, args map[string]any) error {
	if !settingsTools[tool] {
		return nil
	}
	for key := range args {
		if !settingsKeys[key] {
			return fmt.Errorf("%w: %s on %s has no rule in the guard", errUnscoped, key, tool)
		}
	}
	str := func(key string) string { v, _ := args[key].(string); return v }
	switch tool {
	case "update_signature":
		// Only the default address's signature is saved and restored.
		if a := str("send_as"); a != "" && a != e.signatureAddress {
			return fmt.Errorf("%w: send_as is not the default address, whose signature the run saved", errUnscoped)
		}
		if sig := str("signature"); sig != "" && !strings.Contains(sig, e.seed.label.name) {
			return fmt.Errorf("%w: the signature is not the run's", errUnscoped)
		}
	case "create_filter", "create_trash_filter":
		if str("from") != filterSender(e.seed.label) {
			return fmt.Errorf("%w: a filter must match only the run's own sender", errUnscoped)
		}
		if to := str("to"); to != "" && to != e.account {
			return fmt.Errorf("%w: to is not the signed-in account", errUnscoped)
		}
		labels, _ := args["add_labels"].([]any)
		for _, v := range labels {
			if l, _ := v.(string); l != e.seed.label.name {
				return fmt.Errorf("%w: add_labels names a label the run did not make", errUnscoped)
			}
		}
	case "delete_filter":
		if !slices.Contains(e.filters, str("filter_id")) {
			return fmt.Errorf("%w: filter_id names a filter the run did not make", errUnscoped)
		}
	case "set_vacation":
		for _, key := range []string{"subject", "body"} {
			if v := str(key); v != "" && !strings.Contains(v, e.seed.label.name) {
				return fmt.Errorf("%w: the vacation %s is not the run's", errUnscoped, key)
			}
		}
		if on, _ := args["enable"].(bool); on {
			start, err := time.Parse(time.RFC3339, str("start"))
			if err != nil || time.Until(start) < minVacationLead {
				return fmt.Errorf("%w: a vacation reply the run turns on must start at least %s ahead", errUnscoped, minVacationLead)
			}
		}
	}
	return nil
}

// settingsSpikes run after the steps. Their filters match only the run's
// own sender, and each deletes what it made; cleanup deletes any left.
var settingsSpikes = []spike{
	{name: "L", question: "How does Gmail answer filters.create calls sent at once, and does it take the refused " +
		"ones sent again one at a time?", ask: spikeL},
}

// spikeL sends eight filter creates at once, waits, sends each refused
// one again alone, then reads the list for the run's filters and
// deletes every one, one at a time. It reports each phase's answers.
func spikeL(ctx context.Context, x spikeRun) string {
	if !x.spikeL {
		return "not run: answered 2026-09-28 (§18 row 54); -spike-l sends the filter writes again"
	}
	if !x.box.SettingsScope() {
		return "not run: the profile's login did not grant gmail.settings.basic"
	}
	const n = 8
	body := func(i int) map[string]any {
		return map[string]any{
			"criteria": map[string]any{"from": filterSender(x.s.label), "query": fmt.Sprintf("overlap%d", i)},
			"action":   map[string]any{"addLabelIds": []string{"STARRED"}},
		}
	}
	first := make([]probeResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { first[i] = x.box.Probe(ctx, http.MethodPost, "settings/filters", nil, body(i)) })
	}
	wg.Wait()
	var burst, again answers
	var made []string
	for _, r := range first {
		burst.add(r)
		if r.id != "" {
			made = append(made, r.id)
		}
	}
	pause(ctx, 5*time.Second)
	for i, r := range first {
		if r.id != "" {
			continue
		}
		a := x.box.Probe(ctx, http.MethodPost, "settings/filters", nil, body(i))
		again.add(a)
		if a.id != "" {
			made = append(made, a.id)
		}
	}
	pause(ctx, 5*time.Second)
	listed, err := x.box.FiltersFrom(ctx, filterSender(x.s.label))
	var deletes answers
	for _, id := range listed {
		deletes.add(x.box.Probe(ctx, http.MethodDelete, "settings/filters/"+url.PathEscape(id), nil, nil))
	}
	read := fmt.Sprintf("the list then held %d of the run's filters", len(listed))
	if err != nil {
		read = "the list could not be read: " + err.Error()
	}
	return fmt.Sprintf("%d creates at once: %s. 5 s later, the refused ones again one at a time: %s. "+
		"%d were answered with an id; 5 s later %s; deleting them one at a time: %s",
		n, burst, again, len(made), read, deletes)
}

// answers counts how Gmail answered probes, without what they created.
type answers map[string]int

func (a *answers) add(r probeResult) {
	if *a == nil {
		*a = answers{}
	}
	k := fmt.Sprintf("HTTP %d", r.status)
	if r.status/100 != 2 {
		k += fmt.Sprintf(" reason %q message %q", r.reason, r.message)
	}
	(*a)[k]++
}

func (a answers) String() string {
	if len(a) == 0 {
		return "none"
	}
	keys := slices.Sorted(maps.Keys(a))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d× %s", a[k], k)
	}
	return strings.Join(parts, "; ")
}

// pause waits d, or until ctx ends.
func pause(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// spamLater says whether the run's archiving filter lists SPAM among
// its removed labels, from the last step's structured rows.
func spamLater(e *env) string {
	if e.archiveFilter == "" {
		return "no archiving filter was made to look at"
	}
	filters, _ := e.structured["filters"].([]any)
	for _, f := range filters {
		row, _ := f.(map[string]any)
		if row["id"] != e.archiveFilter {
			continue
		}
		removed, _ := row["remove_labels"].([]any)
		for _, l := range removed {
			if ref, _ := l.(map[string]any); ref["id"] == "SPAM" {
				return "15 s and one filter write after its create, the run's archiving filter lists SPAM"
			}
		}
		return "15 s and one filter write after its create, the run's archiving filter does not list SPAM"
	}
	return "the run's archiving filter is not among the filters listed"
}
