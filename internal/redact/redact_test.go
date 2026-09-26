package redact

import (
	"strings"
	"testing"
)

// fakeClientID is shaped like an OAuth client id so the masking bites.
// It is built by concatenation because the leak gate rightly flags the
// shape wherever it appears whole, and no allow-list entry should exist
// for it.
const fakeClientID = "123456789012-" + "abcdefghijklmnopqrstuvwxyz012345" + ".apps.googleusercontent.com"

func TestEmail(t *testing.T) {
	tests := map[string]string{
		"someone@example.com":    "s…@e….com",
		"a@b.co.test":            "a…@b….test",
		"not-an-address":         "n…",
		"":                       "",
		"@example.com":           "@…",
		"ünïcode@exämple.test":   "ü…@e….test",
		"someone@localhostnodot": "s…@l…",
	}
	for in, want := range tests {
		if got := Email(in); got != want {
			t.Errorf("Email(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAccount(t *testing.T) {
	if got := Account("someone@example.com"); got != "…@example.com" {
		t.Errorf("Account = %q", got)
	}
	if got := Account("nobody"); got != "n…" {
		t.Errorf("Account(not an address) = %q", got)
	}
}

func TestAccountsMasksEveryAddress(t *testing.T) {
	in := "The caller does not have permission: someone@example.com and other.person@example.org"
	got := Accounts(in)
	if strings.Contains(got, "someone") || strings.Contains(got, "other.person") {
		t.Errorf("Accounts left a local part: %q", got)
	}
	if !strings.Contains(got, "…@example.com") {
		t.Errorf("Accounts dropped the domain: %q", got)
	}
}

func TestPathMasksTheClientID(t *testing.T) {
	tests := []string{
		"/home/u/.config/google-mail-mcp/client_secret_" + fakeClientID + ".json",
		"/home/u/Downloads/client_secret_" + fakeClientID + " (1).json",
		"/home/u/Downloads/client_secret_123456789012-abcdefghijklmnop.json",
	}
	for _, p := range tests {
		got := Path(p)
		if strings.Contains(got, "123456789012") {
			t.Errorf("Path(%q) = %q still carries the id", p, got)
		}
		if !strings.HasPrefix(got, "/home/u/") {
			t.Errorf("Path(%q) = %q lost the directory", p, got)
		}
	}
	if got := Path("/home/u/client_secret.json"); got != "/home/u/client_secret.json" {
		t.Errorf("Path changed a path with no id: %q", got)
	}
}

func TestClientIDInFreeText(t *testing.T) {
	got := ClientID("client " + fakeClientID + " refused")
	if got != "client <client-id> refused" {
		t.Errorf("ClientID = %q", got)
	}
}

func TestLine(t *testing.T) {
	in := "message 00000000b2c3d4e5 in draft r-1234567890123456789 from someone@example.com token 1//0abcdefghijklmnop"
	got := Line(in)
	for _, leak := range []string{"00000000b2c3d4e5", "1234567890123456789", "someone", "abcdefghijklmnop"} {
		if strings.Contains(got, leak) {
			t.Errorf("Line left %q: %q", leak, got)
		}
	}
	// A commit hash is 40 hex digits, not 16, and is kept.
	sha := "0123456789abcdef0123456789abcdef01234567"
	if got := Line("commit " + sha); !strings.Contains(got, sha) {
		t.Errorf("Line masked a commit hash: %q", got)
	}
}

func TestText(t *testing.T) {
	in := "auth: read client secret /x/client_secret_123456789012-abcdefghijk.json for someone@example.com"
	got := Text(in)
	if strings.Contains(got, "123456789012") || strings.Contains(got, "someone") {
		t.Errorf("Text = %q", got)
	}
}

func TestID(t *testing.T) {
	tests := map[string]string{
		"18c2f0a1b2c3d4e5": "18c2f0…",
		"abcdef":           "[id]",
		"":                 "[id]",
	}
	for in, want := range tests {
		if got := ID(in); got != want {
			t.Errorf("ID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientIDLeavesAPseudoVersionAlone(t *testing.T) {
	const v = "v0.0.0-20260925101500-3f2a9c1b7d4e+dirty"
	if got := ClientID(v); got != v {
		t.Errorf("ClientID(%q) = %q; a pseudo-version is not a client id", v, got)
	}
}

func TestAddressesMasksTheDomainToo(t *testing.T) {
	in := "Delegation denied for someone@example.com: owner other.person@thirdparty.test refused"
	got := Addresses(in)
	for _, gone := range []string{"someone", "other.person", "example", "thirdparty"} {
		if strings.Contains(got, gone) {
			t.Errorf("Addresses left %q: %q", gone, got)
		}
	}
	if want := "Delegation denied for s…@e….com: owner o…@t….test refused"; got != want {
		t.Errorf("Addresses = %q, want %q", got, want)
	}
}
