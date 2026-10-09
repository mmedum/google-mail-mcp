package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// schemaBaseline records the surface of the release being cut as
// testdata/schema-baseline.json. The release commit runs it, as `make
// schema-baseline VERSION=vX.Y.Z`, so the baseline lands with the
// CHANGELOG heading that names it. It is never run to make schema-diff
// pass.
func schemaBaseline(out io.Writer, args []string) error {
	bin, cleanup, err := serverBinary(args)
	if err != nil {
		return err
	}
	defer cleanup()
	_, raw, err := dumpSchemas(bin)
	if err != nil {
		return err
	}
	changelog, err := os.ReadFile(changelogFile)
	if err != nil {
		return err
	}
	prev, err := os.ReadFile(schemaDiffBaseline)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	norm, tools, err := schemaBaselineRecord(out, string(changelog), prev, raw)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(schemaDiffBaseline, norm); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "wrote %s: the %s surface, %d tools\n", schemaDiffBaseline, schemaBaselineVersion(string(changelog)), tools)
	return nil
}

// schemaBaselineRecord decides whether raw, a dump of the build, may
// become the baseline, and returns it normalized with its tool count.
// prev is the baseline it would replace, or nil when there is none.
//
// It compares the build with prev first, and refuses a change that
// breaks a caller unless the release is a new major version, which is
// what a break has to ship as. Overwriting first would leave the diff
// comparing the release with itself.
func schemaBaselineRecord(out io.Writer, changelog string, prev, raw []byte) ([]byte, int, error) {
	want := schemaBaselineVersion(changelog)
	if want == "" {
		return nil, 0, fmt.Errorf("%s names no release yet, so there is no surface to record", changelogFile)
	}
	now, err := schemaDiffDecode(raw, "the built binary")
	if err != nil {
		return nil, 0, err
	}
	if now.version != want {
		return nil, 0, fmt.Errorf("the build is stamped %q, but the release being cut is %s; "+
			"run `make schema-baseline VERSION=%s`", now.version, want, want)
	}
	if prev != nil {
		old, err := schemaDiffDecode(prev, schemaDiffBaseline)
		if err != nil {
			return nil, 0, err
		}
		if breaking := schemaDiffReport(out, old, now); len(breaking) > 0 && !schemaNewMajor(old.version, want) {
			msg := fmt.Sprintf("%s breaks a caller of %q in %d way(s) above, and is not a new major version; %s is unchanged",
				want, old.version, len(breaking), schemaDiffBaseline)
			if old.version == want {
				msg += fmt.Sprintf(". It already holds %s from an earlier run, so a tool new in %s counts as released; "+
					"restore the last release's baseline from its tag, then run this again", want, want)
			}
			return nil, 0, errors.New(msg)
		}
	}
	norm, tools, err := baselineNormalize(raw)
	if err != nil {
		return nil, 0, err
	}
	if tools < schemaDiffMinTools {
		return nil, 0, fmt.Errorf("the built surface has %d tools, below the floor of %d; not writing it", tools, schemaDiffMinTools)
	}
	return norm, tools, nil
}

// baselineNormalize sorts every list the dump carries by its key and
// indents the result, so a regenerated baseline differs only where the
// surface did.
func baselineNormalize(raw []byte) ([]byte, int, error) {
	if _, err := parseDump(raw, "the built binary"); err != nil {
		return nil, 0, err
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, 0, err
	}
	sortBy := func(field, key string) {
		list, _ := d[field].([]any)
		slices.SortFunc(list, func(a, b any) int {
			am, _ := a.(map[string]any)
			bm, _ := b.(map[string]any)
			as, _ := am[key].(string)
			bs, _ := bm[key].(string)
			switch {
			case as < bs:
				return -1
			case as > bs:
				return 1
			}
			return 0
		})
	}
	sortBy("tools", "name")
	sortBy("resources", "uri")
	sortBy("resource_templates", "uriTemplate")
	tools, _ := d["tools"].([]any)
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	return append(b, '\n'), len(tools), nil
}
