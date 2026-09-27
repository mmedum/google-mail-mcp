package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// precommit is what .githooks/pre-commit runs: the checks fast enough
// for every commit that catch what is expensive to undo. A leak reaches
// the history at commit time, and one deleted from the tip is still in
// the log.
//
// gitleaks runs at the version the Makefile pins, the one `make secrets`
// runs in CI, so the hook and CI cannot disagree about the rules.

const precommitHooksDir = ".githooks"

// precommitStep is one thing the hook runs.
type precommitStep struct {
	name string
	run  func(out io.Writer) error
}

func precommit(out io.Writer, _ []string) error {
	steps, err := precommitSteps(".")
	if err != nil {
		return err
	}
	for _, s := range steps {
		_, _ = fmt.Fprintf(out, "-- %s\n", s.name)
		if err := s.run(out); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	_, _ = fmt.Fprintf(out, "precommit ok: %d steps\n", len(steps))
	return nil
}

// precommitSteps is the list, apart from the loop so a test can read it
// without running a scan.
func precommitSteps(root string) ([]precommitStep, error) {
	mk, err := readMakefile(filepath.Join(root, parityMakefile))
	if err != nil {
		return nil, err
	}
	gitleaks, err := mk.variable("GITLEAKS")
	if err != nil {
		return nil, err
	}
	leaks, ok := commands["leaks"]
	if !ok {
		return nil, errors.New("the registry has no leaks gate")
	}
	return []precommitStep{
		{"gofmt on staged files", func(out io.Writer) error {
			files, err := precommitStaged(root)
			if err != nil {
				return err
			}
			return precommitGofmt(out, root, files)
		}},
		// The same implementation `make leaks` calls, through the
		// registry, rather than a second spelling of it.
		{"leaks", func(out io.Writer) error { return leaks.run(out, nil) }},
		{"gitleaks on staged changes", func(out io.Writer) error {
			return precommitRun(out, root, "go", "run", gitleaks, "git", "--staged",
				"--config", ".gitleaks.toml", "--redact", "--no-banner")
		}},
	}, nil
}

// precommitStaged lists the Go files staged for commit.
func precommitStaged(root string) ([]string, error) {
	raw, err := gitOutput(root, "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for name := range strings.SplitSeq(raw, "\x00") {
		if strings.HasSuffix(name, ".go") {
			files = append(files, name)
		}
	}
	return files, nil
}

// precommitGofmt fails on any file gofmt would change.
func precommitGofmt(out io.Writer, root string, files []string) error {
	if len(files) == 0 {
		_, _ = fmt.Fprintln(out, "no Go files staged")
		return nil
	}
	cmd := exec.Command("gofmt", append([]string{"-l"}, files...)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("gofmt: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if bad := strings.Fields(string(raw)); len(bad) > 0 {
		return fmt.Errorf("gofmt would change %s", strings.Join(bad, ", "))
	}
	_, _ = fmt.Fprintf(out, "%d staged Go file(s) formatted\n", len(files))
	return nil
}

func precommitRun(out io.Writer, root, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// installHooks points git at .githooks, after checking the hook is there
// and can run.
func installHooks(out io.Writer, _ []string) error {
	if err := installHooksIn(out, "."); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "git runs %s/pre-commit on every commit\n", precommitHooksDir)
	return nil
}

func installHooksIn(out io.Writer, root string) error {
	hook := filepath.Join(root, precommitHooksDir, "pre-commit")
	info, err := os.Stat(hook)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable, so git would skip it without a word", hook)
	}
	return precommitRun(out, root, "git", "config", "core.hooksPath", precommitHooksDir)
}
