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
	if items := unnamed(slices.Concat(c.added, c.changed), named); len(items) > 0 {
		problems = append(problems, "no footer names "+strings.Join(items, ", "))
		// A BREAKING CHANGE: footer names an item for both checks.
		if rest := slices.DeleteFunc(items, func(i string) bool { return slices.Contains(breakItems, i) }); len(rest) > 0 {
			fix = append(fix, "SCHEMA-CHANGE: "+footerList(rest)+": what changed")
		}
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

// footerToken is a footer line: a token, a colon, a space and a value,
// as Co-Authored-By: is. continuation is a line that starts with white
// space and continues the footer above it.
var (
	footerToken  = regexp.MustCompile(`^(BREAKING CHANGE|[A-Za-z][A-Za-z0-9-]*):[ \t]+(\S.*)$`)
	continuation = regexp.MustCompile(`^[ \t]+\S`)
)

// paragraphBreak is one or more blank lines, which end a paragraph.
var paragraphBreak = regexp.MustCompile(`\n([ \t]*\n)+`)

// A footer is one Token: value of a commit message, its continuation
// lines joined onto its value.
type footer struct{ token, text string }

// footers is a commit message's footer block: the paragraphs at its end
// in which every line is a footer or continues one, never the subject. A
// line of prose that happens to start "BREAKING CHANGE:" is not a footer.
func footers(message string) []footer {
	paras := paragraphBreak.Split(strings.TrimSpace(strings.ReplaceAll(message, "\r\n", "\n")), -1)
	var out []footer
	for i := len(paras) - 1; i > 0; i-- {
		var block []footer
		for _, l := range strings.Split(strings.Trim(paras[i], "\n"), "\n") {
			switch m := footerToken.FindStringSubmatch(l); {
			case m != nil:
				block = append(block, footer{m[1], m[2]})
			case len(block) > 0 && continuation.MatchString(l):
				block[len(block)-1].text += " " + strings.TrimSpace(l)
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

// footerNames is every name a footer's text may carry, each a whole
// token. A colon and a full stop are trimmed from a name rather than
// split on, since a URI holds both. A name in backticks is taken whole,
// trimmed of nothing, so a URI with a comma in it, or one ending in a
// full stop, can be named.
func footerNames(text string) []string {
	var out []string
	for _, m := range stalenessCodeSpan.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	for _, n := range nameSeparator.Split(stalenessCodeSpan.ReplaceAllString(text, " "), -1) {
		if n = strings.TrimRight(n, ":."); n != "" {
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
	if _, err := git("worktree", "add", "--quiet", "--detach", src, rev); err != nil {
		// A checkout that failed can leave its registration behind.
		_ = os.RemoveAll(tmp)
		_, _ = git("worktree", "prune")
		return nil, err
	}
	defer func() {
		_, rmErr := git("worktree", "remove", "--force", src)
		_ = os.RemoveAll(tmp)
		_, _ = git("worktree", "prune")
		if rmErr != nil && err == nil {
			err = rmErr
		}
	}()
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
