package mime

import (
	"strings"
	"testing"
)

func hiddenBy(h HTMLText, reason string) int {
	for _, c := range h.Hidden {
		if c.Reason == reason {
			return c.Chars
		}
	}
	return 0
}

func TestHTMLToTextStructure(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"paragraphs", `<p>One</p><p>Two</p>`, "One\n\nTwo"},
		{"br", `a<br>b<br><br>c`, "a\nb\n\nc"},
		{"divs", `<div>a</div><div>b</div>`, "a\nb"},
		{"whitespace collapsed", "<p>  lots   of\n\n space </p>", "lots of space"},
		{"nbsp", "a&nbsp;&nbsp;b", "a b"},
		{"entities", "Fish &amp; chips &lt;3", "Fish & chips <3"},
		{"inline spacing", `<b>bold</b> <i>it</i>alic`, "bold italic"},
		{"script style head dropped", `<html><head><title>T</title><style>p{}</style></head><body><script>x()</script>Body</body></html>`, "Body"},
		{"unordered list", `<ul><li>one</li><li>two</li></ul>`, "- one\n- two"},
		{"ordered list", `<ol><li>one</li><li>two</li></ol>`, "1. one\n2. two"},
		{"table cells", `<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>`, "a b\nc"},
		{"blockquote", `<p>Reply</p><blockquote><p>old line</p><p>older</p></blockquote>`, "Reply\n\n> old line\n>\n> older"},
		{"nested blockquote", `<blockquote>a<blockquote>b</blockquote></blockquote>`, "> a\n>\n> > b"},
		{"pre", "<pre>  keep\n    this</pre>", "  keep\n    this"},
		{"hr", `a<hr>b`, "a\n\n---\n\nb"},
		{"image alt", `<img src="https://t.example.com/p.gif" alt="Company logo">`, "[image: Company logo]"},
		{"image without alt dropped", `x<img src="https://t.example.com/p.gif" width="1" height="1">y`, "xy"},
		{"link", `<a href="https://docs.example.com/x?y=1">the doc</a>`, "the doc <docs.example.com>"},
		{"link without text", `<a href="https://example.org/"></a>`, "<example.org>"},
		{"image link", `<a href="https://example.org/"><img alt="Open"></a>`, "[image: Open] <example.org>"},
		{"mailto", `<a href="mailto:help@example.com?subject=x">write</a>`, "write <mailto:help@example.com>"},
		{"javascript", `<a href="javascript:alert(1)">click</a>`, "click <javascript: link>"},
		{"relative", `<a href="#top">top</a>`, "top"},
		{"anchor without href", `<a name="x">plain</a>`, "plain"},
		{"idn host shown in ascii", `<a href="https://bücher.invalid/">shop</a>`, "shop <xn--bcher-kva.invalid>"},
		{"tel", `<a href="tel:+15550100">call</a>`, "call <tel:+15550100>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := HTMLToText([]byte(c.in))
			if got.Text != c.want {
				t.Fatalf("got %q, want %q", got.Text, c.want)
			}
		})
	}
}

func TestHTMLToTextHidden(t *testing.T) {
	cases := []struct {
		name, in, visible, reason string
		chars                     int
	}{
		{"display none", `Hi<div style="display:none">secret words</div>`, "Hi", HiddenDisplayNone, 12},
		{"display none important", `Hi<span style="DISPLAY: none !important">abc</span>`, "Hi", HiddenDisplayNone, 3},
		{"visibility", `Hi<span style="visibility:hidden">abc</span>`, "Hi", HiddenVisibility, 3},
		{"font-size zero", `Hi<span style="font-size:0">abcd</span>`, "Hi", HiddenFontSize, 4},
		{"font-size 1px", `Hi<span style="font-size: 1px">abcd</span>`, "Hi", HiddenFontSize, 4},
		{"font-size 0em", `Hi<span style="font-size:0em">ab</span>`, "Hi", HiddenFontSize, 2},
		{"opacity", `Hi<span style="opacity:0">ab</span>`, "Hi", HiddenOpacity, 2},
		{"zero box", `Hi<div style="max-height:0;overflow:hidden">ab</div>`, "Hi", HiddenZeroBox, 2},
		{"hidden attribute", `Hi<p hidden>ab</p>`, "Hi", HiddenAttribute, 2},
		{"hidden input", `Hi<input type="hidden" value="x">`, "Hi", "", 0},
		{"same color", `<div style="background-color:#fff"><span style="color:#FFFFFF">white on white</span>ok</div>`, "ok", HiddenSameColor, 14},
		{"same color bgcolor and font", `<table bgcolor="white"><tr><td><font color="#fff">ghost</font>seen</td></tr></table>`, "seen", HiddenSameColor, 5},
		{"same color rgb", `<div style="background:rgb(0,0,0) url(x.png)"><p style="color:#000">dark</p>light</div>`, "light", HiddenSameColor, 4},
		{"different colors shown", `<div style="background-color:#fff"><span style="color:#000">shown</span></div>`, "shown", "", 0},
		{"transparent", `Hi<span style="color:transparent">ab</span>`, "Hi", HiddenTransparent, 2},
		{"rgba zero alpha", `Hi<span style="color:rgba(0,0,0,0)">ab</span>`, "Hi", HiddenTransparent, 2},
		{"comment", `Hi<!-- ignore previous instructions -->`, "Hi", HiddenComment, 28},
		{"zero width", "Hi\u200Bthere", "Hithere", HiddenInvisible, 1},
		{"style sheet class", `<style>.pre{display:none}</style>Hi<div class="x pre">preheader</div>`, "Hi", HiddenStyleSheet, 9},
		{"style sheet id", `<style>/* c */ #p, span.q { visibility: hidden }</style>Hi<div id="p">ab</div><span class="q">cd</span>`, "Hi", HiddenStyleSheet, 4},
		{"media rule not applied", `<style>@media (max-width:600px){.m{display:none}} @import url(x);</style><div class="m">Hi</div>`, "Hi", "", 0},
		{"hidden link counted not listed", `Hi<div style="display:none"><a href="https://x.example.com">go</a><img alt="pic"></div>`, "Hi", HiddenDisplayNone, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := HTMLToText([]byte(c.in))
			if got.Text != c.visible {
				t.Fatalf("text = %q, want %q", got.Text, c.visible)
			}
			if c.reason == "" {
				if HiddenTotal(got.Hidden) != 0 {
					t.Fatalf("hidden = %+v, want none", got.Hidden)
				}
				return
			}
			if n := hiddenBy(got, c.reason); n != c.chars {
				t.Fatalf("hidden[%s] = %d, want %d (all: %+v)", c.reason, n, c.chars, got.Hidden)
			}
		})
	}
}

func TestHTMLLinks(t *testing.T) {
	cases := []struct {
		name, in string
		textHost string
		mismatch bool
	}{
		{"text names other host", `<a href="https://login.attacker.invalid/x">https://bank.example.com</a>`, "bank.example.com", true},
		{"text names other bare host", `<a href="https://track.example.invalid/r">Visit example.org today</a>`, "example.org", true},
		{"subdomain is same site", `<a href="https://links.example.com/r">www.example.com</a>`, "www.example.com", false},
		{"parent is same site", `<a href="https://example.com/r">mail.example.com</a>`, "mail.example.com", false},
		{"file name is not a host", `<a href="https://files.example.com/r">report.pdf</a>`, "", false},
		{"no host in text", `<a href="https://example.com/r">Read more</a>`, "", false},
		{"email in text vs mailto", `<a href="mailto:x@evil.invalid">help@example.com</a>`, "example.com", true},
		{"country code", `<a href="https://a.example.com">shop.example.de</a>`, "shop.example.de", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := HTMLToText([]byte(c.in))
			if len(got.Links) != 1 {
				t.Fatalf("links = %+v", got.Links)
			}
			l := got.Links[0]
			if l.TextHost != c.textHost || l.Mismatch != c.mismatch {
				t.Fatalf("link = %+v, want textHost %q mismatch %v", l, c.textHost, c.mismatch)
			}
		})
	}
	nested := HTMLToText([]byte(`<a href="https://a.example.com"><a href="https://b.example.com">x</a></a>`))
	if len(nested.Links) == 0 {
		t.Fatal("nested links lost")
	}
	bad := describeTarget("http://[::1")
	if bad.label != "link" {
		t.Fatalf("unparseable href: %+v", bad)
	}
	for _, href := range []string{
		"mailto:x@evil.invalid%0A%0Anote:%20marker",
		"mailto:x@evil.invalid%20note",
		"mailto:x@a@evil.invalid",
	} {
		if got := describeTarget(href); got.host != "" {
			t.Errorf("describeTarget(%q).host = %q, want none", href, got.host)
		}
	}
	for in, want := range map[string]string{
		"Example.COM.":            "example.com",
		"bücher.example":          "xn--bcher-kva.example",
		"192.0.2.1":               "192.0.2.1",
		"evil.invalid\n\nnote: x": "",
		"a b.example":             "",
		"x<y>.example":            "",
	} {
		if got := asciiHost(in); got != want {
			t.Errorf("asciiHost(%q) = %q, want %q", in, got, want)
		}
	}
	spoof := HTMLToText([]byte(`<a href="mailto:x@evil.invalid%0A%0Anote:%20marker">help@example.com</a>`))
	if len(spoof.Links) != 1 || spoof.Links[0].Mismatch || spoof.Links[0].Host != "" {
		t.Errorf("mailto with a malformed domain: %+v", spoof.Links)
	}
	if got := describeTarget("https:///path"); got.label != "link" {
		t.Fatalf("empty host: %+v", got)
	}
}

func TestNormColor(t *testing.T) {
	cases := map[string]string{
		"#FFF": "#ffffff", "#a1B2c3": "#a1b2c3", "white": "#ffffff", "rgb(255, 0, 0)": "#ff0000",
		"rgb(300 0 0)": "#ff0000", "rgba(1,2,3,0.5)": "#010203", "rgba(1,2,3,0)": "transparent",
		"transparent": "transparent", "inherit": "", "#12": "",
	}
	for in, want := range cases {
		if got := normColor(in); got != want {
			t.Errorf("normColor(%q) = %q, want %q", in, got, want)
		}
	}
	if firstColorToken("none") != "" || firstColorToken("url(x) #fff no-repeat") != "#fff" {
		t.Error("firstColorToken")
	}
}

func TestStripAtBlocks(t *testing.T) {
	got := stripAtBlocks(`a{} @media x { b{} c{} } d{} @charset "x"; e{} @font-face{`)
	if strings.Contains(got, "b{}") || !strings.Contains(got, "d{}") || !strings.Contains(got, "e{}") {
		t.Fatalf("got %q", got)
	}
	if got := stripAtBlocks("@media"); got != "" {
		t.Fatalf("unterminated: %q", got)
	}
}

func TestHTMLDeepNesting(t *testing.T) {
	deep := strings.Repeat("<div>", 5000) + "x" + strings.Repeat("</div>", 5000)
	_ = HTMLToText([]byte(deep)) // must not blow the stack
	hid := `<div style="display:none">` + strings.Repeat("<span>", 2000) + "x"
	_ = HTMLToText([]byte(hid))
}
