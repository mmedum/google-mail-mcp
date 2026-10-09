package render

// Saved is one attachment written to disk.
type Saved struct {
	MessageID string
	PartID    string
	// Path is where it was written. Its base name came from the sender.
	Path string
	// DeclaredName is the name the sender gave, when it was unsafe as a
	// file name and was changed.
	DeclaredName string
	// Suffixed is set when the name was taken and a number was added.
	Suffixed bool
	MimeType string
	Bytes    int64
	SHA256   string
}

// Download renders a written attachment. The file name is the
// sender's, so it is shown only inside a block; what the server did is
// said outside it.
func Download(s Saved, o Options) Result {
	return render(o, func(w *writer) Result {
		w.saved(s)
		return Result{Budget: o.budget()}
	})
}

// saved says what was written for one attachment.
func (w *writer) saved(s Saved) {
	w.say("wrote %s from part %s of message %s into GMAIL_LOCAL_DIR", size(s.Bytes), partIDs([]string{s.PartID}), gmailID(s.MessageID))
	w.say("sha256: %s", sha(s.SHA256))
	text := "Path: " + s.Path + "\nType: " + s.MimeType + "\n"
	if s.DeclaredName != "" {
		text += "Declared name: " + s.DeclaredName + "\n"
	}
	w.block("saved file", "", s.MessageID, text)
	if s.DeclaredName != "" {
		w.say("note: the declared name was unsafe as a file name, so the file was given a safe one.")
	}
	if s.Suffixed {
		w.say("note: a file of that name was already there and was left alone; a number was added to this one.")
	}
}

// MaxDownloads is the most attachments one download_attachments call
// writes, as a write names at most 100 items (§4.7).
const MaxDownloads = 100

// Reasons download_attachments skips an attachment when the caller
// names none.
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

// Skipped is an attachment download_attachments skipped, and why: one
// of the Skip* reasons.
type Skipped struct {
	PartID string
	Reason string
}

// Failed is an attachment that was not written, and why, as the [class]
// message of §6.5.
type Failed struct {
	PartID  string
	Class   string
	Message string
}

// Downloads is what one download_attachments call wrote, skipped and
// could not write.
type Downloads struct {
	MessageID string
	Files     []Saved
	Skipped   []Skipped
	Failed    []Failed
}

// DownloadsWritten renders a download_attachments result: each file as
// Download renders one, then what was skipped and what failed. Every
// file written stays written whatever failed after it.
func DownloadsWritten(d Downloads, o Options) Result {
	return render(o, func(w *writer) Result {
		w.say("message %s: saved %s, skipped %s, failed %s", gmailID(d.MessageID), num(len(d.Files)),
			num(len(d.Skipped)), num(len(d.Failed)))
		for _, s := range d.Files {
			w.saved(s)
		}
		for _, p := range d.Skipped {
			w.skipped(p)
		}
		for _, f := range d.Failed {
			w.say("failed part %s: %s", partIDs([]string{f.PartID}), failure(f.Class, f.Message))
		}
		if len(d.Failed) > 0 && len(d.Files) > 0 {
			w.say("note: the files saved before a failure are kept.")
		}
		return Result{Budget: o.budget()}
	})
}

// skipped says why an attachment was skipped.
func (w *writer) skipped(p Skipped) {
	id := partIDs([]string{p.PartID})
	switch p.Reason {
	case SkipInline:
		w.say("skipped part %s: it is shown in the body, such as an image; include_inline saves it", id)
	case SkipReaction:
		w.say("skipped part %s: it is an emoji reaction, not a file", id)
	case SkipInvitationCopy:
		w.say("skipped part %s: it is an invitation's calendar version of the body, and the message also carries the invitation as a file", id)
	case SkipLimit:
		w.say("skipped part %s: it is past the %s parts one call saves; name it in part_ids", id, num(MaxDownloads))
	default:
		w.say("skipped part %s", id)
	}
}
