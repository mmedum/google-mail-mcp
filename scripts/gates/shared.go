package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// What several gates share. Anything a single gate needs lives in that
// gate's own file, named after it.

const (
	// modulePath is this module, as go.mod declares it.
	modulePath = "github.com/mmedum/google-mail-mcp/v2"
	// binaryName is the server's command and archive name.
	binaryName = "google-mail-mcp"
	// mainPackage is what `go build` builds for the server.
	mainPackage = "./cmd/" + binaryName
	// placeholderVersion is the version every committed manifest and
	// registry entry carries. A real version in the tree is a stale
	// version waiting to be published.
	placeholderVersion = "0.0.0-dev"
)

// gitFiles lists the files git would put in a commit from root: tracked
// files and untracked files that are not ignored, relative to root and
// slash-separated. A file deleted from the work tree but still tracked
// is left out, since there is nothing to read.
func gitFiles(root string) ([]string, error) {
	out, err := gitOutput(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var files []string
	for name := range strings.SplitSeq(out, "\x00") {
		if name == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			continue
		}
		files = append(files, name)
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// gitOutput runs git in root and returns what it wrote to stdout.
func gitOutput(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// writeFileAtomic writes through a temporary file in the same directory
// and renames it into place, so a failure half-way leaves the old file
// as it was.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil { //nolint:gosec // a committed, world-readable record
		return err
	}
	return os.Rename(name, path)
}

// schemaDump is what `google-mail-mcp --dump-schemas` writes: the whole
// surface a client can list, with every tool whole so nothing a client
// reads is outside what the gates compare.
//
// Tools are kept raw as well as decoded, so schema-diff can compare the
// parts it does not name by value.
type schemaDump struct {
	Server string `json:"server"`
	// Version is the build's: a release's tag, or "dev" and the like.
	Version           string            `json:"version"`
	SDKVersion        string            `json:"sdk_version"`
	Tools             []dumpTool        `json:"tools"`
	Resources         []json.RawMessage `json:"resources"`
	ResourceTemplates []json.RawMessage `json:"resource_templates"`
	// Kinds names each tool's kind, as internal/tools registered it.
	Kinds map[string]string `json:"kinds"`
}

// dumpTool is one mcp.Tool as the dump carries it.
type dumpTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  *jsonSchema     `json:"inputSchema"`
	OutputSchema *jsonSchema     `json:"outputSchema"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
}

// jsonSchema is the part of a JSON Schema the gates walk.
type jsonSchema struct {
	Type       any                    `json:"type,omitempty"`
	Properties map[string]*jsonSchema `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
	Items      *jsonSchema            `json:"items,omitempty"`
	Enum       []any                  `json:"enum,omitempty"`
}

// UnmarshalJSON takes a boolean schema too, as JSON Schema allows: `true`
// takes any value, so it has no type, and `false` takes none, so its
// type list is empty.
func (s *jsonSchema) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "true":
		*s = jsonSchema{}
		return nil
	case "false":
		*s = jsonSchema{Type: []any{}}
		return nil
	}
	type plain jsonSchema
	return json.Unmarshal(data, (*plain)(s))
}

// parseDump decodes a schema dump. source names where it came from.
func parseDump(b []byte, source string) (*schemaDump, error) {
	var d schemaDump
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if len(d.Tools) == 0 {
		return nil, fmt.Errorf("%s registers no tools: a gate reading it would be looking at nothing", source)
	}
	return &d, nil
}

// dumpSchemas asks a binary for its surface, and returns it decoded and
// as the bytes the binary wrote. Every gate that needs the surface goes
// through here, so none reads a file another wrote at a different time.
// No GMAIL_ setting is inherited, so nothing a maintainer exported
// changes what is read.
func dumpSchemas(bin string) (*schemaDump, []byte, error) {
	cmd := exec.Command(bin, "--dump-schemas")
	cmd.Env = []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GMAIL_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("%s --dump-schemas: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	d, err := parseDump(out, bin+" --dump-schemas")
	if err != nil {
		return nil, nil, err
	}
	return d, out, nil
}

// exeSuffix is what the platform appends to an executable's name.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// serverBinary is the binary a gate was pointed at, or a fresh build of
// the server in a temporary directory. cleanup removes what was built.
func serverBinary(args []string) (bin string, cleanup func(), err error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], func() {}, nil
	}
	dir, err := os.MkdirTemp("", "gates-bin-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	bin = filepath.Join(dir, binaryName+exeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, mainPackage)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("go build %s: %w: %s", mainPackage, err, strings.TrimSpace(stderr.String()))
	}
	return bin, cleanup, nil
}

// problemsError reports a list of problems as one failure, each on its
// own line through out, and nil when there are none.
func problemsError(out interface{ Write([]byte) (int, error) }, what string, problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	for _, p := range problems {
		_, _ = fmt.Fprintln(out, "  "+p)
	}
	return fmt.Errorf("%d problem(s) in %s", len(problems), what)
}

// parseGoDir parses the non-test Go files in dir that the default build
// context, plus any tags given, would compile, in name order. A file
// excluded by a build constraint or a GOOS/GOARCH suffix is skipped, as
// `go build` skips it.
func parseGoDir(fset *token.FileSet, dir string, tags ...string) ([]*ast.File, error) {
	return parseGoDirMode(fset, dir, 0, tags...)
}

// parseGoDirMode is parseGoDir with the parser's mode, for a gate that
// reads comments.
func parseGoDirMode(fset *token.FileSet, dir string, mode parser.Mode, tags ...string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	ctx := build.Default
	ctx.BuildTags = append(slices.Clone(ctx.BuildTags), tags...)
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if ok, err := ctx.MatchFile(dir, name); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, mode)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

// fileName is the path a parsed file was read from.
func fileName(fset *token.FileSet, file *ast.File) string {
	return fset.File(file.Pos()).Name()
}

// markdownSection is the text under a heading line, to the next heading
// of the same or a higher level: a subsection stays inside its parent.
// It is empty when the heading is not there.
func markdownSection(text, heading string) string {
	i := strings.Index(text, "\n"+heading)
	if i < 0 {
		if !strings.HasPrefix(text, heading) {
			return ""
		}
	} else {
		text = text[i+1:]
	}
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	rest := text[len(heading):]
	offset := 0
	for _, line := range strings.SplitAfter(rest, "\n") {
		trimmed := strings.TrimLeft(line, "#")
		if n := len(line) - len(trimmed); n > 0 && n <= level && strings.HasPrefix(trimmed, " ") {
			return rest[:offset]
		}
		offset += len(line)
	}
	return rest
}
