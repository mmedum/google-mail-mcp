package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// The API surface snapshot: every method and every schema field the
// Gmail discovery document publishes. `api-diff` writes it; nobody edits
// it. `api-coverage` and `api-fields` read it offline, so `check` holds
// the verdict records to it without the network.

const (
	// discoveryURL is the Gmail API's published description.
	discoveryURL = "https://gmail.googleapis.com/$discovery/rest?version=v1"
	// discoverySnapshotPath is the committed snapshot.
	discoverySnapshotPath = "testdata/api-surface.json"
	// discoveryPathPrefix is what every method's flat path starts with.
	// The client writes paths under users/me, so it compares what follows.
	discoveryPathPrefix = "gmail/v1/users/{userId}/"
)

// discoverySurface is the snapshot's shape.
type discoverySurface struct {
	Note     string            `json:"note"`
	URL      string            `json:"url"`
	Fetched  string            `json:"fetched"`
	Version  string            `json:"version"`
	Revision string            `json:"revision"`
	Methods  []discoveryMethod `json:"methods"`
	Schemas  []discoverySchema `json:"schemas"`
}

// discoveryMethod is one published method.
type discoveryMethod struct {
	ID     string   `json:"id"`
	Verb   string   `json:"verb"`
	Path   string   `json:"path"`
	Scopes []string `json:"scopes"`
}

// discoverySchema is one published schema with its fields. An inline
// object's fields are listed under a dotted path, "action.forward".
type discoverySchema struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
}

// discoveryNote is written into the snapshot so a reader knows not to
// edit it.
const discoveryNote = "Written by `go run ./scripts/gates api-diff`; nobody edits this. " +
	"Verdicts live in testdata/api-coverage.tsv (methods) and testdata/api-fields.tsv (fields)."

// discoveryLoad reads the committed snapshot.
func discoveryLoad(path string) (*discoverySurface, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a path this repository owns
	if err != nil {
		return nil, err
	}
	var s discoverySurface
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// methodByID indexes the snapshot's methods.
func (s *discoverySurface) methodByID() map[string]discoveryMethod {
	m := make(map[string]discoveryMethod, len(s.Methods))
	for _, x := range s.Methods {
		m[x.ID] = x
	}
	return m
}

// fieldsBySchema indexes the snapshot's fields.
func (s *discoverySurface) fieldsBySchema() map[string][]string {
	m := make(map[string][]string, len(s.Schemas))
	for _, x := range s.Schemas {
		m[x.Name] = x.Fields
	}
	return m
}

// discoveryResource is a node of the discovery document's resource tree.
type discoveryResource struct {
	Methods map[string]struct {
		ID         string   `json:"id"`
		HTTPMethod string   `json:"httpMethod"`
		Path       string   `json:"path"`
		FlatPath   string   `json:"flatPath"`
		Scopes     []string `json:"scopes"`
	} `json:"methods"`
	Resources map[string]discoveryResource `json:"resources"`
}

// discoveryProperty is the part of a discovery schema property that
// decides whether it is an inline object with fields of its own.
type discoveryProperty struct {
	Type                 string                        `json:"type"`
	Ref                  string                        `json:"$ref"`
	Properties           map[string]*discoveryProperty `json:"properties"`
	Items                *discoveryProperty            `json:"items"`
	AdditionalProperties *discoveryProperty            `json:"additionalProperties"`
}

// discoveryParse turns a discovery document into a snapshot. One fetch
// covers methods and fields both.
func discoveryParse(raw []byte, url string) (*discoverySurface, error) {
	var doc struct {
		Version   string                       `json:"version"`
		Revision  string                       `json:"revision"`
		Resources map[string]discoveryResource `json:"resources"`
		Methods   map[string]json.RawMessage   `json:"methods"`
		Schemas   map[string]struct {
			Properties map[string]*discoveryProperty `json:"properties"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse the discovery document: %w", err)
	}
	if len(doc.Methods) > 0 {
		return nil, fmt.Errorf("the discovery document has %d top-level methods, which this reader does not walk",
			len(doc.Methods))
	}
	s := &discoverySurface{Note: discoveryNote, URL: url, Version: doc.Version, Revision: doc.Revision}
	var walk func(map[string]discoveryResource)
	walk = func(res map[string]discoveryResource) {
		for _, node := range res {
			for _, m := range node.Methods {
				scopes := slices.Clone(m.Scopes)
				slices.Sort(scopes)
				s.Methods = append(s.Methods, discoveryMethod{
					ID: m.ID, Verb: m.HTTPMethod, Path: cmp.Or(m.FlatPath, m.Path), Scopes: scopes,
				})
			}
			walk(node.Resources)
		}
	}
	walk(doc.Resources)
	sort.Slice(s.Methods, func(i, j int) bool { return s.Methods[i].ID < s.Methods[j].ID })

	for name, schema := range doc.Schemas {
		fields := []string{}
		discoveryFields(schema.Properties, "", &fields)
		slices.Sort(fields)
		s.Schemas = append(s.Schemas, discoverySchema{Name: name, Fields: fields})
	}
	sort.Slice(s.Schemas, func(i, j int) bool { return s.Schemas[i].Name < s.Schemas[j].Name })
	return s, nil
}

// discoveryFields lists props under prefix, recursing into inline
// objects — an object property with properties of its own, directly or
// as an array's items or a map's values. A $ref is another schema,
// judged under its own name.
func discoveryFields(props map[string]*discoveryProperty, prefix string, out *[]string) {
	for name, p := range props {
		path := prefix + name
		*out = append(*out, path)
		for inner := p; inner != nil; {
			if inner.Ref == "" && len(inner.Properties) > 0 {
				discoveryFields(inner.Properties, path+".", out)
				break
			}
			switch {
			case inner.Items != nil:
				inner = inner.Items
			case inner.AdditionalProperties != nil:
				inner = inner.AdditionalProperties
			default:
				inner = nil
			}
		}
	}
}

// discoveryClientPath is a method's path as the client writes it: under
// users/me, with each {param} written as {}.
func discoveryClientPath(path string) string {
	path = strings.TrimPrefix(path, discoveryPathPrefix)
	var b strings.Builder
	for {
		open := strings.IndexByte(path, '{')
		if open < 0 {
			b.WriteString(path)
			return b.String()
		}
		end := strings.IndexByte(path[open:], '}')
		if end < 0 {
			b.WriteString(path)
			return b.String()
		}
		b.WriteString(path[:open] + "{}")
		path = path[open+end+1:]
	}
}
