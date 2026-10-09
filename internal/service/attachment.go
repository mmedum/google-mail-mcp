package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/v2/internal/fileperm"
	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
)

// maxSuffix bounds the numbered names tried when a file name is taken.
const maxSuffix = 999

// Download is one attachment written to disk.
type Download struct {
	MessageID string
	// Attachment is the part as get_message lists it: its safe name, the
	// name the sender declared, its type and size.
	Attachment mime.Attachment
	// Path is the file written.
	Path string
	// Suffixed is set when Attachment.Filename was taken and a number
	// was added to the name written.
	Suffixed bool
	Bytes    int64
	SHA256   string
}

// DownloadAttachment writes one attachment of a message into dir (§7.3).
// It reads the message to find the part and its name, then streams the
// content into a new file: the name is the part's safe base name, a
// taken name gets a numeric suffix, and nothing is ever overwritten.
// Only a part the message lists as an attachment can be written.
func (s *Service) DownloadAttachment(ctx context.Context, dir, messageID, partID string) (Download, error) {
	if dir == "" {
		return Download{}, errNoLocalDir
	}
	id, g, parsed, err := s.readParts(ctx, messageID)
	if err != nil {
		return Download{}, err
	}
	att, ok := findAttachment(parsed.Attachments, partID)
	if !ok {
		return Download{}, noAttachment(partID)
	}
	root, err := openLocalDir(dir)
	if err != nil {
		return Download{}, err
	}
	defer func() { _ = root.Close() }()
	return s.writeFile(ctx, root, dir, id, att, g.Payload)
}

// Saved is the download as a result shows it.
func (d Download) Saved() render.Saved {
	a := d.Attachment
	s := render.Saved{MessageID: d.MessageID, PartID: a.PartID, Path: d.Path, MimeType: a.MimeType,
		Suffixed: d.Suffixed, Bytes: d.Bytes, SHA256: d.SHA256}
	if a.Renamed {
		s.DeclaredName = a.DeclaredName
	}
	return s
}

// failed records why a part was not written. An error with no class is
// this server's own, and is said as unavailable without its text.
func failed(partID string, err error) render.Failed {
	f := render.Failed{PartID: partID, Class: string(gapi.ClassUnavailable), Message: "the attachment could not be written"}
	var e *gapi.Error
	if errors.As(err, &e) {
		f.Class, f.Message = string(e.Class), e.Message
	}
	return f
}

// DownloadAttachments writes several attachments of one message into dir
// (§7.3), one at a time, as DownloadAttachment writes one. partIDs names
// them; with none, every attachment is written but those Skipped names,
// and inline parts are written when includeInline is set. A part that
// fails is reported in Failed and leaves no file; the files written
// before it stay.
func (s *Service) DownloadAttachments(ctx context.Context, dir, messageID string, partIDs []string, includeInline bool) (render.Downloads, error) {
	if dir == "" {
		return render.Downloads{}, errNoLocalDir
	}
	if len(partIDs) > render.MaxDownloads {
		return render.Downloads{}, gapi.Errf(gapi.ClassInvalid, "part_ids names %d parts; one call saves at most %d",
			len(partIDs), render.MaxDownloads)
	}
	for i, p := range partIDs {
		if slices.Contains(partIDs[:i], p) {
			return render.Downloads{}, gapi.Errf(gapi.ClassInvalid, "part_ids names part %q twice", p)
		}
	}
	id, g, parsed, err := s.readParts(ctx, messageID)
	if err != nil {
		return render.Downloads{}, err
	}
	out := render.Downloads{MessageID: id}
	var chosen []mime.Attachment
	if len(partIDs) > 0 {
		for _, p := range partIDs {
			att, ok := findAttachment(parsed.Attachments, p)
			if !ok {
				out.Failed = append(out.Failed, failed(p, noAttachment(p)))
				continue
			}
			chosen = append(chosen, att)
		}
	} else {
		chosen, out.Skipped = chooseAttachments(parsed.Attachments, includeInline)
	}
	if len(chosen) == 0 {
		return out, nil
	}
	root, err := openLocalDir(dir)
	if err != nil {
		return render.Downloads{}, err
	}
	defer func() { _ = root.Close() }()
	for _, att := range chosen {
		dl, err := s.writeFile(ctx, root, dir, id, att, g.Payload)
		if err != nil {
			out.Failed = append(out.Failed, failed(att.PartID, err))
			continue
		}
		out.Files = append(out.Files, dl.Saved())
	}
	return out, nil
}

// MaxReadBytes is the largest attachment read_attachment reads into a
// result (§7.3). A larger one is refused before it is read.
const MaxReadBytes = 5 << 20

// ReadAttachment reads one attachment of a message as text (§7.3): plain
// text, CSV, Markdown, JSON, HTML, a calendar or an attached message.
// Any other type, a part over MaxReadBytes and a part whose content Gmail
// did not give are refused before it is read.
func (s *Service) ReadAttachment(ctx context.Context, messageID, partID string) (render.AttachmentRead, error) {
	id, g, parsed, err := s.readParts(ctx, messageID)
	if err != nil {
		return render.AttachmentRead{}, err
	}
	att, ok := findAttachment(parsed.Attachments, partID)
	if !ok {
		return render.AttachmentRead{}, noAttachment(partID)
	}
	as := mime.ReadAs(att.MimeType)
	switch {
	case as == "":
		return render.AttachmentRead{}, gapi.Errf(gapi.ClassUnsupported, "part %q is not a type this server reads as "+
			"text: plain text, CSV, Markdown, JSON, HTML, a calendar or an attached message. download_attachment saves "+
			"it, when the person has set GMAIL_LOCAL_DIR", partID)
	case att.ContentMissing:
		return render.AttachmentRead{}, gapi.Errf(gapi.ClassUnsupported, "Gmail returned part %q as parts of its own, "+
			"with no content or attachment id for the part itself, so it cannot be read as one", partID)
	case att.Size > MaxReadBytes:
		return render.AttachmentRead{}, tooLargeToRead(att.Size)
	}
	data, err := s.partData(ctx, id, att, g.Payload)
	if err != nil {
		return render.AttachmentRead{}, err
	}
	if len(data) > MaxReadBytes {
		return render.AttachmentRead{}, tooLargeToRead(len(data))
	}
	out := render.AttachmentRead{MessageID: id, Attachment: att, As: as, Bytes: len(data)}
	if len(parsed.From) > 0 {
		out.From = parsed.From[0]
	}
	if as != mime.ReadMessage {
		// The attachment was found in this payload, so its part is there.
		out.Body = mime.PartText(findPart(g.Payload, att.PartID), data)
		return out, nil
	}
	out.Message = model.Message{Complete: true}
	if len(data) > 0 {
		out.Message = model.NewRawMessage(&gmail.Message{}, model.LabelIndex{}, data)
	}
	// Its parts are not in the mailbox, so no part id names them there.
	for i := range out.Message.Attachments {
		out.Message.Attachments[i].PartID, out.Message.Attachments[i].AttachmentID = "", ""
	}
	return out, nil
}

func tooLargeToRead(size int) error {
	return gapi.Errf(gapi.ClassInvalid, "the attachment is %.1f MB, more than the %d MB this server reads into a result; "+
		"download_attachment saves it, when the person has set GMAIL_LOCAL_DIR", float64(size)/(1<<20), MaxReadBytes>>20)
}

// partData is an attachment's content: fetched when Gmail stored it
// behind an attachment id, else decoded from the message.
func (s *Service) partData(ctx context.Context, messageID string, att mime.Attachment, payload *gmail.MessagePart) ([]byte, error) {
	if att.AttachmentID == "" {
		return inlineData(payload, att.PartID)
	}
	body, err := s.client.GetAttachment(ctx, messageID, att.AttachmentID)
	if err != nil {
		return nil, err
	}
	return decodePart(body.Data)
}

// inlineData is the content of a part Gmail kept in the message.
func inlineData(payload *gmail.MessagePart, partID string) ([]byte, error) {
	var data string
	if p := findPart(payload, partID); p != nil && p.Body != nil {
		data = p.Body.Data
	}
	return decodePart(data)
}

func decodePart(data string) ([]byte, error) {
	b, err := mime.DecodeBase64URL(data)
	if err != nil {
		return nil, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a part that is not base64url")
	}
	return b, nil
}

// chooseAttachments is what download_attachments writes when the caller
// names no part: every attachment but an emoji reaction, an invitation's
// calendar version of the body when the invitation is attached as a file
// too, an inline part unless includeInline, and any past MaxDownloads.
func chooseAttachments(as []mime.Attachment, includeInline bool) ([]mime.Attachment, []render.Skipped) {
	attachedInvitation := slices.ContainsFunc(as, func(a mime.Attachment) bool {
		return a.CalendarMethod != "" && !a.Alternative
	})
	var chosen []mime.Attachment
	var skipped []render.Skipped
	for _, a := range as {
		reason := ""
		switch {
		case model.IsReactionPart(a):
			reason = render.SkipReaction
		case a.CalendarMethod != "" && a.Alternative && attachedInvitation:
			reason = render.SkipInvitationCopy
		case a.Inline && !includeInline:
			reason = render.SkipInline
		case len(chosen) == render.MaxDownloads:
			reason = render.SkipLimit
		}
		if reason != "" {
			skipped = append(skipped, render.Skipped{PartID: a.PartID, Reason: reason})
			continue
		}
		chosen = append(chosen, a)
	}
	return chosen, skipped
}

// readParts resolves a message id and reads the message whole, with its
// parts parsed.
func (s *Service) readParts(ctx context.Context, messageID string) (string, *gmail.Message, *mime.Message, error) {
	id, err := s.messageID(ctx, messageID)
	if err != nil {
		return "", nil, nil, err
	}
	g, err := s.client.GetMessage(ctx, id, gapi.FormatFull)
	if err != nil {
		return "", nil, nil, err
	}
	parsed, err := mime.ParsePayload(g.Payload, nil)
	if err != nil {
		return "", nil, nil, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a message this server could not read")
	}
	return id, g, parsed, nil
}

// errNoLocalDir refuses a download when the person set no directory.
var errNoLocalDir = gapi.Errf(gapi.ClassBlocked, "GMAIL_LOCAL_DIR is not set, so this server writes no files")

func noAttachment(partID string) error {
	return gapi.Errf(gapi.ClassNotFound,
		"the message has no attachment with part_id %q; get_message lists each attachment's part_id", partID)
}

// writable refuses an attachment that cannot be written whole: one over
// the size cap, and one whose content Gmail did not give.
func writable(att mime.Attachment) error {
	if att.ContentMissing {
		return gapi.Errf(gapi.ClassUnsupported, "Gmail returned part %q as parts of its own, with no content or "+
			"attachment id for the part itself, so it cannot be saved as one file", att.PartID)
	}
	if att.Size > gapi.MaxAttachmentBytes {
		// Refused before the read that would spend quota and fill the disk.
		return gapi.Errf(gapi.ClassInvalid, "the attachment is %d MB, larger than the %d MB this server writes",
			att.Size>>20, gapi.MaxAttachmentBytes>>20)
	}
	return nil
}

func openLocalDir(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, gapi.Wrap(gapi.ClassUnavailable, err, "GMAIL_LOCAL_DIR could not be opened")
	}
	return root, nil
}

// writeFile writes one attachment into a new file in root, which is
// dir, and removes the file if the write fails. An attachment that
// cannot be written whole is refused before anything is read or created.
func (s *Service) writeFile(ctx context.Context, root *os.Root, dir, messageID string, att mime.Attachment,
	payload *gmail.MessagePart,
) (Download, error) {
	if err := writable(att); err != nil {
		return Download{}, err
	}
	f, name, suffixed, err := createUnique(root, att.Filename)
	if err != nil {
		return Download{}, err
	}
	out := Download{MessageID: messageID, Attachment: att, Path: filepath.Join(dir, name), Suffixed: suffixed}
	sum := sha256.New()
	n, werr := s.writePart(ctx, messageID, att, payload, f, sum)
	if cerr := f.Close(); werr == nil && cerr != nil {
		werr = writeFailed(cerr)
	}
	if werr == nil {
		if perr := fileperm.RestrictToOwner(out.Path); perr != nil {
			werr = gapi.Wrap(gapi.ClassUnavailable, perr, "the file was written but could not be restricted to its owner")
		}
	}
	if werr != nil {
		// A partial or unprotected file is removed: the caller is told it
		// failed, so no file of its should be left to be taken as whole.
		_ = root.Remove(name)
		return Download{}, werr
	}
	out.Bytes, out.SHA256 = n, hex.EncodeToString(sum.Sum(nil))
	return out, nil
}

// messageID resolves an id given as "rfc822:<Message-ID>" (§6.1).
func (s *Service) messageID(ctx context.Context, id string) (string, error) {
	if rest, ok := strings.CutPrefix(id, rfc822Prefix); ok {
		return s.byRFC822ID(ctx, rest)
	}
	return id, nil
}

// writeFailed is a failure to write into GMAIL_LOCAL_DIR. It is final:
// a full disk does not clear by reading Gmail again.
func writeFailed(err error) *gapi.Error {
	return gapi.Wrap(gapi.ClassUnavailable, err, "the attachment could not be written to the local directory")
}

// localFile is the file a download writes, its failures classified as
// this server's own rather than Gmail's.
type localFile struct{ w io.Writer }

func (l localFile) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if err != nil {
		return n, writeFailed(err)
	}
	return n, nil
}

// writePart writes an attachment's content to f: streamed from Gmail
// when it is stored behind an attachment id, or decoded from the message
// when Gmail inlined it. Each retried read starts f and the hash over.
func (s *Service) writePart(ctx context.Context, messageID string, att mime.Attachment, payload *gmail.MessagePart,
	f *os.File, sum hash.Hash,
) (int64, error) {
	reset := func() (io.Writer, error) {
		sum.Reset()
		if err := f.Truncate(0); err != nil {
			return nil, writeFailed(err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, writeFailed(err)
		}
		return localFile{io.MultiWriter(f, sum)}, nil
	}
	if att.AttachmentID != "" {
		return s.client.DownloadAttachment(ctx, messageID, att.AttachmentID, reset)
	}
	b, err := inlineData(payload, att.PartID)
	if err != nil {
		return 0, err
	}
	w, err := reset()
	if err != nil {
		return 0, err
	}
	n, err := w.Write(b)
	return int64(n), err
}

func findAttachment(as []mime.Attachment, partID string) (mime.Attachment, bool) {
	for _, a := range as {
		if a.PartID == partID {
			return a, true
		}
	}
	return mime.Attachment{}, false
}

func findPart(p *gmail.MessagePart, partID string) *gmail.MessagePart {
	if p == nil {
		return nil
	}
	if p.PartID == partID {
		return p
	}
	for i := range p.Parts {
		if found := findPart(&p.Parts[i], partID); found != nil {
			return found
		}
	}
	return nil
}

// createUnique creates name in root, or name-1, name-2… before its
// extension when it is taken. O_EXCL makes the check and the creation
// one step, so a file that appears in between is never overwritten, and
// the root keeps a symlink from leading the write out of the directory.
func createUnique(root *os.Root, name string) (f *os.File, created string, suffixed bool, err error) {
	ext := path.Ext(name)
	if len(ext) > 20 || ext == name {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i <= maxSuffix; i++ {
		candidate := name
		if i > 0 {
			candidate = stem + "-" + strconv.Itoa(i) + ext
		}
		f, err := root.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, candidate, i > 0, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", false, gapi.Wrap(gapi.ClassUnavailable, err, "a file could not be created in GMAIL_LOCAL_DIR")
		}
	}
	return nil, "", false, gapi.Errf(gapi.ClassConflict,
		"GMAIL_LOCAL_DIR already holds this file name and %d numbered copies of it; move some out", maxSuffix)
}
