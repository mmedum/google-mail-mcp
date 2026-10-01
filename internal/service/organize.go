package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
)

// MaxWriteIDs is the most ids one write names (§4.7): far below Google's
// 1,000, because a write names what it touches and a person reads the
// list.
const MaxWriteIDs = 100

// Targets are the ids a multi-id write names.
type Targets struct {
	MessageIDs, ThreadIDs []string
}

// target is one id and what it names.
type target struct {
	id   string
	kind model.ItemKind
}

// targets checks the ids: at least one, at most MaxWriteIDs, each once.
func (t Targets) list() ([]target, error) {
	var out []target
	seen := map[target]bool{}
	for _, g := range []struct {
		ids  []string
		kind model.ItemKind
	}{{t.MessageIDs, model.KindMessage}, {t.ThreadIDs, model.KindThread}} {
		for _, id := range g.ids {
			tg := target{id: strings.TrimSpace(id), kind: g.kind}
			if tg.id == "" {
				return nil, gapi.Errf(gapi.ClassInvalid, "an id is empty")
			}
			if !seen[tg] {
				seen[tg] = true
				out = append(out, tg)
			}
		}
	}
	switch {
	case len(out) == 0:
		return nil, gapi.Errf(gapi.ClassInvalid, "name at least one message_ids or thread_ids entry; a write never takes a search (§4.7)")
	case len(out) > MaxWriteIDs:
		return nil, gapi.Errf(gapi.ClassInvalid, "%d ids is more than the %d one call may name; split the list", len(out), MaxWriteIDs)
	}
	return out, nil
}

// Relabel is what modify_labels was asked for.
type Relabel struct {
	Targets
	Add, Remove []string
}

// ModifyLabels adds and removes labels on each message and thread named,
// one item at a time, and reports each item's labels before and after
// (§4.7, §4.12). An item already in the state asked for is not written
// and is reported unchanged; one that fails is reported with its class
// while the rest go on.
func (s *Service) ModifyLabels(ctx context.Context, in Relabel) (model.ItemsWrite, error) {
	out := model.ItemsWrite{Op: "modify_labels", DryRun: gapi.WritesForbidden(ctx)}
	targets, err := in.list()
	if err != nil {
		return out, err
	}
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return out, gapi.Errf(gapi.ClassInvalid, "give add or remove: the labels to change")
	}
	ls, err := s.labels(ctx)
	if err != nil {
		return out, err
	}
	add, remove, err := relabelIDs(ls.all, in.Add, in.Remove)
	if err != nil {
		return out, err
	}
	out.Add, out.Remove = ls.index.Refs(add), ls.index.Refs(remove)
	for _, v := range model.Verbs {
		if slices.Contains(add, v.Label) && v.Add || slices.Contains(remove, v.Label) && !v.Add {
			out.Verbs = append(out.Verbs, v.Name)
		}
	}
	out.Items = s.each(ctx, targets, ls.index, s.relabel(add, remove))
	return out, nil
}

// relabelIDs resolves add and remove to label ids, refusing what Gmail
// sets itself and what trash and restore are for.
func relabelIDs(all []gmail.Label, addNames, removeNames []string) (add, remove []string, err error) {
	if add, err = resolveLabels(all, dedupe(addNames)); err != nil {
		return nil, nil, err
	}
	if remove, err = resolveLabels(all, dedupe(removeNames)); err != nil {
		return nil, nil, err
	}
	for _, id := range append(slices.Clone(add), remove...) {
		switch id {
		case "SENT", "DRAFT":
			return nil, nil, gapi.Errf(gapi.ClassConflict, "Gmail sets %s itself; it cannot be added or removed by hand", id)
		case "TRASH":
			return nil, nil, gapi.Errf(gapi.ClassInvalid, "use trash and restore to move mail in and out of the trash")
		}
	}
	if err := addedAndRemoved(add, remove); err != nil {
		return nil, nil, err
	}
	return add, remove, nil
}

// addedAndRemoved refuses a label both added and removed in one change.
func addedAndRemoved(add, remove []string) error {
	for _, id := range add {
		if slices.Contains(remove, id) {
			return gapi.Errf(gapi.ClassInvalid, "label %s is in both add and remove", id)
		}
	}
	return nil
}

// needs is the part of a change a message's labels do not already show.
func needs(labels, add, remove []string) (a, r []string) {
	for _, id := range add {
		if !slices.Contains(labels, id) {
			a = append(a, id)
		}
	}
	for _, id := range remove {
		if slices.Contains(labels, id) {
			r = append(r, id)
		}
	}
	return a, r
}

// relabel is modify_labels' item write. A message sends only the part of
// the change it needs; a thread sends the whole change, which Gmail
// applies to each of its messages, once any one needs it. Drafts are
// refused, or skipped inside a thread (§2.12).
func (s *Service) relabel(add, remove []string) itemWrite {
	return itemWrite{
		check: func(t target, labels map[string][]string) (bool, error) {
			pending := false
			for id, ls := range labels {
				if slices.Contains(ls, "DRAFT") {
					if t.kind == model.KindMessage {
						return false, gapi.Errf(gapi.ClassUnsupported, "message %s is a draft, and Gmail does not label drafts (§2.12)", id)
					}
					continue
				}
				if a, r := needs(ls, add, remove); len(a)+len(r) > 0 {
					pending = true
				}
			}
			return !pending, nil
		},
		write: func(ctx context.Context, t target, before []string) ([]string, error) {
			if t.kind == model.KindThread {
				th, err := s.client.ModifyThread(ctx, t.id, gmail.ModifyThreadRequest{AddLabelIDs: add, RemoveLabelIDs: remove})
				return s.threadAfter(ctx, t.id, th, err)
			}
			a, r := needs(before, add, remove)
			m, err := s.client.ModifyMessage(ctx, t.id, gmail.ModifyMessageRequest{AddLabelIDs: a, RemoveLabelIDs: r})
			return m.LabelIDs, err
		},
		// Gmail skips a thread's drafts, so their labels stay as they are.
		predict: func(labels map[string][]string) []string {
			return applied(labels, add, remove, true)
		},
	}
}

// Move is what trash or restore was asked for.
type Move struct {
	Targets
	// Restore takes out of the trash; otherwise into it.
	Restore bool
}

// Trash moves each message and thread named into the trash, or out of it
// (§4.6). What is already where it was asked to go is reported
// unchanged, "already in the trash", and not written.
func (s *Service) Trash(ctx context.Context, in Move) (model.ItemsWrite, error) {
	op := "trash"
	if in.Restore {
		op = "restore"
	}
	out := model.ItemsWrite{Op: op, DryRun: gapi.WritesForbidden(ctx)}
	targets, err := in.list()
	if err != nil {
		return out, err
	}
	ls, err := s.labels(ctx)
	if err != nil {
		return out, err
	}
	out.Items = s.each(ctx, targets, ls.index, s.move(in.Restore))
	return out, nil
}

// move is trash's and restore's item write. A draft goes with
// delete_draft instead.
func (s *Service) move(restore bool) itemWrite {
	return itemWrite{
		check: func(t target, labels map[string][]string) (bool, error) {
			done := true
			for id, ls := range labels {
				if t.kind == model.KindMessage && slices.Contains(ls, "DRAFT") {
					return false, gapi.Errf(gapi.ClassInvalid, "message %s is a draft; delete_draft removes a draft", id)
				}
				if slices.Contains(ls, "TRASH") == restore {
					done = false
				}
			}
			return done, nil
		},
		write: func(ctx context.Context, t target, _ []string) ([]string, error) {
			switch {
			case t.kind == model.KindThread && restore:
				th, err := s.client.UntrashThread(ctx, t.id)
				return s.threadAfter(ctx, t.id, th, err)
			case t.kind == model.KindThread:
				th, err := s.client.TrashThread(ctx, t.id)
				return s.threadAfter(ctx, t.id, th, err)
			case restore:
				m, err := s.client.UntrashMessage(ctx, t.id)
				return m.LabelIDs, err
			default:
				m, err := s.client.TrashMessage(ctx, t.id)
				return m.LabelIDs, err
			}
		},
		// Gmail's trash takes a message out of the inbox, and its restore
		// does not put it back (§18 row 70).
		predict: func(labels map[string][]string) []string {
			if restore {
				return applied(labels, nil, []string{"TRASH"}, false)
			}
			return applied(labels, []string{"TRASH"}, []string{"INBOX"}, false)
		},
	}
}

// Purge is what delete_permanently was asked for.
type Purge struct {
	Targets
	// Confirm must be set: nothing deleted this way can be restored.
	Confirm bool
}

// DeletePermanently deletes each message and thread named for good,
// skipping the trash (§4.6). Each item is read first, so the result
// shows the labels it had; a draft's message is refused for that item
// and points to delete_draft.
func (s *Service) DeletePermanently(ctx context.Context, in Purge) (model.ItemsWrite, error) {
	out := model.ItemsWrite{Op: "delete_permanently", DryRun: gapi.WritesForbidden(ctx)}
	targets, err := in.list()
	if err != nil {
		return out, err
	}
	if !in.Confirm && !out.DryRun {
		return out, gapi.Errf(gapi.ClassBlocked,
			"delete_permanently skips the trash and cannot be undone; trash keeps mail for 30 days. Pass confirm: true to delete")
	}
	ls, err := s.labels(ctx)
	if err != nil {
		return out, err
	}
	threads := 0
	for _, t := range targets {
		if t.kind == model.KindThread {
			threads++
		}
	}
	if err := ask(ctx, render.AskDeletePermanently(len(targets)-threads, threads)); err != nil {
		return out, err
	}
	out.Items = s.each(ctx, targets, ls.index, s.purge())
	return out, nil
}

// purge is delete_permanently's item write. A delete Gmail answers 404
// after the item was read is reported deleted: it is gone, whether by an
// earlier attempt of this call or elsewhere.
func (s *Service) purge() itemWrite {
	return itemWrite{
		check: func(t target, labels map[string][]string) (bool, error) {
			for id, ls := range labels {
				switch {
				case !slices.Contains(ls, "DRAFT"):
				case t.kind == model.KindMessage:
					return false, gapi.Errf(gapi.ClassInvalid, "message %s is a draft; delete_draft removes a draft", id)
				default:
					// threads.delete would take the draft with it, past the
					// delete_draft that says it is gone for good.
					return false, gapi.Errf(gapi.ClassInvalid,
						"thread %s holds draft message %s; delete it with delete_draft first, or name the thread's other messages", t.id, id)
				}
			}
			return false, nil
		},
		write: func(ctx context.Context, t target, _ []string) ([]string, error) {
			var err error
			if t.kind == model.KindThread {
				err = s.client.DeleteThread(ctx, t.id)
			} else {
				err = s.client.DeleteMessage(ctx, t.id)
			}
			if classOf(err) == gapi.ClassNotFound {
				err = nil
			}
			return nil, err
		},
	}
}

// itemWrite is one kind of multi-id write.
type itemWrite struct {
	// check looks at the labels of an item's messages, keyed by message
	// id: done when nothing needs writing, or an error refusing the item.
	check func(t target, labels map[string][]string) (done bool, refused error)
	// write changes the item, whose labels were before, and returns its
	// labels after.
	write func(ctx context.Context, t target, before []string) ([]string, error)
	// predict is the labels a dry run says the item would have after the
	// write, from its messages' labels; nil leaves them unreported.
	predict func(labels map[string][]string) []string
}

// each runs one write per item, at most fanOutLimit at a time: a read of
// the item's labels, then the write if one is needed. A failure is that
// item's, never the batch's.
func (s *Service) each(ctx context.Context, targets []target, index model.LabelIndex, w itemWrite) []model.Item {
	items := make([]model.Item, len(targets))
	_, _ = fanOut(ctx, len(targets), func(ctx context.Context, i int) (struct{}, error) {
		items[i] = s.one(ctx, targets[i], index, w)
		return struct{}{}, nil
	})
	return items
}

func (s *Service) one(ctx context.Context, t target, index model.LabelIndex, w itemWrite) model.Item {
	item := model.Item{ID: t.id, Kind: t.kind}
	labels, err := s.itemLabels(ctx, t)
	if err != nil {
		fail(&item, err)
		return item
	}
	before := union(labels)
	item.Before = index.Refs(before)
	done, err := w.check(t, labels)
	switch {
	case err != nil:
		fail(&item, err)
	case done:
		item.Outcome, item.After = model.Unchanged, item.Before
	case gapi.WritesForbidden(ctx):
		item.Outcome = model.WouldChange
		if w.predict != nil {
			item.After = index.Refs(w.predict(labels))
		}
	default:
		after, err := w.write(ctx, t, before)
		if err != nil {
			fail(&item, err)
			break
		}
		item.Outcome, item.After = model.Changed, index.Refs(after)
	}
	return item
}

// fail records an item's failure with its class.
func fail(item *model.Item, err error) {
	item.Outcome = model.Failed
	item.Class, item.Error = string(gapi.ClassUnavailable), "the call failed"
	var e *gapi.Error
	if errors.As(err, &e) {
		item.Class, item.Error = string(e.Class), e.Message
	}
}

// itemLabels reads the labels of an item's messages, keyed by message
// id: 20 units for a message, 40 for a thread.
func (s *Service) itemLabels(ctx context.Context, t target) (map[string][]string, error) {
	if t.kind == model.KindThread {
		th, err := s.client.GetThread(ctx, t.id, gapi.FormatMinimal)
		if err != nil {
			return nil, err
		}
		return labelsOf(th.Messages), nil
	}
	m, err := s.client.GetMessage(ctx, t.id, gapi.FormatMinimal)
	if err != nil {
		return nil, err
	}
	return map[string][]string{m.ID: m.LabelIDs}, nil
}

// threadAfter is a thread's labels after a write: from Gmail's answer
// when it lists the messages, otherwise read again.
func (s *Service) threadAfter(ctx context.Context, id string, th *gmail.Thread, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	if len(th.Messages) > 0 {
		return union(labelsOf(th.Messages)), nil
	}
	labels, err := s.itemLabels(ctx, target{id: id, kind: model.KindThread})
	return union(labels), err
}

// labelsOf keys a thread's messages' labels by message id.
func labelsOf(ms []gmail.Message) map[string][]string {
	out := make(map[string][]string, len(ms))
	for _, m := range ms {
		out[m.ID] = m.LabelIDs
	}
	return out
}

// applied is every label the item's messages would carry once add and
// remove are applied to each, sorted; with skipDrafts a draft keeps its own.
func applied(labels map[string][]string, add, remove []string, skipDrafts bool) []string {
	var all []string
	for _, ls := range labels {
		if skipDrafts && slices.Contains(ls, "DRAFT") {
			all = append(all, ls...)
			continue
		}
		for _, l := range ls {
			if !slices.Contains(remove, l) {
				all = append(all, l)
			}
		}
		all = append(all, add...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// union is every label on any of the messages, sorted.
func union(labels map[string][]string) []string {
	var out []string
	for _, ls := range labels {
		for _, l := range ls {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	slices.Sort(out)
	return out
}
