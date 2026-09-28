package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The MCP registry entry, generated at publish time rather than
// committed: everything in it is derived — the name and URLs from the
// module path, the description from the bundle manifest, the hash from
// the release's own checksums.txt, which the publish workflow verifies
// with cosign before this reads it.
//
// With no arguments this is the check: the entry is built with the
// placeholder version and a dummy hash and held to the vendored schema
// and to the rules the registry enforces in code but not in its schema.
// With TAG CHECKSUMS it prints the entry to publish.

const (
	// serverJSONDescriptionMax is the registry schema's cap.
	serverJSONDescriptionMax = 100
	// serverJSONPublishWorkflow is the workflow that publishes it.
	serverJSONPublishWorkflow = ".github/workflows/publish-mcp.yml"
)

// serverJSONChecksumRow is a checksums.txt row: the hash, then the file.
var serverJSONChecksumRow = regexp.MustCompile(`^([a-f0-9]{64})\s+\*?(\S+)$`)

type serverJSONEntry struct {
	Schema      string              `json:"$schema"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Version     string              `json:"version"`
	WebsiteURL  string              `json:"websiteUrl"`
	Repository  serverJSONRepo      `json:"repository"`
	Packages    []serverJSONPackage `json:"packages"`
}

type serverJSONRepo struct {
	URL    string `json:"url"`
	Source string `json:"source"`
}

type serverJSONPackage struct {
	RegistryType    string              `json:"registryType"`
	RegistryBaseURL string              `json:"registryBaseUrl,omitempty"`
	Identifier      string              `json:"identifier"`
	FileSHA256      string              `json:"fileSha256"`
	Version         string              `json:"version"`
	Transport       serverJSONTransport `json:"transport"`
}

type serverJSONTransport struct {
	Type string `json:"type"`
}

// serverJSON is `server-json` (check) or `server-json TAG CHECKSUMS`.
func serverJSON(out io.Writer, args []string) error {
	switch len(args) {
	case 0:
		return serverJSONCheck(out, ".")
	case 2:
		return serverJSONPublish(out, ".", args[0], args[1])
	}
	return fmt.Errorf("usage: server-json [TAG CHECKSUMS]; no arguments checks, two print the entry to publish")
}

// majorSuffix is the /vN element Go adds to a module path from v2 on.
var majorSuffix = regexp.MustCompile(`^v[2-9][0-9]*$`)

// serverJSONOwnerRepo splits the module path into owner and repository.
// A major version from v2 on ends the path in /vN, which is Go's, not
// GitHub's, and is dropped.
func serverJSONOwnerRepo() (string, string, error) {
	parts := strings.Split(modulePath, "/")
	if n := len(parts); n == 4 && majorSuffix.MatchString(parts[3]) {
		parts = parts[:3]
	}
	if len(parts) != 3 || parts[0] != "github.com" {
		return "", "", fmt.Errorf("module path %q is not github.com/OWNER/REPO", modulePath)
	}
	return parts[1], parts[2], nil
}

// serverJSONBuild makes the entry for a version and a bundle row.
func serverJSONBuild(root, version, bundle, sum string) (serverJSONEntry, error) {
	owner, repo, err := serverJSONOwnerRepo()
	if err != nil {
		return serverJSONEntry{}, err
	}
	m, _, err := mcpbRead(filepath.Join(root, mcpbManifestPath))
	if err != nil {
		return serverJSONEntry{}, err
	}
	return serverJSONEntry{
		Schema:      vendorRegistrySchemaURL,
		Name:        fmt.Sprintf("io.github.%s/%s", owner, repo),
		Description: m.Description,
		Version:     version,
		WebsiteURL:  fmt.Sprintf("https://github.com/%s/%s#readme", owner, repo),
		Repository:  serverJSONRepo{URL: fmt.Sprintf("https://github.com/%s/%s", owner, repo), Source: "github"},
		Packages: []serverJSONPackage{{
			RegistryType: "mcpb",
			Identifier:   fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", owner, repo, version, bundle),
			FileSHA256:   sum,
			Version:      version,
			Transport:    serverJSONTransport{Type: "stdio"},
		}},
	}, nil
}

// serverJSONProblems holds an entry to the vendored schema and to the
// registry's unwritten rules: its validator refuses things its schema
// does not, and an accepted wrong entry cannot be withdrawn.
func serverJSONProblems(e serverJSONEntry) []string {
	var problems []string
	raw, err := json.Marshal(e)
	if err != nil {
		return []string{err.Error()}
	}
	if err := vendorValidate(vendorRegistrySchemaFile, "the registry entry", raw); err != nil {
		problems = append(problems, err.Error())
	}
	owner, repo, err := serverJSONOwnerRepo()
	if err != nil {
		return append(problems, err.Error())
	}
	if want := fmt.Sprintf("io.github.%s/%s", owner, repo); e.Name != want {
		problems = append(problems, fmt.Sprintf("name is %q, want %q: the io.github. namespace is what GitHub "+
			"OIDC proves at publish time", e.Name, want))
	}
	if n := len(e.Description); n == 0 || n > serverJSONDescriptionMax {
		problems = append(problems, fmt.Sprintf("description is %d characters; the registry takes 1 to %d, "+
			"and it comes from the bundle manifest's description", n, serverJSONDescriptionMax))
	}
	if len(e.Packages) != 1 {
		return append(problems, fmt.Sprintf("%d packages; this server publishes exactly one, the bundle", len(e.Packages)))
	}
	p := e.Packages[0]
	if p.RegistryType != "mcpb" {
		problems = append(problems, fmt.Sprintf("registryType is %q, want mcpb", p.RegistryType))
	}
	if p.RegistryBaseURL != "" {
		problems = append(problems, "registryBaseUrl is set; an mcpb package carries its full URL in identifier")
	}
	if p.Version != e.Version {
		problems = append(problems, fmt.Sprintf("the package says version %q and the entry %q", p.Version, e.Version))
	}
	if p.Transport.Type != "stdio" {
		problems = append(problems, fmt.Sprintf("transport is %q, want stdio", p.Transport.Type))
	}
	if len(p.FileSHA256) != 64 || strings.Trim(p.FileSHA256, "0123456789abcdef") != "" {
		problems = append(problems, fmt.Sprintf("fileSha256 %q is not 64 lowercase hex characters", p.FileSHA256))
	}
	u, err := url.Parse(p.Identifier)
	if err != nil {
		return append(problems, "identifier is not a URL: "+err.Error())
	}
	wantPrefix := fmt.Sprintf("/%s/%s/releases/download/v%s/", owner, repo, e.Version)
	switch {
	case u.Scheme != "https":
		problems = append(problems, "identifier is not https: "+p.Identifier)
	case u.Host != "github.com":
		problems = append(problems, "identifier is not on github.com: "+p.Identifier)
	case !strings.HasPrefix(u.Path, wantPrefix):
		problems = append(problems, fmt.Sprintf("identifier %s is not this release's asset (%s...)", p.Identifier, wantPrefix))
	case path.Ext(u.Path) != ".mcpb":
		problems = append(problems, "identifier does not name a .mcpb: "+p.Identifier)
	case !strings.Contains(strings.ToLower(p.Identifier), "mcp"):
		problems = append(problems, "identifier does not contain \"mcp\", which the registry requires: "+p.Identifier)
	}
	return problems
}

// serverJSONCheck builds the entry as a publish would, with the
// placeholder version and a dummy hash, and holds it and the publish
// workflow to the rules.
func serverJSONCheck(out io.Writer, root string) error {
	e, err := serverJSONBuild(root, placeholderVersion, mcpbPackName(placeholderVersion), strings.Repeat("0", 64))
	if err != nil {
		return err
	}
	problems := serverJSONProblems(e)
	steps, wfProblems := serverJSONWorkflowProblems(root)
	problems = append(problems, wfProblems...)
	if err := problemsError(out, "the registry entry", problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "server-json: %s valid against %s and the registry's rules; %s: %d steps, the "+
		"checksums verified before they are read\n", e.Name, vendorRegistrySchemaFile, serverJSONPublishWorkflow, steps)
	return nil
}

// serverJSONPublish prints the entry for a release.
func serverJSONPublish(out io.Writer, root, tag, checksums string) error {
	version := strings.TrimPrefix(tag, "v")
	if version == "" || version == placeholderVersion {
		return fmt.Errorf("server-json needs a release tag; got %q", tag)
	}
	bundle, sum, err := serverJSONBundleRow(checksums)
	if err != nil {
		return err
	}
	if want := mcpbPackName(version); bundle != want {
		return fmt.Errorf("%s lists %s; the release for %s publishes %s", checksums, bundle, tag, want)
	}
	e, err := serverJSONBuild(root, version, bundle, sum)
	if err != nil {
		return err
	}
	if problems := serverJSONProblems(e); len(problems) > 0 {
		return fmt.Errorf("the entry would be refused or wrong:\n  %s", strings.Join(problems, "\n  "))
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(b))
	return err
}

// serverJSONBundleRow finds the one .mcpb row in a checksums file. None
// or two is a release that did not build as meant.
func serverJSONBundleRow(checksums string) (name, sum string, err error) {
	data, err := os.ReadFile(checksums) //nolint:gosec // a path the release passes in
	if err != nil {
		return "", "", err
	}
	rows := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		m := serverJSONChecksumRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		rows++
		if path.Ext(m[2]) != ".mcpb" {
			continue
		}
		if name != "" {
			return "", "", fmt.Errorf("%s lists more than one .mcpb: %s and %s", checksums, name, m[2])
		}
		name, sum = path.Base(m[2]), m[1]
	}
	switch {
	case rows == 0:
		return "", "", fmt.Errorf("%s has no checksum rows", checksums)
	case name == "":
		return "", "", fmt.Errorf("%s lists no .mcpb among %d rows; the bundle was not packed or not named "+
			"in checksum.extra_files", checksums, rows)
	}
	return name, sum, nil
}

// serverJSONIdentity is the certificate identity pinned to this
// repository's release workflow at the exact tag.
var serverJSONIdentity = regexp.MustCompile(
	`--certificate-identity\s+\\?\s*"?https://github\.com/\$\{(REPO|\{ github\.repository \}\})\}/\.github/workflows/release\.yml@refs/tags/\$\{\{?\s*(TAG|inputs\.tag)\s*\}?\}?"?`)

// serverJSONWorkflowProblems holds the publish workflow to the order that
// makes the hash trustworthy: cosign verify-blob over checksums.txt, with
// the identity pinned to release.yml at the exact tag, in an earlier step
// of the same job than the one running `gates server-json`.
func serverJSONWorkflowProblems(root string) (int, []string) {
	file := serverJSONPublishWorkflow
	wf, err := readWorkflow(filepath.Join(root, filepath.FromSlash(file)), file)
	if err != nil {
		return 0, []string{"cannot read " + file + ": " + err.Error()}
	}
	steps := 0
	for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
		job := wf.Jobs[name]
		steps += len(job.Steps)
		verified := -1
		for i, s := range job.Steps {
			if strings.Contains(s.Run, "cosign verify-blob") && strings.Contains(s.Run, "checksums.txt") &&
				serverJSONIdentity.MatchString(s.Run) {
				verified = i
			}
			if strings.Contains(s.Run, "scripts/gates server-json") {
				if verified < 0 {
					return steps, []string{fmt.Sprintf("%s: step %q reads checksums.txt before a cosign verify-blob "+
						"pinned to release.yml@refs/tags/<tag> has checked it", file, s.Name)}
				}
				return steps, nil
			}
		}
	}
	return steps, []string{file + " never runs `go run ./scripts/gates server-json TAG CHECKSUMS`"}
}
