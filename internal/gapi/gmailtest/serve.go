package gmailtest

import (
	"encoding/base64"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// The handlers run with s.mu held.

func (s *Server) getProfile(w http.ResponseWriter, _ *http.Request, _ []string) {
	address := Account
	if s.ProfileAddress != "" {
		address = s.ProfileAddress
	}
	writeJSON(w, gmail.Profile{
		EmailAddress:  address,
		MessagesTotal: int32(len(s.messages)), //nolint:gosec // small
		ThreadsTotal:  int32(len(s.threads)),  //nolint:gosec // small
		HistoryID:     strconv.FormatUint(s.historyID, 10),
	})
}

// listParams reads the parameters every listing shares.
type listParams struct {
	terms            []term
	labelIDs         []string
	includeSpamTrash bool
	max              int
	token            string
}

func readList(w http.ResponseWriter, r *http.Request) (listParams, bool) {
	q := r.URL.Query()
	p := listParams{
		terms:            parseQuery(q.Get("q")),
		labelIDs:         q["labelIds"],
		includeSpamTrash: q.Get("includeSpamTrash") == "true",
		max:              100,
		token:            q.Get("pageToken"),
	}
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid maxResults")
			return p, false
		}
		p.max = min(n, 500)
	}
	return p, true
}

// page slices n results. ok is false when the token is not one the fake
// issued. empty is set for the empty first page EmptyFirstPage asks for.
func (s *Server) page(w http.ResponseWriter, p listParams, n int) (start, end int, next string, ok bool) {
	if p.token == "" && s.EmptyFirstPage && n > 0 {
		return 0, 0, "page-0", true
	}
	if p.token != "" {
		v, found := strings.CutPrefix(p.token, "page-")
		off, err := strconv.Atoi(v)
		if !found || err != nil || off < 0 || off > n {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid pageToken")
			return 0, 0, "", false
		}
		start = off
	}
	end = min(start+p.max, n)
	if end < n {
		next = "page-" + strconv.Itoa(end)
	}
	return start, end, next, true
}

// matching returns matching messages, newest first.
func (s *Server) matching(p listParams) []*message {
	var out []*message
	for _, m := range s.messages {
		if visible(m, p.includeSpamTrash, p.terms, p.labelIDs) && s.matches(m, p.terms, p.labelIDs) {
			out = append(out, m)
		}
	}
	sortNewest(out)
	return out
}

func sortNewest(ms []*message) {
	sort.Slice(ms, func(i, j int) bool {
		if !ms[i].internalDate.Equal(ms[j].internalDate) {
			return ms[i].internalDate.After(ms[j].internalDate)
		}
		return ms[i].id > ms[j].id
	})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, _ []string) {
	p, ok := readList(w, r)
	if !ok {
		return
	}
	all := s.matching(p)
	start, end, next, ok := s.page(w, p, len(all))
	if !ok {
		return
	}
	resp := gmail.ListMessagesResponse{NextPageToken: next, ResultSizeEstimate: uint32(len(all))} //nolint:gosec // small
	for _, m := range all[start:end] {
		resp.Messages = append(resp.Messages, gmail.Message{ID: m.id, ThreadID: m.threadID})
	}
	writeJSON(w, resp)
}

func readFormat(w http.ResponseWriter, r *http.Request) (string, []string, bool) {
	f := r.URL.Query().Get("format")
	if f == "" {
		f = "full"
	}
	switch f {
	case "full", "metadata", "minimal", "raw":
		return f, r.URL.Query()["metadataHeaders"], true
	}
	writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid format")
	return "", nil, false
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, args []string) {
	format, headers, ok := readFormat(w, r)
	if !ok {
		return
	}
	m, found := s.messages[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	writeJSON(w, s.render(m, format, headers))
}

func (s *Server) getAttachment(w http.ResponseWriter, _ *http.Request, args []string) {
	if _, found := s.messages[args[0]]; !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	b, found := s.store[args[1]]
	if !found || !strings.HasPrefix(args[1], "att-"+args[0]+"-") {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid attachment token")
		return
	}
	writeJSON(w, gmail.MessagePartBody{Size: int32(len(b)), Data: base64.URLEncoding.EncodeToString(b)}) //nolint:gosec // small
}

// threadsNewest returns thread ids with a matching message, ordered by
// their newest message.
func (s *Server) threadsNewest(p listParams) []string {
	var ids []string
	seen := map[string]bool{}
	for _, m := range s.matching(p) {
		if !seen[m.threadID] {
			seen[m.threadID] = true
			ids = append(ids, m.threadID)
		}
	}
	latest := func(id string) *message {
		ms := s.threads[id]
		return s.messages[ms[len(ms)-1]]
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return latest(ids[i]).internalDate.After(latest(ids[j]).internalDate)
	})
	return ids
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request, _ []string) {
	p, ok := readList(w, r)
	if !ok {
		return
	}
	ids := s.threadsNewest(p)
	start, end, next, ok := s.page(w, p, len(ids))
	if !ok {
		return
	}
	resp := gmail.ListThreadsResponse{NextPageToken: next, ResultSizeEstimate: uint32(len(ids))} //nolint:gosec // small
	for _, id := range ids[start:end] {
		ms := s.threads[id]
		last := s.messages[ms[len(ms)-1]]
		resp.Threads = append(resp.Threads, gmail.Thread{ID: id, Snippet: last.snippet, HistoryID: s.threadHistory(id)})
	}
	writeJSON(w, resp)
}

func (s *Server) threadHistory(id string) string {
	var h uint64
	for _, mid := range s.threads[id] {
		h = max(h, s.messages[mid].historyID)
	}
	return strconv.FormatUint(h, 10)
}

func (s *Server) getThread(w http.ResponseWriter, r *http.Request, args []string) {
	format, headers, ok := readFormat(w, r)
	if !ok {
		return
	}
	if format == "raw" {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid format")
		return
	}
	ids, found := s.threads[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	t := gmail.Thread{ID: args[0], HistoryID: s.threadHistory(args[0])}
	for _, id := range ids {
		t.Messages = append(t.Messages, s.render(s.messages[id], format, headers))
	}
	writeJSON(w, t)
}

func (s *Server) listLabels(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, gmail.ListLabelsResponse{Labels: s.labelList()})
}

func (s *Server) getLabel(w http.ResponseWriter, _ *http.Request, args []string) {
	l, found := s.labels[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	c := *l
	threads, unreadThreads := map[string]bool{}, map[string]bool{}
	for _, m := range s.messages {
		if !slices.Contains(m.labels, l.ID) {
			continue
		}
		c.MessagesTotal++
		threads[m.threadID] = true
		if slices.Contains(m.labels, "UNREAD") {
			c.MessagesUnread++
			unreadThreads[m.threadID] = true
		}
	}
	c.ThreadsTotal, c.ThreadsUnread = int32(len(threads)), int32(len(unreadThreads)) //nolint:gosec // small
	writeJSON(w, c)
}

func (s *Server) draftIDs() []string {
	ids := make([]string, 0, len(s.drafts))
	for id := range s.drafts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.messages[s.drafts[ids[i]]], s.messages[s.drafts[ids[j]]]
		return a.internalDate.After(b.internalDate)
	})
	return ids
}

func (s *Server) listDrafts(w http.ResponseWriter, r *http.Request, _ []string) {
	p, ok := readList(w, r)
	if !ok {
		return
	}
	var ids []string
	for _, id := range s.draftIDs() {
		if s.matches(s.messages[s.drafts[id]], p.terms, nil) {
			ids = append(ids, id)
		}
	}
	start, end, next, ok := s.page(w, p, len(ids))
	if !ok {
		return
	}
	resp := gmail.ListDraftsResponse{NextPageToken: next, ResultSizeEstimate: uint32(len(ids))} //nolint:gosec // small
	for _, id := range ids[start:end] {
		m := s.messages[s.drafts[id]]
		resp.Drafts = append(resp.Drafts, gmail.Draft{ID: id, Message: &gmail.Message{ID: m.id, ThreadID: m.threadID}})
	}
	writeJSON(w, resp)
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request, args []string) {
	format, _, ok := readFormat(w, r)
	if !ok {
		return
	}
	mid, found := s.drafts[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	m := s.render(s.messages[mid], format, nil)
	writeJSON(w, gmail.Draft{ID: args[0], Message: &m})
}

func (s *Server) listHistory(w http.ResponseWriter, r *http.Request, _ []string) {
	q := r.URL.Query()
	start, err := strconv.ParseUint(q.Get("startHistoryId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid startHistoryId")
		return
	}
	if start < s.HistoryFloor {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	p, ok := readList(w, r)
	if !ok {
		return
	}
	var kinds []historyKind
	for _, t := range q["historyTypes"] {
		if k, ok := historyTypeNames[t]; ok {
			kinds = append(kinds, k)
		}
	}
	label := q.Get("labelId")
	var recs []historyRecord
	for _, h := range s.history {
		if h.id <= start {
			continue
		}
		if len(kinds) > 0 && !slices.Contains(kinds, h.kind) {
			continue
		}
		if label != "" && !slices.Contains(h.labels, label) && !slices.Contains(s.messages[h.message].labels, label) {
			continue
		}
		recs = append(recs, h)
	}
	p.max = min(p.max, 500)
	first, end, next, ok := s.page(w, listParams{max: p.max, token: p.token}, len(recs))
	if !ok {
		return
	}
	resp := gmail.ListHistoryResponse{HistoryID: strconv.FormatUint(s.historyID, 10), NextPageToken: next}
	for _, h := range recs[first:end] {
		resp.History = append(resp.History, s.historyWire(h))
	}
	writeJSON(w, resp)
}
