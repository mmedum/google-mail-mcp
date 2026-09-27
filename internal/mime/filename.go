package mime

import (
	"path"
	"strings"
	"unicode/utf8"
)

// maxNameBytes keeps a base name well under every filesystem's limit
// of 255 bytes, with room for a numeric suffix on a clash.
const maxNameBytes = 200

var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// SafeBaseName reduces a declared attachment name to a base name safe to
// create in a directory: no directory part, no control, invisible or
// bidi-control characters (which is how "invoice\u202Etxt.exe" reads as
// a text file), no leading dots, no characters Windows refuses, no
// device name, and at most 200 bytes with the extension kept. It
// returns "" when nothing usable is left.
func SafeBaseName(name string) string {
	name, _ = StripInvisible(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		case r == '\\':
			return '/'
		case strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		}
		return r
	}, name)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(strings.TrimLeft(name, ". \t"), ". \t")
	if name == "" {
		return ""
	}
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if windowsReserved[strings.ToLower(strings.TrimSpace(stem))] {
		name = "_" + name
	}
	if len(name) > maxNameBytes {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := truncateUTF8(strings.TrimSuffix(name, ext), maxNameBytes-len(ext))
		name = stem + ext
	}
	return name
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var fallbackExt = map[string]string{
	"application/pdf":  ".pdf",
	"application/zip":  ".zip",
	"application/ics":  ".ics",
	"text/calendar":    ".ics",
	"text/plain":       ".txt",
	"text/html":        ".html",
	"text/csv":         ".csv",
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
	"message/rfc822":   ".eml",
	"application/json": ".json",
}

// fallbackName names an attachment that declared no usable name.
func fallbackName(mediatype string) string {
	if ext, ok := fallbackExt[mediatype]; ok {
		return "attachment" + ext
	}
	return "attachment.bin"
}
