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

// MaxDownloads is the most attachments one download_attachments call
// writes, as a write names at most 100 items (§4.7).
const MaxDownloads = 100

// Reasons an attachment is passed over when the caller names none.
const (
	// SkipInline is a part shown in the body, such as an image.
	SkipInline = "inline"
	// SkipReaction is an emoji reaction's part, which is not a file.
	SkipReaction = "reaction"
	// SkipInvitationCopy is an invitation's calendar version of the
	// body, when the message also carries the invitation as a file.
	SkipInvitationCopy = "invitation_copy"
	// SkipLimit is a part past the MaxDownloads one call writes.
	SkipLimit = "limit"
)

// Skipped is an attachment passed over, and why: one of the Skip*
// reasons.
type Skipped struct {
	PartID string
	Reason string
}

// Failed is an attachment that was not written, and why, as §6.5's
// class and message.
type Failed struct {
	PartID         string
	Class, Message string
}

// failed records why a part was not written. An error with no class is
// this server's own, and is said as unavailable without its text.
func failed(partID string, err error) Failed {
	f := Failed{PartID: partID, Class: string(gapi.ClassUnavailable), Message: "the attachment could not be written"}
	var e *gapi.Error
	if errors.As(err, &e) {
		f.Class, f.Message = string(e.Class), e.Message
	}
	return f
}

// Downloads is what one download_attachments call did with each part.
type Downloads struct {
	MessageID string
	Files     []Download
	Skipped   []Skipped
	Failed    []Failed
}

// DownloadAttachments writes several attachments of one message into dir
// (§7.3), one at a time, as DownloadAttachment writes one. partIDs names
// them; with none, every attachment is written but those Skipped names,
// and inline parts are written when includeInline is set. A part that
// fails is reported in Failed and leaves no file; the files written
// before it stay.
func (s *Service) DownloadAttachments(ctx context.Context, dir, messageID string, partIDs []string, includeInline bool) (Downloads, error) {
	if dir == "" {
		return Downloads{}, errNoLocalDir
	}
	if len(partIDs) > MaxDownloads {
		return Downloads{}, gapi.Errf(gapi.ClassInvalid, "part_ids names %d parts; one call saves at most %d", len(partIDs), MaxDownloads)
	}
	for i, p := range partIDs {
		if slices.Contains(partIDs[:i], p) {
			return Downloads{}, gapi.Errf(gapi.ClassInvalid, "part_ids names part %q twice", p)
		}
	}
	id, g, parsed, err := s.readParts(ctx, messageID)
	if err != nil {
		return Downloads{}, err
	}
	out := Downloads{MessageID: id}
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
		return Downloads{}, err
	}
	defer func() { _ = root.Close() }()
	for _, att := range chosen {
		dl, err := s.writeFile(ctx, root, dir, id, att, g.Payload)
		if err != nil {
			out.Failed = append(out.Failed, failed(att.PartID, err))
			continue
		}
		out.Files = append(out.Files, dl)
	}
	return out, nil
}

// chooseAttachments is what download_attachments writes when the caller
// names no part: every attachment but an emoji reaction, an invitation's
// calendar version of the body when the invitation is attached as a file
// too, an inline part unless includeInline, and any past MaxDownloads.
func chooseAttachments(as []mime.Attachment, includeInline bool) ([]mime.Attachment, []Skipped) {
	attachedInvitation := slices.ContainsFunc(as, func(a mime.Attachment) bool {
		return a.CalendarMethod != "" && !a.Alternative
	})
	var chosen []mime.Attachment
	var skipped []Skipped
	for _, a := range as {
		reason := ""
		switch {
		case strings.EqualFold(a.MimeType, model.ReactionType):
			reason = SkipReaction
		case a.CalendarMethod != "" && a.Alternative && attachedInvitation:
			reason = SkipInvitationCopy
		case a.Inline && !includeInline:
			reason = SkipInline
		case len(chosen) == MaxDownloads:
			reason = SkipLimit
		}
		if reason != "" {
			skipped = append(skipped, Skipped{PartID: a.PartID, Reason: reason})
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
	var data string
	if p := findPart(payload, att.PartID); p != nil && p.Body != nil {
		data = p.Body.Data
	}
	b, err := mime.DecodeBase64URL(data)
	if err != nil {
		return 0, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a part that is not base64url")
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
