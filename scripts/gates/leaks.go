package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// leaks looks for anything from a real mailbox or account in the files
// git would commit (docs/architecture.md §9.1).
//
// Every rule is an allow-list anchored on a shape the server's generated
// fields cannot take: an @ with a dotted domain, a Gmail web URL, a
// keyword before a 16-hex id, the Google OAuth shapes. A deny-list naming
// what to look for would itself be the disclosure. Subjects and bodies
// are ordinary words and no pattern finds them; that half is structural —
// fixtures are generated and the live driver reads only what it wrote.
func leaks(out io.Writer, _ []string) error {
	return leaksTree(out, ".", leaksMinFiles)
}

// leaksHistory runs the same rules over every blob, commit message and
// tag message reachable from any ref. A leak deleted from the tip is
// still in the log.
func leaksHistory(out io.Writer, _ []string) error {
	return leaksScanHistory(out, ".")
}

// leaksMinFiles is the floor on files read from the working tree.
const leaksMinFiles = 20

// leaksAllow is one exemption from a rule, with the reason it is safe.
type leaksAllow struct {
	re     *regexp.Regexp
	reason string
}

// leaksRule is one shape of leak.
type leaksRule struct {
	name string
	re   *regexp.Regexp
	// group is the submatch the allow-list is held against; 0 is the
	// whole match.
	group int
	allow []leaksAllow
}

// leaksFixtureID is the mark of a generated id: a counter formatted into
// 16 hex digits, with or without a Gmail-like prefix, leaves a run of at
// least eight zeros ("0000000000000001", "18f0000000000002"). A real
// Gmail id is derived from a millisecond clock and random bits, and a run
// that long is not something one carries by chance.
const leaksFixtureID = "00000000"

// leaksReservedDomain matches the domains RFC 2606 and RFC 6761 reserve
// for documentation and tests, and their subdomains.
var leaksReservedDomain = regexp.MustCompile(
	`(?i)@([A-Za-z0-9\-]+\.)*(example\.(com|org|net)|[A-Za-z0-9\-]+\.(example|test|invalid|localhost)|example|test|invalid|localhost)$`)

// leaksRules is the allow-list. Every entry in every allow carries its
// reason, which TestLeaksAllowListHasReasons asserts.
var leaksRules = []leaksRule{
	{
		name: "an address at a domain somebody could own",
		re:   regexp.MustCompile(`[A-Za-z0-9._%+\-=]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`),
		allow: []leaksAllow{
			{leaksReservedDomain, "RFC 2606 and RFC 6761 reserve these domains; they can never deliver"},
			{regexp.MustCompile(`^noreply@anthropic\.com$`),
				"the Co-Authored-By trailer on commits; a vendor's no-reply address that identifies nobody"},
			{regexp.MustCompile(`@users\.noreply\.github\.com$`),
				"GitHub's no-reply addresses, which exist so a commit carries no real address"},
		},
	},
	{
		// Unanchored on purpose: this finds a link anywhere in committed
		// text. It never decides whether a URL may be fetched, which is
		// what an anchor would guard.
		name: "a Gmail web link to a mailbox",
		re:   regexp.MustCompile(`mail\.google\.com/mail/u/[0-9]+`),
	},
	{
		// A keyword and a separator, then 16 hex digits. The keyword is
		// the anchor: a bare hex run is a hash or a timestamp as often as
		// an id.
		name:  "a message, thread or draft id outside the fixture range",
		re:    regexp.MustCompile(`(?i)\b(?:message|thread|draft|msg)[a-z_]*(?:id)?["']?\s*[:=/ ]\s*["']?([0-9a-f]{16})\b`),
		group: 1,
		allow: []leaksAllow{
			{regexp.MustCompile(leaksFixtureID), "a generated id carries a run of eight zeros, which a real one does not"},
		},
	},
	{
		name: "an OAuth client id",
		re:   regexp.MustCompile(`\b[0-9]{6,}-[a-z0-9]{20,}\.apps\.googleusercontent\.com\b`),
	},
	{
		name: "an OAuth client secret",
		re:   regexp.MustCompile(`GOCSPX-[0-9A-Za-z_\-]{10,}`),
	},
	{
		name: "an OAuth refresh token",
		re:   regexp.MustCompile(`\b1//0[0-9A-Za-z_\-]{20,}`),
	},
	{
		name: "an OAuth access token",
		re:   regexp.MustCompile(`\bya29\.[0-9A-Za-z_\-]{20,}`),
	},
	{
		name: "a Google API key",
		re:   regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),
	},
}

// leaksSkip are files of machine-generated hashes, by repository path.
var leaksSkip = map[string]string{
	"go.sum": "module hashes; nothing in it was written by a person",
}

// leaksExecutableMagic starts a linked executable. A binary built beside
// the source and swept in by `git add -A` is the finding itself.
var leaksExecutableMagic = [][]byte{
	[]byte("\x7fELF"),                                  // Linux, BSD
	[]byte("MZ"),                                       // Windows PE
	{0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}, // Mach-O, 32-bit
	{0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}, // Mach-O, 64-bit
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal
}

// leaksMaxBinary is what an ordinary binary file may weigh. None is
// tracked today; a fixture that needs to be binary is far under this.
const leaksMaxBinary = 1 << 20

// leaksFind reports every leak in text, each redacted.
func leaksFind(text string) []string {
	var out []string
	for _, rule := range leaksRules {
		for _, m := range rule.re.FindAllStringSubmatch(text, -1) {
			value := m[rule.group]
			if slices.ContainsFunc(rule.allow, func(a leaksAllow) bool { return a.re.MatchString(value) }) {
				continue
			}
			out = append(out, rule.name+": "+leaksRedact(m[0]))
		}
	}
	return out
}

// leaksRedact shortens a finding so the report does not reprint it:
// enough to find it with a search, not enough to read it.
func leaksRedact(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "…" + s[len(s)-2:] + " (" + strconv.Itoa(len(s)) + " chars)"
}

// leaksBinary reports whether data is not text: a NUL in the first 8000
// bytes, which is git's own rule.
func leaksBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// leaksArtifact reports why a binary file does not belong, or "".
func leaksArtifact(data []byte) string {
	for _, magic := range leaksExecutableMagic {
		if bytes.HasPrefix(data, magic) {
			return fmt.Sprintf("a compiled executable (%d bytes); nothing built belongs in the tree", len(data))
		}
	}
	if len(data) > leaksMaxBinary {
		return fmt.Sprintf("%d bytes of binary, past the %d-byte limit; no rule here can read it", len(data), leaksMaxBinary)
	}
	return ""
}

// leaksTree scans what git would commit from root: tracked files and
// untracked files that are not ignored. The untracked half is where a
// phase's new files are, before anything else has read them.
func leaksTree(out io.Writer, root string, minFiles int) error {
	files, err := gitFiles(root)
	if err != nil {
		return err
	}
	var findings []string
	scanned, binaries := 0, 0
	for _, name := range files {
		if _, skip := leaksSkip[name]; skip {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		if leaksBinary(data) {
			binaries++
			if why := leaksArtifact(data); why != "" {
				findings = append(findings, name+": "+why)
			}
			continue
		}
		scanned++
		for _, f := range leaksFind(string(data)) {
			findings = append(findings, name+": "+f)
		}
	}
	if len(findings) > 0 {
		slices.Sort(findings)
		for _, f := range findings {
			_, _ = fmt.Fprintln(out, "  "+f)
		}
		return fmt.Errorf("%d finding(s) that look like they came from a real mailbox or account", len(findings))
	}
	if scanned < minFiles {
		return fmt.Errorf("read %d text file(s), want at least %d; the scan is not seeing the repository", scanned, minFiles)
	}
	_, _ = fmt.Fprintf(out, "leaks ok (%d text files and %d binary files, %d rules)\n", scanned, binaries, len(leaksRules))
	return nil
}

// leaksObject is one git object read from `git cat-file --batch`.
type leaksObject struct {
	sha, kind string
	body      []byte
}

// leaksScanHistory scans every blob and every commit and tag message.
//
// A commit or tag is scanned from its message down: the header lines
// carry the author, committer and tagger that git writes itself, which are
// public in every repository by construction.
func leaksScanHistory(out io.Writer, root string) error {
	shas, paths, err := leaksHistoryObjects(root)
	if err != nil {
		return err
	}
	var h leaksHistoryScan
	if err := leaksCatFile(root, shas, func(o leaksObject) { h.scan(o, paths) }); err != nil {
		return err
	}

	// The floor, derived from git rather than guessed: every commit
	// reachable from a ref must have been read.
	count, err := gitOutput(root, "rev-list", "--all", "--count")
	if err != nil {
		return err
	}
	want, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return fmt.Errorf("rev-list --count: %w", err)
	}
	if len(h.findings) > 0 {
		slices.Sort(h.findings)
		for _, f := range h.findings {
			_, _ = fmt.Fprintln(out, "  "+f)
		}
		return fmt.Errorf("%d finding(s) in the history", len(h.findings))
	}
	if want == 0 || h.commits < want || h.blobs == 0 {
		return fmt.Errorf("read %d commit(s) of %d and %d blob(s); the scan is not seeing the history",
			h.commits, want, h.blobs)
	}
	_, _ = fmt.Fprintf(out, "history leaks ok (%d commits, %d tag messages, %d blobs)\n", h.commits, h.tagMsgs, h.blobs)
	return nil
}

// leaksHistoryObjects lists every object reachable from a ref, and every
// annotated tag, which is an object of its own. paths maps a blob to the
// path it was first seen at.
func leaksHistoryObjects(root string) (shas []string, paths map[string]string, err error) {
	listing, err := gitOutput(root, "rev-list", "--all", "--objects")
	if err != nil {
		return nil, nil, err
	}
	paths = map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(listing), "\n") {
		sha, path, _ := strings.Cut(line, " ")
		if sha == "" {
			continue
		}
		if _, seen := paths[sha]; !seen {
			shas = append(shas, sha)
		}
		paths[sha] = path
	}
	tags, err := gitOutput(root, "for-each-ref", "--format=%(objectname) %(objecttype)", "refs/tags")
	if err != nil {
		return nil, nil, err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(tags), "\n") {
		sha, kind, _ := strings.Cut(line, " ")
		if _, seen := paths[sha]; kind == "tag" && !seen {
			shas = append(shas, sha)
			paths[sha] = ""
		}
	}
	return shas, paths, nil
}

// leaksHistoryScan accumulates what a history scan read and found.
type leaksHistoryScan struct {
	findings                []string
	blobs, commits, tagMsgs int
}

// scan reads one object: a blob's content, or a commit's or tag's
// message.
func (h *leaksHistoryScan) scan(o leaksObject, paths map[string]string) {
	where := o.kind + " " + o.sha[:min(8, len(o.sha))]
	switch o.kind {
	case "blob":
		path := paths[o.sha]
		if _, skip := leaksSkip[path]; skip {
			return
		}
		h.blobs++
		where = path + "@" + o.sha[:8]
		if leaksBinary(o.body) {
			if why := leaksArtifact(o.body); why != "" {
				h.findings = append(h.findings, where+": "+why)
			}
			return
		}
		for _, f := range leaksFind(string(o.body)) {
			h.findings = append(h.findings, where+": "+f)
		}
	case "commit", "tag":
		if o.kind == "commit" {
			h.commits++
		} else {
			h.tagMsgs++
		}
		_, message, _ := strings.Cut(string(o.body), "\n\n")
		for _, f := range leaksFind(message) {
			h.findings = append(h.findings, where+" message: "+f)
		}
	}
}

// leaksCatFile reads objects through one `git cat-file --batch` and
// hands each to fn as it arrives, so the history is never held whole.
func leaksCatFile(root string, shas []string, fn func(leaksObject)) error {
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(shas, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git cat-file: %w", err)
	}
	readErr := leaksReadBatch(bufio.NewReader(stdout), len(shas), fn)
	if readErr != nil {
		// Drain what is left so git can exit rather than block on a
		// full pipe.
		_, _ = io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return readErr
}

// leaksReadBatch reads n objects in `git cat-file --batch` form.
func leaksReadBatch(r *bufio.Reader, n int, fn func(leaksObject)) error {
	for range n {
		header, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("git cat-file: short output: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			return fmt.Errorf("git cat-file: %q", strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return fmt.Errorf("git cat-file: %q", strings.TrimSpace(header))
		}
		body := make([]byte, size+1) // the object and its trailing newline
		if _, err := io.ReadFull(r, body); err != nil {
			return fmt.Errorf("git cat-file: %w", err)
		}
		fn(leaksObject{sha: fields[0], kind: fields[1], body: body[:size]})
	}
	return nil
}
