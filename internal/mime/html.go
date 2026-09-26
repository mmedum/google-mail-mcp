package mime

import (
	"bytes"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/idna"
)

// Reasons text was hidden from a reader and dropped (§4.1.2).
const (
	HiddenDisplayNone = "display:none"
	HiddenVisibility  = "visibility:hidden"
	HiddenAttribute   = "hidden attribute"
	HiddenFontSize    = "zero or near-zero font size"
	HiddenOpacity     = "opacity:0"
	HiddenZeroBox     = "zero-size box with overflow hidden"
	HiddenSameColor   = "text colored like its background"
	HiddenTransparent = "transparent text"
	HiddenStyleSheet  = "hidden by a style sheet rule"
	HiddenComment     = "HTML comment"
	HiddenInvisible   = "zero-width or bidi-control characters"
)

// HiddenCount is how many characters were dropped for one reason.
type HiddenCount struct {
	Reason string
	Chars  int
}

// Link is an anchor found in HTML. Host is the target's host in ASCII
// form, so a look-alike Unicode domain shows as xn--; for mailto it is
// the address's domain. TextHost is the host the visible text names, if
// any. Both hold only host grammar, or are empty, so they compare as
// hosts; they are still the sender's, and shown only inside a block.
type Link struct {
	Text     string
	Href     string
	Host     string
	TextHost string
	// Mismatch is set when the text names a host that is not the
	// target's host or one of its subdomains or parents.
	Mismatch bool
}

// HTMLText is the result of converting HTML to text.
type HTMLText struct {
	Text   string
	Hidden []HiddenCount
	Links  []Link
}

// HiddenTotal sums the hidden counts.
func HiddenTotal(hs []HiddenCount) int {
	n := 0
	for _, h := range hs {
		n += h.Chars
	}
	return n
}

// HTMLToText converts an HTML body to text a reader would see. Nothing
// is fetched. Scripts, styles and the head are dropped; text a reader
// would not see is dropped and counted; links render as `text <host>`;
// images as `[image: alt]`.
func HTMLToText(src []byte) HTMLText {
	src = bytes.ToValidUTF8(src, []byte("\uFFFD"))
	doc, err := html.Parse(bytes.NewReader(src))
	if err != nil {
		// html.Parse only fails on a reader error; bytes cannot fail.
		return HTMLText{Text: string(src)}
	}
	c := &converter{hidden: map[string]int{}}
	c.sheet = collectStyleRules(doc)
	c.walk(doc, state{})
	text, inv := StripInvisible(c.out.finish())
	c.hidden[HiddenInvisible] += inv
	return HTMLText{Text: text, Hidden: sortedHidden(c.hidden), Links: c.links}
}

func sortedHidden(m map[string]int) []HiddenCount {
	var out []HiddenCount
	for r, n := range m {
		if n > 0 {
			out = append(out, HiddenCount{Reason: r, Chars: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chars != out[j].Chars {
			return out[i].Chars > out[j].Chars
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

type state struct {
	hidden string // the reason, once an ancestor hid the subtree
	color  string
	bg     string
	pre    bool
	quote  int
}

type converter struct {
	out    textOut
	hidden map[string]int
	links  []Link
	sheet  styleSheet
	depth  int
	// lists is the open lists, innermost last: 0 for an unordered list,
	// the next item's number for an ordered one.
	lists []int
}

// maxDepth bounds recursion on a pathologically nested document.
const maxDepth = 512

var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Title: true,
	atom.Template: true, atom.Svg: true, atom.Object: true, atom.Embed: true,
	atom.Iframe: true, atom.Math: true, atom.Select: true, atom.Datalist: true,
}

var paraBlocks = map[atom.Atom]bool{
	atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
	atom.H5: true, atom.H6: true, atom.Table: true, atom.Blockquote: true,
	atom.Ul: true, atom.Ol: true, atom.Pre: true, atom.Dl: true, atom.Figure: true,
	atom.Address: true,
}

var lineBlocks = map[atom.Atom]bool{
	atom.Div: true, atom.Tr: true, atom.Li: true, atom.Dt: true, atom.Dd: true,
	atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true,
	atom.Nav: true, atom.Aside: true, atom.Main: true, atom.Form: true,
	atom.Center: true, atom.Figcaption: true, atom.Caption: true, atom.Tbody: true,
	atom.Thead: true, atom.Tfoot: true, atom.Fieldset: true, atom.Details: true,
	atom.Summary: true,
}

func (c *converter) walk(n *html.Node, st state) {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > maxDepth {
		return
	}
	switch n.Type {
	case html.CommentNode:
		c.hidden[HiddenComment] += visibleLen(n.Data)
		return
	case html.TextNode:
		c.text(n.Data, st)
		return
	case html.ElementNode:
		c.element(n, st)
		return
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch, st)
	}
}

func (c *converter) text(s string, st state) {
	if st.hidden == "" && st.color != "" && st.color == st.bg {
		st.hidden = HiddenSameColor
	}
	if st.hidden != "" {
		c.hidden[st.hidden] += visibleLen(s)
		return
	}
	if st.pre {
		c.out.pre(s, st.quote)
		return
	}
	c.out.words(s, st.quote)
}

func (c *converter) element(n *html.Node, st state) {
	if skipped[n.DataAtom] {
		return
	}
	st = c.inherit(n, st)
	if st.hidden != "" {
		// Count the subtree's text without rendering it.
		c.countHidden(n, st.hidden)
		return
	}

	switch n.DataAtom {
	case atom.Br:
		c.out.br()
		return
	case atom.Hr:
		c.out.para(st.quote)
		c.out.words("---", st.quote)
		c.out.para(st.quote)
		return
	case atom.Img:
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			c.out.words("[image: "+collapse(alt)+"]", st.quote)
		}
		return
	case atom.A:
		if href := attr(n, "href"); href != "" {
			c.link(n, href, st)
			return
		}
	case atom.Td, atom.Th:
		c.out.space()
	case atom.Blockquote:
		st.quote++
	case atom.Pre:
		st.pre = true
	case atom.Ul:
		c.lists = append(c.lists, 0)
		defer c.popList()
	case atom.Ol:
		c.lists = append(c.lists, 1)
		defer c.popList()
	}

	block := paraBlocks[n.DataAtom] || lineBlocks[n.DataAtom]
	switch {
	case paraBlocks[n.DataAtom]:
		c.out.para(st.quote)
	case lineBlocks[n.DataAtom]:
		c.out.newline(st.quote)
	}
	if n.DataAtom == atom.Li {
		c.out.words(c.bullet(), st.quote)
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch, st)
	}
	if block {
		if paraBlocks[n.DataAtom] {
			c.out.para(st.quote - boolInt(n.DataAtom == atom.Blockquote))
		} else {
			c.out.newline(st.quote)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// bullet is a list item's marker: "-" in an unordered list, the next
// number in an ordered one.
func (c *converter) bullet() string {
	if len(c.lists) == 0 || c.lists[len(c.lists)-1] == 0 {
		return "- "
	}
	n := c.lists[len(c.lists)-1]
	c.lists[len(c.lists)-1]++
	return strconv.Itoa(n) + ". "
}

func (c *converter) popList() { c.lists = c.lists[:len(c.lists)-1] }

func (c *converter) countHidden(n *html.Node, reason string) {
	var walk func(*html.Node, int)
	walk = func(n *html.Node, d int) {
		if d > maxDepth {
			return
		}
		switch n.Type {
		case html.TextNode:
			c.hidden[reason] += visibleLen(n.Data)
			return
		case html.CommentNode:
			c.hidden[HiddenComment] += visibleLen(n.Data)
			return
		case html.ElementNode:
			if skipped[n.DataAtom] {
				return
			}
			if n.DataAtom == atom.Img {
				c.hidden[reason] += visibleLen(attr(n, "alt"))
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch, d+1)
		}
	}
	walk(n, 0)
}

// link renders an anchor as `text <host>`.
func (c *converter) link(n *html.Node, href string, st state) {
	sub := &converter{hidden: c.hidden, sheet: c.sheet, depth: c.depth}
	inner := st
	inner.quote = 0
	inner.pre = false
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		sub.walk(ch, inner)
	}
	c.links = append(c.links, sub.links...)
	text := collapse(sub.out.finish())

	l := Link{Text: text, Href: strings.TrimSpace(href)}
	target := describeTarget(l.Href)
	l.Host = target.host
	if target.host != "" {
		for _, h := range textHosts(text) {
			if !sameSite(h, target.host) {
				l.TextHost, l.Mismatch = h, true
				break
			}
			if l.TextHost == "" {
				l.TextHost = h
			}
		}
	}
	c.links = append(c.links, l)

	switch {
	case text == "" && target.label == "":
		return
	case text == "":
		c.out.words("<"+target.label+">", st.quote)
	case target.label == "":
		c.out.words(text, st.quote)
	default:
		c.out.words(text+" <"+target.label+">", st.quote)
	}
}

type target struct {
	host  string // for comparison: ASCII host, or the mailto domain; "" if not host-shaped
	label string // what the reader sees
}

func describeTarget(href string) target {
	u, err := url.Parse(href)
	if err != nil {
		return target{label: "link"}
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		h := asciiHost(u.Hostname())
		if h == "" {
			return target{label: "link"}
		}
		return target{host: h, label: h}
	case "mailto":
		addr := u.Opaque
		if addr == "" {
			addr = u.Path
		}
		addr, _, _ = strings.Cut(addr, "?")
		if dec, err := url.PathUnescape(addr); err == nil {
			addr = dec
		}
		_, domain, _ := strings.Cut(addr, "@")
		return target{host: asciiHost(domain), label: "mailto:" + addr}
	case "tel":
		return target{label: "tel:" + u.Opaque}
	case "":
		// A fragment or relative link has no host to show.
		return target{}
	default:
		return target{label: strings.ToLower(u.Scheme) + ": link"}
	}
}

// asciiHost returns h as an ASCII host name or IP address, or "" when
// it is neither. A sender controls the href, so nothing but host
// grammar passes.
func asciiHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" {
		return ""
	}
	for _, p := range []*idna.Profile{idna.Lookup, idna.Punycode} {
		if a, err := p.ToASCII(h); err == nil && hostShaped(a) {
			return a
		}
	}
	return ""
}

// hostShaped reports whether h is an IP address without a zone, or dot
// separated labels of ASCII letters, digits, hyphens and underscores.
func hostShaped(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	if len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
			default:
				return false
			}
		}
	}
	return true
}

var (
	hostInText  = regexp.MustCompile(`(?i)(?:https?://)?(?:[\p{L}\p{N}](?:[\p{L}\p{N}-]*[\p{L}\p{N}])?\.)+[\p{L}]{2,63}\b`)
	emailInText = regexp.MustCompile(`(?i)[^\s<>@]+@((?:[\p{L}\p{N}-]+\.)+[\p{L}]{2,63})`)
)

// commonTLDs is where a bare word.word in link text is taken to name a
// host. A two-letter country code also counts. A scheme or "www."
// makes any suffix count. This keeps "report.pdf" from reading as a
// host.
var commonTLDs = map[string]bool{
	"com": true, "org": true, "net": true, "edu": true, "gov": true, "mil": true,
	"int": true, "info": true, "biz": true, "app": true, "dev": true, "io": true,
	"ai": true, "co": true, "me": true, "online": true, "site": true, "shop": true,
	"store": true, "xyz": true, "top": true, "club": true, "cloud": true,
	"email": true, "link": true, "live": true, "support": true, "security": true,
	"example": true, "invalid": true, "test": true, "local": true, "bank": true,
	"page": true, "services": true, "account": true, "login": true,
}

// textHosts returns the hosts a link's visible text names.
func textHosts(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		h = asciiHost(h)
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, m := range emailInText.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	masked := emailInText.ReplaceAllString(text, " ")
	for _, m := range hostInText.FindAllString(masked, -1) {
		lower := strings.ToLower(m)
		h := strings.TrimPrefix(strings.TrimPrefix(lower, "https://"), "http://")
		explicit := h != lower || strings.HasPrefix(h, "www.")
		tld := h[strings.LastIndexByte(h, '.')+1:]
		if explicit || commonTLDs[tld] || utf8.RuneCountInString(tld) == 2 && isLetters(tld) {
			add(h)
		}
	}
	return out
}

func isLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// sameSite reports whether a and b are the same host, ignoring "www.",
// or one is a subdomain of the other.
func sameSite(a, b string) bool {
	a, b = strings.TrimPrefix(a, "www."), strings.TrimPrefix(b, "www.")
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// inherit applies an element's attributes and inline style to the
// state its children see.
func (c *converter) inherit(n *html.Node, st state) state {
	if _, ok := attrOK(n, "hidden"); ok {
		st.hidden = HiddenAttribute
		return st
	}
	if n.DataAtom == atom.Input && strings.EqualFold(attr(n, "type"), "hidden") {
		st.hidden = HiddenAttribute
		return st
	}
	if r := c.sheet.hides(n); r != "" {
		st.hidden = r
		return st
	}
	if v := attr(n, "bgcolor"); v != "" {
		if col := normColor(v); col != "" {
			st.bg = col
		}
	}
	if n.DataAtom == atom.Font {
		if col := normColor(attr(n, "color")); col != "" {
			st.color = col
		}
	}
	decls := parseStyle(attr(n, "style"))
	if r := hidingReason(decls); r != "" {
		st.hidden = r
		return st
	}
	if v, ok := decls["color"]; ok {
		col := normColor(v)
		if col == "transparent" {
			st.hidden = HiddenTransparent
			return st
		}
		if col != "" {
			st.color = col
		}
	}
	for _, k := range []string{"background-color", "background"} {
		if v, ok := decls[k]; ok {
			if col := normColor(firstColorToken(v)); col != "" && col != "transparent" {
				st.bg = col
			}
		}
	}
	return st
}

// hidingReason says whether a declaration block hides its element.
func hidingReason(d map[string]string) string {
	if v := d["display"]; v == "none" {
		return HiddenDisplayNone
	}
	if v := d["visibility"]; v == "hidden" || v == "collapse" {
		return HiddenVisibility
	}
	if v, ok := d["font-size"]; ok && tinyLength(v, 1) {
		return HiddenFontSize
	}
	if v, ok := d["opacity"]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64); err == nil && f == 0 {
			return HiddenOpacity
		}
	}
	if o := d["overflow"]; o == "hidden" {
		for _, k := range []string{"max-height", "height", "max-width", "width"} {
			if v, ok := d[k]; ok && tinyLength(v, 0) {
				return HiddenZeroBox
			}
		}
	}
	return ""
}

var lengthRe = regexp.MustCompile(`^([0-9]*\.?[0-9]+)\s*(px|pt|em|rem|%|ex|ch|vw|vh)?$`)

// tinyLength reports whether a CSS length is zero, or at most maxPx
// pixels or points.
func tinyLength(v string, maxPx float64) bool {
	m := lengthRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return false
	}
	if f == 0 {
		return true
	}
	return (m[2] == "px" || m[2] == "pt") && f <= maxPx
}

// parseStyle parses an inline style attribute into lowercased
// declarations; the last one of a name wins, as in CSS.
func parseStyle(s string) map[string]string {
	d := map[string]string{}
	for decl := range strings.SplitSeq(s, ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		v = strings.TrimSpace(strings.TrimSuffix(v, "!important"))
		if k != "" {
			d[k] = v
		}
	}
	return d
}

var namedColors = map[string]string{
	"white": "#ffffff", "black": "#000000", "red": "#ff0000", "green": "#008000",
	"blue": "#0000ff", "yellow": "#ffff00", "gray": "#808080", "grey": "#808080", // both are CSS keywords
	"silver": "#c0c0c0", "navy": "#000080", "orange": "#ffa500", "purple": "#800080",
	"transparent": "transparent",
}

var rgbRe = regexp.MustCompile(`^rgba?\(\s*(\d+)\s*[, ]\s*(\d+)\s*[, ]\s*(\d+)\s*(?:[,/]\s*([0-9.]+%?)\s*)?\)$`)

// normColor reduces a color to #rrggbb or "transparent", or "" when
// it cannot be read.
func normColor(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if c, ok := namedColors[v]; ok {
		return c
	}
	if m := rgbRe.FindStringSubmatch(v); m != nil {
		if m[4] != "" {
			if a, err := strconv.ParseFloat(strings.TrimSuffix(m[4], "%"), 64); err == nil && a == 0 {
				return "transparent"
			}
		}
		var rgb [3]int
		for i := range 3 {
			n, _ := strconv.Atoi(m[i+1])
			rgb[i] = min(n, 255)
		}
		return fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2])
	}
	v = strings.TrimPrefix(v, "#")
	if len(v) == 3 && isHexString(v) {
		return "#" + string([]byte{v[0], v[0], v[1], v[1], v[2], v[2]})
	}
	if len(v) == 6 && isHexString(v) {
		return "#" + v
	}
	return ""
}

func isHexString(s string) bool {
	for i := range len(s) {
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

// rgbToken is an rgb() or rgba() color inside a longer value.
var rgbToken = regexp.MustCompile(`rgba?\([^)]*\)`)

// firstColorToken picks the color out of a background shorthand.
func firstColorToken(v string) string {
	if m := rgbToken.FindString(v); m != "" {
		return m
	}
	for f := range strings.FieldsSeq(v) {
		if normColor(f) != "" {
			return f
		}
	}
	return ""
}

// styleSheet holds the classes and ids a <style> element hides with a
// simple rule. Selectors other than .class, tag.class and #id are not
// evaluated, and rules inside @media are skipped because they apply
// only on some screens.
type styleSheet struct {
	classes map[string]string
	ids     map[string]string
}

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRule    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	classSel   = regexp.MustCompile(`^[a-z0-9]*\.([a-z0-9_-]+)$`)
	idSel      = regexp.MustCompile(`^#([a-z0-9_-]+)$`)
)

func collectStyleRules(doc *html.Node) styleSheet {
	sh := styleSheet{classes: map[string]string{}, ids: map[string]string{}}
	var walk func(*html.Node, int)
	walk = func(n *html.Node, d int) {
		if d > maxDepth {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Style {
			var b strings.Builder
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				if ch.Type == html.TextNode {
					b.WriteString(ch.Data)
				}
			}
			sh.add(b.String())
			return
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch, d+1)
		}
	}
	walk(doc, 0)
	return sh
}

func (sh styleSheet) add(css string) {
	css = strings.ToLower(cssComment.ReplaceAllString(css, ""))
	css = stripAtBlocks(css)
	for _, m := range cssRule.FindAllStringSubmatch(css, -1) {
		reason := hidingReason(parseStyle(m[2]))
		if reason == "" {
			continue
		}
		for sel := range strings.SplitSeq(m[1], ",") {
			sel = strings.TrimSpace(sel)
			if s := classSel.FindStringSubmatch(sel); s != nil {
				sh.classes[s[1]] = HiddenStyleSheet
			} else if s := idSel.FindStringSubmatch(sel); s != nil {
				sh.ids[s[1]] = HiddenStyleSheet
			}
		}
	}
}

// stripAtBlocks removes @media and other at-rule blocks, nested braces
// included.
func stripAtBlocks(css string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(css, '@')
		if i < 0 {
			b.WriteString(css)
			return b.String()
		}
		b.WriteString(css[:i])
		rest := css[i:]
		open := strings.IndexAny(rest, "{;")
		if open < 0 {
			return b.String()
		}
		if rest[open] == ';' {
			css = rest[open+1:]
			continue
		}
		depth, j := 0, open
		for ; j < len(rest); j++ {
			if rest[j] == '{' {
				depth++
			} else if rest[j] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j >= len(rest) {
			return b.String()
		}
		css = rest[j+1:]
	}
}

func (sh styleSheet) hides(n *html.Node) string {
	if len(sh.classes) == 0 && len(sh.ids) == 0 {
		return ""
	}
	for cl := range strings.FieldsSeq(strings.ToLower(attr(n, "class"))) {
		if r := sh.classes[cl]; r != "" {
			return r
		}
	}
	if id := strings.ToLower(attr(n, "id")); id != "" {
		return sh.ids[id]
	}
	return ""
}

func attr(n *html.Node, key string) string {
	v, _ := attrOK(n, key)
	return v
}

func attrOK(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

// visibleLen counts the characters of s a reader would have seen had it
// been shown: whitespace runs count as one.
func visibleLen(s string) int {
	return utf8.RuneCountInString(collapse(s))
}

// collapse folds whitespace runs to one space and trims.
func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, isSpace), " ")
}

func isSpace(r rune) bool { return unicode.IsSpace(r) || r == 0xA0 }

// textOut accumulates lines. Quote depth prefixes each line with "> ".
type textOut struct {
	lines   []string
	cur     strings.Builder
	curOpen bool
	brk     int // pending break: 0 none, 1 newline, 2 blank line
	spc     bool
	// curQuote is the open line's quote depth, lastQuote the previous
	// line's; a blank line between them takes the shallower.
	curQuote, lastQuote int
}

func (o *textOut) startLine(quote int) {
	if o.curOpen && o.brk == 0 {
		return
	}
	if o.curOpen {
		o.lines = append(o.lines, strings.TrimRight(o.cur.String(), " "))
		o.cur.Reset()
		o.curOpen = false
		o.lastQuote = o.curQuote
	}
	if o.brk == 2 && len(o.lines) > 0 && strings.Trim(o.lines[len(o.lines)-1], "> ") != "" {
		o.lines = append(o.lines, strings.TrimRight(strings.Repeat("> ", min(quote, o.lastQuote)), " "))
	}
	o.brk = 0
	o.spc = false
	o.cur.WriteString(strings.Repeat("> ", quote))
	o.curOpen = true
	o.curQuote = quote
}

func (o *textOut) words(s string, quote int) {
	f := strings.FieldsFunc(s, isSpace)
	if len(f) == 0 {
		if s != "" && o.curOpen {
			o.spc = true
		}
		return
	}
	lead := isSpace(firstRune(s))
	o.startLine(quote)
	if (lead || o.spc) && !o.atLineStart(quote) {
		o.cur.WriteByte(' ')
	}
	o.cur.WriteString(strings.Join(f, " "))
	o.spc = isSpace(lastRune(s))
}

func (o *textOut) atLineStart(quote int) bool {
	return o.cur.Len() == len(strings.Repeat("> ", quote))
}

func (o *textOut) pre(s string, quote int) {
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			o.newline(quote)
		}
		if line == "" {
			continue
		}
		o.startLine(quote)
		o.cur.WriteString(strings.TrimRight(line, "\r"))
	}
}

func (o *textOut) space() {
	if o.curOpen {
		o.spc = true
	}
}

// newline ends the current line, as a block element does.
func (o *textOut) newline(int) {
	o.brk = max(o.brk, 1)
}

// br is a line break: two in a row make a blank line.
func (o *textOut) br() {
	if o.brk >= 1 {
		o.brk = 2
		return
	}
	o.brk = 1
}

func (o *textOut) para(int) {
	o.brk = 2
}

func (o *textOut) finish() string {
	if o.curOpen {
		o.lines = append(o.lines, strings.TrimRight(o.cur.String(), " "))
		o.cur.Reset()
		o.curOpen = false
	}
	// Collapse blank-line runs and trim both ends.
	var out []string
	blank := 0
	for _, l := range o.lines {
		if strings.Trim(l, "> ") == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}
