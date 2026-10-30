package mime

import (
	"errors"
	"html"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	sigAda   = `<div>Ada Quill<br>Example Org</div>`
	sigLogo  = `<img src="https://example.com/logo.png" alt="">`
	endAda   = `<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br><div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><div>Ada Quill<br>Example Org</div></div>`
	aboveAda = `<div><br clear="all"></div><div><div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><div>Ada Quill<br>Example Org</div></div></div>`
	endLogo  = `<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br><div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature"><img src="https://example.com/logo.png" alt=""></div>`
)

// A signature goes below the body as Gmail's composer writes one: ending
// the message after the delimiter, or above a quote without it; in HTML
// given, before its close; and never below a signature already there.
func TestBodies(t *testing.T) {
	cases := []struct {
		name, text string
		h          *string
		sig, quote string
		plain, htm string
		err        error
	}{
		{"no signature", "Hi.", nil, "", "", "Hi.", "<p>Hi.</p>\n", nil},
		{"ending", "Hi.", nil, sigAda, "", "Hi.\n\n-- \nAda Quill\nExample Org\n", "<p>Hi.</p>\n" + endAda, nil},
		{"above a quote", "Hi.\n", nil, sigAda, "> q\n", "Hi.\n\nAda Quill\nExample Org\n\n> q\n",
			"<p>Hi.</p>\n" + aboveAda + "<p>&gt; q</p>\n", nil},
		{"images alone, ending", "Hi.", nil, sigLogo, "", "Hi.\n\n-- \n", "<p>Hi.</p>\n" + endLogo, nil},
		{"images alone, above a quote", "Hi.", nil, sigLogo, "> q", "Hi.\n\n> q",
			"<p>Hi.</p>\n" + sigAbove.lead + sigAbove.open + sigLogo + sigAbove.close + "<p>&gt; q</p>\n", nil},
		{"an empty body", "", nil, sigAda, "", "-- \nAda Quill\nExample Org\n", endAda, nil},
		{"hidden text left out of the text", "Hi.", nil, `<div>Ada<span style="display:none">CANARY</span></div>`, "",
			"Hi.\n\n-- \nAda\n", "<p>Hi.</p>\n" + sigEnding.lead + sigEnding.open +
				`<div>Ada<span style="display:none">CANARY</span></div>` + sigEnding.close, nil},
		{"HTML given, before its close", "Hi.", ptrTo("<html><body><p>Hi.</p></BODY></html>"), sigAda, "",
			"Hi.\n\n-- \nAda Quill\nExample Org\n", "<html><body><p>Hi.</p>" + endAda + "</BODY></html>", nil},
		{"HTML given that holds the composer's signature", "Hi.",
			ptrTo(`<p>Hi.</p><div class="gmail_signature" data-smartmail="gmail_signature">Ada</div>`), sigAda, "", "", "", ErrSignedHTML},
		{"HTML given that holds one, with none to add", "Hi.", ptrTo(`<div data-smartmail="gmail_signature">Ada</div>`), "", "",
			"Hi.", `<div data-smartmail="gmail_signature">Ada</div>`, nil},
		{"HTML given that quotes another's signature", "Hi.",
			ptrTo(`<p>Hi.</p><div class="gmail_quote"><div class="gmail_signature">Bo</div></div>`), sigAda, "",
			"Hi.\n\n-- \nAda Quill\nExample Org\n",
			`<p>Hi.</p><div class="gmail_quote"><div class="gmail_signature">Bo</div></div>` + endAda, nil},
		{"text that ends with the signature's", "Hi.\n\nAda Quill\nExample Org  \n\n", nil, sigAda, "", "", "", ErrSignedText},
		{"text that holds it above a quote", "Hi.\n\nAda Quill\nExample Org\n\nOn Mon, Bo wrote:\n> x", nil, sigAda, "",
			"", "", ErrSignedText},
		{"text that holds it with plain spaces", "Hi.\n\nAda Quill\nExample Org", nil, `<div>Ada&nbsp;Quill<br>Example&nbsp;Org</div>`,
			"", "", "", ErrSignedText},
		{"a one-line signature a body signs off with", "Thanks,\nAda", nil, `<div>Ada</div>`, "",
			"Thanks,\nAda\n\n-- \nAda\n", "<p>Thanks,<br>\nAda</p>\n" + sigEnding.lead + sigEnding.open + `<div>Ada</div>` + sigEnding.close, nil},
		{"text that ends with it after a delimiter", "Hi.\n-- \nAda Quill\nExample Org", nil, sigAda, "", "", "", ErrSignedText},
		{"text that ends with it, inside a line", "Hi, Ada Quill\nExample Org", nil, sigAda, "",
			"Hi, Ada Quill\nExample Org\n\n-- \nAda Quill\nExample Org\n",
			"<p>Hi, Ada Quill<br>\nExample Org</p>\n" + endAda, nil},
	}
	for _, c := range cases {
		out, err := Bodies(c.text, c.h, c.sig, c.quote)
		if !errors.Is(err, c.err) || out.Text != c.plain || out.HTML != c.htm || len(out.Inline) != 0 {
			t.Errorf("%s: %q %q %d images %v; want %q %q %v", c.name, out.Text, out.HTML, len(out.Inline), err, c.plain, c.htm, c.err)
		}
	}
}

// An image a signature carries inside its HTML goes as a part of its
// own, named by a Content-ID made from the image, once however often it
// shows; anything else stays as written.
func TestBodiesMoveImagesIntoParts(t *testing.T) {
	sig := `<div>Ada<img src="data:image/png;base64,UE5HREFUQQ==" alt="logo">` +
		`<IMG SRC='data:image/jpg;base64,T1RI&#10;RVJQTkc='><img src="data:image/png;base64,UE5H REFUQQ=="></div>` +
		`<img src="https://example.com/remote.png"><img src="data:image/svg+xml;base64,PHN2Zy8+">` +
		`<img src="data:image/png;base64,!!!"><img data-src="data:image/png;base64,UE5HREFUQQ==" src="https://example.com/x.png">` +
		`<img src="data:image/png;name=logo.png;base64,TkFNRURQTkc=">`
	out, err := Bodies("Hi.", nil, sig, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "<p>Hi.</p>\n" + sigEnding.lead + sigEnding.open +
		`<div>Ada<img src="cid:sig.2d4566582844690f@signature" alt="logo">` +
		`<IMG SRC='data:image/jpg;base64,T1RI&#10;RVJQTkc='><img src="cid:sig.2d4566582844690f@signature"></div>` +
		`<img src="https://example.com/remote.png"><img src="cid:sig.d4dc56669143034f@signature">` +
		`<img src="data:image/png;base64,!!!"><img data-src="data:image/png;base64,UE5HREFUQQ==" src="https://example.com/x.png">` +
		`<img src="cid:sig.cace2150934eb976@signature">` + sigEnding.close
	if out.HTML != want {
		t.Errorf("HTML %q, want %q", out.HTML, want)
	}
	var got []string
	for _, img := range out.Inline {
		got = append(got, img.ContentID+" "+img.MediaType+" "+string(img.Content))
	}
	if strings.Join(got, "; ") != "sig.2d4566582844690f@signature image/png PNGDATA; "+
		"sig.d4dc56669143034f@signature image/svg+xml <svg/>; sig.cace2150934eb976@signature image/png NAMEDPNG" {
		t.Errorf("inline %q, want the PNG once, the SVG and the named PNG", got)
	}
	if out.Text != "Hi.\n\n-- \nAda[image: logo]\n" {
		t.Errorf("text %q", out.Text)
	}
}

// A new body keeps the signature the draft carries, unless the edit
// drops it or sets another; it ends the message, since a new body
// replaces a quote. A draft whose parts are not one the server made
// carries none, and still needs both bodies.
func TestEditRawCarriesTheSignature(t *testing.T) {
	signed := build(t, Outgoing{Text: "Old text.\n\n-- \nAda Quill\nExample Org\n", HTML: "<p>Old text.</p>\n" + endAda})
	quoted := build(t, Outgoing{Text: "Old text.\n\nAda Quill\nExample Org\n\nOn Mon, Ada wrote:\n> Hello.\n",
		HTML: "<p>Old text.</p>\n" + aboveAda + "<p>On Mon, Ada wrote:<br>\n&gt; Hello.</p>\n"})
	logo := build(t, Outgoing{Text: "Old text.\n\n-- \n", HTML: "<p>Old text.</p>\n" + endLogo})
	made := build(t, Outgoing{Text: "Old text.\n", HTML: "<p>Old text.</p>\n"})
	plain := build(t, Outgoing{Text: "Old text.\n"})
	hand := build(t, Outgoing{Text: "Old text.\n\n-- \nAda Quill\nExample Org\n", HTML: "<p>Old <b>text</b>.</p>" + endAda})
	textGone := build(t, Outgoing{Text: "Old text.\n", HTML: "<p>Old text.</p>\n" + endAda})
	// Each would read as made if the delimiter did not have to stand on a
	// line of its own.
	dashesBefore := build(t, Outgoing{Text: "A\n\n-- x\n", HTML: "<p>A</p>\n<p>x</p>\n" + endLogo})
	dashesInside := build(t, Outgoing{Text: "A\n\nB -- C\n", HTML: "<p>A</p>\n<p>B C</p>\n" + endLogo})
	// Someone else's signature, in the composer's own markup, inside a
	// reply's quote and an inline forward that Gmail's composer wrote.
	quotedBlock := build(t, Outgoing{Text: "Thanks.\n\nOn Mon, Bo wrote:\n> Offer attached.\n> Bo Vale\n",
		HTML: `<div dir="ltr">Thanks.</div><br><div class="gmail_quote"><blockquote class="gmail_quote"><div dir="ltr">Offer attached.` +
			sigAbove.lead + sigAbove.open + `<div>Bo Vale<img src="https://example.com/pixel.png"></div>` + sigAbove.close +
			`</div></blockquote></div>`})
	forwardedBlock := build(t, Outgoing{Text: "See below.\n\n---------- Forwarded message ---------\nOffer attached.\n\n-- \nBo Vale\n",
		HTML: `<div dir="ltr">See below.<br><div class="gmail_quote">---------- Forwarded message ---------<br>Offer attached.` +
			sigEnding.lead + sigEnding.open + `<div>Bo Vale</div>` + sigEnding.close + `</div></div>`})
	// The quote repeats the signature's text, below it.
	repeated := build(t, Outgoing{Text: "Old text.\n\nRae\n\nOn Mon, Ada wrote:\n> Thanks Rae, see you.\n> Rae\n",
		HTML: "<p>Old text.</p>\n" + sigAbove.lead + sigAbove.open + "<div>Rae</div>" + sigAbove.close +
			"<p>On Mon, Ada wrote:<br>\n&gt; Thanks Rae, see you.<br>\n&gt; Rae</p>\n"})
	openAbove := build(t, Outgoing{Text: "Old text.\n\nAda Quill\n\n> q\n",
		HTML: "<p>Old text.</p>\n" + sigAbove.lead + sigAbove.open + "<div>Ada Quill" + sigAbove.close + "<p>&gt; q</p>\n"})
	textInside := build(t, Outgoing{Text: "A\n\nX Ada Quill\nExample Org\n\n> q\n",
		HTML: "<p>A</p>\n<p>X</p>\n" + aboveAda + "<p>&gt; q</p>\n"})
	open := build(t, Outgoing{Text: "Old text.\n\n-- \nAda Quill\n",
		HTML: "<p>Old text.</p>\n" + sigEnding.lead + sigEnding.open + "<div>Ada Quill" + sigEnding.close})

	const newSigned = "New.\n\n-- \nAda Quill\nExample Org\n"
	cases := []struct {
		name, text   string
		raw          []byte
		mode         HTMLMode
		html, sig    *string
		err          error
		plain, htm   string
		carried, now string
	}{
		{"a new body keeps it", "New.", signed, KeepShape, nil, nil, nil, newSigned, "<p>New.</p>\n" + endAda, sigAda, sigAda},
		{"one above a quote ends the new body", "New.", quoted, KeepShape, nil, nil, nil, newSigned, "<p>New.</p>\n" + endAda,
			sigAda, sigAda},
		{"images alone keep the delimiter", "New.", logo, KeepShape, nil, nil, nil, "New.\n\n-- \n", "<p>New.</p>\n" + endLogo,
			sigLogo, sigLogo},
		{"an emptied body keeps it", "", signed, KeepShape, nil, nil, nil, "-- \nAda Quill\nExample Org\n", endAda, sigAda, sigAda},
		{"none set drops it", "New.", signed, KeepShape, nil, ptrTo(""), nil, "New.", "<p>New.</p>\n", sigAda, ""},
		{"another replaces it", "New.", signed, KeepShape, nil, ptrTo("<p>Rae</p>"), nil, "New.\n\n-- \nRae\n",
			"<p>New.</p>\n" + sigEnding.lead + sigEnding.open + "<p>Rae</p>" + sigEnding.close, sigAda, "<p>Rae</p>"},
		{"one is set on a made draft", "New.", made, KeepShape, nil, ptrTo(sigAda), nil, newSigned, "<p>New.</p>\n" + endAda,
			"", sigAda},
		{"plain only drops it with the HTML", "New.", signed, PlainOnly, nil, nil, nil, "New.", "(none)", sigAda, ""},
		{"a plain draft takes none", "New.", plain, KeepShape, nil, ptrTo(sigAda), ErrPlainSignature, "", "", "", ""},
		{"with HTML, a plain draft takes one", "New.", plain, WithHTML, nil, ptrTo(sigAda), nil, newSigned,
			"<p>New.</p>\n" + endAda, "", sigAda},
		{"HTML written by hand still needs both", "New.", hand, KeepShape, nil, nil, ErrBothBodies, "", "", "", ""},
		{"new HTML keeps it", "New.", signed, KeepShape, ptrTo("<p><b>New.</b></p>"), nil, nil, newSigned,
			"<p><b>New.</b></p>" + endAda, sigAda, sigAda},
		{"HTML written by hand carries none", "New.", hand, KeepShape, ptrTo("<p><b>New.</b></p>"), nil, nil, "New.",
			"<p><b>New.</b></p>", "", ""},
		{"a block in a quoted reply is not the draft's", "New.", quotedBlock, KeepShape, ptrTo("<p>New.</p>"), nil, nil,
			"New.", "<p>New.</p>", "", ""},
		{"a block in a forward is not the draft's", "New.", forwardedBlock, KeepShape, ptrTo("<p>New.</p>"), nil, nil,
			"New.", "<p>New.</p>", "", ""},
		{"new HTML with a signature of its own", "New.", signed, KeepShape, ptrTo(`<p>New.</p>` + endAda), nil, ErrSignedHTML,
			"", "", "", ""},
		{"text gone from the plain part", "New.", textGone, KeepShape, nil, nil, ErrBothBodies, "", "", "", ""},
		{"a delimiter with more on its line", "New.", dashesBefore, KeepShape, nil, nil, ErrBothBodies, "", "", "", ""},
		{"a delimiter inside a line", "New.", dashesInside, KeepShape, nil, nil, ErrBothBodies, "", "", "", ""},
		{"a signature's text inside a line", "New.", textInside, KeepShape, nil, nil, ErrBothBodies, "", "", "", ""},
		{"a div left open, ending", "New.", open, KeepShape, nil, nil, nil, "New.\n\n-- \nAda Quill\n",
			"<p>New.</p>\n" + sigEnding.lead + sigEnding.open + "<div>Ada Quill" + sigEnding.close, "<div>Ada Quill", "<div>Ada Quill"},
		{"a div left open above a quote", "New.", openAbove, KeepShape, nil, nil, ErrBothBodies, "", "", "", ""},
		{"a quote that repeats it", "New.", repeated, KeepShape, nil, nil, nil, "New.\n\n-- \nRae\n",
			"<p>New.</p>\n" + sigEnding.lead + sigEnding.open + "<div>Rae</div>" + sigEnding.close, "<div>Rae</div>", "<div>Rae</div>"},
		{"a new body that ends with it", "New.\n\nAda Quill\nExample Org", signed, KeepShape, nil, nil, ErrSignedText,
			"", "", "", ""},
	}
	for _, c := range cases {
		ed, err := EditRaw(c.raw, Edit{Text: ptrTo(c.text), HTML: c.html, HTMLMode: c.mode, Signature: c.sig})
		if c.err != nil || err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%s: err %v, want %v", c.name, err, c.err)
			}
			continue
		}
		gotPlain, gotHTML := "", "(none)"
		for _, l := range leaves(ed.Raw) {
			switch l.mediaType {
			case "text/plain":
				gotPlain = string(l.data)
			case "text/html":
				gotHTML = string(l.data)
			}
		}
		if gotPlain != c.plain || gotHTML != c.htm || ed.Carried != c.carried || ed.Signature != c.now {
			t.Errorf("%s: plain %q html %q carried %q signature %q; want %q %q %q %q", c.name, gotPlain, gotHTML,
				ed.Carried, ed.Signature, c.plain, c.htm, c.carried, c.now)
		}
	}
}

// FuzzSignatureIsCarried holds that a signature this package writes below
// any body is found, carried below a new one, and found there again, its
// line endings read back as a part's are.
func FuzzSignatureIsCarried(f *testing.F) {
	for _, s := range [][2]string{{"Hi Ada,\n\nSee you.\n", "Ada Quill\nExample Org"}, {"", "-- "}, {"a\n-- \nb", "<b>&</b>"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, body, name string) {
		if !utf8.ValidString(body) || !utf8.ValidString(name) {
			t.Skip()
		}
		sig := "<div>" + html.EscapeString(name) + "</div>"
		out, err := Bodies(body, nil, sig, "")
		if errors.Is(err, ErrSignedText) {
			t.Skip() // a body that ends with the signature takes no second
		}
		if err != nil {
			t.Fatal(err)
		}
		want := normalizeNewlines(sig)
		raw := build(t, Outgoing{Text: out.Text, HTML: out.HTML, Inline: out.Inline})
		for i := range 2 {
			ed, err := EditRaw(raw, Edit{Text: ptrTo("New.")})
			if err != nil || ed.Carried != want || ed.Signature != want {
				t.Fatalf("signed %q below %q, edit %d: carried %q, wrote %q, err %v", sig, body, i, ed.Carried, ed.Signature, err)
			}
			raw = ed.Raw
		}
	})
}

// A signature's images live in parts beside the HTML: a new body keeps
// them, one the edit drops or replaces takes its own with it, and one it
// sets brings them, never twice.
func TestEditRawCarriesTheSignaturesImages(t *testing.T) {
	logo := `<div>Ada<img src="data:image/png;base64,UE5HREFUQQ=="></div>`
	other := `<div>Bo<img src="data:image/png;base64,T1RIRVJQTkc="></div>`
	signed, err := Bodies("Old text.", nil, logo, "")
	if err != nil {
		t.Fatal(err)
	}
	raw := build(t, Outgoing{Text: signed.Text, HTML: signed.HTML, Inline: signed.Inline,
		Attachments: []OutAttachment{attA}})
	made := build(t, Outgoing{Text: "Old text.\n", HTML: "<p>Old text.</p>\n", Attachments: []OutAttachment{attA}})
	const cidAda, cidBo = "sig.2d4566582844690f@signature", "sig.fea0ae0a8e8d6693@signature"
	cases := []struct {
		name   string
		raw    []byte
		mode   HTMLMode
		sig    *string
		images []string
		html   string
	}{
		{"a new body keeps them", raw, KeepShape, nil, []string{cidAda},
			`<p>New.</p>` + "\n" + sigEnding.lead + sigEnding.open + `<div>Ada<img src="cid:` + cidAda + `"></div>` + sigEnding.close},
		{"the same one set again keeps one", raw, KeepShape, &logo, []string{cidAda},
			`<p>New.</p>` + "\n" + sigEnding.lead + sigEnding.open + `<div>Ada<img src="cid:` + cidAda + `"></div>` + sigEnding.close},
		{"another takes its place", raw, KeepShape, &other, []string{cidBo},
			`<p>New.</p>` + "\n" + sigEnding.lead + sigEnding.open + `<div>Bo<img src="cid:` + cidBo + `"></div>` + sigEnding.close},
		{"dropped, they go", raw, KeepShape, ptrTo(""), nil, "<p>New.</p>\n"},
		{"plain only takes them with the HTML", raw, PlainOnly, nil, nil, "(none)"},
		{"set on a made draft, they come", made, KeepShape, &logo, []string{cidAda},
			`<p>New.</p>` + "\n" + sigEnding.lead + sigEnding.open + `<div>Ada<img src="cid:` + cidAda + `"></div>` + sigEnding.close},
	}
	for _, c := range cases {
		ed, err := EditRaw(c.raw, Edit{Text: ptrTo("New."), HTMLMode: c.mode, Signature: c.sig})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var images []string
		gotHTML, attached := "(none)", false
		m := ParseRaw(ed.Raw)
		for _, l := range leaves(ed.Raw) {
			switch {
			case l.mediaType == "text/html":
				gotHTML = string(l.data)
			case l.declaredName() == "a.txt":
				attached = true
			case l.mediaType == "image/png":
				images = append(images, strings.Trim(headerGet(l.headers, "Content-ID"), "<>"))
			}
		}
		if gotHTML != c.html || strings.Join(images, ",") != strings.Join(c.images, ",") || !attached ||
			m.Body.Source == "" {
			t.Errorf("%s: html %q images %v attachment %v; want %q %v", c.name, gotHTML, images, attached, c.html, c.images)
		}
	}
}

// HTML a new body replaces in Gmail's own layout, which keeps the images
// a level above the plain and HTML pair, takes the images it no longer
// shows with it; one it still shows stays.
func TestEditRawPrunesImagesTheNewHTMLDoesNotShow(t *testing.T) {
	raw := []byte("Content-Type: multipart/related; boundary=r\r\n\r\n--r\r\n" +
		"Content-Type: multipart/alternative; boundary=a\r\n\r\n--a\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nOld.\r\n" +
		"--a\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Old.</p><img src=\"cid:ii_one\"><img src=\"cid:ii_two\">\r\n--a--\r\n" +
		"--r\r\nContent-Type: image/png\r\nContent-ID: <ii_css>\r\nContent-Transfer-Encoding: base64\r\n\r\nR01BSUxJTUc=\r\n" +
		"--r\r\nContent-Type: image/png\r\nContent-ID: <ii four>\r\nContent-Transfer-Encoding: base64\r\n\r\nR01BSUxJTUc=\r\n" +
		"--r\r\nContent-Type: image/png\r\nContent-ID: <ii_one>\r\nContent-Transfer-Encoding: base64\r\n\r\nR01BSUxJTUc=\r\n" +
		"--r\r\nContent-Type: image/png\r\nContent-ID: <ii_two>\r\nContent-Transfer-Encoding: base64\r\n\r\nR01BSUxJTUc=\r\n" +
		"--r\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-ID: <ii_pdf>\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERg==\r\n--r--\r\n")
	ed, err := EditRaw(raw, Edit{Text: ptrTo("New."),
		HTML: ptrTo(`<p style="background:url(cid:ii_css)">New.</p><img src="cid:ii_two"><img src="cid:ii%20four">`)})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, l := range leaves(ed.Raw) {
		parts = append(parts, l.mediaType+" "+strings.Trim(headerGet(l.headers, "Content-ID"), "<>"))
	}
	if got := strings.Join(parts, ", "); got != "text/plain , text/html , image/png ii_css, image/png ii four, "+
		"image/png ii_two, application/pdf ii_pdf" || ed.Pruned != 1 {
		t.Errorf("parts %s, %d pruned; want the plain text, the HTML, the three images still shown and the attachment, "+
			"one pruned", got, ed.Pruned)
	}
}
