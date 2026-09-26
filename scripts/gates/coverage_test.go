package main

import (
	"fmt"
	"strings"
	"testing"
)

// coverageTestProfile builds a profile: for each package, blocks of one
// statement, the first `covered` of them hit. Every block appears twice,
// as it does when two test binaries loaded the package.
func coverageTestProfile(pkgs map[string][2]int) string {
	var b strings.Builder
	b.WriteString("mode: atomic\n")
	for pkg, n := range pkgs {
		for i := range n[0] {
			hits := 0
			if i < n[1] {
				hits = 1
			}
			line := fmt.Sprintf("%s/%s/f.go:%d.1,%d.2 1 %d\n", modulePath, pkg, i+1, i+1, hits)
			b.WriteString(line)
			b.WriteString(strings.Replace(line, fmt.Sprintf(" %d\n", hits), " 0\n", 1))
		}
	}
	return b.String()
}

func coverageTestRun(t *testing.T, pkgs map[string][2]int, list []string) ([]string, []string) {
	t.Helper()
	blocks, err := coverageRead(strings.NewReader(coverageTestProfile(pkgs)))
	if err != nil {
		t.Fatal(err)
	}
	return coverageScore(blocks, list)
}

var coverageTestPackages = []string{"cmd/google-mail-mcp", "internal/a", "internal/a/sub", "internal/b",
	"internal/c", "internal/version", "scripts/gates"}

func coverageTestGood() map[string][2]int {
	return map[string][2]int{
		"cmd/google-mail-mcp": {40, 26}, "internal/a": {30, 27}, "internal/a/sub": {20, 20},
		"internal/b": {20, 18}, "internal/c": {10, 9},
	}
}

func TestCoveragePassesAboveTheFloors(t *testing.T) {
	lines, problems := coverageTestRun(t, coverageTestGood(), coverageTestPackages)
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	table := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	for _, want := range []string{"internal/a 90.0% (floor 80%)", "cmd/google-mail-mcp 65.0% (floor 60%)",
		"internal/version exempt:", "scripts/gates exempt:"} {
		if !strings.Contains(table, want) {
			t.Errorf("the table does not say %q:\n%s", want, table)
		}
	}
}

// A package is scored on its own files. With the parent at 70% and a
// fully covered child, a prefix match would report the parent above 80%.
func TestCoverageScoresAPackageOnItsOwnFiles(t *testing.T) {
	pkgs := coverageTestGood()
	pkgs["internal/a"] = [2]int{30, 21}
	_, problems := coverageTestRun(t, pkgs, coverageTestPackages)
	if !strings.Contains(strings.Join(problems, "\n"), "internal/a is at 70.0%") {
		t.Errorf("the parent was not scored alone: %v", problems)
	}
}

func TestCoverageRefuses(t *testing.T) {
	t.Run("an untested package", func(t *testing.T) {
		pkgs := coverageTestGood()
		pkgs["internal/c"] = [2]int{10, 0}
		_, problems := coverageTestRun(t, pkgs, coverageTestPackages)
		if !strings.Contains(strings.Join(problems, "\n"), "internal/c is at 0.0%") {
			t.Errorf("got %v", problems)
		}
	})
	t.Run("the floor on packages", func(t *testing.T) {
		_, problems := coverageTestRun(t, map[string][2]int{"internal/a": {200, 200}}, []string{"internal/a", "internal/version", "scripts/gates", "cmd/google-mail-mcp"})
		if !strings.Contains(strings.Join(problems, "\n"), "scored 1 packages") {
			t.Errorf("got %v", problems)
		}
	})
	t.Run("the floor on blocks", func(t *testing.T) {
		small := map[string][2]int{"cmd/google-mail-mcp": {5, 5}, "internal/a": {5, 5}, "internal/a/sub": {5, 5},
			"internal/b": {5, 5}, "internal/c": {5, 5}}
		_, problems := coverageTestRun(t, small, coverageTestPackages)
		if !strings.Contains(strings.Join(problems, "\n"), "want at least 100") {
			t.Errorf("got %v", problems)
		}
	})
	t.Run("a stale exemption", func(t *testing.T) {
		_, problems := coverageTestRun(t, coverageTestGood(), coverageTestPackages[:5])
		if !strings.Contains(strings.Join(problems, "\n"), "the exemption for internal/version names no package") {
			t.Errorf("got %v", problems)
		}
	})
}

func TestCoverageExemptionsCarryReasons(t *testing.T) {
	for pkg, reason := range coverageExempt {
		if len(strings.TrimSpace(reason)) < 20 {
			t.Errorf("the exemption for %s has no real reason: %q", pkg, reason)
		}
	}
	for pkg, f := range coverageFloors {
		if len(strings.TrimSpace(f.reason)) < 20 || f.floor >= coverageDefaultFloor {
			t.Errorf("the floor for %s is %v with reason %q; a floor is lower than the default and says why",
				pkg, f.floor, f.reason)
		}
	}
}

func TestCoverageReadRefusesAMalformedLine(t *testing.T) {
	if _, err := coverageRead(strings.NewReader("mode: set\nx.go:1.1,2.2 1\n")); err == nil {
		t.Error("a two-field line was accepted")
	}
}
