package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// schemaAck fails a pull request whose tool surface differs from its
// merge-base's, unless the commits it adds name every item that changed
// in a footer. Annotations count: a flipped destructiveHint changes
// whether a client asks the person. Both sides are built from commits,
// the merge-base and the head, never from the checkout, which in CI is
// the merge commit.
//
// A gate that could not run, a failed build or git call, says so, and a
// built binary exits 2 for it; a refusal exits 1.
func schemaAck(out io.Writer, args []string) error {
	c, messages, base, err := ackInputs(args[0], args[1])
	if err != nil {
		return couldNotRun{err}
	}
	return checkAck(out, c, messages, base)
}

// ackInputs is what checkAck judges: the changes from the merge-base of
// baseRef and head to head, with each break kept as one only if it also
// breaks the newest release; the messages of the commits in between; and
// the merge-base, shortened.
func ackInputs(baseRef, head string) (surfaceChanges, []string, string, error) {
	base, err := git("merge-base", baseRef, head)
	if err != nil {
		return surfaceChanges{}, nil, "", err
	}
	was, err := surfaceAt(base)
	if err != nil {
		return surfaceChanges{}, nil, "", err
	}
	now, err := surfaceAt(head)
	if err != nil {
		return surfaceChanges{}, nil, "", err
	}
	c := schemaDiffCompare(was, now)
	if len(c.breaking) > 0 {
		released, err := releasedBreaks(baseRef, now)
		if err != nil {
			return surfaceChanges{}, nil, "", err
		}
		c = releaseBreaksOnly(c, released)
	}
	log, err := git("log", "--format=%B%x00", base+".."+head)
	if err != nil {
		return surfaceChanges{}, nil, "", err
	}
	return c, strings.Split(log, "\x00"), base[:min(12, len(base))], nil
}

// surfaceAt is the tool surface of the server built at rev.
func surfaceAt(rev string) (*schemaDiffSurface, error) {
	raw, err := dumpAt(rev)
	if err != nil {
		return nil, err
	}
	return schemaDiffDecode(raw, "the build at "+rev)
}

// releasedBreaks is what head breaks of the newest release's surface: the
// baseline the base branch records, not the head's, which a branch cut
// before the last release would leave behind. Before the first release
// there is no baseline, and nothing is a break.
func releasedBreaks(baseRef string, head *schemaDiffSurface) ([]change, error) {
	listed, err := git("ls-tree", "--name-only", baseRef, "--", schemaDiffBaseline)
	if err != nil || listed == "" {
		return nil, err
	}
	data, err := git("show", baseRef+":"+schemaDiffBaseline)
	if err != nil {
		return nil, err
	}
	release, err := schemaDiffDecode([]byte(data), schemaDiffBaseline+" on "+baseRef)
	if err != nil {
		return nil, err
	}
	return schemaDiffCompare(release, head).breaking, nil
}

// releaseBreaksOnly keeps as breaks only those the head also makes
// against the released surface. Any other break narrows something no
// release shipped, so it breaks no caller and is a plain change. A break
// is matched by its item and field, never its text, which can differ
// between the two comparisons: a type change names the type it was.
func releaseBreaksOnly(c surfaceChanges, released []change) surfaceChanges {
	type spot struct{ item, field string }
	inRelease := map[spot]bool{}
	for _, b := range released {
		inRelease[spot{b.item, b.field}] = true
	}
	var breaking []change
	for _, b := range c.breaking {
		if inRelease[spot{b.item, b.field}] {
			breaking = append(breaking, b)
		} else {
			c.changed = append(c.changed, b)
		}
	}
	c.breaking = breaking
	return c
}

// checkAck is the rule, over the changes and the commit messages, so a
// test can hand it both. Every added or changed item must be named in an
// acknowledging footer, and every break in a BREAKING CHANGE: one. It
// reports the changes on w, and a refusal names the footer that fixes it.
func checkAck(w io.Writer, c surfaceChanges, messages []string, base string) error {
	if c.empty() {
		_, _ = fmt.Fprintf(w, "the tool surface is unchanged since %s\n", base)
		return nil
	}
	_, _ = fmt.Fprintf(w, "against the merge-base %s: %d added, %d changed, %d breaking\n",
		base, len(c.added), len(c.changed), len(c.breaking))
	c.write(w)
	named, breaks := acknowledged(messages)
	var problems, fix []string
	breakItems := unnamed(c.breaking, breaks)
	if len(breakItems) > 0 {
		problems = append(problems, "no BREAKING CHANGE: footer names "+strings.Join(breakItems, ", "))
		fix = append(fix, "BREAKING CHANGE: "+footerList(breakItems)+": what broke")
	}
	// A BREAKING CHANGE: footer names an item for both checks, so an
	// unnamed break is not asked for twice.
	items := unnamed(slices.Concat(c.added, c.changed), named)
	if rest := slices.DeleteFunc(items, func(i string) bool { return slices.Contains(breakItems, i) }); len(rest) > 0 {
		problems = append(problems, "no footer names "+strings.Join(rest, ", "))
		fix = append(fix, "SCHEMA-CHANGE: "+footerList(rest)+": what changed")
	}
	if len(problems) > 0 {
		return fmt.Errorf("the tool surface changed since %s, and %s. Add an empty commit (git commit --allow-empty), "+
			"never an amend, whose message ends with:\n\n%s", base, strings.Join(problems, ", and "), strings.Join(fix, "\n"))
	}
	_, _ = fmt.Fprintf(w, "the change since %s is acknowledged\n", base)
	return nil
}

// footerList is names as a footer has to write them: a name the footer
// would split, or trim, in backticks, so the footer printed is one that
// passes.
func footerList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n
		if nameSeparator.MatchString(n) || strings.TrimRight(n, ":.") != n {
			out[i] = "`" + n + "`"
		}
	}
	return strings.Join(out, ", ")
}

// unnamed is each item of cs that names does not hold, sorted, once.
func unnamed(cs []change, names map[string]bool) []string {
	var out []string
	for _, c := range cs {
		if !names[c.item] {
			out = append(out, c.item)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// footerStart is a line that starts a footer: a token, then ": " or
// " #", then its value, as Co-Authored-By: and Closes #12 are
// (Conventional Commits 1.0.0, items 8 and 9).
var footerStart = regexp.MustCompile(`^(BREAKING CHANGE|[A-Za-z][A-Za-z0-9-]*)(:[ \t]+| #)(\S.*)$`)

// paragraphBreak is one or more blank lines, which end a paragraph.
var paragraphBreak = regexp.MustCompile(`\n([ \t]*\n)+`)

// A footer is one token and its value, the lines that continue it joined
// on, each on its own line. ref marks a "Token #value" footer, which
// refers to something, such as an issue, rather than describing.
type footer struct {
	token, text string
	ref         bool
}

// footers is a commit message's footer block: the paragraphs at its end
// whose first line starts a footer, never the subject. Inside one, a line
// that starts a footer ends the value before it, and any other line
// continues that value, wrapped at the margin or indented (item 10). A
// paragraph whose first line is prose ends the block, so a line of prose
// that happens to start "BREAKING CHANGE:" is not a footer.
func footers(message string) []footer {
	paras := paragraphBreak.Split(strings.TrimSpace(strings.ReplaceAll(message, "\r\n", "\n")), -1)
	var out []footer
	for i := len(paras) - 1; i > 0; i-- {
		var block []footer
		for _, l := range strings.Split(strings.Trim(paras[i], "\n"), "\n") {
			switch m := footerStart.FindStringSubmatch(l); {
			case m != nil:
				block = append(block, footer{m[1], m[3], m[2] == " #"})
			case len(block) > 0:
				block[len(block)-1].text += "\n" + strings.TrimSpace(l)
			default:
				return out
			}
		}
		out = append(block, out...)
	}
	return out
}

// ackToken says whether a footer's token acknowledges a change to the
// tool surface, and whether a break of a release. Conventional Commits
// reads a token in any case but BREAKING CHANGE, which must be
// uppercase, and BREAKING-CHANGE is its synonym.
func ackToken(token string) (isBreak, ok bool) {
	switch {
	case token == "BREAKING CHANGE" || token == "BREAKING-CHANGE":
		return true, true
	case strings.EqualFold(token, "SCHEMA-CHANGE"):
		return false, true
	}
	return false, false
}

// nameSeparator is what separates the names in a footer's text.
var nameSeparator = regexp.MustCompile("[\\s,;()'\"]+")

// descriptionStart is the colon and white space that end a footer's list
// of names.
var descriptionStart = regexp.MustCompile(`:\s`)

// footerNames is every name a footer's value lists: the part before its
// first colon and white space, ending sooner at a line that does not end
// with a comma, each an item between commas. What follows is the
// description, free text that names nothing, so a tool whose name is an
// ordinary word is not acknowledged by a sentence that uses it. A colon
// and a full stop are trimmed from a name rather than split on, since a
// URI holds both. A name in backticks is taken whole, trimmed of
// nothing, so a URI with a comma in it, or one ending in a full stop,
// can be named.
func footerNames(text string) []string {
	list := text
	if at := descriptionStart.FindStringIndex(text); at != nil {
		list = text[:at[0]]
	}
	// A list wraps only after a comma: a line that does not end with one
	// ends it, and what follows describes, so prose on the next line of a
	// bare list names nothing.
	lines := strings.Split(list, "\n")
	for i, l := range lines {
		if !strings.HasSuffix(strings.TrimSpace(l), ",") {
			list = strings.Join(lines[:i+1], "\n")
			break
		}
	}
	var out []string
	for _, m := range stalenessCodeSpan.FindAllStringSubmatch(list, -1) {
		out = append(out, m[1])
	}
	// The rest is split on commas alone, so prose is one item that names
	// nothing rather than words that each might.
	for _, n := range strings.Split(stalenessCodeSpan.ReplaceAllString(list, ""), ",") {
		n = strings.TrimRight(strings.Trim(n, " \t\n;()'\""), ":.")
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// acknowledged reads every message's footers for the items they name:
// named holds each one a SCHEMA-CHANGE: or BREAKING CHANGE: footer
// names, breaks each one a BREAKING CHANGE: footer names. A name is a
// whole token, so get_message does not name get_messages.
func acknowledged(messages []string) (named, breaks map[string]bool) {
	named, breaks = map[string]bool{}, map[string]bool{}
	for _, message := range messages {
		for _, f := range footers(message) {
			// A "Token #value" footer acknowledges nothing: Conventional
			// Commits writes BREAKING CHANGE with a colon and a space (item
			// 12), and its value has no description to tell from names.
			if f.ref {
				continue
			}
			isBreak, ok := ackToken(f.token)
			if !ok {
				continue
			}
			for _, n := range footerNames(f.text) {
				named[n] = true
				if isBreak {
					breaks[n] = true
				}
			}
		}
	}
	return named, breaks
}

// dumpAt builds the server as it was at a revision, trimmed and without
// cgo as `make build` does, and dumps its surface. The worktree is
// removed on every path out, a failed build included, and a removal that
// fails says so rather than leave a registration that breaks the next
// run.
func dumpAt(rev string) (raw []byte, err error) {
	tmp, err := os.MkdirTemp("", "gates-schema-*")
	if err != nil {
		return nil, err
	}
	src := filepath.Join(tmp, "src")
	// The cleanup runs on every path out, a failed checkout included,
	// which can leave its registration behind: prune drops it once the
	// directory is gone.
	defer func() {
		_, rmErr := git("worktree", "remove", "--force", src)
		_ = os.RemoveAll(tmp)
		_, _ = git("worktree", "prune")
		if rmErr != nil && err == nil {
			err = rmErr
		}
	}()
	if _, err := git("worktree", "add", "--quiet", "--detach", src, rev); err != nil {
		return nil, err
	}
	bin := filepath.Join(tmp, binaryName+exeSuffix())
	build := exec.Command("go", "build", "-trimpath", "-o", bin, mainPackage)
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build %s: %w: %s", rev, err, out)
	}
	_, raw, err = dumpSchemas(bin)
	return raw, err
}

// git runs git in the working directory and returns its output trimmed.
func git(args ...string) (string, error) {
	out, err := gitOutput(".", args...)
	return strings.TrimSpace(out), err
}
