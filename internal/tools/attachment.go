package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mmedum/google-mail-mcp/internal/model"
	"github.com/mmedum/google-mail-mcp/internal/render"
	"github.com/mmedum/google-mail-mcp/internal/service"
)

// DownloadAttachmentIn names one attachment.
type DownloadAttachmentIn struct {
	MessageID string `json:"message_id" jsonschema:"message id, or rfc822: followed by a Message-ID header value"`
	PartID    string `json:"part_id" jsonschema:"the attachment's part_id as get_message lists it; \"\" is the message's own top-level part"`
}

// DownloadOut is the file written.
type DownloadOut struct {
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
	Rendered
	Cost
}

func registerAttachment(s *mcp.Server, d Deps) {
	svc := service.New(d.Client)

	register(s, d, Spec{Name: "download_attachment", Kind: ReadWritesLocally, Description: "Save one attachment " +
		"into the directory the person configured as GMAIL_LOCAL_DIR, and nowhere else. The file is named after the " +
		"attachment, made safe as a file name; an existing file is never overwritten, a number is added instead. The " +
		"content is written to disk, not returned: open the file only if the person asks. Take message_id and " +
		"part_id from get_message. About 40 units." + untrustedNote},
		func(ctx context.Context, in DownloadAttachmentIn) (DownloadOut, error) {
			dl, err := svc.DownloadAttachment(ctx, d.Config.LocalDir, in.MessageID, in.PartID)
			if err != nil {
				return DownloadOut{}, err
			}
			a := dl.Attachment
			saved := render.Saved{MessageID: dl.MessageID, PartID: a.PartID, Path: dl.Path, MimeType: a.MimeType,
				Suffixed: dl.Suffixed, Bytes: dl.Bytes, SHA256: dl.SHA256}
			if a.Renamed {
				saved.DeclaredName = a.DeclaredName
			}
			return DownloadOut{MessageID: dl.MessageID, PartID: a.PartID, UntrustedPath: model.Untrusted(dl.Path),
				UntrustedDeclaredName: model.Untrusted(saved.DeclaredName), UntrustedMimeType: model.Untrusted(a.MimeType),
				Bytes: dl.Bytes, SHA256: dl.SHA256, Suffixed: saved.Suffixed,
				Rendered: readOf(render.Download(saved, render.Options{}))}, nil
		})
}
