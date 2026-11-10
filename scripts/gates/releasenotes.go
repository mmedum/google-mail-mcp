package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// The release body is the CHANGELOG section for the tag, verbatim: it
// was written for somebody deciding whether to upgrade, and commit
// subjects were not. release.yml writes it outside the checkout, since
// an untracked file makes goreleaser's tree dirty.

func releaseNotes(out io.Writer, args []string) error {
	file := changelogFile
	if len(args) > 1 {
		file = args[1]
	}
	raw, err := os.ReadFile(file) //nolint:gosec // a repository path, or one a test names
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(args[0], "v")
	body := releaseNotesSection(string(raw), version)
	if !releaseNotesHasContent(body) {
		return fmt.Errorf("%s has no entries under ## [%s]; the release body would be empty, and a tag is "+
			"not the moment to find the entry was never written", file, version)
	}
	_, err = fmt.Fprintln(out, body)
	return err
}

// releaseNotesSection is the text under `## [version]`, blank lines at
// either end removed.
func releaseNotesSection(changelog, version string) string {
	lines, from, to := changelogSection(changelog, version)
	body := lines[from:to]
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	return strings.Join(body, "\n")
}

// changelogFooter is a link-reference line of the footer, with its URL
// or not yet: either ends a section.
var changelogFooter = regexp.MustCompile(`^\[[^\]]+\]:`)

// changelogSection is a changelog's lines, CRLF read as LF, and the range
// of them under `## [version]`, empty when there is no such heading. It
// stops at the next h2 and at the link-reference footer, and reads nothing
// inside fenced code as either. The docs gate scans the [Unreleased] range
// it gives, so the scan reads what a release note will say.
func changelogSection(changelog, version string) (lines []string, from, to int) {
	lines = strings.Split(strings.ReplaceAll(changelog, "\r\n", "\n"), "\n")
	want := "## [" + version + "]"
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) })
	if start < 0 {
		return lines, 0, 0
	}
	var fence docsFence
	for to = start + 1; to < len(lines); to++ {
		line := lines[to]
		if !fence.code(line) && (docsEndsSection(line, 2) || changelogFooter.MatchString(line)) {
			break
		}
	}
	return lines, start + 1, to
}

// releaseNotesHasContent reports whether a section says anything beyond
// its subheadings.
func releaseNotesHasContent(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			return true
		}
	}
	return false
}
