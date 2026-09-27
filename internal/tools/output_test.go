package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/internal/model"
)

// outputTypes are the tools' output types by name. The list of names is
// read from the register calls in the source, so a new tool whose output
// is missing here fails the test rather than going unchecked.
var outputTypes = map[string]reflect.Type{
	"ProfileOut":     reflect.TypeFor[ProfileOut](),
	"SignatureOut":   reflect.TypeFor[SignatureOut](),
	"FilterWriteOut": reflect.TypeFor[FilterWriteOut](),
	"VacationOut":    reflect.TypeFor[VacationOut](),
	"ThreadsOut":     reflect.TypeFor[ThreadsOut](),
	"MessagesOut":    reflect.TypeFor[MessagesOut](),
	"ThreadOut":      reflect.TypeFor[ThreadOut](),
	"MessageOut":     reflect.TypeFor[MessageOut](),
	"LabelsOut":      reflect.TypeFor[LabelsOut](),
	"DraftsOut":      reflect.TypeFor[DraftsOut](),
	"DraftOut":       reflect.TypeFor[DraftOut](),
	"ChangesOut":     reflect.TypeFor[ChangesOut](),
	"SettingsOut":    reflect.TypeFor[SettingsOut](),
	"FiltersOut":     reflect.TypeFor[FiltersOut](),
	"DownloadOut":    reflect.TypeFor[DownloadOut](),

	"DraftWriteOut": reflect.TypeFor[DraftWriteOut](),
	"ItemsOut":      reflect.TypeFor[ItemsOut](),
	"LabelWriteOut": reflect.TypeFor[LabelWriteOut](),

	"SendDraftOut":   reflect.TypeFor[SendDraftOut](),
	"LabelDeleteOut": reflect.TypeFor[LabelDeleteOut](),
}

// registeredOutputs names the output type of every handler passed to
// register in the package's own source.
func registeredOutputs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "register" || len(call.Args) != 4 {
				return true
			}
			fn, ok := call.Args[3].(*ast.FuncLit)
			if !ok || fn.Type.Results == nil {
				t.Errorf("%s: register's handler is not a function literal", fset.Position(call.Pos()))
				return true
			}
			id, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
			if !ok {
				t.Errorf("%s: register's handler returns an unnamed type", fset.Position(call.Pos()))
				return true
			}
			names = append(names, id.Name)
			return true
		})
	}
	return names
}

var untrustedType = reflect.TypeFor[model.Untrusted]()

// TestUntrustedFieldsAreNamed holds §4.1's naming both ways over every
// tool's output: a field of type model.Untrusted, or a slice of them, is
// named untrusted_*, and a field named untrusted_* is of that type.
func TestUntrustedFieldsAreNamed(t *testing.T) {
	names := registeredOutputs(t)
	if len(names) < 8 {
		t.Fatalf("found %d register calls; is the test reading the package?", len(names))
	}
	untrusted := 0
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for f := range typ.Fields() {
			if !f.IsExported() {
				continue
			}
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Anonymous && tag == "" {
				walk(f.Type, path)
				continue
			}
			elem := f.Type
			for elem.Kind() == reflect.Slice || elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			isUntrusted := elem == untrustedType
			named := strings.HasPrefix(tag, "untrusted_")
			switch {
			case isUntrusted && !named:
				t.Errorf("%s.%s holds mail but is named %q, not untrusted_*", path, f.Name, tag)
			case named && !isUntrusted:
				t.Errorf("%s.%s is named %q but is a %s, not model.Untrusted", path, f.Name, tag, f.Type)
			case isUntrusted:
				untrusted++
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	for _, name := range names {
		typ, ok := outputTypes[name]
		if !ok {
			t.Errorf("register's output %s is not in outputTypes", name)
			continue
		}
		walk(typ, name)
	}
	if untrusted < 10 {
		t.Fatalf("checked only %d untrusted fields", untrusted)
	}
}
