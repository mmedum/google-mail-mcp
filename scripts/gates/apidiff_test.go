package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apiDiffTestDoc is a discovery document with n methods and one schema
// whose inline object must be walked.
func apiDiffTestDoc(n int, revision string) []byte {
	methods := map[string]any{}
	for i := range n {
		methods[fmt.Sprintf("m%d", i)] = map[string]any{
			"id": fmt.Sprintf("gmail.users.x.m%d", i), "httpMethod": "GET",
			"path":     fmt.Sprintf("gmail/v1/users/{userId}/x/m%d", i),
			"flatPath": fmt.Sprintf("gmail/v1/users/{userId}/x/m%d", i),
			"scopes":   []string{"b", "a"},
		}
	}
	doc := map[string]any{
		"version": "v1", "revision": revision,
		"resources": map[string]any{"users": map[string]any{"resources": map[string]any{
			"x": map[string]any{"methods": methods}}}},
		"schemas": map[string]any{"Filter": map[string]any{"properties": map[string]any{
			"id":     map[string]any{"type": "string"},
			"action": map[string]any{"type": "object", "properties": map[string]any{"forward": map[string]any{"type": "string"}}},
			"list":   map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{}}}},
			"ref":    map[string]any{"$ref": "Other"},
		}}},
	}
	b, _ := json.Marshal(doc)
	return b
}

func TestDiscoveryParseWalksInlineObjects(t *testing.T) {
	s, err := discoveryParse(apiDiffTestDoc(2, "1"), "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Methods) != 2 || s.Methods[0].ID != "gmail.users.x.m0" || strings.Join(s.Methods[0].Scopes, ",") != "a,b" {
		t.Errorf("methods = %+v", s.Methods)
	}
	got := strings.Join(s.Schemas[0].Fields, ",")
	if got != "action,action.forward,id,list,list.a,ref" {
		t.Errorf("fields = %s", got)
	}
}

// A refetch that reaches nothing fails and leaves the snapshot's bytes
// alone. The port is bound and released rather than guessed, so the
// test cannot pass because something happened to answer on it.
func TestAPIDiffClosedPortWritesNothing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + listener.Addr().String() + "/discovery"
	_ = listener.Close()

	path := filepath.Join(t.TempDir(), "api-surface.json")
	committed := []byte(`{"note":"committed"}` + "\n")
	if err := os.WriteFile(path, committed, 0o600); err != nil {
		t.Fatal(err)
	}
	var out sink
	err = apiDiffWith(&http.Client{Timeout: 5 * time.Second}, dead, path, time.Now(), &out)
	if err == nil || !strings.Contains(err.Error(), "the committed snapshot is unchanged") {
		t.Fatalf("a refetch that reached nothing returned %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, committed) {
		t.Errorf("the snapshot moved:\n%s", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("%d files left beside the snapshot, want 1", len(entries))
	}
}

func TestAPIDiffRefusesABadAnswer(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status":  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"garbage": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"short":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(apiDiffTestDoc(3, "1")) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "api-surface.json")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := apiDiffWith(srv.Client(), srv.URL, path, time.Now(), &sink{}); err == nil {
				t.Fatal("accepted")
			}
			if got, _ := os.ReadFile(path); string(got) != "old" {
				t.Errorf("the snapshot moved: %s", got)
			}
		})
	}
}

func TestAPIDiffWritesAndReports(t *testing.T) {
	doc := apiDiffTestDoc(apiDiffMinMethods, "2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(doc) }))
	defer srv.Close()

	old, err := discoveryParse(apiDiffTestDoc(apiDiffMinMethods+1, "1"), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	old.Methods[0].Verb = "POST"
	path := filepath.Join(t.TempDir(), "api-surface.json")
	b, _ := json.Marshal(old)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var out sink
	if err := apiDiffWith(srv.Client(), srv.URL, path, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), &out); err != nil {
		t.Fatal(err)
	}
	out.mustSay(t, "CHANGED gmail.users.x.m0: POST")
	out.mustSay(t, fmt.Sprintf("GONE    gmail.users.x.m%d", apiDiffMinMethods))
	out.mustSay(t, "revision 1 -> 2")
	out.mustSay(t, fmt.Sprintf("snapshot written: %d methods, 1 schemas, 6 fields, revision 2", apiDiffMinMethods))
	got, err := discoveryLoad(path)
	if err != nil || got.Fetched != "2026-09-25" || len(got.Methods) != apiDiffMinMethods || got.Note == "" {
		t.Errorf("written snapshot = %+v, %v", got, err)
	}
}
