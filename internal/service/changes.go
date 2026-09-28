package service

import (
	"context"
	"slices"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// History page sizes: Google's own default and cap for history.list.
const (
	DefaultHistoryMax = 100
	MaxHistoryMax     = 500
)

// changeTypes are the kinds list_changes accepts, in order, each with
// the historyTypes value history.list takes for it.
var changeTypes = []struct {
	kind        model.ChangeKind
	historyType string
}{
	{model.ChangeAdded, "messageAdded"},
	{model.ChangeDeleted, "messageDeleted"},
	{model.ChangeLabelsAdded, "labelAdded"},
	{model.ChangeLabelsRemoved, "labelRemoved"},
}

func historyType(kind string) (string, bool) {
	for _, c := range changeTypes {
		if string(c.kind) == kind {
			return c.historyType, true
		}
	}
	return "", false
}

func kindNames() string {
	names := make([]string, len(changeTypes))
	for i, c := range changeTypes {
		names[i] = string(c.kind)
	}
	return strings.Join(names, ", ")
}

// ChangesQuery is what list_changes was asked for.
type ChangesQuery struct {
	HistoryID string
	Label     string
	Kinds     []string
	Max       int
	PageToken string
}

// Changes is one page of changes, or the news that the cursor expired.
type Changes struct {
	Changes []model.Change
	// Expired is set when Gmail no longer holds history from HistoryID
	// on (§2.8). Changes is then empty because they are lost, not
	// because there were none.
	Expired bool
	// HistoryID is where the next call starts: the mailbox's current
	// history id. It is safe to use once no page is left; while
	// NextPageToken is set, the pages continue from the same start.
	HistoryID     string
	NextPageToken string
	// Label is the label the listing was limited to, resolved.
	Label *model.LabelRef
	// Start is the history id the listing was read from, as sent.
	Start string
}

// Changes lists what changed after a history id (§7.6). A 404 is an
// expired cursor, reported with the current history id to restart from,
// never as an empty list.
func (s *Service) Changes(ctx context.Context, q ChangesQuery) (Changes, error) {
	o, err := historyOptions(q)
	if err != nil {
		return Changes{}, err
	}
	var (
		ls  labelSet
		res *gmail.ListHistoryResponse
		out = Changes{Start: o.StartHistoryID}
	)
	if q.Label != "" {
		if ls, err = s.labels(ctx); err != nil {
			return Changes{}, err
		}
		if o.LabelID, err = resolveLabel(ls.all, q.Label); err != nil {
			return Changes{}, err
		}
		out.Label = &model.LabelRef{ID: o.LabelID, Name: ls.index.Name(o.LabelID)}
		res, err = s.client.ListHistory(ctx, o)
	} else {
		ls, res, err = withLabels(ctx, s, func(ctx context.Context) (*gmail.ListHistoryResponse, error) {
			return s.client.ListHistory(ctx, o)
		})
	}
	if c, ok := gapi.ClassOf(err); ok && c == gapi.ClassNotFound {
		if out.Label != nil {
			// A label deleted since it was resolved is also a 404. Reporting
			// that as an expired cursor would move the caller's cursor past
			// changes it could still read, so the label is checked again.
			if gone, lerr := s.labelGone(ctx, out.Label.ID); lerr != nil {
				return Changes{}, lerr
			} else if gone {
				return Changes{}, gapi.Errf(gapi.ClassNotFound, "the label was deleted while it was being read; list_labels shows the labels now")
			}
		}
		p, perr := s.client.Profile(ctx)
		if perr != nil {
			return Changes{}, perr
		}
		out.Expired, out.HistoryID, out.Changes = true, p.HistoryID, []model.Change{}
		return out, nil
	}
	if err != nil {
		return Changes{}, err
	}
	out.Changes = model.NewChanges(res.History, ls.index)
	out.HistoryID, out.NextPageToken = res.HistoryID, res.NextPageToken
	return out, nil
}

// labelGone reports whether a label id is no longer in the label list.
func (s *Service) labelGone(ctx context.Context, id string) (bool, error) {
	ls, err := s.labels(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range ls.all {
		if l.ID == id {
			return false, nil
		}
	}
	return true, nil
}

// historyOptions checks a changes query before anything is sent.
func historyOptions(q ChangesQuery) (gapi.HistoryOptions, error) {
	id := strings.TrimSpace(q.HistoryID)
	if _, ok := gmail.ParseHistoryID(id); !ok {
		return gapi.HistoryOptions{}, gapi.Errf(gapi.ClassInvalid,
			"history_id must be a decimal history id, from get_profile or a previous list_changes")
	}
	size, err := pageSizeOf(q.Max, DefaultHistoryMax, MaxHistoryMax)
	if err != nil {
		return gapi.HistoryOptions{}, err
	}
	o := gapi.HistoryOptions{StartHistoryID: id, Max: size, PageToken: q.PageToken}
	for _, k := range q.Kinds {
		t, ok := historyType(k)
		if !ok {
			return gapi.HistoryOptions{}, gapi.Errf(gapi.ClassInvalid, "kinds takes %s", kindNames())
		}
		if !slices.Contains(o.Types, t) {
			o.Types = append(o.Types, t)
		}
	}
	return o, nil
}
