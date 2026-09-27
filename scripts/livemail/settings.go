//go:build live

package main

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The settings steps, run only when the profile's login granted
// gmail.settings.basic. drive saves the default address's signature and
// the vacation reply before them and restores both after, exactly, with
// its own client: update_signature takes plain text and could not put an
// HTML signature back. Filters match only the run's own sender address,
// which no mail comes from, and are deleted by the steps or by cleanup.

// needSettings keeps a settings step to profiles whose login granted the
// one scope Gmail accepts for it.
func needSettings(e *env) string {
	if !e.settings {
		return "the profile's login did not grant gmail.settings.basic; log in with GMAIL_ENABLE_SETTINGS=true to drive it"
	}
	return ""
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

// recordFilter keeps the id of a filter a step created.
func recordFilter(e *env, text string) error {
	m := filterIDIn.FindStringSubmatch(text)
	if m == nil {
		return errors.New("the result names no filter")
	}
	e.filters = append(e.filters, m[1])
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
	{name: "set the signature, dry run", tool: "update_signature", quiet: true, needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"signature": "Synthetic signature for " + e.seed.label.name, "dry_run": true}
		},
		check: func(_ *env, text string) error { return want(text, "dry run: would set the signature of") }},

	{name: "set the signature", tool: "update_signature", quiet: true, needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"send_as": e.signatureAddress, "signature": "Synthetic signature for " + e.seed.label.name + "\nline two"}
		},
		check: func(e *env, text string) error {
			if err := want(text, "set the signature of"); err != nil {
				return err
			}
			return want(text, "Synthetic signature for "+e.seed.label.name+"\nline two")
		}},

	{name: "clear the signature", tool: "update_signature", quiet: true, needs: needSettings,
		args:  func(*env) map[string]any { return map[string]any{"signature": ""} },
		check: func(_ *env, text string) error { return want(text, "signature after: none") }},

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

	{name: "a trashing filter without confirm is refused", tool: "create_filter", refuses: "blocked", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "trash", "trash": true}
		},
		check: func(_ *env, text string) error { return want(text, "confirm: true") }},

	{name: "create a trashing filter", tool: "create_filter", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "trash", "trash": true, "mark_read": true, "confirm": true}
		},
		check: func(e *env, text string) error {
			if err := recordFilter(e, text); err != nil {
				return err
			}
			return want(text, "moves matching mail to the trash")
		}},

	// Gmail keeps a second identical filter, so the server's check is the
	// only one. This filter has no size, which Gmail rounds (§18 row 52).
	{name: "an identical filter is refused", tool: "create_filter", refuses: "conflict", needs: needSettings,
		args: func(e *env) map[string]any {
			return map[string]any{"from": filterSender(e.seed.label), "query": "trash", "trash": true, "mark_read": true, "confirm": true}
		},
		check: func(_ *env, text string) error { return want(text, "already does this") }},

	{name: "a filter delete without confirm is refused", tool: "delete_filter", refuses: "blocked", needs: needFilter,
		args:  func(e *env) map[string]any { return map[string]any{"filter_id": e.filters[0]} },
		check: func(_ *env, text string) error { return want(text, "confirm: true") }},

	{name: "delete a filter, dry run", tool: "delete_filter", needs: needFilter,
		args:  func(e *env) map[string]any { return map[string]any{"filter_id": e.filters[0], "dry_run": true} },
		check: func(_ *env, text string) error { return want(text, "dry run: would delete filter") }},

	{name: "delete the filter", tool: "delete_filter", needs: needFilter, check: deletedFirst,
		args: func(e *env) map[string]any { return map[string]any{"filter_id": e.filters[0], "confirm": true} }},

	{name: "delete the trashing filter", tool: "delete_filter", needs: needFilter, check: deletedFirst,
		args: func(e *env) map[string]any { return map[string]any{"filter_id": e.filters[0], "confirm": true} }},

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
	settingsTools = map[string]bool{"update_signature": true, "create_filter": true, "delete_filter": true, "set_vacation": true}
	settingsKeys  = map[string]bool{
		// Checked below.
		"send_as": true, "signature": true, "from": true, "to": true, "add_labels": true, "filter_id": true,
		"start": true, "body": true, "subject": true,
		// Flags, and criteria that name nothing of the account's.
		"dry_run": true, "confirm": true, "enable": true, "audience": true, "end": true, "query": true,
		"negated_query": true, "has_attachment": true, "exclude_chats": true, "size": true, "size_comparison": true,
		"remove_labels": true, "archive": true, "mark_read": true, "star": true, "trash": true,
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
	case "create_filter":
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
