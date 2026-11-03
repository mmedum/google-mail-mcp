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
// required, or taking fewer values of an enum; an output no longer
// required, so no longer always there. Reported: what was added, and any other change to a tool
// (description, annotations, _meta, a schema reshaped), so a reviewer
// looks at it. A break passes only when the unreleased CHANGELOG
// declares it: a **Breaking:** entry names its tool, and no version
// heading there keeps the baseline's major (CLAUDE.md rule 14).
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
	if now.count["tool"] < schemaDiffMinTools {
		return fmt.Errorf("the built surface has %d tools, below the floor of %d", now.count["tool"], schemaDiffMinTools)
	}
	changes := schemaDiffReport(out, old, now)
	breaking := changes.breaking

	want := schemaBaselineVersion(changelog)
	var problems []string
	if want != "" && old.version != want {
		problems = append(problems, fmt.Sprintf("the baseline is the %q surface, but %s's newest release is %s; "+
			"record it in that release's commit with `make schema-baseline VERSION=%s`",
			old.version, changelogFile, want, want))
	}
	if len(breaking) > 0 {
		undeclared, err := schemaDiffUndeclared(breaking, changelog, old.version)
		switch {
		case err != nil:
			problems = append(problems, err.Error())
		case len(undeclared) > 0:
			for _, u := range undeclared {
				_, _ = fmt.Fprintln(out, "  undeclared: "+u)
			}
			problems = append(problems, fmt.Sprintf("%d breaking change(s) against %s that the CHANGELOG does not declare; "+
				"a release that makes one names its tool, in backticks, in a **Breaking:** entry above [%s], and "+
				"raises the major version", len(undeclared), schemaDiffBaseline, strings.TrimPrefix(old.version, "v")))
		default:
			_, _ = fmt.Fprintf(out, "  %d breaking change(s), each declared under **Breaking:** for a major release\n",
				len(breaking))
		}
	}
	if len(problems) == 0 && want != "" && strings.TrimSpace(stalenessUnreleased(changelog)) == "" && !changes.empty() {
		problems = append(problems, fmt.Sprintf("nothing is under [Unreleased], so this build is %s, and its surface "+
			"differs from the baseline; in %s's release commit, run `make schema-baseline VERSION=%s`, "+
			"otherwise say what changed under [Unreleased]", want, want, want))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// schemaDiffLists are the dump's lists of named items: the word the
// report gives an item of each, and the field that names it. Every
// other key but schemaDiffStamp is compared whole, so a list the dump
// gains later is still seen.
var schemaDiffLists = map[string]struct{ word, id string }{
	"tools":              {"tool", "name"},
	"resources":          {"resource", "uri"},
	"resource_templates": {"resource template", "uriTemplate"},
	"prompts":            {"prompt", "name"},
}

// schemaDiffStamp are the dump's keys that are not compared whole: the
// build's stamp, and the kinds, which are compared tool by tool.
var schemaDiffStamp = []string{"server", "version", "sdk_version", "kinds"}

// schemaDiffItem is one item of a dumped list. name is what a footer or
// a **Breaking:** entry names it by: a tool's or a prompt's name, a
// resource's URI or template.
type schemaDiffItem struct {
	name string
	tool bool
	body map[string]any
}

// schemaDiffSurface is a dump decoded generically, so every field a
// client can read is compared, including ones no type here names.
type schemaDiffSurface struct {
	version string
	kinds   map[string]string
	// items is every item of every list in schemaDiffLists, by how the
	// report says it: the list's word and the item's name.
	items map[string]schemaDiffItem
	// rest is every other key, such as the instructions.
	rest map[string]any
	// count is how many items each list holds, by its word.
	count map[string]int
}

func schemaDiffDecode(b []byte, source string) (*schemaDiffSurface, error) {
	d, err := parseDump(b, source)
	if err != nil {
		return nil, err
	}
	var whole map[string]any
	if err := json.Unmarshal(b, &whole); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	for _, k := range schemaDiffStamp {
		delete(whole, k)
	}
	s := &schemaDiffSurface{version: d.Version, kinds: d.Kinds, items: map[string]schemaDiffItem{},
		rest: map[string]any{}, count: map[string]int{}}
	for key, v := range whole {
		list, named := schemaDiffLists[key]
		if !named {
			s.rest[key] = v
			continue
		}
		entries, _ := v.([]any)
		for _, e := range entries {
			body, _ := e.(map[string]any)
			name, _ := body[list.id].(string)
			if name == "" {
				return nil, fmt.Errorf("%s: an entry of %s has no %s", source, key, list.id)
			}
			label := list.word + " " + name
			if _, dup := s.items[label]; dup {
				return nil, fmt.Errorf("%s: %s listed twice", source, label)
			}
			s.items[label] = schemaDiffItem{name: name, tool: key == "tools", body: body}
			s.count[list.word]++
		}
	}
	return s, nil
}

// A change is one difference between two dumps: the item it is to, which
// is what a footer or a **Breaking:** entry names, and how the report
// says it. field is what a break is matched by across two comparisons:
// on a tool still there the field it breaks, such as "input id", and on
// a removed item its whole label, list included, so a removed prompt and
// a tool of one name differ.
type change struct {
	item, text, field string
}

// surfaceChanges is what differs between two surfaces. breaking is what
// breaks a caller; changed is every other difference, a description, an
// annotation, a _meta mark, a kind or the instructions, for a reviewer
// to look at.
type surfaceChanges struct {
	added, changed, breaking []change
}

func (c surfaceChanges) empty() bool {
	return len(c.added) == 0 && len(c.changed) == 0 && len(c.breaking) == 0
}

// schemaDiffCompare is what changed from old to now. An item gone breaks
// a caller, and so does a tool still there that broke at any depth
// (schemaDiffTool). Every other difference is listed, a broken tool's
// included: the break names only the fields it broke.
func schemaDiffCompare(old, now *schemaDiffSurface) surfaceChanges {
	var c surfaceChanges
	var fields []change
	for _, label := range sortedUnion(old.items, now.items) {
		was, inOld := old.items[label]
		is, inNow := now.items[label]
		switch {
		case !inNow:
			c.breaking = append(c.breaking, change{item: was.name, text: label + " removed", field: label})
		case !inOld:
			c.added = append(c.added, change{item: is.name, text: label})
		default:
			who := label
			if was.tool {
				who = was.name
			}
			for _, k := range sortedUnion(was.body, is.body) {
				if !reflect.DeepEqual(was.body[k], is.body[k]) {
					c.changed = append(c.changed, change{item: was.name, text: who + ": " + k + " changed"})
				}
			}
			if !was.tool {
				continue
			}
			// The kind decides which setting a tool sits behind.
			if k, n := old.kinds[was.name], now.kinds[was.name]; k != n {
				c.changed = append(c.changed, change{item: was.name,
					text: fmt.Sprintf("%s: kind changed from %q to %q", was.name, k, n)})
			}
			fields = append(fields, schemaDiffTool(was.name, was.body, is.body)...)
		}
	}
	for _, part := range changedParts(old.rest, now.rest) {
		top, _, _ := strings.Cut(part, ".")
		c.changed = append(c.changed, change{item: top, text: part + " changed"})
	}
	c.breaking = append(c.breaking, fields...)
	return c
}

// write prints the changes, one a line.
func (c surfaceChanges) write(out io.Writer) {
	for _, a := range c.added {
		_, _ = fmt.Fprintln(out, "  added: "+a.text)
	}
	for _, ch := range c.changed {
		_, _ = fmt.Fprintln(out, "  look at this: "+ch.text)
	}
	for _, b := range c.breaking {
		_, _ = fmt.Fprintln(out, "  BREAKING: "+b.text)
	}
}

// schemaDiffReport prints what changed from old to now, under a line
// counting both surfaces, and returns it.
func schemaDiffReport(out io.Writer, old, now *schemaDiffSurface) surfaceChanges {
	c := schemaDiffCompare(old, now)
	_, _ = fmt.Fprintf(out, "schema-diff: baseline (%s) %d tools, %d resources, %d templates; built %d tools, %d resources, %d templates\n",
		old.version, old.count["tool"], old.count["resource"], old.count["resource template"],
		now.count["tool"], now.count["resource"], now.count["resource template"])
	c.write(out)
	return c
}

// changedParts names what differs between two dumped objects, one level
// deep, so a changed mode's instructions read as instructions.read_only.
// Values are compared decoded, so key order is no change.
func changedParts(o, n map[string]any) []string {
	var parts []string
	for _, k := range sortedUnion(o, n) {
		if reflect.DeepEqual(o[k], n[k]) {
			continue
		}
		om, oMap := o[k].(map[string]any)
		nm, nMap := n[k].(map[string]any)
		if !oMap || !nMap {
			parts = append(parts, k)
			continue
		}
		for _, sub := range sortedUnion(om, nm) {
			if !reflect.DeepEqual(om[sub], nm[sub]) {
				parts = append(parts, k+"."+sub)
			}
		}
	}
	return parts
}

// sortedUnion is every key of a and b, sorted.
func sortedUnion[V any](a, b map[string]V) []string {
	keys := slices.AppendSeq(slices.Collect(maps.Keys(a)), maps.Keys(b))
	slices.Sort(keys)
	return slices.Compact(keys)
}

// schemaDiffTool is the breaking half for one tool.
func schemaDiffTool(name string, was, now map[string]any) []change {
	var out []change
	wasIn, _ := was["inputSchema"].(map[string]any)
	nowIn, _ := now["inputSchema"].(map[string]any)
	out = append(out, schemaDiffFields(name, "", wasIn, nowIn, true)...)
	wasOut, _ := was["outputSchema"].(map[string]any)
	nowOut, _ := now["outputSchema"].(map[string]any)
	if wasOut != nil && nowOut == nil {
		out = append(out, change{item: name, text: name + ": output schema removed", field: "output"})
	}
	out = append(out, schemaDiffFields(name, "", wasOut, nowOut, false)...)
	return out
}

// schemaBroke is a break of tool's input or output field at path, said
// as the report says it, so the text and the field cannot drift apart.
func schemaBroke(tool string, input bool, path, how string) change {
	side := "output"
	if input {
		side = "input"
	}
	return change{item: tool, text: tool + " " + side + ": field " + path + " " + how, field: side + " " + path}
}

// schemaDiffFields walks two object schemas together. A property lost
// or retyped at any depth is breaking; for inputs, so is a property
// newly required, including a nested one under a property that existed;
// for outputs, a property no longer required, which a caller read as
// always there.
//
// A path names a field the way a caller reaches it: `opts.deep`, and
// `messages[].id` for a field of each element of a list.
func schemaDiffFields(tool, prefix string, was, now map[string]any, input bool) []change {
	if was == nil || now == nil {
		return nil
	}
	var out []change
	wasProps, _ := was["properties"].(map[string]any)
	nowProps, _ := now["properties"].(map[string]any)
	for _, p := range slices.Sorted(maps.Keys(wasProps)) {
		child, ok := nowProps[p]
		if !ok {
			out = append(out, schemaBroke(tool, input, prefix+p, "removed"))
			continue
		}
		out = append(out, schemaDiffField(tool, prefix+p, schemaDiffNode(wasProps[p]), schemaDiffNode(child), input)...)
	}
	wasRequired, nowRequired := schemaDiffSet(was["required"]), schemaDiffSet(now["required"])
	if input {
		for _, r := range schemaDiffStrings(now["required"]) {
			if !wasRequired[r] {
				out = append(out, schemaBroke(tool, input, prefix+r, "newly required"))
			}
		}
		return out
	}
	for _, r := range schemaDiffStrings(was["required"]) {
		// A field removed is reported as removed.
		if _, kept := nowProps[r]; kept && !nowRequired[r] {
			out = append(out, schemaBroke(tool, input, prefix+r, "no longer always there"))
		}
	}
	return out
}

func schemaDiffSet(v any) map[string]bool {
	set := map[string]bool{}
	for _, s := range schemaDiffStrings(v) {
		set[s] = true
	}
	return set
}

// schemaDiffEnumBreaks says how an input's enum change breaks a caller,
// or "" when it does not: a value it took and no longer takes, or a
// limit to a list where it took any value.
func schemaDiffEnumBreaks(was, now any) string {
	nowList, limited := now.([]any)
	if !limited {
		return ""
	}
	wasList, _ := was.([]any)
	if wasList == nil {
		b, _ := json.Marshal(nowList)
		return "now takes only " + string(b)
	}
	var lost []any
	for _, v := range wasList {
		if !slices.ContainsFunc(nowList, func(w any) bool { return reflect.DeepEqual(v, w) }) {
			lost = append(lost, v)
		}
	}
	if len(lost) == 0 {
		return ""
	}
	b, _ := json.Marshal(lost)
	return "no longer takes " + string(b)
}

// schemaDiffField compares one field both schemas have: its type, then
// the fields inside it, then a list's elements. A list with no schema
// for its elements takes any, as the schema `true` does.
func schemaDiffField(tool, path string, was, now map[string]any, input bool) []change {
	if was == nil || now == nil {
		return nil
	}
	var out []change
	if b := schemaDiffTypeBreaks(was["type"], now["type"], input); b != "" {
		out = append(out, schemaBroke(tool, input, path, b))
	}
	if input && schemaDiffAny(now) {
		// An input that takes any value breaks no caller, whatever was
		// inside it before.
		return out
	}
	if b := schemaDiffEnumBreaks(was["enum"], now["enum"]); input && b != "" {
		out = append(out, schemaBroke(tool, input, path, b))
	}
	out = append(out, schemaDiffFields(tool, path+".", was, now, input)...)
	wasItems, nowItems := schemaDiffNode(was["items"]), schemaDiffNode(now["items"])
	if wasItems != nil || nowItems != nil {
		if wasItems == nil {
			wasItems = map[string]any{}
		}
		if nowItems == nil {
			nowItems = map[string]any{}
		}
		out = append(out, schemaDiffField(tool, path+"[]", wasItems, nowItems, input)...)
	}
	return out
}

// schemaDiffNode is a schema as an object. JSON Schema lets a schema be
// a boolean: `true` takes any value and reads as an object with no type,
// `false` takes none and reads as one whose type list is empty. Anything
// else is nil.
func schemaDiffNode(v any) map[string]any {
	switch s := v.(type) {
	case map[string]any:
		return s
	case bool:
		if s {
			return map[string]any{}
		}
		return map[string]any{"type": []any{}}
	}
	return nil
}

// schemaDiffAny reports a schema that takes any value: no type, enum,
// fields or elements of its own.
func schemaDiffAny(s map[string]any) bool {
	for _, k := range []string{"type", "enum", "properties", "items"} {
		if _, ok := s[k]; ok {
			return false
		}
	}
	return true
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
		switch {
		case t == nil:
			return "any"
		case len(schemaDiffTypeList(t)) == 0:
			return "none"
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

// schemaDiffUndeclared is each breaking change that the unreleased part
// of the CHANGELOG, everything above the baseline's own heading, does
// not declare: a **Breaking:** entry there names its tool, resource or
// template in backticks. A version heading there that does not raise
// the baseline's major is one too, since only a major release may break
// the surface.
func schemaDiffUndeclared(breaking []change, changelog, baseline string) ([]string, error) {
	version := strings.TrimPrefix(baseline, "v")
	unreleased, _, found := strings.Cut(changelog, "\n## ["+version+"]")
	if !found {
		return nil, fmt.Errorf("%s has no ## [%s] heading for the baseline %s, so no break can be declared against it",
			changelogFile, version, baseline)
	}
	named := map[string]bool{}
	for line := range strings.Lines(unreleased) {
		if !strings.Contains(line, "**Breaking:**") {
			continue
		}
		for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
			named[m[1]] = true
		}
	}
	var out []string
	for _, b := range breaking {
		if !named[b.item] {
			out = append(out, b.text)
		}
	}
	for _, m := range changelogVersionHeading.FindAllStringSubmatch(unreleased, -1) {
		if !schemaNewMajor(baseline, "v"+m[1]) {
			out = append(out, fmt.Sprintf("release %s breaks the surface without raising the major version from %s", m[1], version))
		}
	}
	return out, nil
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
