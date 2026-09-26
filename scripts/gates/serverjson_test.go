package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serverJSONRoot is a repository root with the good manifest and the
// given publish workflow.
func serverJSONRoot(t *testing.T, workflow string) string {
	t.Helper()
	root := mcpbRoot(t, mcpbFixture(t))
	path := filepath.Join(root, serverJSONPublishWorkflow)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func serverJSONGoodWorkflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "serverjson", "publish-mcp.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestServerJSONCommittedPasses(t *testing.T) {
	var out sink
	if err := serverJSONCheck(&out, filepath.Join("..", "..")); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	out.mustSay(t, "io.github.mmedum/google-mail-mcp valid")
}

func TestServerJSONCheckRefusesTheWorkflow(t *testing.T) {
	good := serverJSONGoodWorkflow(t)
	verify := good[strings.Index(good, "      - name: Verify the signature"):strings.Index(good, "      - name: Write the entry")]
	cases := map[string]struct{ workflow, want string }{
		"verify after the read": {
			strings.Replace(strings.Replace(good, verify, "", 1), "      - name: Publish it", verify+"      - name: Publish it", 1),
			"reads checksums.txt before"},
		"identity not pinned to the tag": {
			strings.Replace(good, "release.yml@refs/tags/${TAG}", "release.yml@refs/heads/main", 1),
			"reads checksums.txt before"},
		"never generates the entry": {
			strings.Replace(good, "scripts/gates server-json", "scripts/gates other", 1),
			"never runs"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out sink
			if err := serverJSONCheck(&out, serverJSONRoot(t, tc.workflow)); err == nil {
				t.Fatalf("accepted:\n%s", out.String())
			}
			out.mustSay(t, tc.want)
		})
	}
}

func TestServerJSONRules(t *testing.T) {
	good := func(t *testing.T) serverJSONEntry {
		e, err := serverJSONBuild(mcpbRoot(t, mcpbFixture(t)), "1.2.3", mcpbPackName("1.2.3"), strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	if p := serverJSONProblems(good(t)); len(p) != 0 {
		t.Fatalf("the good entry: %v", p)
	}
	cases := map[string]struct {
		mutate func(e *serverJSONEntry)
		want   string
	}{
		"http": {func(e *serverJSONEntry) {
			e.Packages[0].Identifier = strings.Replace(e.Packages[0].Identifier, "https", "http", 1)
		}, "not https"},
		"another host": {func(e *serverJSONEntry) {
			e.Packages[0].Identifier = strings.Replace(e.Packages[0].Identifier, "github.com", "example.com", 1)
		}, "not on github.com"},
		"not the release": {func(e *serverJSONEntry) {
			e.Packages[0].Identifier = strings.Replace(e.Packages[0].Identifier, "v1.2.3", "v1.2.2", 1)
		}, "not this release's asset"},
		"not a bundle": {func(e *serverJSONEntry) {
			e.Packages[0].Identifier = strings.TrimSuffix(e.Packages[0].Identifier, ".mcpb") + ".zip"
		}, "does not name a .mcpb"},
		"base url":         {func(e *serverJSONEntry) { e.Packages[0].RegistryBaseURL = "https://github.com" }, "registryBaseUrl is set"},
		"bad hash":         {func(e *serverJSONEntry) { e.Packages[0].FileSHA256 = "ABC" }, "not 64 lowercase hex"},
		"wrong namespace":  {func(e *serverJSONEntry) { e.Name = "com.example/google-mail-mcp" }, "io.github. namespace"},
		"long description": {func(e *serverJSONEntry) { e.Description = strings.Repeat("x", 101) }, "101 characters"},
		"two packages":     {func(e *serverJSONEntry) { e.Packages = append(e.Packages, e.Packages[0]) }, "2 packages"},
		"sse transport":    {func(e *serverJSONEntry) { e.Packages[0].Transport.Type = "sse" }, "transport is"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := good(t)
			tc.mutate(&e)
			p := strings.Join(serverJSONProblems(e), "\n")
			if !strings.Contains(p, tc.want) {
				t.Errorf("problems %q do not say %q", p, tc.want)
			}
		})
	}
}

func TestServerJSONPublish(t *testing.T) {
	root := mcpbRoot(t, mcpbFixture(t))
	sum := strings.Repeat("b", 64)
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "checksums.txt")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	archive := strings.Repeat("c", 64) + "  google-mail-mcp_1.2.3_linux_amd64.tar.gz\n"
	var out sink
	if err := serverJSONPublish(&out, root, "v1.2.3", write(archive+sum+"  google-mail-mcp_1.2.3.mcpb\n")); err != nil {
		t.Fatal(err)
	}
	var e serverJSONEntry
	if err := json.Unmarshal([]byte(out.String()), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if e.Version != "1.2.3" || e.Packages[0].FileSHA256 != sum ||
		e.Packages[0].Identifier != "https://github.com/mmedum/google-mail-mcp/releases/download/v1.2.3/google-mail-mcp_1.2.3.mcpb" {
		t.Errorf("entry %+v", e)
	}

	for name, tc := range map[string]struct{ body, want string }{
		"no bundle":       {archive, "lists no .mcpb"},
		"two bundles":     {sum + "  a.mcpb\n" + sum + "  b.mcpb\n", "more than one .mcpb"},
		"empty":           {"", "no checksum rows"},
		"another version": {sum + "  google-mail-mcp_1.2.2.mcpb\n", "publishes google-mail-mcp_1.2.3.mcpb"},
	} {
		t.Run(name, func(t *testing.T) {
			var s sink
			err := serverJSONPublish(&s, root, "1.2.3", write(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want %q", err, tc.want)
			}
		})
	}
	var s sink
	if err := serverJSONPublish(&s, root, "v"+placeholderVersion, write(archive)); err == nil {
		t.Error("the placeholder was published")
	}
}
