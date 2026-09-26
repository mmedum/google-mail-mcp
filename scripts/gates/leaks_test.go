package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Planted values are joined at run time, so this file never carries one
// whole and the gate needs no marker to excuse itself.
func leaksPlant(parts ...string) string { return strings.Join(parts, "") }

// TestLeaksRulesCatchPlantedShapes is the scanner watching itself fail.
func TestLeaksRulesCatchPlantedShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		leaks bool
	}{
		{"address at a real domain", leaksPlant("write to alice", "@", "acme.co.uk today"), true},
		{"address at a free-mail domain", leaksPlant("from bob.smith", "@", "gmail.com"), true},
		{"example.com", "a@example.com", false},
		{"subdomain of example.org", "a@mail.example.org", false},
		{"invalid TLD", "reader@livemail.invalid", false},
		{"test TLD", "a@b.test", false},
		{"co-author trailer", leaksPlant("noreply", "@", "anthropic.com"), false},
		{"GitHub no-reply", leaksPlant("123+someone", "@", "users.noreply.github.com"), false},
		{"a module version is not an address", "golang.org/x/tools@v0.30.0", false},
		{"Gmail web link", leaksPlant("https://mail.google.com/mail/u/", "0/#inbox/FMfcgz"), true},
		{"the link shape in prose", "`mail.google.com/mail/u/` URLs", false},
		{"message id", leaksPlant("message ", "18c3f2a4b5d6e7f8"), true},
		{"thread id in JSON", leaksPlant(`"thread_id": "`, "18c3f2a4b5d6e7f8", `"`), true},
		{"msg id after a slash", leaksPlant("messages/", "18c3f2a4b5d6e7f8"), true},
		{"draft message id", leaksPlant("draftMessageId=", "18c3f2a4b5d6e7f8"), true},
		{"fixture message id", "message 0000000000000001", false},
		{"fixture thread id", `"threadId": "000000000000002a"`, false},
		{"fixture id with a Gmail-like prefix", `"message":"18f0000000000002"`, false},
		{"seven zeros is not a fixture", leaksPlant(`"message":"18f0000000`, `a3b2c1"`), true},
		{"a hash is not an id without its keyword", "sha 18c3f2a4b5d6e7f8", false},
		{"commit sha after a keyword word", "message f06c13b6b1a9625abc9e6e439d9c05a8f2190e94", false},
		{"oauth client id", leaksPlant("123456789012-", "abcdefghijklmnopqrstuvwxyz012345", ".apps.googleusercontent.com"), true},
		{"client secret", leaksPlant("GOCSPX", "-abcdefghijklmnop1234"), true},
		{"refresh token", leaksPlant("1/", "/0", "9abcdefGhijklmnop-qrstu"), true},
		{"access token", leaksPlant("ya29", ".a0AfB_byC1234567890abcdef"), true},
		{"api key", leaksPlant("AIza", "SyA1234567890abcdefghijklmnopqrstuv"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := leaksFind(tc.text)
			if tc.leaks && len(got) == 0 {
				t.Errorf("a planted shape went unnoticed: %s", tc.text)
			}
			if !tc.leaks && len(got) > 0 {
				t.Errorf("false positive on %s: %v", tc.text, got)
			}
		})
	}
}

// A finding says what and roughly where, and never reprints the value.
func TestLeaksFindingsAreRedacted(t *testing.T) {
	addr := leaksPlant("alice.person", "@", "acme.co.uk")
	found := leaksFind("to " + addr)
	if len(found) != 1 {
		t.Fatalf("found %v", found)
	}
	if strings.Contains(found[0], addr) || strings.Contains(found[0], "acme") {
		t.Errorf("the finding reprints the value: %s", found[0])
	}
}

// Every exemption is an argued decision, so each carries a reason.
func TestLeaksAllowListHasReasons(t *testing.T) {
	entries := 0
	for _, rule := range leaksRules {
		for _, a := range rule.allow {
			entries++
			if strings.TrimSpace(a.reason) == "" {
				t.Errorf("rule %q allows %s without a reason", rule.name, a.re)
			}
		}
	}
	for name, reason := range leaksSkip {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is skipped without a reason", name)
		}
	}
	// The count is stated, so a new exemption is a change to this test
	// as well as to the list.
	if entries != 4 {
		t.Errorf("%d allow-list entries, want 4; a new one is an argued decision", entries)
	}
	if len(leaksRules) < 8 {
		t.Errorf("%d rules; the §9.1 shapes need at least 8", len(leaksRules))
	}
}

func TestLeaksArtifact(t *testing.T) {
	if leaksArtifact([]byte("\x7fELF\x02\x01\x01\x00")) == "" {
		t.Error("an ELF header is not a finding")
	}
	if leaksArtifact([]byte{0xcf, 0xfa, 0xed, 0xfe, 0}) == "" {
		t.Error("a Mach-O header is not a finding")
	}
	if leaksArtifact([]byte("MZ\x90\x00")) == "" {
		t.Error("a PE header is not a finding")
	}
	if leaksArtifact([]byte("\x89PNG\r\n\x1a\n\x00")) != "" {
		t.Error("a small image is a finding")
	}
}

// leaksRepo makes a git repository with n clean files committed.
func leaksRepo(t *testing.T, n int) string {
	t.Helper()
	files := map[string]string{"go.sum": leaksPlant("x ", "bob", "@", "acme.co.uk")}
	for i := range n {
		files[filepath.Join("docs", "f"+string(rune('a'+i))+".md")] = "reader@example.com\n"
	}
	root := writeTree(t, files)
	leaksGitIn(t, root, "init", "-q")
	leaksGitIn(t, root, "add", "-A")
	leaksGitIn(t, root, "commit", "-qm", "first")
	return root
}

func leaksGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestLeaksTreeScansTrackedAndUntracked(t *testing.T) {
	root := leaksRepo(t, 3)
	var out sink
	if err := leaksTree(&out, root, 3); err != nil {
		t.Fatalf("clean tree failed: %v\n%s", err, out.String())
	}
	out.mustSay(t, "leaks ok (3 text files")

	// An untracked file is read before `git add -A` can sweep it in.
	if err := os.WriteFile(filepath.Join(root, "new.md"), []byte(leaksPlant("x ", "carol", "@", "acme.co.uk")), 0o600); err != nil {
		t.Fatal(err)
	}
	out = sink{}
	if err := leaksTree(&out, root, 3); err == nil {
		t.Fatal("an untracked leak passed")
	}
	out.mustSay(t, "new.md: an address")
}

func TestLeaksTreeRefusesABinaryAndAShortScan(t *testing.T) {
	root := leaksRepo(t, 3)
	if err := os.WriteFile(filepath.Join(root, "gates"), []byte("\x7fELF\x02\x01\x01\x00binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaksGitIn(t, root, "add", "-f", "gates")
	var out sink
	if err := leaksTree(&out, root, 3); err == nil {
		t.Fatal("a tracked executable passed")
	}
	out.mustSay(t, "gates: a compiled executable")

	if err := leaksTree(&sink{}, leaksRepo(t, 2), 3); err == nil || !strings.Contains(err.Error(), "want at least 3") {
		t.Errorf("a scan under the floor passed: %v", err)
	}
}

func TestLeaksHistoryReadsBlobsMessagesAndTags(t *testing.T) {
	root := leaksRepo(t, 2)
	var out sink
	if err := leaksScanHistory(&out, root); err != nil {
		t.Fatalf("clean history failed: %v\n%s", err, out.String())
	}
	out.mustSay(t, "history leaks ok (1 commits, 0 tag messages")

	// A leak deleted from the tip is still in a blob.
	path := filepath.Join(root, "gone.md")
	if err := os.WriteFile(path, []byte(leaksPlant("message ", "18c3f2a4b5d6e7f8")), 0o600); err != nil {
		t.Fatal(err)
	}
	leaksGitIn(t, root, "add", "-A")
	leaksGitIn(t, root, "commit", "-qm", "add")
	leaksGitIn(t, root, "rm", "-q", "gone.md")
	leaksGitIn(t, root, "commit", "-qm", "remove")
	// And one in a commit message, and one in a tag message.
	leaksGitIn(t, root, "commit", "-q", "--allow-empty", "-m", leaksPlant("thanks dave", "@", "acme.co.uk"))
	leaksGitIn(t, root, "tag", "-a", "v0.0.1", "-m", leaksPlant("thread ", "18c3f2a4b5d6e7f9"))

	out = sink{}
	if err := leaksScanHistory(&out, root); err == nil {
		t.Fatal("a leak in the history passed")
	}
	out.mustSay(t, "gone.md@")
	out.mustSay(t, "message: an address")
	out.mustSay(t, "tag ")

	// The tree is clean, so the tree scan passes: the history is what
	// holds these.
	if err := leaksTree(&sink{}, root, 2); err != nil {
		t.Errorf("the tree scan failed on a clean tree: %v", err)
	}
}
