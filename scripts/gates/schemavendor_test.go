package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVendorSchemasLoadAndCarryTheirFloor(t *testing.T) {
	if len(vendorSchemas) < 2 {
		t.Fatalf("%d vendored schemas, want the manifest's and the registry's", len(vendorSchemas))
	}
	for name := range vendorSchemas {
		if _, err := vendorLoad(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		raw, _ := vendorFS.ReadFile("schemas/" + name)
		if n, _ := vendorPropertyCount(raw); n < vendorMinProperties {
			t.Errorf("%s declares %d properties", name, n)
		}
	}
}

func TestVendorResolveRefusesAWrongDigestAndAStub(t *testing.T) {
	stub := []byte(`{"type":"object","properties":{"a":{},"b":{}}}`)
	_, err := vendorResolve("stub", vendorSchema{sha256: strings.Repeat("0", 64)}, stub)
	if err == nil || !strings.Contains(err.Error(), "recorded digest") {
		t.Errorf("a wrong digest: %v", err)
	}
	_, err = vendorResolve("stub", vendorSchema{sha256: vendorDigest(stub)}, stub)
	if err == nil || !strings.Contains(err.Error(), "below the floor of 20") {
		t.Errorf("a two-property stub: %v", err)
	}
}

func TestVendorManifestSchemaChosenByVersion(t *testing.T) {
	if name, err := vendorManifestSchemaFile("0.3"); err != nil || name != "mcpb-manifest-v0.3.schema.json" {
		t.Errorf("0.3 → %q, %v", name, err)
	}
	if _, err := vendorManifestSchemaFile("0.4"); err == nil {
		t.Error("0.4 has no vendored schema and was accepted")
	}
}

func TestSchemaRefetch(t *testing.T) {
	name := "mcpb-manifest-v0.3.schema.json"
	good, _ := vendorFS.ReadFile("schemas/" + name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			_, _ = w.Write(good)
		case "/changed":
			_, _ = w.Write(append(good, ' '))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var out sink
	if err := schemaRefetchWith(&out, srv.Client(), map[string]string{name: srv.URL + "/same"}); err != nil {
		t.Errorf("unchanged upstream failed: %v", err)
	}
	out.mustSay(t, "1 vendored schema(s) match")
	if err := schemaRefetchWith(&out, srv.Client(), map[string]string{name: srv.URL + "/changed"}); err == nil ||
		!strings.Contains(err.Error(), "changed upstream") {
		t.Errorf("changed upstream: %v", err)
	}
	if err := schemaRefetchWith(&out, srv.Client(), map[string]string{name: srv.URL + "/gone"}); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404: %v", err)
	}

	// A closed port fails loudly, and nothing is written: the copies are
	// embedded, so there is nothing this could write to.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String() + "/x"
	_ = l.Close()
	if err := schemaRefetchWith(&out, &http.Client{Timeout: schemaRefetchTimeout}, map[string]string{name: closed}); err == nil {
		t.Error("a closed port passed")
	}
	if schemaRefetchTimeout <= 0 {
		t.Error("schema-refetch has no timeout")
	}
}
