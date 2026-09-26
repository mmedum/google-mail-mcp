package gmailtest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	stdmime "mime"
	stdmultipart "mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mmedum/google-mail-mcp/internal/gmail"
)

// The write handlers model what the server's logic depends on (§13):
// threading by the three conditions of §2.5, a draft's message replaced
// with a new id on every update (spike A), SENT and DRAFT refused by
// hand, labels that differ only in case refused (spike I), permanent
// deletion refused without https://mail.google.com/ (spike G), and
// every change recorded in history. They run with s.mu held.

// readDraft reads a drafts.create or drafts.update body: JSON with the
// message's raw bytes, or a multipart upload with JSON metadata and the
// message beside it.
func readDraft(w http.ResponseWriter, r *http.Request) (threadID string, raw []byte, ok bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Unreadable body")
		return "", nil, false
	}
	var d gmail.Draft
	mt, params, _ := stdmime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/related" {
		if r.URL.Query().Get("uploadType") != "multipart" {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Upload without uploadType=multipart")
			return "", nil, false
		}
		meta, media, err := splitRelated(body, params["boundary"])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Bad multipart upload")
			return "", nil, false
		}
		body, raw = meta, media
	}
	if err := json.Unmarshal(body, &d); err != nil || d.Message == nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Missing draft message")
		return "", nil, false
	}
	if raw == nil {
		if raw, err = base64.URLEncoding.DecodeString(d.Message.Raw); err != nil || len(raw) == 0 {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid raw")
			return "", nil, false
		}
	}
	return d.Message.ThreadID, raw, true
}

// splitRelated reads a two-part multipart/related upload: JSON metadata,
// then the message.
func splitRelated(body []byte, boundary string) (meta, media []byte, err error) {
	mr := stdmultipart.NewReader(bytes.NewReader(body), boundary)
	var parts [][]byte
	var types []string
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, nil, err
		}
		parts, types = append(parts, b), append(types, p.Header.Get("Content-Type"))
	}
	if len(parts) != 2 || !strings.HasPrefix(types[0], "application/json") || types[1] != "message/rfc822" {
		return nil, nil, errors.New("not JSON metadata and a message/rfc822 part")
	}
	return parts[0], parts[1], nil
}

var rePrefix = regexp.MustCompile(`(?i)^\s*((re|fwd?|aw)\s*:\s*)+`)

// sameSubject compares subjects as Gmail's threading does: prefixes
// such as "Re:" do not count.
func sameSubject(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(rePrefix.ReplaceAllString(s, ""))) }
	return norm(a) == norm(b)
}

// threadFor decides which thread a draft joins. It joins threadID only
// when all three of §2.5's conditions hold: the thread is named, the
// draft's In-Reply-To or References names a message in it, and the
// subjects match. Otherwise it starts its own thread.
func (s *Server) threadFor(threadID string, p parsedRaw, own string) string {
	ids, ok := s.threads[threadID]
	if threadID == "" || !ok {
		return own
	}
	refs := headerValue(p.headers, "In-Reply-To") + " " + headerValue(p.headers, "References")
	linked := false
	for _, id := range ids {
		if m := s.messages[id]; m != nil && strings.Contains(refs, m.rfc822ID) {
			linked = true
		}
	}
	first := s.messages[ids[0]]
	if !linked || !sameSubject(decodedHeader(p.headers, "Subject"), first.subject) {
		return own
	}
	return threadID
}

// storeRaw adds a message the server wrote, as Gmail stores it: the
// bytes as received, the payload read from them.
func (s *Server) storeRaw(raw []byte, threadID string, labels []string) (*message, error) {
	p, err := parseRaw(raw)
	if err != nil {
		return nil, err
	}
	id := s.nextID()
	return s.storeParsed(raw, p, id, s.threadFor(threadID, p, id), labels), nil
}

// storeParsed adds a message already parsed, in the thread given.
func (s *Server) storeParsed(raw []byte, p parsedRaw, id, thread string, labels []string) *message {
	s.clock = s.clock.Add(time.Minute)
	m := &message{
		id: id, threadID: thread, labels: labels, internalDate: s.clock, raw: raw,
		rfc822ID:      headerValue(p.headers, "Message-ID"),
		from:          strings.ToLower(decodedHeader(p.headers, "From")),
		to:            strings.ToLower(decodedHeader(p.headers, "To") + " " + decodedHeader(p.headers, "Cc")),
		subject:       strings.ToLower(decodedHeader(p.headers, "Subject")),
		text:          strings.ToLower(p.text),
		snippet:       snippet(p.text),
		hasAttachment: hasFilename(p.body),
	}
	s.insert(m, p.body, p.headers, labels)
	return m
}

// replaceMessageID puts Gmail's own Message-ID on a created draft, as a
// live run found it does (§15, spike B): the client's is not kept.
func replaceMessageID(raw []byte, id string) []byte {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		return raw
	}
	lines := strings.Split(string(raw[:end]), "\r\n")
	out := lines[:0]
	for i := 0; i < len(lines); i++ {
		name, _, _ := strings.Cut(lines[i], ":")
		if strings.EqualFold(name, "Message-ID") {
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t")) {
				i++
			}
			continue
		}
		out = append(out, lines[i])
	}
	out = append(out, "Message-Id: "+id)
	return append([]byte(strings.Join(out, "\r\n")), raw[end:]...)
}

// drop removes a message and records its deletion.
func (s *Server) drop(id string) {
	m := s.messages[id]
	delete(s.messages, id)
	ids := s.threads[m.threadID]
	if i := slices.Index(ids, id); i >= 0 {
		ids = slices.Delete(ids, i, i+1)
	}
	if len(ids) == 0 {
		delete(s.threads, m.threadID)
	} else {
		s.threads[m.threadID] = ids
	}
	s.historyID++
	s.history = append(s.history, historyRecord{id: s.historyID, kind: kindDeleted, message: id, thread: m.threadID})
}

func (s *Server) draftOut(w http.ResponseWriter, draftID string, m *message) {
	writeJSON(w, gmail.Draft{ID: draftID, Message: &gmail.Message{ID: m.id, ThreadID: m.threadID, LabelIDs: slices.Clone(m.labels)}})
}

func (s *Server) createDraft(w http.ResponseWriter, r *http.Request, _ []string) {
	threadID, raw, ok := readDraft(w, r)
	if !ok {
		return
	}
	if threadID != "" && s.threads[threadID] == nil {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	m, err := s.storeRaw(replaceMessageID(raw, "<draft."+strconv.FormatUint(s.counter+1, 16)+"@mail.example.com>"),
		threadID, []string{"DRAFT"})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid raw message")
		return
	}
	id := "r" + s.nextID()
	s.drafts[id] = m.id
	s.draftOut(w, id, m)
}

func (s *Server) updateDraft(w http.ResponseWriter, r *http.Request, args []string) {
	old, found := s.drafts[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	threadID, raw, ok := readDraft(w, r)
	if !ok {
		return
	}
	m, err := s.storeRaw(raw, threadID, []string{"DRAFT"})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid raw message")
		return
	}
	s.drop(old)
	s.drafts[args[0]] = m.id
	s.draftOut(w, args[0], m)
}

func (s *Server) deleteDraft(w http.ResponseWriter, _ *http.Request, args []string) {
	mid, found := s.drafts[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	delete(s.drafts, args[0])
	s.drop(mid)
	w.WriteHeader(http.StatusNoContent)
}

// modifyBody reads addLabelIds and removeLabelIds and checks them the way
// Gmail refuses: an unknown label, or SENT or DRAFT by hand (§2.12).
func (s *Server) modifyBody(w http.ResponseWriter, r *http.Request) (add, remove []string, ok bool) {
	var req gmail.ModifyMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid JSON")
		return nil, nil, false
	}
	for _, l := range append(slices.Clone(req.AddLabelIDs), req.RemoveLabelIDs...) {
		if _, known := s.labels[l]; !known || l == "SENT" || l == "DRAFT" {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid label: "+l)
			return nil, nil, false
		}
	}
	return req.AddLabelIDs, req.RemoveLabelIDs, true
}

func (s *Server) minimal(m *message) gmail.Message { return s.render(m, "minimal", nil) }

func (s *Server) threadOut(w http.ResponseWriter, id string) {
	t := gmail.Thread{ID: id, HistoryID: s.threadHistory(id)}
	for _, mid := range s.threads[id] {
		t.Messages = append(t.Messages, s.minimal(s.messages[mid]))
	}
	writeJSON(w, t)
}

func (s *Server) modifyMessage(w http.ResponseWriter, r *http.Request, args []string) {
	m, found := s.messages[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	add, remove, ok := s.modifyBody(w, r)
	if !ok {
		return
	}
	if slices.Contains(m.labels, "DRAFT") {
		writeError(w, http.StatusBadRequest, "failedPrecondition", "A draft cannot be labeled.")
		return
	}
	s.relabel(m.id, add, remove)
	writeJSON(w, s.minimal(m))
}

func (s *Server) modifyThread(w http.ResponseWriter, r *http.Request, args []string) {
	ids, found := s.threads[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	add, remove, ok := s.modifyBody(w, r)
	if !ok {
		return
	}
	for _, id := range ids {
		if !slices.Contains(s.messages[id].labels, "DRAFT") {
			s.relabel(id, add, remove)
		}
	}
	s.threadOut(w, args[0])
}

func (s *Server) trashMessage(w http.ResponseWriter, _ *http.Request, args []string) {
	s.moveMessage(w, args[0], []string{"TRASH"}, nil)
}

func (s *Server) untrashMessage(w http.ResponseWriter, _ *http.Request, args []string) {
	s.moveMessage(w, args[0], nil, []string{"TRASH"})
}

func (s *Server) moveMessage(w http.ResponseWriter, id string, add, remove []string) {
	m, found := s.messages[id]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	s.relabel(id, add, remove)
	writeJSON(w, s.minimal(m))
}

func (s *Server) trashThread(w http.ResponseWriter, _ *http.Request, args []string) {
	s.moveThread(w, args[0], []string{"TRASH"}, nil)
}

func (s *Server) untrashThread(w http.ResponseWriter, _ *http.Request, args []string) {
	s.moveThread(w, args[0], nil, []string{"TRASH"})
}

func (s *Server) moveThread(w http.ResponseWriter, id string, add, remove []string) {
	ids, found := s.threads[id]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	for _, mid := range ids {
		s.relabel(mid, add, remove)
	}
	s.threadOut(w, id)
}

// labelClash answers a name Gmail refuses (spike I): a system label's
// name in any case is invalid, and a user label's in any case is taken.
func (s *Server) labelClash(w http.ResponseWriter, name, except string) bool {
	for _, l := range s.labels {
		if l.ID == except || !strings.EqualFold(l.Name, name) {
			continue
		}
		if l.Type == gmail.LabelTypeSystem {
			writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid label name")
		} else {
			writeError(w, http.StatusConflict, "aborted", "Label name exists or conflicts")
		}
		return true
	}
	return false
}

var colorShape = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// applyLabel copies the fields a create or patch sets, refusing values
// Gmail refuses.
func applyLabel(w http.ResponseWriter, dst *gmail.Label, src gmail.Label) bool {
	bad := func(msg string) bool {
		writeError(w, http.StatusBadRequest, "invalidArgument", msg)
		return false
	}
	if src.Name != "" {
		dst.Name = strings.TrimSpace(src.Name)
	}
	switch src.LabelListVisibility {
	case "":
	case "labelShow", "labelShowIfUnread", "labelHide":
		dst.LabelListVisibility = src.LabelListVisibility
	default:
		return bad("Invalid labelListVisibility")
	}
	switch src.MessageListVisibility {
	case "":
	case "show", "hide":
		dst.MessageListVisibility = src.MessageListVisibility
	default:
		return bad("Invalid messageListVisibility")
	}
	if c := src.Color; c != nil {
		if !colorShape.MatchString(c.TextColor) || !colorShape.MatchString(c.BackgroundColor) {
			return bad("Invalid color")
		}
		cc := *c
		dst.Color = &cc
	}
	return true
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request, _ []string) {
	var in gmail.Label
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid label name")
		return
	}
	if s.labelClash(w, strings.TrimSpace(in.Name), "") {
		return
	}
	l := &gmail.Label{ID: "Label_" + strconv.Itoa(s.nextLabel()), Type: gmail.LabelTypeUser,
		LabelListVisibility: "labelShow", MessageListVisibility: "show"}
	if !applyLabel(w, l, in) {
		return
	}
	s.labels[l.ID] = l
	writeJSON(w, l)
}

// nextLabel is the next user label number.
func (s *Server) nextLabel() int {
	n := 0
	for id := range s.labels {
		if v, ok := strings.CutPrefix(id, "Label_"); ok {
			if k, err := strconv.Atoi(v); err == nil {
				n = max(n, k)
			}
		}
	}
	return n + 1
}

func (s *Server) patchLabel(w http.ResponseWriter, r *http.Request, args []string) {
	l, found := s.labels[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	var in gmail.Label
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid JSON")
		return
	}
	if l.Type == gmail.LabelTypeSystem {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid label: system labels cannot be changed")
		return
	}
	if in.Name != "" && s.labelClash(w, strings.TrimSpace(in.Name), l.ID) {
		return
	}
	next := *l
	if !applyLabel(w, &next, in) {
		return
	}
	*l = next
	writeJSON(w, l)
}

// sendDraft sends a draft as Gmail does (spikes B and C): the draft is
// gone, and its message is filed in SENT in the draft's thread, under a
// new id and a new Message-ID. A draft with no recipient is refused, as
// Gmail refuses it.
func (s *Server) sendDraft(w http.ResponseWriter, r *http.Request, _ []string) {
	var d gmail.Draft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.ID == "" {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Missing draft id")
		return
	}
	mid, found := s.drafts[d.ID]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	old := s.messages[mid]
	// Gmail replaces the draft's Message-ID on the sent copy (spike B).
	raw := replaceMessageID(old.raw, "<sent."+strconv.FormatUint(s.counter+1, 16)+"@mail.example.com>")
	p, err := parseRaw(raw)
	if err != nil || strings.TrimSpace(headerValue(p.headers, "To")+headerValue(p.headers, "Cc")+headerValue(p.headers, "Bcc")) == "" {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Recipient address required")
		return
	}
	sent := s.storeParsed(raw, p, s.nextID(), old.threadID, []string{"SENT"})
	delete(s.drafts, d.ID)
	s.drop(mid)
	writeJSON(w, gmail.Message{ID: sent.id, ThreadID: sent.threadID, LabelIDs: slices.Clone(sent.labels)})
}

// needsFull refuses a permanent delete when the fake's token does not
// hold https://mail.google.com/, as Gmail answers under gmail.modify
// (spike G).
func (s *Server) needsFull(w http.ResponseWriter) bool {
	if s.FullScope {
		return false
	}
	writeError(w, http.StatusForbidden, "insufficientPermissions", "Request had insufficient authentication scopes.")
	return true
}

func (s *Server) deleteMessage(w http.ResponseWriter, _ *http.Request, args []string) {
	_, found := s.messages[args[0]]
	s.deleteAll(w, []string{args[0]}, found)
}

func (s *Server) deleteThread(w http.ResponseWriter, _ *http.Request, args []string) {
	ids, found := s.threads[args[0]]
	s.deleteAll(w, ids, found)
}

// deleteAll deletes messages for good, with the drafts they hold.
func (s *Server) deleteAll(w http.ResponseWriter, ids []string, found bool) {
	if s.needsFull(w) {
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	for _, id := range slices.Clone(ids) {
		s.forgetDraftOf(id)
		s.drop(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// forgetDraftOf removes the draft whose message is id, if any: deleting
// a draft's message deletes the draft.
func (s *Server) forgetDraftOf(id string) {
	for d, mid := range s.drafts {
		if mid == id {
			delete(s.drafts, d)
		}
	}
}

// deleteLabel removes a user label and takes it off every message.
func (s *Server) deleteLabel(w http.ResponseWriter, _ *http.Request, args []string) {
	l, found := s.labels[args[0]]
	if !found {
		writeError(w, http.StatusNotFound, "notFound", "Requested entity was not found.")
		return
	}
	if l.Type == gmail.LabelTypeSystem {
		writeError(w, http.StatusBadRequest, "invalidArgument", "Invalid delete request")
		return
	}
	for id, m := range s.messages {
		if slices.Contains(m.labels, l.ID) {
			s.relabel(id, nil, []string{l.ID})
		}
	}
	delete(s.labels, l.ID)
	w.WriteHeader(http.StatusNoContent)
}
