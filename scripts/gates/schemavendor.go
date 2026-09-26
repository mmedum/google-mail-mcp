package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
)

// The published schemas this repository's documents cite, vendored so
// the gates can validate against them offline.
//
// The recorded digest proves these are the bytes somebody reviewed. It
// does not prove upstream still serves them; `schema-refetch` asks that,
// and docs/release.md runs it at release time.
//
//go:embed schemas/*.json
var vendorFS embed.FS

// vendorSchema is one vendored file: where it came from and its digest.
type vendorSchema struct {
	source string
	sha256 string
}

const (
	// vendorRegistrySchemaURL is the registry schema server.json cites.
	vendorRegistrySchemaURL = "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json"
	// vendorRegistrySchemaFile is its vendored copy.
	vendorRegistrySchemaFile = "server-2025-12-11.schema.json"
	// vendorMinProperties is the floor on property declarations a
	// vendored schema must carry, so a stub cannot pass for the real one.
	vendorMinProperties = 20
)

var vendorSchemas = map[string]vendorSchema{
	vendorRegistrySchemaFile: {
		source: vendorRegistrySchemaURL,
		sha256: "3fba09590c99f61735d234822279f4223fab9e300c0a81e81c91ab62a4114de0",
	},
	"mcpb-manifest-v0.3.schema.json": {
		source: "https://raw.githubusercontent.com/anthropics/mcpb/v2.1.2/schemas/mcpb-manifest-v0.3.schema.json",
		sha256: "3a0ac9d845711a1b9b17dfa5a52f8b60628239d6a86a9db417206a9efc78592d",
	},
}

// vendorManifestSchemaFile names the vendored schema for a manifest
// version, so the manifest is validated against the format it declares
// rather than a fixed one.
func vendorManifestSchemaFile(manifestVersion string) (string, error) {
	name := "mcpb-manifest-v" + manifestVersion + ".schema.json"
	if _, ok := vendorSchemas[name]; !ok {
		return "", fmt.Errorf("manifest_version %q has no vendored schema (%s); vendor it with its digest "+
			"before declaring that version", manifestVersion, name)
	}
	return name, nil
}

// vendorLoad returns a vendored schema, resolved, after checking its
// digest and its property floor.
func vendorLoad(name string) (*jsonschema.Resolved, error) {
	want, ok := vendorSchemas[name]
	if !ok {
		return nil, fmt.Errorf("no vendored schema named %q", name)
	}
	raw, err := vendorFS.ReadFile("schemas/" + name)
	if err != nil {
		return nil, fmt.Errorf("read vendored %s: %w", name, err)
	}
	return vendorResolve(name, want, raw)
}

// vendorResolve checks raw against its record and resolves it.
func vendorResolve(name string, want vendorSchema, raw []byte) (*jsonschema.Resolved, error) {
	if got := vendorDigest(raw); got != want.sha256 {
		return nil, fmt.Errorf("vendored %s hashes to %s and the recorded digest is %s; a refresh updates "+
			"the digest in the same commit", name, got, want.sha256)
	}
	if n, err := vendorPropertyCount(raw); err != nil {
		return nil, fmt.Errorf("vendored %s: %w", name, err)
	} else if n < vendorMinProperties {
		return nil, fmt.Errorf("vendored %s declares %d properties, below the floor of %d; a stub is not the "+
			"published schema", name, n, vendorMinProperties)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("vendored %s is not a schema: %w", name, err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve vendored %s: %w", name, err)
	}
	return resolved, nil
}

func vendorDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// vendorPropertyCount counts every key of every "properties" object in a
// schema document, at any depth.
func vendorPropertyCount(raw []byte) (int, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	var walk func(v any) int
	walk = func(v any) int {
		n := 0
		switch t := v.(type) {
		case map[string]any:
			if props, ok := t["properties"].(map[string]any); ok {
				n += len(props)
			}
			for _, k := range slices.Sorted(maps.Keys(t)) {
				n += walk(t[k])
			}
		case []any:
			for _, e := range t {
				n += walk(e)
			}
		}
		return n
	}
	return walk(doc), nil
}

// vendorValidate holds a document to a vendored schema. what names the
// document in the failure.
func vendorValidate(schemaName, what string, raw []byte) error {
	resolved, err := vendorLoad(schemaName)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", what, err)
	}
	if err := resolved.Validate(doc); err != nil {
		return fmt.Errorf("%s does not satisfy %s: %w", what, schemaName, err)
	}
	return nil
}
