package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// api-fields is api-coverage one level down: every field a judged
// schema publishes is modeled by internal/gmail or left out with a
// reason, and every field internal/gmail models is one Google publishes.
//
// A judged schema is one §8b names or one internal/gmail declares a
// struct for. Structs match schemas by name, and a struct with no schema
// of its name fails, so a rename cannot take a type out of the check.

const (
	apiFieldsRecordPath = "testdata/api-fields.tsv"
	apiFieldsWireDir    = "internal/gmail"
	// apiFieldsMinReason is the shortest reason an out row may give.
	apiFieldsMinReason = 20
	// apiFieldsMinJudged and apiFieldsMinModeled are floors on what the
	// gate read, measured 2026-09-25: 130 published fields judged, 125
	// modeled.
	apiFieldsMinJudged  = 125
	apiFieldsMinModeled = 120
)

// apiFieldsRequired is §8b's list: judged whether or not a struct
// models them yet.
var apiFieldsRequired = []string{
	"Message", "MessagePart", "MessagePartHeader", "MessagePartBody", "Thread", "Draft", "Label",
	"LabelColor", "History", "HistoryMessageAdded", "HistoryMessageDeleted", "HistoryLabelAdded",
	"HistoryLabelRemoved", "Profile", "ListMessagesResponse", "ListThreadsResponse",
	"ListDraftsResponse", "ListHistoryResponse", "VacationSettings", "AutoForwarding",
	"ForwardingAddress", "ImapSettings", "PopSettings", "LanguageSettings", "SendAs", "Filter",
	"FilterCriteria", "FilterAction",
}

// apiFieldsLimits are the judged list and the floors, a parameter so a
// test can use a small fixture.
type apiFieldsLimits struct {
	required        []string
	judged, modeled int
}

var apiFieldsFloors = apiFieldsLimits{apiFieldsRequired, apiFieldsMinJudged, apiFieldsMinModeled}

func apiFields(out io.Writer, _ []string) error { return apiFieldsCheck(".", apiFieldsFloors, out) }

func apiFieldsCheck(root string, floor apiFieldsLimits, out io.Writer) error {
	surface, err := discoveryLoad(filepath.Join(root, discoverySnapshotPath))
	if err != nil {
		return fmt.Errorf("read the snapshot (run `go run ./scripts/gates api-diff`): %w", err)
	}
	published := surface.fieldsBySchema()
	structs, files, err := apiFieldsStructs(root)
	if err != nil {
		return err
	}
	if len(structs) == 0 {
		return fmt.Errorf("%s declares no structs in %d files; the gate would judge nothing", apiFieldsWireDir, files)
	}
	rows, problems := readTSV(filepath.Join(root, apiFieldsRecordPath), 3)

	judged := map[string]bool{}
	for _, name := range floor.required {
		judged[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(structs)) {
		if _, ok := published[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s declares %s, and Google publishes no schema of that "+
				"name; a wire struct is named for the schema it models", apiFieldsWireDir, name))
			continue
		}
		judged[name] = true
	}

	record := map[string]tsvRow{}
	modeled := 0
	for _, r := range rows {
		record[r.fields[0]] = r
		counted, rowProblems := apiFieldsRow(r, published, judged, structs)
		if counted {
			modeled++
		}
		problems = append(problems, rowProblems...)
	}

	total := 0
	for _, schema := range slices.Sorted(maps.Keys(judged)) {
		fields, ok := published[schema]
		if !ok {
			problems = append(problems, fmt.Sprintf("§8b names %s and the snapshot publishes no such schema", schema))
			continue
		}
		for _, f := range fields {
			total++
			if _, ok := record[schema+"."+f]; !ok {
				problems = append(problems, fmt.Sprintf("%s.%s is published and has no row in %s",
					schema, f, apiFieldsRecordPath))
			}
		}
		for _, tag := range slices.Sorted(maps.Keys(structs[schema])) {
			if !slices.Contains(fields, tag) {
				problems = append(problems, fmt.Sprintf("%s.%s: %s models a field Google does not publish",
					schema, tag, structs[schema][tag]))
			}
		}
	}
	if total < floor.judged {
		problems = append(problems, fmt.Sprintf("%d published fields judged, below the floor of %d", total, floor.judged))
	}
	if modeled < floor.modeled {
		problems = append(problems, fmt.Sprintf("%d fields modeled, below the floor of %d", modeled, floor.modeled))
	}
	if err := problemsError(out, apiFieldsRecordPath, problems); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "api-fields ok: %d schemas judged, %d published fields, %d modeled, %d out; "+
		"%d structs in %d files of %s\n", len(judged), total, modeled, total-modeled, len(structs), files,
		apiFieldsWireDir)
	return nil
}

// apiFieldsRow checks one record row against the snapshot and the wire
// structs. counted is whether the row says modeled.
func apiFieldsRow(r tsvRow, published map[string][]string, judged map[string]bool,
	structs map[string]map[string]string,
) (counted bool, problems []string) {
	key, verdict, reason := r.fields[0], r.fields[1], r.fields[2]
	at := fmt.Sprintf("%s:%d", apiFieldsRecordPath, r.line)
	schema, field, ok := strings.Cut(key, ".")
	if !ok {
		return false, []string{fmt.Sprintf("%s: %q is not Schema.field", at, key)}
	}
	if !slices.Contains(published[schema], field) {
		return false, []string{fmt.Sprintf("%s: %s is not a published field; remove the row", at, key)}
	}
	if !judged[schema] {
		problems = append(problems, fmt.Sprintf("%s: %s is not a judged schema; §8b does not name it "+
			"and no struct models it", at, schema))
	}
	top, _, _ := strings.Cut(field, ".")
	_, isModeled := structs[schema][top]
	switch verdict {
	case "modeled":
		counted = true
		if !isModeled {
			problems = append(problems, fmt.Sprintf("%s: %s is recorded as modeled and %s.%s has no "+
				"field tagged %q", at, key, apiFieldsWireDir, schema, top))
		}
	case "out":
		if len(reason) < apiFieldsMinReason {
			problems = append(problems, fmt.Sprintf("%s: %s is out with a reason of %d characters; "+
				"say why in at least %d", at, key, len(reason), apiFieldsMinReason))
		}
		if isModeled && top == field {
			problems = append(problems, fmt.Sprintf("%s: %s is recorded as out and %s models it", at, key,
				apiFieldsWireDir))
		}
	default:
		problems = append(problems, fmt.Sprintf("%s: verdict %q is not modeled or out", at, verdict))
	}
	return counted, problems
}

// apiFieldsStructs reads every exported struct in the wire package's
// non-test files: struct name to JSON field name to where it is declared.
func apiFieldsStructs(root string) (map[string]map[string]string, int, error) {
	dir := filepath.Join(root, filepath.FromSlash(apiFieldsWireDir))
	fset := token.NewFileSet()
	files, err := parseGoDir(fset, dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", apiFieldsWireDir, err)
	}
	out := map[string]map[string]string{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			out[ts.Name.Name] = apiFieldsTags(fset, root, st)
			return false
		})
	}
	return out, len(files), nil
}

// apiFieldsTags maps a struct's JSON field names to where each is declared.
func apiFieldsTags(fset *token.FileSet, root string, st *ast.StructType) map[string]string {
	fields := map[string]string{}
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			continue
		}
		name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		pos := fset.Position(f.Pos())
		rel, _ := filepath.Rel(root, pos.Filename)
		fields[name] = fmt.Sprintf("%s:%d", filepath.ToSlash(rel), pos.Line)
	}
	return fields
}
