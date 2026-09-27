package main

import (
	"strings"
	"testing"
)

const outcomesRegister = `package tools

type Kind int

const (
	Read Kind = iota
	Write
)

type Spec struct {
	Name string
	Kind Kind
}
`

const outcomesHonest = `package tools

import (
	"context"
	"errors"
)

type TrashInput struct {
	ID     string
	DryRun bool
	Thread bool
}

type client struct{}

func (client) Trash(context.Context, string) (string, error) { return "", nil }

type Deps struct{ Client client }

var specs = []Spec{{Name: "trash", Kind: Write}, {Name: "get_message", Kind: Read}}

func trash(ctx context.Context, d Deps, in TrashInput) (string, error) {
	if in.DryRun {
		return "would move the message to the trash", nil
	}
	if in.Thread {
		state, err := d.Client.Trash(ctx, in.ID)
		return "the thread is now " + state, err
	}
	if in.ID == "" && in.Thread {
		return "", errors.New("[invalid] an id is required")
	}
	return "", nil
}
`

// The dishonest branch: it tests the request and states what is now
// true, asking nobody.
const outcomesDishonest = `package tools

type RestoreInput struct {
	ID     string
	Thread bool
}

var more = []Spec{{Name: "restore", Kind: Write}}

func restore(in RestoreInput) string {
	if in.Thread {
		return "every message in the thread is back in the inbox"
	}
	return ""
}
`

func runOutcomes(t *testing.T, files map[string]string, claims string) (string, error) {
	t.Helper()
	if claims != "" {
		files["claims.tsv"] = claims
	}
	root := writeTree(t, files)
	var out sink
	err := outcomesCheck(&out, root, []string{"tools"}, "claims.tsv")
	return out.String(), err
}

func TestOutcomesPassesHonestBranches(t *testing.T) {
	out, err := runOutcomes(t, map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": outcomesHonest}, "")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "outcomes ok (2 files in") || !strings.Contains(out, "1 write tool(s), 2 boolean input field(s), 3 branch(es)") {
		t.Errorf("the gate does not say what it read: %s", out)
	}
}

func TestOutcomesRefusesAnOutcomeFromTheRequest(t *testing.T) {
	files := map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": outcomesHonest,
		"tools/restore.go": outcomesDishonest}
	out, err := runOutcomes(t, files, "")
	if err == nil {
		t.Fatalf("passed:\n%s", out)
	}
	if !strings.Contains(out, "tools/restore.go:12: this branch tests the request field Thread") {
		t.Errorf("the finding does not name the branch:\n%s", out)
	}
}

func TestOutcomesClaimsAreHeldBothWays(t *testing.T) {
	files := func() map[string]string {
		return map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": outcomesHonest,
			"tools/restore.go": outcomesDishonest}
	}
	// The branch excused with a reason passes.
	root := "tools/restore.go:Thread\tthe handler read the thread back two lines earlier\n"
	if out, err := runOutcomes(t, files(), root); err != nil {
		t.Fatalf("an excused branch failed: %v\n%s", err, out)
	}
	// A row with no reason fails.
	if out, err := runOutcomes(t, files(), "tools/restore.go:Thread\t\n"); err == nil || !strings.Contains(out, "no reason") {
		t.Errorf("a row without a reason passed: %v\n%s", err, out)
	}
	// A row for a branch that no longer states an outcome fails.
	stale := map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": outcomesHonest}
	if out, err := runOutcomes(t, stale, root); err == nil || !strings.Contains(out, "delete the row") {
		t.Errorf("a stale row passed: %v\n%s", err, out)
	}
}

// With write tools registered and no boolean inputs, the gate is not
// reading their inputs; with no write tools, the floor stays at zero.
func TestOutcomesFloors(t *testing.T) {
	noBools := `package tools

var specs = []Spec{{Name: "trash", Kind: Write}}

type TrashInput struct{ ID string }
`
	out, err := runOutcomes(t, map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": noBools}, "")
	if err == nil || !strings.Contains(err.Error(), "1 write tool(s) registered") {
		t.Errorf("a write tool with no boolean input passed: %v\n%s", err, out)
	}
	readsOnly := `package tools

var specs = []Spec{{Name: "get_message", Kind: Read}}
`
	if out, err := runOutcomes(t, map[string]string{"tools/register.go": outcomesRegister, "tools/read.go": readsOnly}, ""); err != nil {
		t.Errorf("a read-only surface failed: %v\n%s", err, out)
	}
	if _, err := runOutcomes(t, map[string]string{"tools/register.go": outcomesRegister}, ""); err == nil {
		t.Error("one file passed the file floor")
	}
	if _, err := runOutcomes(t, map[string]string{"elsewhere/x.go": "package x\n"}, ""); err == nil {
		t.Error("a missing directory passed")
	}
}

// This repository names inputs FooIn; the gate reads them as it reads
// FooInput.
func TestOutcomesReadsInputsNamedIn(t *testing.T) {
	dishonest := strings.ReplaceAll(outcomesDishonest, "RestoreInput", "RestoreIn")
	files := map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": outcomesHonest,
		"tools/restore.go": dishonest}
	out, err := runOutcomes(t, files, "")
	if err == nil || !strings.Contains(out, "tools/restore.go:12: this branch tests the request field Thread") {
		t.Fatalf("an input named RestoreIn was not read: %v\n%s", err, out)
	}
}

// A request acted on in the service, under a name that is not FooIn, and
// a confirm passed as a bare bool, are read too.
func TestOutcomesReadsServiceRequests(t *testing.T) {
	service := `package tools

type Move struct {
	ID      string
	Restore bool
}

type Service struct{}

func (s *Service) Trash(in Move, confirm bool) string {
	if in.Restore {
		return "every message is back in the inbox"
	}
	if confirm {
		return "the draft is deleted"
	}
	return ""
}
`
	files := map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": outcomesHonest,
		"tools/service.go": service}
	out, err := runOutcomes(t, files, "")
	if err == nil || !strings.Contains(out, "tests the request field Restore") || !strings.Contains(out, "tests the request field confirm") {
		t.Fatalf("the service's branches were not read: %v\n%s", err, out)
	}
}

// Write tools with inputs but no branch on any of them: the gate is not
// reading the code that acts on requests.
func TestOutcomesFloorOnBranches(t *testing.T) {
	quiet := `package tools

var specs = []Spec{{Name: "trash", Kind: Write}}

type TrashIn struct {
	ID     string
	DryRun bool
}

func trash(in TrashIn) string { return in.ID }
`
	out, err := runOutcomes(t, map[string]string{"tools/register.go": outcomesRegister, "tools/trash.go": quiet}, "")
	if err == nil || !strings.Contains(err.Error(), "no branch") {
		t.Errorf("a surface with no branch passed: %v\n%s", err, out)
	}
}
