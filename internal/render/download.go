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
		res := Result{Budget: o.budget()}
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
		return res
	})
}
