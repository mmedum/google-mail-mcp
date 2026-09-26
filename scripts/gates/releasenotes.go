package main

import (
	"fmt"
	"io"
	"os"
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
// either end removed. It stops at the next h2 and at the link-reference
// footer, and reads nothing inside a fenced block as either.
func releaseNotesSection(changelog, version string) string {
	want := "## [" + version + "]"
	var body []string
	inside, fenced := false, false
	for line := range strings.SplitSeq(changelog, "\n") {
		line = strings.TrimRight(line, "\r")
		if !inside {
			if strings.HasPrefix(line, want) {
				inside = true
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		} else if !fenced && (strings.HasPrefix(line, "## ") || changelogLinkDefinition.MatchString(line)) {
			break
		}
		body = append(body, line)
	}
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	return strings.Join(body, "\n")
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
