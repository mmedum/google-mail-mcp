package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// The packer. goreleaser runs it from the universal binary's post hook,
// the one point where every binary exists and checksums.txt has not been
// written; `checksum.extra_files` and `release.extra_files` then cover
// and upload the bundle. A .mcpb is a deflate zip with the manifest at
// the root and the binaries under server/, which the standard library
// writes.

// mcpbPackTime is stamped on every entry, so the same inputs give the
// same bytes. Unset writes zeroes that display as 1980-00-00.
var mcpbPackTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// mcpbPackMode is the mode of every staged file. The desktop forces the
// execute bit on the entry point alone, so anything else arrives
// unrunnable unless staged executable.
const mcpbPackMode os.FileMode = 0o755

// mcpbPackName is the bundle's file name for a version.
func mcpbPackName(version string) string {
	return binaryName + "_" + version + ".mcpb"
}

// mcpbPack is `mcpb-pack DIST VERSION OUT`, as the goreleaser hook
// calls it.
func mcpbPack(out io.Writer, args []string) error {
	_, err := mcpbPackTo(out, ".", args[0], args[1], args[2], true)
	return err
}

// mcpbPackTo packs the bundle from dist into bundle and reads it back.
// runVersion says whether to run the host's staged binary for its
// --version.
func mcpbPackTo(out io.Writer, root, dist, version, bundle string, runVersion bool) (string, error) {
	version = strings.TrimPrefix(version, "v")
	if version == "" || version == placeholderVersion {
		return "", fmt.Errorf("mcpb-pack needs the release version; %q is the committed placeholder", version)
	}
	if want := mcpbPackName(version); filepath.Base(bundle) != want {
		return "", fmt.Errorf("the bundle is named %q; the release publishes %q", filepath.Base(bundle), want)
	}
	m, raw, err := mcpbRead(filepath.Join(root, mcpbManifestPath))
	if err != nil {
		return "", err
	}
	if m.Version != placeholderVersion {
		return "", fmt.Errorf("%s says version %q; the committed manifest must carry %q",
			mcpbManifestPath, m.Version, placeholderVersion)
	}
	problems := mcpbValidate(m, mcpbFiles, launcherNamesIn(launcherScript(mcpbFiles)))
	problems = append(problems, mcpbDocumentProblems(m, raw)...)
	if err := problemsError(out, mcpbManifestPath, problems); err != nil {
		return "", fmt.Errorf("the manifest does not describe the bundle this would pack: %w", err)
	}

	stamped, err := mcpbStamp(raw, version)
	if err != nil {
		return "", err
	}

	sources := map[string]string{}
	for _, f := range mcpbFiles {
		if f.launcher {
			continue
		}
		path, err := mcpbPackSource(dist, f)
		if err != nil {
			return "", err
		}
		sources[f.path] = path
	}

	if runVersion {
		note, err := mcpbPackHostVersion(sources, version)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintln(out, "mcpb-pack: "+note)
	}

	if err := mcpbPackWrite(bundle, stamped, sources); err != nil {
		return "", err
	}
	n, err := mcpbPackReadBack(bundle, version)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(out, "mcpb-pack: %s: %d entries, version %s, read back\n", bundle, n, version)
	return bundle, nil
}

// mcpbStamp writes the version through a JSON decode and encode, never
// a substitution over text, keeping every key the manifest carries.
func mcpbStamp(raw []byte, version string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["version"] = version
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// mcpbPackSource finds the one file a staged entry's glob names. Two is
// a failure: the dist layout carries variants, and a bundle packed from
// the wrong binary is not something a checksum catches.
func mcpbPackSource(dist string, f mcpbStaged) (string, error) {
	pattern := filepath.Join(dist, filepath.FromSlash(f.glob))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s: nothing matches %s", f.path, pattern)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("%s: %s matches %d files (%s); exactly one is packed", f.path, pattern,
		len(matches), strings.Join(matches, ", "))
}

// mcpbPackHostVersion runs the staged binary for this host and requires
// its --version to name the version being stamped: the fifth of the
// places a bundle claims a version, and the one a user sees at runtime.
// A snapshot is exempt, since goreleaser stamps it from the last tag.
func mcpbPackHostVersion(sources map[string]string, version string) (string, error) {
	if strings.Contains(strings.ToLower(version), "snapshot") {
		return "a snapshot version; the --version read-back is skipped", nil
	}
	for _, f := range mcpbFiles {
		if f.goos != runtime.GOOS || (f.goarch != runtime.GOARCH && f.goarch != "all") {
			continue
		}
		path := sources[f.path]
		got, err := exec.Command(path, "--version").Output() //nolint:gosec // a binary this release built
		if err != nil {
			return "", fmt.Errorf("%s --version: %w", path, err)
		}
		if !strings.Contains(string(got), version) {
			return "", fmt.Errorf("the manifest would say %s and %s --version says %q", version, f.path,
				strings.TrimSpace(string(got)))
		}
		return fmt.Sprintf("%s --version reports %s", f.path, version), nil
	}
	return fmt.Sprintf("no staged binary runs on %s/%s; the --version read-back is skipped",
		runtime.GOOS, runtime.GOARCH), nil
}

// mcpbPackWrite builds the archive in memory, in a fixed entry order
// with a fixed time and mode, and writes it into place atomically.
func mcpbPackWrite(bundle string, manifest []byte, sources map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(bundle), 0o750); err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry := func(name string, mode os.FileMode) (io.Writer, error) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mcpbPackTime}
		h.SetMode(mode)
		return zw.CreateHeader(h)
	}
	w, err := entry("manifest.json", 0o644)
	if err != nil {
		return err
	}
	if _, err := w.Write(manifest); err != nil {
		return err
	}
	for _, f := range mcpbFiles {
		w, err := entry(f.path, mcpbPackMode)
		if err != nil {
			return err
		}
		if f.launcher {
			if _, err := io.WriteString(w, launcherScript(mcpbFiles)); err != nil {
				return err
			}
			continue
		}
		if err := mcpbPackCopy(w, sources[f.path]); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return writeFileAtomic(bundle, buf.Bytes())
}

func mcpbPackCopy(w io.Writer, path string) error {
	f, err := os.Open(path) //nolint:gosec // a path the packer resolved from dist
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// mcpbPackReadBack opens the written archive and holds it to the table:
// exactly the staged entries plus the manifest, each with the fixed mode
// and time, and the manifest carrying the version.
func mcpbPackReadBack(bundle, version string) (int, error) {
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		return 0, fmt.Errorf("read back %s: %w", bundle, err)
	}
	defer func() { _ = zr.Close() }()

	want := []string{"manifest.json"}
	for _, f := range mcpbFiles {
		want = append(want, f.path)
	}
	var got []string
	var problems []string
	for _, f := range zr.File {
		got = append(got, f.Name)
		if !f.Modified.Equal(mcpbPackTime) {
			problems = append(problems, fmt.Sprintf("%s is stamped %s", f.Name, f.Modified))
		}
		if f.Name != "manifest.json" && f.Mode().Perm() != mcpbPackMode {
			problems = append(problems, fmt.Sprintf("%s has mode %v", f.Name, f.Mode().Perm()))
		}
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				return 0, err
			}
			var doc struct {
				Version string `json:"version"`
			}
			err = json.NewDecoder(rc).Decode(&doc)
			_ = rc.Close()
			if err != nil || doc.Version != version {
				problems = append(problems, fmt.Sprintf("the packed manifest says version %q (%v)", doc.Version, err))
			}
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		problems = append(problems, fmt.Sprintf("the archive holds [%s], want [%s]",
			strings.Join(got, " "), strings.Join(want, " ")))
	}
	if len(problems) > 0 {
		return 0, fmt.Errorf("%s read back wrong: %s", bundle, strings.Join(problems, "; "))
	}
	return len(got), nil
}
