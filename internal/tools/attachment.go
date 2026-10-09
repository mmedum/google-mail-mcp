package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/v2/internal/mime"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
	"github.com/mmedum/google-mail-mcp/v2/internal/render"
	"github.com/mmedum/google-mail-mcp/v2/internal/service"
)

// DownloadAttachmentIn names one attachment.
type DownloadAttachmentIn struct {
	MessageID string `json:"message_id" jsonschema:"message id, or rfc822: followed by a Message-ID header value"`
	PartID    string `json:"part_id" jsonschema:"the attachment's part_id as get_message lists it; \"\" is the message's own top-level part"`
}

// SavedFile is one attachment written to disk.
type SavedFile struct {
	MessageID string `json:"message_id"`
	PartID    string `json:"part_id"`
	// UntrustedPath is where the file was written. Its base name is the
	// sender's declared name made safe, so it is the sender's text.
	UntrustedPath model.Untrusted `json:"untrusted_path"`
	// UntrustedDeclaredName is the name the sender declared, set when it
	// was unsafe as a file name and the file got a safe one.
	UntrustedDeclaredName model.Untrusted `json:"untrusted_declared_name,omitempty"`
	UntrustedMimeType     model.Untrusted `json:"untrusted_mime_type"`
	Bytes                 int64           `json:"bytes"`
	SHA256                string          `json:"sha256"`
	// Suffixed is set when a file of that name was already there, left
	// alone, and a number was added to this one's name.
	Suffixed bool `json:"suffixed"`
}

// DownloadOut is the file written.
type DownloadOut struct {
	SavedFile
	Rendered
	Cost
}

// DownloadAttachmentsIn names the attachments of one message to save.
type DownloadAttachmentsIn struct {
	MessageID     string   `json:"message_id" jsonschema:"message id, or rfc822: followed by a Message-ID header value"`
	PartIDs       []string `json:"part_ids,omitempty" jsonschema:"the attachments' part_ids as get_message lists them, at most 100; left out, every attachment that is not inline, an emoji reaction or an invitation's calendar version of the body"`
	IncludeInline bool     `json:"include_inline,omitempty" jsonschema:"with part_ids left out, also save inline parts, such as images shown in the body"`
}

// PassedPart is an attachment download_attachments passed over.
type PassedPart struct {
	PartID string `json:"part_id"`
	Reason string `json:"reason" jsonschema:"inline (shown in the body; include_inline saves it), reaction (an emoji reaction), invitation_copy (an invitation's calendar version of the body, when the message also carries the invitation as a file) or limit (past the 100 parts one call saves)"`
}

// FailedPart is an attachment that was not saved.
type FailedPart struct {
	PartID string `json:"part_id"`
	Error  string `json:"error" jsonschema:"why it was not saved, as [class] message; no file of it is left"`
}

// DownloadsOut is what download_attachments did with each attachment.
type DownloadsOut struct {
	MessageID string       `json:"message_id"`
	Files     []SavedFile  `json:"files" jsonschema:"the files written, in the message's order; each stays written whatever failed after it"`
	Skipped   []PassedPart `json:"skipped"`
	Failed    []FailedPart `json:"failed"`
	Rendered
	Cost
}

// ReadAttachmentIn names one attachment to read as text.
type ReadAttachmentIn struct {
	MessageID   string `json:"message_id" jsonschema:"message id, or rfc822: followed by a Message-ID header value"`
	PartID      string `json:"part_id" jsonschema:"the attachment's part_id as get_message lists it"`
	Offset      int    `json:"offset,omitempty" jsonschema:"next_offset from the previous read, to continue a long attachment"`
	BudgetChars int    `json:"budget_chars,omitempty" jsonschema:"characters of text to return; default 24000, between 2000 and 100000"`
	ShowQuoted  bool   `json:"show_quoted,omitempty" jsonschema:"keep quoted replies and signatures in plain text, HTML and an attached message instead of collapsing them"`
}

// AttachmentOut is one attachment read as text.
type AttachmentOut struct {
	MessageID         string          `json:"message_id"`
	PartID            string          `json:"part_id"`
	UntrustedFilename model.Untrusted `json:"untrusted_filename"`
	UntrustedMimeType model.Untrusted `json:"untrusted_mime_type"`
	Bytes             int             `json:"bytes" jsonschema:"the size of the content read"`
	ReadAs            string          `json:"read_as" jsonschema:"text (plain text, CSV, Markdown or JSON, as written), html (converted to text), calendar (as written) or message (an attached email, read as get_message reads one)"`
	// Message is the attached message, when ReadAs is message.
	Message *MessageMeta `json:"message,omitempty" jsonschema:"the attached email, when read_as is message, as get_message gives one. It is not in the mailbox: it has no id, thread or labels, its date is its own Date header, and its attachments have an empty part_id, since they cannot be read or saved apart from it"`
	// HiddenCharsRemoved and LinkMismatches are about the text read and
	// the attachment's type; an attached message carries its own in
	// message.
	HiddenCharsRemoved int `json:"hidden_chars_removed,omitempty"`
	LinkMismatches     int `json:"link_mismatches,omitempty"`
	Rendered
	Cost
}

// downloadNote ends both download descriptions.
const downloadNote = " Files go into the directory the person configured as GMAIL_LOCAL_DIR, and nowhere else. Each is " +
	"named after the attachment, made safe as a file name; an existing file is never overwritten, a number is added " +
	"instead. The content is written to disk, not returned: open a file only if the person asks."

func registerAttachment(s *mcp.Server, d Deps) {
	svc := service.New(d.Client)

	register(s, d, Spec{Name: "read_attachment", Kind: Read, Description: "Read one attachment as text: plain text, " +
		"CSV, Markdown or JSON as written, HTML converted to text with nothing it links to fetched, a calendar file as " +
		"written, or an attached email, read as get_message reads one. Any other type is refused, and so is anything " +
		"over 5 MB; download_attachment saves those. A long attachment is cut at a paragraph and continued with offset. " +
		"Take message_id and part_id from get_message. About 20 units, and 20 more when Gmail stores the attachment " +
		"apart from the message." + untrustedNote},
		func(ctx context.Context, in ReadAttachmentIn) (AttachmentOut, error) {
			o, err := Shape{BudgetChars: in.BudgetChars, ShowQuoted: in.ShowQuoted}.options()
			if err != nil {
				return AttachmentOut{}, err
			}
			o.Offset = in.Offset
			a, err := svc.ReadAttachment(ctx, in.MessageID, in.PartID)
			if err != nil {
				return AttachmentOut{}, err
			}
			out := AttachmentOut{MessageID: a.MessageID, PartID: a.Attachment.PartID,
				UntrustedFilename: model.Untrusted(a.Attachment.Filename), UntrustedMimeType: model.Untrusted(a.Attachment.MimeType),
				Bytes: a.Bytes, ReadAs: a.As, HiddenCharsRemoved: a.Body.HiddenChars() + a.Attachment.Hidden, LinkMismatches: len(a.Body.Mismatches()),
				Rendered: readOf(render.Attachment(a, o))}
			if a.As == mime.ReadMessage {
				out.Message = ptr(messageMeta(a.Message))
			}
			return out, nil
		})

	register(s, d, Spec{Name: "download_attachment", Kind: ReadWritesLocally, Description: "Save one attachment." +
		downloadNote + " Take message_id and part_id from get_message. About 40 units." + untrustedNote},
		func(ctx context.Context, in DownloadAttachmentIn) (DownloadOut, error) {
			dl, err := svc.DownloadAttachment(ctx, d.Config.LocalDir, in.MessageID, in.PartID)
			if err != nil {
				return DownloadOut{}, err
			}
			return DownloadOut{SavedFile: savedFile(dl), Rendered: readOf(render.Download(saved(dl), render.Options{}))}, nil
		})

	register(s, d, Spec{Name: "download_attachments", Kind: ReadWritesLocally, Description: "Save several " +
		"attachments of one message: those part_ids names, or every attachment but inline parts, emoji reactions and " +
		"an invitation's calendar version of the body." + downloadNote + " Each attachment is saved or fails on its " +
		"own: the files saved before a failure are kept, and the result lists each file, each part passed over and " +
		"each failure. Take message_id and part_ids from get_message. About 20 units, and 20 more for each attachment " +
		"Gmail stores apart from the message." + untrustedNote},
		func(ctx context.Context, in DownloadAttachmentsIn) (DownloadsOut, error) {
			dls, err := svc.DownloadAttachments(ctx, d.Config.LocalDir, in.MessageID, in.PartIDs, in.IncludeInline)
			if err != nil {
				return DownloadsOut{}, err
			}
			out := DownloadsOut{MessageID: dls.MessageID, Files: mapSlice(dls.Files, savedFile),
				Skipped: []PassedPart{}, Failed: []FailedPart{}}
			r := render.Downloads{MessageID: dls.MessageID, Files: mapSlice(dls.Files, saved)}
			for _, p := range dls.Skipped {
				out.Skipped = append(out.Skipped, PassedPart(p))
				r.Passed = append(r.Passed, render.Passed(p))
			}
			for _, f := range dls.Failed {
				out.Failed = append(out.Failed, FailedPart{PartID: f.PartID, Error: "[" + f.Class + "] " + f.Message})
				r.NotSaved = append(r.NotSaved, render.NotSaved(f))
			}
			out.Rendered = readOf(render.DownloadsWritten(r, render.Options{}))
			return out, nil
		})
}

func savedFile(dl service.Download) SavedFile {
	s := saved(dl)
	return SavedFile{MessageID: s.MessageID, PartID: s.PartID, UntrustedPath: model.Untrusted(s.Path),
		UntrustedDeclaredName: model.Untrusted(s.DeclaredName), UntrustedMimeType: model.Untrusted(s.MimeType),
		Bytes: s.Bytes, SHA256: s.SHA256, Suffixed: s.Suffixed}
}

func saved(dl service.Download) render.Saved {
	a := dl.Attachment
	s := render.Saved{MessageID: dl.MessageID, PartID: a.PartID, Path: dl.Path, MimeType: a.MimeType,
		Suffixed: dl.Suffixed, Bytes: dl.Bytes, SHA256: dl.SHA256}
	if a.Renamed {
		s.DeclaredName = a.DeclaredName
	}
	return s
}
