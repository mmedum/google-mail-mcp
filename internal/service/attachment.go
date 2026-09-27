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
	"strconv"
	"strings"

	"github.com/mmedum/google-mail-mcp/internal/fileperm"
	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gmail"
	"github.com/mmedum/google-mail-mcp/internal/mime"
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
		return Download{}, gapi.Errf(gapi.ClassBlocked, "GMAIL_LOCAL_DIR is not set, so this server writes no files")
	}
	id, err := s.messageID(ctx, messageID)
	if err != nil {
		return Download{}, err
	}
	g, err := s.client.GetMessage(ctx, id, gapi.FormatFull)
	if err != nil {
		return Download{}, err
	}
	parsed, err := mime.ParsePayload(g.Payload, nil)
	if err != nil {
		return Download{}, gapi.Wrap(gapi.ClassUnavailable, err, "Gmail returned a message this server could not read")
	}
	att, ok := findAttachment(parsed.Attachments, partID)
	if !ok {
		return Download{}, gapi.Errf(gapi.ClassNotFound,
			"the message has no attachment with part_id %q; get_message lists each attachment's part_id", partID)
	}
	if att.Size > gapi.MaxAttachmentBytes {
		// Refused before the read that would spend quota and fill the disk.
		return Download{}, gapi.Errf(gapi.ClassInvalid, "the attachment is %d MB, larger than the %d MB this server writes",
			att.Size>>20, gapi.MaxAttachmentBytes>>20)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return Download{}, gapi.Wrap(gapi.ClassUnavailable, err, "GMAIL_LOCAL_DIR could not be opened")
	}
	defer func() { _ = root.Close() }()
	f, name, suffixed, err := createUnique(root, att.Filename)
	if err != nil {
		return Download{}, err
	}
	out := Download{MessageID: id, Attachment: att, Path: filepath.Join(dir, name), Suffixed: suffixed}
	sum := sha256.New()
	n, werr := s.writePart(ctx, id, att, g.Payload, f, sum)
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
