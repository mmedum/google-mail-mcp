package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"

	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	smokeFakeOnce sync.Once
	smokeFakeBin  string
	smokeFakeErr  error
	smokeFakeDir  string
)

// smokeFake builds the fake server once per test binary.
func smokeFake(t *testing.T) string {
	t.Helper()
	smokeFakeOnce.Do(func() {
		dir, err := filepath.Abs("testdata/smoke")
		if err != nil {
			smokeFakeErr = err
			return
		}
		// Not t.TempDir: the binary outlives the first test that builds it.
		smokeFakeDir, smokeFakeErr = os.MkdirTemp("", "gates-smoke-fake-")
		if smokeFakeErr != nil {
			return
		}
		smokeFakeBin = filepath.Join(smokeFakeDir, "fakeserver"+exeSuffix())
		cmd := exec.Command("go", "build", "-o", smokeFakeBin, "./fakeserver")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			smokeFakeErr = err
			t.Logf("%s", out)
		}
	})
	if smokeFakeErr != nil {
		t.Fatalf("build the fake server: %v", smokeFakeErr)
	}
	return smokeFakeBin
}

func TestSmokePassesAWellBehavedServer(t *testing.T) {
	bin := smokeFake(t)
	t.Setenv("SMOKE_FAKE_MODE", "")
	var out sink
	if err := smokeRun(&out, bin); err != nil {
		t.Fatal(err)
	}
	out.mustSay(t, "tools registered: default 2, read-only 1, send 3, destructive 3, of 4 in the full surface")
	out.mustSay(t, "frames read")
}

// Each misbehavior, one at a time, and the failure it must produce.
func TestSmokeFailsEachWay(t *testing.T) {
	bin := smokeFake(t)
	cases := map[string]string{
		"crash":               "",
		"stray":               "not JSON-RPC",
		"exit1":               "must be exit 0",
		"noclass":             "want an [auth] tool error",
		"downgrade":           "the server negotiated",
		"leaky-readonly":      "read-only mode registers create_draft, a write tool it must not",
		"leaky-send":          "default mode registers send_draft, a send tool it must not",
		"no-meta":             "send_draft is annotated",
		"missing-destructive": "destructive mode lacks delete_message",
		"undumped":            "which --dump-schemas does not",
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SMOKE_FAKE_MODE", mode)
			err := smokeRun(&sink{}, bin)
			if err == nil {
				t.Fatalf("mode %s passed", mode)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("mode %s failed with %q, want it to say %q", mode, err, want)
			}
		})
	}
}

func TestSmokeAbruptDisconnectMatchesByCode(t *testing.T) {
	bin := smokeFake(t)
	env, _, cleanup, err := smokeEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv("SMOKE_FAKE_MODE", "")
	if err := smokeAbruptDisconnect(bin, env); err != nil {
		t.Errorf("a clean hang-up failed: %v", err)
	}
	t.Setenv("SMOKE_FAKE_MODE", "wrongcode")
	env, _, cleanup2, _ := smokeEnv()
	defer cleanup2()
	if err := smokeAbruptDisconnect(bin, env); err == nil || !strings.Contains(err.Error(), "error -32000") {
		t.Errorf("an unlisted error code passed: %v", err)
	}
}

func TestSmokeRefusesAnSDKWithoutTheRevision(t *testing.T) {
	saved := smokeSupported
	defer func() { smokeSupported = saved }()
	smokeSupported = func() []string { return []string{"2024-11-05"} }
	if err := smokeRun(&sink{}, "unused"); err == nil || !strings.Contains(err.Error(), "lacks the current revision") {
		t.Errorf("got %v", err)
	}
}

func TestSmokeEnvStripsSettingsAndIsolatesHome(t *testing.T) {
	t.Setenv("GMAIL_ENABLE_SEND", "true")
	t.Setenv("GMAIL_REFRESH_TOKEN", "x")
	env, _, cleanup, err := smokeEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"GMAIL_ENABLE_SEND", "GMAIL_REFRESH_TOKEN"} {
		if strings.Contains(joined, bad) {
			t.Errorf("%s reached the child", bad)
		}
	}
	if !strings.Contains(joined, "GMAIL_PROFILE=smoke-") || !strings.Contains(joined, "GMAIL_CONFIG_DIR=") {
		t.Errorf("isolation settings missing: %s", joined)
	}
}

func TestSmokeAwaitedIsDerived(t *testing.T) {
	got := smokeAwaited([]smokeFrame{{id: 1}, {method: "n"}, {id: 7}})
	if len(got) != 2 || got[0] != 1 || got[1] != 7 {
		t.Errorf("awaited %v", got)
	}
}

func TestSmokeProbesTheSDKsNewestRevision(t *testing.T) {
	newest := mcp.SupportedProtocolVersions()[0]
	if smokeDiscoverRevision != newest {
		t.Errorf("server/discover is probed at %s; the linked SDK's newest revision is %s", smokeDiscoverRevision, newest)
	}
	if !slices.Contains(mcp.SupportedProtocolVersions(), smokeCurrentRevision) {
		t.Errorf("the handshake is probed at %s, which the linked SDK no longer supports", smokeCurrentRevision)
	}
	if !slices.Contains(mcp.SupportedProtocolVersions(), smokePreviousRevision) {
		t.Errorf("smoke probes %s, which the linked SDK no longer supports", smokePreviousRevision)
	}
}

func TestSmokeFailsAServerWithoutServerDiscover(t *testing.T) {
	bin := smokeFake(t)
	t.Setenv("SMOKE_FAKE_MODE", "legacy")
	var out sink
	err := smokeRun(&out, bin)
	if err == nil || !strings.Contains(err.Error(), "server/discover") {
		t.Fatalf("err = %v; want a server/discover failure", err)
	}
}

// Every kind combination register.go writes reads back as its kind, and
// one it never writes is refused.
func TestSmokeKindFollowsRegister(t *testing.T) {
	for _, tc := range []struct{ annotations, meta, want string }{
		{`{"readOnlyHint":true,"idempotentHint":true,"openWorldHint":false}`, ``, smokeKindRead},
		{`{"destructiveHint":false,"openWorldHint":false}`, ``, smokeKindWrite},
		{`{"destructiveHint":false,"openWorldHint":true}`, `{"anthropic/requiresUserInteraction":true}`, smokeKindSend},
		{`{"destructiveHint":true,"idempotentHint":true,"openWorldHint":false}`,
			`{"anthropic/requiresUserInteraction":true}`, smokeKindDestructive},
		{`{"destructiveHint":true,"idempotentHint":true,"openWorldHint":false}`, ``, smokeKindWriteForGood},
		{`{"destructiveHint":false,"openWorldHint":true}`, ``, ""},
		{`{}`, ``, ""},
		{`{"readOnlyHint":true}`, `{"anthropic/requiresUserInteraction":true}`, ""},
	} {
		got, err := smokeKind("t", json.RawMessage(tc.annotations), json.RawMessage(tc.meta))
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("smokeKind(%s, %s) = %q, %v; want %q", tc.annotations, tc.meta, got, err, tc.want)
		}
	}
}

// Every kind is registered by some mode, and only read tools by all.
func TestSmokeModesCoverEveryKind(t *testing.T) {
	seen := map[string]int{}
	for _, m := range smokeModes {
		for _, k := range m.registers {
			seen[k]++
		}
	}
	for _, k := range []string{smokeKindRead, smokeKindWrite, smokeKindWriteForGood, smokeKindSend, smokeKindDestructive} {
		if seen[k] == 0 {
			t.Errorf("no mode registers %s tools", k)
		}
	}
	if seen[smokeKindRead] != len(smokeModes) {
		t.Errorf("read tools are registered in %d of %d modes", seen[smokeKindRead], len(smokeModes))
	}
}
