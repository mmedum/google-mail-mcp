package service

import (
	"context"
	"strings"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
)

// rfc822Prefix marks an id given as an RFC 5322 Message-ID (§6.1).
const rfc822Prefix = "rfc822:"

// Profile reads the account.
func (s *Service) Profile(ctx context.Context) (model.Profile, error) {
	p, err := s.client.Profile(ctx)
	if err != nil {
		return model.Profile{}, err
	}
	return model.NewProfile(*p), nil
}

// page is one page of a listing: its rows' ids and where it continues.
type page struct {
	ids      []string
	next     string
	estimate int
}

// listPage lists one page and reads each row with get. The label list
// is read alongside the listing, unless wanted names labels to filter
// by: then it is read first, to resolve them into the listing's ids.
func listPage[T any](ctx context.Context, s *Service, o gapi.ListOptions, wanted []string,
	list func(context.Context, gapi.ListOptions) (page, error),
	get func(ctx context.Context, id string, labels model.LabelIndex) (T, error),
) ([]T, page, error) {
	var (
		ls  labelSet
		p   page
		err error
	)
	if len(wanted) > 0 {
		if ls, err = s.labels(ctx); err != nil {
			return nil, page{}, err
		}
		if o.LabelIDs, err = resolveLabels(ls.all, wanted); err != nil {
			return nil, page{}, err
		}
		p, err = list(ctx, o)
	} else {
		ls, p, err = withLabels(ctx, s, func(ctx context.Context) (page, error) { return list(ctx, o) })
	}
	if err != nil {
		return nil, page{}, err
	}
	rows, err := fanOut(ctx, len(p.ids), func(ctx context.Context, i int) (T, error) {
		return get(ctx, p.ids[i], ls.index)
	})
	return rows, p, err
}

// SearchThreads lists threads and reads each with its headers only.
func (s *Service) SearchThreads(ctx context.Context, in Search) (render.ThreadList, Searched, error) {
	o, searched, err := listOptions(in)
	if err != nil {
		return render.ThreadList{}, searched, err
	}
	threads, p, err := listPage(ctx, s, o, in.Labels,
		func(ctx context.Context, o gapi.ListOptions) (page, error) {
			res, err := s.client.ListThreads(ctx, o)
			if err != nil {
				return page{}, err
			}
			p := page{next: res.NextPageToken, estimate: int(res.ResultSizeEstimate)}
			for _, t := range res.Threads {
				p.ids = append(p.ids, t.ID)
			}
			return p, nil
		},
		func(ctx context.Context, id string, labels model.LabelIndex) (model.Thread, error) {
			g, err := s.client.GetThread(ctx, id, gapi.FormatMetadata, listingHeaders...)
			if err != nil {
				return model.Thread{}, err
			}
			return model.NewThread(g, labels, nil)
		})
	if err != nil {
		return render.ThreadList{}, searched, err
	}
	return render.ThreadList{Threads: threads, NextPageToken: p.next, ResultSizeEstimate: p.estimate}, searched, nil
}

// SearchMessages lists messages and reads each with its headers only.
func (s *Service) SearchMessages(ctx context.Context, in Search) (render.MessageList, Searched, error) {
	o, searched, err := listOptions(in)
	if err != nil {
		return render.MessageList{}, searched, err
	}
	msgs, p, err := listPage(ctx, s, o, in.Labels,
		func(ctx context.Context, o gapi.ListOptions) (page, error) {
			res, err := s.client.ListMessages(ctx, o)
			if err != nil {
				return page{}, err
			}
			p := page{next: res.NextPageToken, estimate: int(res.ResultSizeEstimate)}
			for _, m := range res.Messages {
				p.ids = append(p.ids, m.ID)
			}
			return p, nil
		},
		func(ctx context.Context, id string, labels model.LabelIndex) (model.Message, error) {
			g, err := s.client.GetMessage(ctx, id, gapi.FormatMetadata, listingHeaders...)
			if err != nil {
				return model.Message{}, err
			}
			return model.NewMessage(g, labels, nil)
		})
	if err != nil {
		return render.MessageList{}, searched, err
	}
	return render.MessageList{Messages: msgs, NextPageToken: p.next, ResultSizeEstimate: p.estimate}, searched, nil
}

// Thread reads a whole thread, fetching every body part Gmail left
// behind an attachment id.
func (s *Service) Thread(ctx context.Context, id string) (model.Thread, error) {
	ls, g, err := withLabels(ctx, s, func(ctx context.Context) (*gmail.Thread, error) {
		return s.client.GetThread(ctx, id, gapi.FormatFull)
	})
	if err != nil {
		return model.Thread{}, err
	}
	t, err := model.NewThread(g, ls.index, nil)
	if err != nil {
		return model.Thread{}, err
	}
	wire := make([]*gmail.Message, len(t.Messages))
	for i := range wire {
		wire[i] = &g.Messages[i]
	}
	if err := s.fetchParts(ctx, ls.index, wire, t.Messages); err != nil {
		return model.Thread{}, err
	}
	return t, nil
}

// Message reads one message by id, or by "rfc822:<Message-ID>".
func (s *Service) Message(ctx context.Context, id string) (model.Message, error) {
	ls, g, err := withLabels(ctx, s, func(ctx context.Context) (*gmail.Message, error) {
		id, err := s.messageID(ctx, id)
		if err != nil {
			return nil, err
		}
		return s.client.GetMessage(ctx, id, gapi.FormatFull)
	})
	if err != nil {
		return model.Message{}, err
	}
	return s.complete(ctx, ls.index, g)
}

// complete converts a message read whole and fetches the parts its body
// left behind an attachment id.
func (s *Service) complete(ctx context.Context, labels model.LabelIndex, g *gmail.Message) (model.Message, error) {
	m, err := model.NewMessage(g, labels, nil)
	if err != nil {
		return model.Message{}, err
	}
	msgs := []model.Message{m}
	if err := s.fetchParts(ctx, labels, []*gmail.Message{g}, msgs); err != nil {
		return model.Message{}, err
	}
	return msgs[0], nil
}

// byRFC822ID finds the one message carrying a Message-ID header. Mailing
// lists deliver one Message-ID more than once, which is ambiguous.
func (s *Service) byRFC822ID(ctx context.Context, mid string) (string, error) {
	mid = strings.TrimSpace(mid)
	if mid == "" {
		return "", gapi.Errf(gapi.ClassInvalid, "rfc822: needs a Message-ID after it")
	}
	res, err := s.client.ListMessages(ctx, gapi.ListOptions{Q: "rfc822msgid:" + mid, IncludeSpamTrash: true, Max: 2})
	if err != nil {
		return "", err
	}
	switch len(res.Messages) {
	case 0:
		return "", gapi.Errf(gapi.ClassNotFound, "no message carries that Message-ID")
	case 1:
		return res.Messages[0].ID, nil
	}
	return "", gapi.Errf(gapi.ClassAmbiguous, "more than one message carries that Message-ID (ids %s, %s); pass an id",
		res.Messages[0].ID, res.Messages[1].ID)
}

// Labels lists the labels, reading each one's counts when asked: the
// listing leaves them out, and each label is one more unit.
func (s *Service) Labels(ctx context.Context, counts bool) ([]model.Label, error) {
	ls, err := s.labels(ctx)
	if err != nil {
		return nil, err
	}
	if !counts {
		return model.NewLabels(ls.all, false), nil
	}
	full, err := fanOut(ctx, len(ls.all), func(ctx context.Context, i int) (gmail.Label, error) {
		g, err := s.client.GetLabel(ctx, ls.all[i].ID)
		if err != nil {
			return gmail.Label{}, err
		}
		return *g, nil
	})
	if err != nil {
		return nil, err
	}
	return model.NewLabels(full, true), nil
}

// Drafts lists drafts and reads each draft's headers.
func (s *Service) Drafts(ctx context.Context, q string, max int, pageToken string) (render.DraftList, error) {
	size, err := pageSize(max)
	if err != nil {
		return render.DraftList{}, err
	}
	o := gapi.ListOptions{Q: strings.TrimSpace(q), Max: size, PageToken: pageToken}
	drafts, p, err := listPage(ctx, s, o, nil,
		func(ctx context.Context, o gapi.ListOptions) (page, error) {
			res, err := s.client.ListDrafts(ctx, o)
			if err != nil {
				return page{}, err
			}
			p := page{next: res.NextPageToken, estimate: int(res.ResultSizeEstimate)}
			for _, d := range res.Drafts {
				p.ids = append(p.ids, d.ID)
			}
			return p, nil
		},
		func(ctx context.Context, id string, labels model.LabelIndex) (model.Draft, error) {
			g, err := s.client.GetDraft(ctx, id, gapi.FormatMetadata)
			if err != nil {
				return model.Draft{}, err
			}
			return model.NewDraft(g, labels, nil)
		})
	if err != nil {
		return render.DraftList{}, err
	}
	return render.DraftList{Drafts: drafts, NextPageToken: p.next, ResultSizeEstimate: p.estimate}, nil
}

// Draft reads one draft whole. The id of the message inside it is the
// witness update_draft will require (§4.4).
func (s *Service) Draft(ctx context.Context, id string) (model.Draft, error) {
	ls, g, err := withLabels(ctx, s, func(ctx context.Context) (*gmail.Draft, error) {
		return s.client.GetDraft(ctx, id, gapi.FormatFull)
	})
	if err != nil {
		return model.Draft{}, err
	}
	if g == nil || g.Message == nil {
		return model.NewDraft(g, ls.index, nil)
	}
	m, err := s.complete(ctx, ls.index, g.Message)
	if err != nil {
		return model.Draft{}, err
	}
	return model.Draft{ID: g.ID, Message: m}, nil
}
