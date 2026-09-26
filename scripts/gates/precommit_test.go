package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrecommitStepsAreTheFastGates(t *testing.T) {
	root := writeTree(t, map[string]string{"Makefile": "GITLEAKS ?= github.com/zricethezav/gitleaks/v8@v8.30.1\n"})
	steps, err := precommitSteps(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.name)
	}
	if got := strings.Join(names, ", "); got != "gofmt on staged files, leaks, gitleaks on staged changes" {
		t.Errorf("steps = %s", got)
	}
	if _, err := precommitSteps(writeTree(t, map[string]string{"Makefile": "X = y\n"})); err == nil {
		t.Error("a Makefile with no GITLEAKS pin was accepted; the hook would run an unpinned scanner")
	}
}

func TestPrecommitGofmt(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("no gofmt")
	}
	root := writeTree(t, map[string]string{
		"good.go": "package a\n",
		"bad.go":  "package a\nvar  x=1\n",
	})
	var out sink
	if err := precommitGofmt(&out, root, []string{"good.go"}); err != nil {
		t.Errorf("a formatted file failed: %v", err)
	}
	out.mustSay(t, "1 staged Go file(s) formatted")
	if err := precommitGofmt(&out, root, []string{"good.go", "bad.go"}); err == nil || !strings.Contains(err.Error(), "bad.go") {
		t.Errorf("an unformatted file passed: %v", err)
	}
}

func TestInstallHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := writeTree(t, map[string]string{".githooks/pre-commit": "#!/bin/sh\nexec go run ./scripts/gates precommit\n"})
	hook := filepath.Join(root, ".githooks", "pre-commit")
	initCmd := exec.Command("git", "init", "-q")
	initCmd.Dir = root
	if err := initCmd.Run(); err != nil {
		t.Fatal(err)
	}
	var out sink
	if runtime.GOOS != "windows" {
		if err := installHooksIn(&out, root); err == nil {
			t.Error("a hook git would not run was installed")
		}
		if err := os.Chmod(hook, 0o755); err != nil { //nolint:gosec // a test hook
			t.Fatal(err)
		}
	}
	if err := installHooksIn(&out, root); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "core.hooksPath")
	cmd.Dir = root
	got, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(got)) != ".githooks" {
		t.Errorf("core.hooksPath = %q, %v", got, err)
	}
}
