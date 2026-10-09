package mime

import (
	"net/url"
	"strings"
)

// Unsubscribe is what a message's List-Unsubscribe header offers
// (RFC 2369 §3.2), each list in the header's order, which RFC 2369 §2
// makes the sender's order of preference. It is the sender's data: the
// server reads it and never visits or writes to an address in it.
type Unsubscribe struct {
	// URLs are the http and https addresses.
	URLs []string
	// Mailto are the mailto URIs, with any subject or body they carry.
	Mailto []string
	// OneClick is set when one of URLs is https and the message carries
	// one List-Unsubscribe-Post header reading exactly
	// "List-Unsubscribe=One-Click" (RFC 8058 §3.1). The sender declares
	// it; whether a DKIM signature covers the two headers is not checked.
	OneClick bool
}

// Limits a hostile header cannot exceed: a URI longer than maxUnsubURI
// is skipped, and each list keeps its first maxUnsubURIs.
const (
	maxUnsubURI  = 2048
	maxUnsubURIs = 4
)

// oneClickPost is the one value RFC 8058 §3.1 allows in
// List-Unsubscribe-Post.
const oneClickPost = "List-Unsubscribe=One-Click"

// parseUnsubscribe reads List-Unsubscribe and List-Unsubscribe-Post from
// a header block. A header given twice is read once, its first time,
// and makes one-click false: RFC 8058 asks for one of each.
func parseUnsubscribe(hs []Header) Unsubscribe {
	var lists, posts []string
	for _, h := range hs {
		switch {
		case strings.EqualFold(h.Name, "List-Unsubscribe"):
			lists = append(lists, h.Value)
		case strings.EqualFold(h.Name, "List-Unsubscribe-Post"):
			posts = append(posts, h.Value)
		}
	}
	if len(lists) == 0 {
		return Unsubscribe{}
	}
	u := parseListUnsubscribe(lists[0])
	if len(lists) == 1 && len(posts) == 1 && strings.TrimSpace(posts[0]) == oneClickPost {
		for _, l := range u.URLs {
			if strings.HasPrefix(strings.ToLower(l), "https:") {
				u.OneClick = true
				break
			}
		}
	}
	return u
}

// parseListUnsubscribe reads a List-Unsubscribe value as RFC 2369 §2
// tells a client to: angle-bracketed URIs separated by commas, comments
// and whitespace between them ignored, whitespace inside the brackets
// removed. The first item that is not an angle-bracketed URI ends the
// reading, and so does anything but a comma after one. Only http, https
// and mailto URIs are kept; OneClick is left false.
func parseListUnsubscribe(v string) Unsubscribe {
	var u Unsubscribe
	i := skipCFWS(v, 0)
	for i < len(v) && v[i] == '<' {
		end := strings.IndexByte(v[i+1:], '>')
		if end < 0 {
			break
		}
		u.add(strings.Join(strings.Fields(v[i+1:i+1+end]), ""))
		i = skipCFWS(v, i+end+2)
		if i >= len(v) || v[i] != ',' {
			break
		}
		i = skipCFWS(v, i+1)
	}
	return u
}

// add files one URI under its scheme, or drops it.
func (u *Unsubscribe) add(uri string) {
	if uri == "" || len(uri) > maxUnsubURI || !printableASCII(uri) {
		return
	}
	p, err := url.Parse(uri)
	if err != nil {
		return
	}
	switch strings.ToLower(p.Scheme) {
	case "http", "https":
		if p.Host != "" && len(u.URLs) < maxUnsubURIs {
			u.URLs = append(u.URLs, uri)
		}
	case "mailto":
		if p.Opaque != "" && len(u.Mailto) < maxUnsubURIs {
			u.Mailto = append(u.Mailto, uri)
		}
	}
}

// skipCFWS skips whitespace and RFC 5322 comments, which nest and may
// quote a character with a backslash, from byte i.
func skipCFWS(v string, i int) int {
	depth := 0
	for i < len(v) {
		c := v[i]
		switch {
		case c == '\\' && depth > 0:
			i++
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0 && c != ' ' && c != '\t' && c != '\r' && c != '\n':
			return i
		}
		i++
	}
	return i
}

// printableASCII reports whether s is visible ASCII only: a URI with a
// control or non-ASCII character in it is not one to show as a link.
func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}
