package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// schemaBaseline writes testdata/schema-baseline.json from the built
// binary. It is how the released surface is frozen: run it when cutting
// a release, never to make schema-diff pass.
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
	norm, tools, err := baselineNormalize(raw)
	if err != nil {
		return err
	}
	if tools < schemaDiffMinTools {
		return fmt.Errorf("the built surface has %d tools, below the floor of %d; not writing it", tools, schemaDiffMinTools)
	}
	if err := writeFileAtomic(schemaDiffBaseline, norm); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "wrote %s: %d tools\n", schemaDiffBaseline, tools)
	return nil
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
