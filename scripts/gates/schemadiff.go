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
	"strings"
)

// schemaDiffBaseline is the released surface a change may add to and
// never take from (CLAUDE.md rule 14). It is committed, so the gate
// works before the first tag.
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
// or output field lost, at any depth; an input newly required. Reported:
// what was added, and any other change to a tool (description,
// annotations, _meta, a schema reshaped), so a reviewer looks at it.
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
	baseline, err := os.ReadFile(schemaDiffBaseline)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no baseline at %s: run `go run ./scripts/gates schema-baseline` and commit it; "+
			"a missing baseline must not pass as an unchanged surface", schemaDiffBaseline)
	}
	if err != nil {
		return err
	}
	return schemaDiffCompare(out, baseline, current)
}

// schemaDiffSurface is a dump decoded generically, so every field a
// client can read is compared, including ones no type here names.
type schemaDiffSurface struct {
	tools     map[string]map[string]any
	resources map[string]map[string]any
	templates map[string]map[string]any
}

func schemaDiffDecode(b []byte, source string) (*schemaDiffSurface, error) {
	if _, err := parseDump(b, source); err != nil {
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
	s := &schemaDiffSurface{tools: map[string]map[string]any{}, resources: map[string]map[string]any{},
		templates: map[string]map[string]any{}}
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

func schemaDiffCompare(out io.Writer, baselineBytes, currentBytes []byte) error {
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

	_, _ = fmt.Fprintf(out, "schema-diff: baseline %d tools, %d resources, %d templates; built %d tools, %d resources, %d templates\n",
		len(old.tools), len(old.resources), len(old.templates), len(now.tools), len(now.resources), len(now.templates))
	for _, a := range added {
		_, _ = fmt.Fprintln(out, "  added: "+strings.TrimSuffix(a, " removed"))
	}
	for _, c := range changed {
		_, _ = fmt.Fprintln(out, "  look at this: "+c)
	}
	if len(breaking) > 0 {
		for _, b := range breaking {
			_, _ = fmt.Fprintln(out, "  BREAKING: "+b)
		}
		return fmt.Errorf("%d breaking change(s) against %s", len(breaking), schemaDiffBaseline)
	}
	return nil
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
// at any depth is breaking; for inputs, so is a property newly
// required, including a nested one under a property that existed.
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
		wc := schemaDiffObject(wasProps[p])
		nc := schemaDiffObject(child)
		out = append(out, schemaDiffFields(where, prefix+p+".", wc, nc, input)...)
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

// schemaDiffObject is a property's object schema, looking through an
// array's items.
func schemaDiffObject(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if items, ok := m["items"].(map[string]any); ok {
		return items
	}
	return m
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
