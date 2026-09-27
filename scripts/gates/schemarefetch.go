package main

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// schemaRefetchTimeout bounds each fetch, so a slow CDN is a failure
// rather than a hang.
const schemaRefetchTimeout = 30 * time.Second

// schemaRefetch fetches each vendored schema's source and reports whether
// upstream still serves the reviewed bytes. It writes nothing: a refresh
// is a decision somebody makes after reading what changed.
func schemaRefetch(out io.Writer, _ []string) error {
	sources := map[string]string{}
	for name, v := range vendorSchemas {
		sources[name] = v.source
	}
	return schemaRefetchWith(out, &http.Client{Timeout: schemaRefetchTimeout}, sources)
}

// schemaRefetchWith is the fetch against any client and source list, so
// a test can point it at a local server or a closed port.
func schemaRefetchWith(out io.Writer, client *http.Client, sources map[string]string) error {
	var drifted []string
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		want, ok := vendorSchemas[name]
		if !ok {
			return fmt.Errorf("%s is not a vendored schema", name)
		}
		resp, err := client.Get(sources[name])
		if err != nil {
			return fmt.Errorf("fetch %s: %w", name, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("fetch %s: HTTP %d", name, resp.StatusCode)
		}
		if got := vendorDigest(body); got != want.sha256 {
			drifted = append(drifted, fmt.Sprintf("%s: vendored %s, upstream now %s", name, want.sha256[:12], got[:12]))
			continue
		}
		_, _ = fmt.Fprintf(out, "schema-refetch: %s unchanged upstream\n", name)
	}
	if len(drifted) > 0 {
		return fmt.Errorf("changed upstream; read the change, refresh the file and its digest in one commit:\n  %s",
			strings.Join(drifted, "\n  "))
	}
	_, _ = fmt.Fprintf(out, "schema-refetch: %d vendored schema(s) match what their sources serve\n", len(sources))
	return nil
}
