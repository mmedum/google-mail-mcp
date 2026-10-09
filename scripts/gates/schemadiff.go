package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// schemaDiffBaseline is the surface of the CHANGELOG's newest release,
// recorded in that release's commit. A change may add to it and never
// take from it (CLAUDE.md rule 14). It is a committed file, never a tag:
// CI's checkout is shallow and has no tags.
const schemaDiffBaseline = "testdata/schema-baseline.json"

// schemaDiffMinTools is the floor on the built surface: every tool of
// the released surface, which the dump registers whatever the flags say. Raise it when a
// release adds tools; the baseline's own count is held too, so it can
// only rise.
const schemaDiffMinTools = 27

// schemaDiff compares the built binary's whole surface — every tool as
// the SDK lists it, resources and templates — with the baseline.
//
// Breaking, and failed: a tool, resource or template removed; an input
// or output field lost or retyped, at any depth; an input newly
// required. Reported: what was added, and any other change to a tool
// (description, annotations, _meta, a schema reshaped), so a reviewer
// looks at it.
//
// It also fails when the baseline is not the newest release's: an older
// one protects an older surface, so whatever shipped since could be
// dropped and nothing would say. With nothing under [Unreleased] the
// build is that release, so its surface must be the baseline's exactly,
// which proves a release commit recorded the baseline rather than
// relabeling it.
func schemaDiff(out io.Writer, args []string) error {
	bin, cleanup, err := serverBinary(args)
	if err != nil {
		return err
	}
	defer cleanup()
	_, current, err := dumpSchemas(bin)
	if err != nil {
		return err
	}
	changelog, err := os.ReadFile(changelogFile)
	if err != nil {
		return err
	}
	baseline, err := os.ReadFile(schemaDiffBaseline)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no baseline at %s: record the newest release's surface with `make schema-baseline VERSION=vX.Y.Z` "+
			"and commit it; a missing baseline must not pass as an unchanged surface", schemaDiffBaseline)
	}
	if err != nil {
		return err
	}
	return schemaDiffCheck(out, string(changelog), baseline, current)
}

// schemaDiffCheck is the gate on what it reads: the CHANGELOG, the
// baseline and the built binary's dump.
func schemaDiffCheck(out io.Writer, changelog string, baselineBytes, currentBytes []byte) error {
	old, err := schemaDiffDecode(baselineBytes, schemaDiffBaseline)
	if err != nil {
		return err
	}
	now, err := schemaDiffDecode(currentBytes, "the built binary")
	if err != nil {
		return err
	}
	if len(now.tools) < schemaDiffMinTools {
		return fmt.Errorf("the built surface has %d tools, below the floor of %d", len(now.tools), schemaDiffMinTools)
	}
	breaking := schemaDiffReport(out, old, now)

	want := schemaBaselineVersion(changelog)
	var problems []string
	if want != "" && old.version != want {
		problems = append(problems, fmt.Sprintf("the baseline is the %q surface, but %s's newest release is %s; "+
			"record it in that release's commit with `make schema-baseline VERSION=%s`",
			old.version, changelogFile, want, want))
	}
	if len(breaking) > 0 {
		problems = append(problems, fmt.Sprintf("%d breaking change(s) against %s", len(breaking), schemaDiffBaseline))
	}
	if len(problems) == 0 && want != "" && strings.TrimSpace(stalenessUnreleased(changelog)) == "" &&
		!schemaDiffSame(baselineBytes, currentBytes) {
		problems = append(problems, fmt.Sprintf("nothing is under [Unreleased], so this build is %s, and its surface "+
			"differs from the baseline; in %s's release commit, run `make schema-baseline VERSION=%s`, "+
			"otherwise say what changed under [Unreleased]", want, want, want))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// schemaDiffSurface is a dump decoded generically, so every field a
// client can read is compared, including ones no type here names.
type schemaDiffSurface struct {
	version   string
	kinds     map[string]string
	tools     map[string]map[string]any
	resources map[string]map[string]any
	templates map[string]map[string]any
}

func schemaDiffDecode(b []byte, source string) (*schemaDiffSurface, error) {
	d, err := parseDump(b, source)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Tools             []map[string]any `json:"tools"`
		Resources         []map[string]any `json:"resources"`
		ResourceTemplates []map[string]any `json:"resource_templates"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	s := &schemaDiffSurface{version: d.Version, kinds: d.Kinds, tools: map[string]map[string]any{},
		resources: map[string]map[string]any{}, templates: map[string]map[string]any{}}
	index := func(items []map[string]any, key string, into map[string]map[string]any) error {
		for _, it := range items {
			k, _ := it[key].(string)
			if k == "" {
				return fmt.Errorf("%s: an entry has no %s", source, key)
			}
			if _, dup := into[k]; dup {
				return fmt.Errorf("%s: %s listed twice", source, k)
			}
			into[k] = it
		}
		return nil
	}
	if err := index(raw.Tools, "name", s.tools); err != nil {
		return nil, err
	}
	if err := index(raw.Resources, "uri", s.resources); err != nil {
		return nil, err
	}
	if err := index(raw.ResourceTemplates, "uriTemplate", s.templates); err != nil {
		return nil, err
	}
	return s, nil
}

// schemaDiffReport prints what changed from old to now, and returns what
// breaks a caller.
func schemaDiffReport(out io.Writer, old, now *schemaDiffSurface) []string {
	var breaking, changed []string
	breaking = append(breaking, schemaDiffRemoved("tool", old.tools, now.tools)...)
	breaking = append(breaking, schemaDiffRemoved("resource", old.resources, now.resources)...)
	breaking = append(breaking, schemaDiffRemoved("resource template", old.templates, now.templates)...)

	for _, name := range slices.Sorted(maps.Keys(old.tools)) {
		was, ok := now.tools[name]
		if !ok {
			continue
		}
		before := old.tools[name]
		b := schemaDiffTool(name, before, was)
		breaking = append(breaking, b...)
		for _, key := range slices.Sorted(maps.Keys(schemaDiffUnion(before, was))) {
			if !reflect.DeepEqual(before[key], was[key]) {
				changed = append(changed, name+": "+key+" changed")
			}
		}
		// The kind decides which setting a tool sits behind.
		if k, n := old.kinds[name], now.kinds[name]; k != n {
			changed = append(changed, fmt.Sprintf("%s: kind changed from %q to %q", name, k, n))
		}
	}
	for _, kind := range []struct {
		what     string
		old, now map[string]map[string]any
	}{{"resource", old.resources, now.resources}, {"resource template", old.templates, now.templates}} {
		for _, k := range slices.Sorted(maps.Keys(kind.old)) {
			if n, ok := kind.now[k]; ok && !reflect.DeepEqual(kind.old[k], n) {
				changed = append(changed, kind.what+" "+k+" changed")
			}
		}
	}

	added := schemaDiffRemoved("tool", now.tools, old.tools)
	added = append(added, schemaDiffRemoved("resource", now.resources, old.resources)...)
	added = append(added, schemaDiffRemoved("resource template", now.templates, old.templates)...)

	_, _ = fmt.Fprintf(out, "schema-diff: baseline (%s) %d tools, %d resources, %d templates; built %d tools, %d resources, %d templates\n",
		old.version, len(old.tools), len(old.resources), len(old.templates), len(now.tools), len(now.resources), len(now.templates))
	for _, a := range added {
		_, _ = fmt.Fprintln(out, "  added: "+strings.TrimSuffix(a, " removed"))
	}
	for _, c := range changed {
		_, _ = fmt.Fprintln(out, "  look at this: "+c)
	}
	for _, b := range breaking {
		_, _ = fmt.Fprintln(out, "  BREAKING: "+b)
	}
	return breaking
}

// schemaDiffSame reports whether two dumps publish the same surface,
// field for field: tools, resources, templates and kinds. The version
// and SDK stamps are not part of it, and neither is the order of keys
// or of the lists.
func schemaDiffSame(a, b []byte) bool {
	var surfaces [2]map[string]any
	for i, raw := range [][]byte{a, b} {
		norm, _, err := baselineNormalize(raw)
		if err != nil || json.Unmarshal(norm, &surfaces[i]) != nil {
			return false
		}
		delete(surfaces[i], "version")
		delete(surfaces[i], "sdk_version")
	}
	return reflect.DeepEqual(surfaces[0], surfaces[1])
}

func schemaDiffUnion(a, b map[string]any) map[string]bool {
	u := map[string]bool{}
	for k := range a {
		u[k] = true
	}
	for k := range b {
		u[k] = true
	}
	return u
}

func schemaDiffRemoved(what string, old, now map[string]map[string]any) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(old)) {
		if _, ok := now[k]; !ok {
			out = append(out, what+" "+k+" removed")
		}
	}
	return out
}

// schemaDiffTool is the breaking half for one tool.
func schemaDiffTool(name string, was, now map[string]any) []string {
	var out []string
	wasIn, _ := was["inputSchema"].(map[string]any)
	nowIn, _ := now["inputSchema"].(map[string]any)
	out = append(out, schemaDiffFields(name+" input", "", wasIn, nowIn, true)...)
	wasOut, _ := was["outputSchema"].(map[string]any)
	nowOut, _ := now["outputSchema"].(map[string]any)
	if wasOut != nil && nowOut == nil {
		out = append(out, name+": output schema removed")
	}
	out = append(out, schemaDiffFields(name+" output", "", wasOut, nowOut, false)...)
	return out
}

// schemaDiffFields walks two object schemas together. A property lost
// or retyped at any depth is breaking; for inputs, so is a property
// newly required, including a nested one under a property that existed.
//
// A path names a field the way a caller reaches it: `opts.deep`, and
// `messages[].id` for a field of each element of a list.
func schemaDiffFields(where, prefix string, was, now map[string]any, input bool) []string {
	if was == nil || now == nil {
		return nil
	}
	var out []string
	wasProps, _ := was["properties"].(map[string]any)
	nowProps, _ := now["properties"].(map[string]any)
	for _, p := range slices.Sorted(maps.Keys(wasProps)) {
		child, ok := nowProps[p]
		if !ok {
			out = append(out, fmt.Sprintf("%s: field %s%s removed", where, prefix, p))
			continue
		}
		wc, _ := wasProps[p].(map[string]any)
		nc, _ := child.(map[string]any)
		out = append(out, schemaDiffField(where, prefix+p, wc, nc, input)...)
	}
	if input {
		required := map[string]bool{}
		for _, r := range schemaDiffStrings(was["required"]) {
			required[r] = true
		}
		for _, r := range schemaDiffStrings(now["required"]) {
			if !required[r] {
				out = append(out, fmt.Sprintf("%s: field %s%s newly required", where, prefix, r))
			}
		}
	}
	return out
}

// schemaDiffField compares one field both schemas have: its type, then
// the fields inside it, then a list's elements.
func schemaDiffField(where, path string, was, now map[string]any, input bool) []string {
	if was == nil || now == nil {
		return nil
	}
	var out []string
	if b := schemaDiffTypeBreaks(was["type"], now["type"], input); b != "" {
		out = append(out, fmt.Sprintf("%s: field %s %s", where, path, b))
	}
	out = append(out, schemaDiffFields(where, path+".", was, now, input)...)
	wasItems, _ := was["items"].(map[string]any)
	nowItems, _ := now["items"].(map[string]any)
	switch {
	case wasItems != nil && nowItems == nil:
		out = append(out, fmt.Sprintf("%s: field %s[] removed", where, path))
	case wasItems != nil:
		out = append(out, schemaDiffField(where, path+"[]", wasItems, nowItems, input)...)
	}
	return out
}

// schemaDiffTypeBreaks says how a field's type change breaks a caller,
// or "" when it does not. An input may take more types than it did, and
// an output may return fewer; the other way round a caller that sent or
// read the old type is broken. `"string"` to `["null","string"]` is a
// break in an output, where a caller read the field as always there.
// No type at all is any type.
func schemaDiffTypeBreaks(was, now any, input bool) string {
	if reflect.DeepEqual(was, now) {
		return ""
	}
	wide, narrow := now, was
	if !input {
		wide, narrow = was, now
	}
	if schemaDiffTypesCover(wide, narrow) {
		return ""
	}
	spell := func(t any) string {
		if t == nil {
			return "any"
		}
		b, _ := json.Marshal(t)
		return string(b)
	}
	return fmt.Sprintf("changed type from %s to %s", spell(was), spell(now))
}

// schemaDiffTypesCover reports whether every type narrow allows, wide
// allows too.
func schemaDiffTypesCover(wide, narrow any) bool {
	if wide == nil {
		return true
	}
	if narrow == nil {
		return false
	}
	allowed := map[string]bool{}
	for _, t := range schemaDiffTypeList(wide) {
		allowed[t] = true
	}
	// An integer is a number.
	if allowed["number"] {
		allowed["integer"] = true
	}
	for _, t := range schemaDiffTypeList(narrow) {
		if !allowed[t] {
			return false
		}
	}
	return true
}

// schemaDiffTypeList is a schema's type as a list, whether it was
// written as one name or several.
func schemaDiffTypeList(t any) []string {
	if s, ok := t.(string); ok {
		return []string{s}
	}
	return schemaDiffStrings(t)
}

func schemaDiffStrings(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// schemaBaselineVersion is the release whose surface the baseline must
// hold: the CHANGELOG's newest version heading, or empty before the
// first release.
//
// Between releases that heading is the last tag. In a release commit it
// is the release being cut, and the baseline is recorded in that same
// commit. Recording it after the tag instead would fail every branch
// from the moment the tag is pushed until a second change lands.
func schemaBaselineVersion(changelog string) string {
	if m := changelogVersionHeading.FindStringSubmatch(changelog); m != nil {
		return "v" + m[1]
	}
	return ""
}

// schemaNewMajor reports whether release to is a later major version
// than release from. An unreadable version is not.
func schemaNewMajor(from, to string) bool {
	major := func(v string) int {
		head, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), ".")
		n, err := strconv.Atoi(head)
		if err != nil {
			return -1
		}
		return n
	}
	f, t := major(from), major(to)
	return f >= 0 && t > f
}
