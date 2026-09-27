package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// The record files — API verdicts, field verdicts, live-driver waivers —
// share one reader, so they cannot disagree about blank lines, comments,
// a trailing TAB or a key listed twice.

// tsvRow is one record line: its fields, and the line it came from.
type tsvRow struct {
	fields []string
	line   int
}

// readTSV reads a tab-separated record file, skipping blank lines and
// lines starting with #. cols is how many fields a row must carry, and
// the first field is the key that must be unique.
//
// Problems are returned rather than raised, so a person editing the
// file sees every mistake at once.
func readTSV(path string, cols int) ([]tsvRow, []string) {
	f, err := os.Open(path) //nolint:gosec // a path this repository owns
	if err != nil {
		return nil, []string{"cannot read " + path + ": " + err.Error()}
	}
	defer func() { _ = f.Close() }()

	var rows []tsvRow
	var problems []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		// Spaces and a carriage return go; a trailing TAB does not,
		// because it is an empty last field, and "no reason given" is a
		// better message than "wrong number of columns".
		line := strings.TrimRight(scanner.Text(), " \r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != cols {
			problems = append(problems, fmt.Sprintf("%s:%d: want %d tab-separated columns, got %d",
				path, n, cols, len(fields)))
			continue
		}
		if name := fields[0]; seen[name] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s is listed twice", path, n, name))
		} else {
			seen[name] = true
		}
		rows = append(rows, tsvRow{fields: fields, line: n})
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, path+": "+err.Error())
	}
	return rows, problems
}
