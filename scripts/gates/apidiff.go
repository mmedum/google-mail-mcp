package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// apiDiffTimeout bounds the fetch, so a slow network fails rather than
// hangs.
const apiDiffTimeout = 60 * time.Second

// apiDiffMinMethods refuses to write a snapshot that has lost most of
// the API, which is a broken fetch rather than a changed API.
const apiDiffMinMethods = 70

// apiDiff refetches the discovery document and rewrites the snapshot.
//
// Manual, because it needs the network; `check` reads the committed
// snapshot. On any failure it leaves the committed file untouched.
func apiDiff(out io.Writer, _ []string) error {
	client := &http.Client{Timeout: apiDiffTimeout}
	return apiDiffWith(client, discoveryURL, discoverySnapshotPath, time.Now(), out)
}

// apiDiffWith is apiDiff with its inputs named, so a test can point it
// at a closed port and a temporary snapshot.
func apiDiffWith(client *http.Client, url, path string, now time.Time, out io.Writer) error {
	const untouched = " (the committed snapshot is unchanged)"
	resp, err := client.Get(url) //nolint:noctx // bounded by the client's timeout
	if err != nil {
		return fmt.Errorf("fetch the discovery document: %w"+untouched, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the discovery document answered %d"+untouched, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read the discovery document: %w"+untouched, err)
	}
	next, err := discoveryParse(raw, url)
	if err != nil {
		return fmt.Errorf("%w"+untouched, err)
	}
	if len(next.Methods) < apiDiffMinMethods {
		return fmt.Errorf("the discovery document yielded %d methods, below the floor of %d"+untouched,
			len(next.Methods), apiDiffMinMethods)
	}
	next.Fetched = now.UTC().Format("2006-01-02")

	before, loadErr := discoveryLoad(path)
	buf, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, append(buf, '\n')); err != nil {
		return err
	}
	if loadErr != nil {
		_, _ = fmt.Fprintf(out, "no previous snapshot (%v)\n", loadErr)
	} else {
		for _, line := range apiDiffChanges(before, next) {
			_, _ = fmt.Fprintln(out, "  "+line)
		}
	}
	fields := 0
	for _, s := range next.Schemas {
		fields += len(s.Fields)
	}
	_, _ = fmt.Fprintf(out, "snapshot written: %d methods, %d schemas, %d fields, revision %s\n",
		len(next.Methods), len(next.Schemas), fields, next.Revision)
	return nil
}

// apiDiffChanges names what appeared, vanished or moved between two
// snapshots: each is a verdict somebody has to write or remove.
func apiDiffChanges(before, after *discoverySurface) []string {
	var lines []string
	had, has := before.methodByID(), after.methodByID()
	for _, id := range slices.Sorted(maps.Keys(has)) {
		m := has[id]
		old, ok := had[id]
		switch {
		case !ok:
			lines = append(lines, fmt.Sprintf("NEW     %s (%s %s): needs a row in testdata/api-coverage.tsv",
				id, m.Verb, m.Path))
		case old.Verb != m.Verb || old.Path != m.Path:
			lines = append(lines, fmt.Sprintf("CHANGED %s: %s %s -> %s %s", id, old.Verb, old.Path, m.Verb, m.Path))
		case !slices.Equal(old.Scopes, m.Scopes):
			lines = append(lines, fmt.Sprintf("SCOPES  %s: %s -> %s", id,
				strings.Join(old.Scopes, " "), strings.Join(m.Scopes, " ")))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(had)) {
		if _, ok := has[id]; !ok {
			lines = append(lines, "GONE    "+id+": remove its row from testdata/api-coverage.tsv")
		}
	}
	fieldSet := func(s *discoverySurface) map[string]bool {
		m := map[string]bool{}
		for _, sc := range s.Schemas {
			for _, f := range sc.Fields {
				m[sc.Name+"."+f] = true
			}
		}
		return m
	}
	oldFields, newFields := fieldSet(before), fieldSet(after)
	for _, f := range slices.Sorted(maps.Keys(newFields)) {
		if !oldFields[f] {
			lines = append(lines, "NEW     field "+f+": judge it in testdata/api-fields.tsv if its schema is judged")
		}
	}
	for _, f := range slices.Sorted(maps.Keys(oldFields)) {
		if !newFields[f] {
			lines = append(lines, "GONE    field "+f)
		}
	}
	if before.Revision != after.Revision {
		lines = append(lines, fmt.Sprintf("revision %s -> %s", before.Revision, after.Revision))
	}
	return lines
}
