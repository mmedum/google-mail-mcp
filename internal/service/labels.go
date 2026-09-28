package service

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/mime"
	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
)

// LabelSpec is a label's name and look, as create_label and update_label
// take them. Empty fields are left as they are, or Gmail's default.
type LabelSpec struct {
	Name string
	// InLabelList is "show", "show_if_unread" or "hide": whether the
	// label is listed in Gmail's label list.
	InLabelList string
	// InMessageList is "show" or "hide": whether the label is shown on
	// messages in a listing.
	InMessageList string
	// TextColor and BackgroundColor are "#rrggbb" from Gmail's palette,
	// given together.
	TextColor, BackgroundColor string
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// maxLabelName bounds a label name; Gmail documents no limit, and this
// server shows names in its own voice (render.labelName) cut at 225.
const maxLabelName = 225

// wire checks a spec and turns it into the fields of a label write.
func (sp LabelSpec) wire() (gmail.Label, []string, error) {
	var l gmail.Label
	var changed []string
	if sp.Name != "" {
		name := strings.TrimSpace(sp.Name)
		if name == "" || utf8.RuneCountInString(name) > maxLabelName ||
			mime.HasControl(name) {
			return l, nil, gapi.Errf(gapi.ClassInvalid, "name must be 1 to %d characters with no line breaks", maxLabelName)
		}
		l.Name = name
		changed = append(changed, "name")
	}
	if sp.InLabelList != "" {
		v, ok := model.LabelListWords[sp.InLabelList]
		if !ok {
			return l, nil, gapi.Errf(gapi.ClassInvalid, "in_label_list must be show, show_if_unread or hide")
		}
		l.LabelListVisibility = v
		changed = append(changed, "in_label_list")
	}
	if sp.InMessageList != "" {
		v, ok := model.MessageListWords[sp.InMessageList]
		if !ok {
			return l, nil, gapi.Errf(gapi.ClassInvalid, "in_message_list must be show or hide")
		}
		l.MessageListVisibility = v
		changed = append(changed, "in_message_list")
	}
	if sp.TextColor != "" || sp.BackgroundColor != "" {
		if !hexColor.MatchString(sp.TextColor) || !hexColor.MatchString(sp.BackgroundColor) {
			return l, nil, gapi.Errf(gapi.ClassInvalid,
				"text_color and background_color go together, each #rrggbb from Gmail's label palette")
		}
		l.Color = &gmail.LabelColor{TextColor: strings.ToLower(sp.TextColor), BackgroundColor: strings.ToLower(sp.BackgroundColor)}
		changed = append(changed, "color")
	}
	return l, changed, nil
}

// nameTaken refuses a name Gmail would refuse (spike I, §6.2): a system
// label's in any case, or another user label's in any case.
func nameTaken(all []gmail.Label, name, except string) error {
	for _, l := range all {
		if l.ID == except || !strings.EqualFold(l.Name, name) {
			continue
		}
		if l.Type == gmail.LabelTypeSystem {
			return gapi.Errf(gapi.ClassInvalid, "that name is a system label's; Gmail refuses it for a user label")
		}
		return gapi.Errf(gapi.ClassConflict, "label %s already has that name, ignoring case, and Gmail refuses a second", l.ID)
	}
	return nil
}

// CreateLabel creates a user label. The name is checked against the
// label list first, so a dry run says what Gmail would answer.
func (s *Service) CreateLabel(ctx context.Context, sp LabelSpec) (model.LabelWrite, error) {
	out := model.LabelWrite{Op: "create", DryRun: gapi.WritesForbidden(ctx)}
	if strings.TrimSpace(sp.Name) == "" {
		return out, gapi.Errf(gapi.ClassInvalid, "name is required")
	}
	l, _, err := sp.wire()
	if err != nil {
		return out, err
	}
	ls, err := s.labels(ctx)
	if err != nil {
		return out, err
	}
	if err := nameTaken(ls.all, l.Name, ""); err != nil {
		return out, err
	}
	if out.DryRun {
		// Gmail's defaults for what was not given.
		preview := gmail.Label{Type: gmail.LabelTypeUser, LabelListVisibility: "labelShow", MessageListVisibility: "show"}
		mergeLabel(&preview, l)
		out.After = model.NewLabel(preview, false)
		return out, nil
	}
	created, err := s.client.CreateLabel(ctx, l)
	if err != nil {
		return out, err
	}
	out.After = model.NewLabel(*created, false)
	return out, nil
}

// UpdateLabel patches a user label's name, visibility or color; what is
// not given is not sent, so nothing else about the label changes (§4.4).
func (s *Service) UpdateLabel(ctx context.Context, label string, sp LabelSpec) (model.LabelWrite, error) {
	out := model.LabelWrite{Op: "update", DryRun: gapi.WritesForbidden(ctx)}
	patch, changed, err := sp.wire()
	if err != nil {
		return out, err
	}
	if len(changed) == 0 {
		return out, gapi.Errf(gapi.ClassInvalid, "nothing to change: give name, in_label_list, in_message_list or the colors")
	}
	ls, err := s.labels(ctx)
	if err != nil {
		return out, err
	}
	id, err := resolveLabel(ls.all, label)
	if err != nil {
		return out, err
	}
	var current gmail.Label
	for _, l := range ls.all {
		if l.ID == id {
			current = l
		}
	}
	if current.Type == gmail.LabelTypeSystem {
		return out, gapi.Errf(gapi.ClassInvalid, "%s is a system label; Gmail does not rename or recolor it", id)
	}
	if patch.Name != "" {
		if err := nameTaken(ls.all, patch.Name, id); err != nil {
			return out, err
		}
	}
	before := model.NewLabel(current, false)
	out.Before, out.Changed = &before, changed
	if out.DryRun {
		after := current
		mergeLabel(&after, patch)
		out.After = model.NewLabel(after, false)
		return out, nil
	}
	updated, err := s.client.PatchLabel(ctx, id, patch)
	if err != nil {
		return out, err
	}
	out.After = model.NewLabel(*updated, false)
	return out, nil
}

// DeleteLabel deletes a user label, which takes it off every message
// and thread that carries it and cannot be undone. The label is read
// with its counts first, so the result says how much mail loses it.
func (s *Service) DeleteLabel(ctx context.Context, label string, confirm bool) (model.LabelWrite, error) {
	out := model.LabelWrite{Op: "delete", DryRun: gapi.WritesForbidden(ctx)}
	if !confirm && !out.DryRun {
		return out, gapi.Errf(gapi.ClassBlocked,
			"delete_label removes the label from every message that carries it and cannot be undone. Pass confirm: true to delete it")
	}
	ls, err := s.labels(ctx)
	if err != nil {
		return out, err
	}
	id, err := resolveLabel(ls.all, label)
	if err != nil {
		return out, err
	}
	for _, l := range ls.all {
		if l.ID == id && l.Type == gmail.LabelTypeSystem {
			return out, gapi.Errf(gapi.ClassInvalid, "%s is a system label; Gmail does not delete it", id)
		}
	}
	g, err := s.client.GetLabel(ctx, id)
	if err != nil {
		return out, err
	}
	before := model.NewLabel(*g, true)
	out.Before = &before
	if out.DryRun {
		return out, nil
	}
	if err := ask(ctx, render.AskDeleteLabel(before)); err != nil {
		return out, err
	}
	if err := s.client.DeleteLabel(ctx, id); err != nil {
		if classOf(err) != gapi.ClassNotFound {
			return out, err
		}
		out.Gone = true
	}
	return out, nil
}

// mergeLabel applies a patch to a label, for a dry run's preview.
func mergeLabel(l *gmail.Label, p gmail.Label) {
	if p.Name != "" {
		l.Name = p.Name
	}
	if p.LabelListVisibility != "" {
		l.LabelListVisibility = p.LabelListVisibility
	}
	if p.MessageListVisibility != "" {
		l.MessageListVisibility = p.MessageListVisibility
	}
	if p.Color != nil {
		l.Color = p.Color
	}
}
