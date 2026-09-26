package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// The CHANGELOG gates: a pull request that changes what ships adds an
// entry under [Unreleased] (`changelog`, CI only, since it needs the
// pull request's base), and every heading has its link reference
// (`changelog-links`, in check, since it reads one file).

const changelogFile = "CHANGELOG.md"

// changelogWatched are the trees a reader of the changelog expects to
// see named there. Tests and fixtures under them are not: they change
// nothing a user runs.
var changelogWatched = []string{"cmd/", "internal/", "packaging/", "go.mod"}

var (
	changelogVersionHeading = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)\]`)
	changelogLinkDefinition = regexp.MustCompile(`(?m)^\[([^\]]+)\]:\s*(\S+)`)
)

func changelog(out io.Writer, args []string) error {
	return changelogCheck(out, ".", args[0], args[1])
}

// changelogCheck fails when the diff from base to head touches watched
// source without adding a line under [Unreleased].
func changelogCheck(out io.Writer, root, base, head string) error {
	changed, err := gitOutput(root, "diff", "--name-only", base, head)
	if err != nil {
		return err
	}
	files := strings.Fields(changed)
	watched := changelogTouched(files)
	if len(watched) == 0 {
		_, _ = fmt.Fprintf(out, "changelog: %d files changed, none of them shipped source; no entry needed\n", len(files))
		return nil
	}
	// A large context, so the [Unreleased] heading is in the hunk however
	// far below it the entry sits.
	diff, err := gitOutput(root, "diff", "--unified=99999", base, head, "--", changelogFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) == "" {
		return fmt.Errorf("%d shipped file(s) changed (%s) and %s did not", len(watched),
			strings.Join(watched[:min(3, len(watched))], ", "), changelogFile)
	}
	n := changelogAddedUnderUnreleased(diff)
	if n == 0 {
		return fmt.Errorf("%s changed and no line was added under [Unreleased]; an entry under a released "+
			"heading is invisible to the release it belongs to", changelogFile)
	}
	_, _ = fmt.Fprintf(out, "changelog ok: %d shipped file(s) changed, %d line(s) added under [Unreleased]\n", len(watched), n)
	return nil
}

// changelogTouched are the changed files a changelog reader cares about.
func changelogTouched(files []string) []string {
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.Contains(f, "/testdata/") {
			continue
		}
		for _, p := range changelogWatched {
			if strings.HasPrefix(f, p) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// changelogAddedUnderUnreleased counts added lines under [Unreleased].
//
// A release cut renames [Unreleased] to the version, so its entries no
// longer sit under [Unreleased] and are still where they belong. When
// the diff removes the [Unreleased] heading, the version heading it
// added is read as the unreleased section.
func changelogAddedUnderUnreleased(diff string) int {
	cut := strings.Contains(diff, "\n-## [Unreleased]")
	count, inHunk, inside := 0, false, false
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+## [") || strings.HasPrefix(line, " ## ["):
			inside = strings.Contains(line, "[Unreleased]") || (cut && strings.HasPrefix(line, "+## ["))
		case inside && strings.HasPrefix(line, "+") && strings.TrimSpace(line[1:]) != "":
			count++
		}
	}
	return count
}

func changelogLinks(out io.Writer, _ []string) error {
	data, err := os.ReadFile(changelogFile)
	if err != nil {
		return err
	}
	versions, problems := changelogLinkProblems(string(data))
	if err := problemsError(out, changelogFile, problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "changelog-links ok: [Unreleased] and %d version heading(s), every one linked\n", versions)
	return nil
}

// changelogLinkProblems holds the link references against the headings.
// Before the first release there are no version headings, and
// [Unreleased] must still be there and linked.
func changelogLinkProblems(text string) (int, []string) {
	var problems []string
	if !strings.Contains(text, "\n## [Unreleased]") && !strings.HasPrefix(text, "## [Unreleased]") {
		problems = append(problems, "there is no ## [Unreleased] heading, so there is nowhere for the next entry")
	}
	headings := changelogVersionHeading.FindAllStringSubmatch(text, -1)
	versions := make([]string, 0, len(headings))
	for _, m := range headings {
		versions = append(versions, m[1])
	}
	links := map[string]string{}
	for _, m := range changelogLinkDefinition.FindAllStringSubmatch(text, -1) {
		links[m[1]] = m[2]
	}
	unreleased, ok := links["Unreleased"]
	if !ok {
		problems = append(problems, "[Unreleased] has no link reference, so the heading renders as literal brackets")
	} else if !strings.HasPrefix(unreleased, "https://") {
		problems = append(problems, fmt.Sprintf("[Unreleased] links to %q, which is not an https URL", unreleased))
	}
	for _, v := range versions {
		if _, ok := links[v]; !ok {
			problems = append(problems, fmt.Sprintf("## [%s] has no [%s]: link reference", v, v))
		}
	}
	// [Unreleased] compares from the newest release, or it claims that
	// release's work has not shipped.
	if len(versions) > 0 && ok {
		if want := "v" + versions[0] + "..."; !strings.Contains(unreleased, want) {
			problems = append(problems, fmt.Sprintf("[Unreleased] compares from %q and the newest release is %s; "+
				"it should compare from %s", unreleased, versions[0], want))
		}
	}
	for name := range links {
		if name != "Unreleased" && !slices.Contains(versions, name) && changelogVersionHeading.MatchString("## ["+name+"]") {
			problems = append(problems, fmt.Sprintf("[%s]: is a link reference with no heading", name))
		}
	}
	slices.Sort(problems)
	return len(versions), problems
}
