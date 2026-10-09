package mime

import (
	"slices"
	"strings"
	"testing"
)

func TestParseListUnsubscribe(t *testing.T) {
	long := "https://a.example/" + strings.Repeat("x", maxUnsubURI)
	for _, tc := range []struct {
		name         string
		v            string
		urls, mailto []string
	}{
		{"RFC 2369's web and mail example",
			"<http://www.host.example/list.cgi?cmd=unsub&lst=list>,\r\n    <mailto:list-request@host.example?subject=unsubscribe>",
			[]string{"http://www.host.example/list.cgi?cmd=unsub&lst=list"}, []string{"mailto:list-request@host.example?subject=unsubscribe"}},
		{"a comment before the first URI",
			"(Use this command to get off the list) <mailto:list-manager@host.example?body=unsubscribe%20list>",
			nil, []string{"mailto:list-manager@host.example?body=unsubscribe%20list"}},
		{"the header's order is kept",
			"<https://b.example/u>, <mailto:a@example.com>, <HTTP://a.example/u>, <mailto:b@example.com>",
			[]string{"https://b.example/u", "HTTP://a.example/u"}, []string{"mailto:a@example.com", "mailto:b@example.com"}},
		{"whitespace inside the brackets is removed",
			"< https://a.example/ un\tsub >", []string{"https://a.example/unsub"}, nil},
		{"a field not starting with a bracket is ignored", "mailto:a@example.com", nil, nil},
		{"text after a URI ends the reading", "<https://a.example/u> please, <mailto:a@example.com>",
			[]string{"https://a.example/u"}, nil},
		{"a separator other than a comma ends the reading", "<https://a.example/u>; <https://b.example/u>",
			[]string{"https://a.example/u"}, nil},
		{"an item without brackets ends the reading", "<https://a.example/u>, mailto:a@example.com, <https://b.example/u>",
			[]string{"https://a.example/u"}, nil},
		{"an empty item ends the reading", "<https://a.example/u>,, <https://b.example/u>", []string{"https://a.example/u"}, nil},
		{"comments between items, nested and with a quoted parenthesis",
			"<mailto:a@example.com> (x (y) \\) z) , (w) <https://b.example/u>",
			[]string{"https://b.example/u"}, []string{"mailto:a@example.com"}},
		{"other schemes are dropped and reading goes on",
			"<ftp://a.example/u>, <javascript:alert(1)>, <tel:123>, <https://b.example/u>", []string{"https://b.example/u"}, nil},
		{"a web address with no host is dropped", "<https:///u>, <http:u>", nil, nil},
		{"a mailto with no address is dropped", "<mailto:>", nil, nil},
		{"a URI with an invisible or non-ASCII character is dropped",
			"<https://a.example/\u202eu>, <https://b\u00e9.example/u>, <https://c.example/u>", []string{"https://c.example/u"}, nil},
		{"an unclosed bracket yields nothing", "<https://a.example/u", nil, nil},
		{"an over-long URI is dropped", "<" + long + ">, <https://b.example/u>", []string{"https://b.example/u"}, nil},
		{"each list keeps its first four",
			"<https://1.example>, <https://2.example>, <https://3.example>, <https://4.example>, <https://5.example>",
			[]string{"https://1.example", "https://2.example", "https://3.example", "https://4.example"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := parseListUnsubscribe(tc.v)
			if !slices.Equal(u.URLs, tc.urls) || !slices.Equal(u.Mailto, tc.mailto) || u.OneClick {
				t.Errorf("got urls %q mailto %q one-click %v; want %q and %q", u.URLs, u.Mailto, u.OneClick, tc.urls, tc.mailto)
			}
		})
	}
}

func TestOneClickUnsubscribe(t *testing.T) {
	const web = "<mailto:a@example.com>, <https://a.example/u>"
	for _, tc := range []struct {
		name    string
		headers []Header
		want    bool
	}{
		{"https and the one value", []Header{{"List-Unsubscribe", web}, {"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"}}, true},
		{"surrounding whitespace is not part of the value",
			[]Header{{"list-unsubscribe", web}, {"LIST-UNSUBSCRIBE-POST", " List-Unsubscribe=One-Click "}}, true},
		{"no Post header", []Header{{"List-Unsubscribe", web}}, false},
		{"http is not https", []Header{{"List-Unsubscribe", "<http://a.example/u>"}, {"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"}}, false},
		{"mail only", []Header{{"List-Unsubscribe", "<mailto:a@example.com>"}, {"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"}}, false},
		{"another case", []Header{{"List-Unsubscribe", web}, {"List-Unsubscribe-Post", "list-unsubscribe=one-click"}}, false},
		{"more than the one pair", []Header{{"List-Unsubscribe", web}, {"List-Unsubscribe-Post", "List-Unsubscribe=One-Click&x=1"}}, false},
		{"two Post headers", []Header{{"List-Unsubscribe", web}, {"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"},
			{"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"}}, false},
		{"two List-Unsubscribe headers", []Header{{"List-Unsubscribe", web}, {"List-Unsubscribe", "<https://b.example/u>"},
			{"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUnsubscribe(tc.headers).OneClick; got != tc.want {
				t.Errorf("one-click = %v; want %v", got, tc.want)
			}
		})
	}
}

// A message read either way carries the parsed header.
func TestMessageCarriesUnsubscribe(t *testing.T) {
	m := ParseRaw([]byte("From: a@example.com\r\nList-Unsubscribe: <https://a.example/u>,\r\n <mailto:a@example.com>\r\n" +
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nbody\r\n"))
	want := Unsubscribe{URLs: []string{"https://a.example/u"}, Mailto: []string{"mailto:a@example.com"}, OneClick: true}
	if !slices.Equal(m.Unsubscribe.URLs, want.URLs) || !slices.Equal(m.Unsubscribe.Mailto, want.Mailto) || !m.Unsubscribe.OneClick {
		t.Errorf("unsubscribe = %+v; want %+v", m.Unsubscribe, want)
	}
}

// FuzzListUnsubscribe holds what any header yields: at most four of
// each, every one a visible-ASCII http, https or mailto URI found in
// the header once its whitespace is removed, and read back the same
// when written out again.
func FuzzListUnsubscribe(f *testing.F) {
	for _, s := range []string{
		"<mailto:a@example.com>, <https://a.example/u>",
		"(c (n) \\)) <https://a.example/ u>,<mailto:x>",
		"<https://a.example/u> x, <mailto:a@example.com>",
		"<<https://a.example>>, <", "(", "<mailto:%>", "<HTTPS://A.EXAMPLE/?a=<b>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		u := parseListUnsubscribe(v)
		if len(u.URLs) > maxUnsubURIs || len(u.Mailto) > maxUnsubURIs || u.OneClick {
			t.Fatalf("over the caps or one-click from the header alone: %+v", u)
		}
		squeezed := strings.Join(strings.Fields(v), "")
		for _, uri := range slices.Concat(u.URLs, u.Mailto) {
			if uri == "" || len(uri) > maxUnsubURI || !printableASCII(uri) || !strings.Contains(squeezed, uri) {
				t.Fatalf("kept %q, which is empty, too long, not visible ASCII or not in the header", uri)
			}
		}
		for _, uri := range u.URLs {
			if l := strings.ToLower(uri); !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
				t.Fatalf("%q kept as a web address", uri)
			}
		}
		for _, uri := range u.Mailto {
			if !strings.HasPrefix(strings.ToLower(uri), "mailto:") {
				t.Fatalf("%q kept as a mail address", uri)
			}
		}
		if len(u.URLs) > 0 {
			again := parseListUnsubscribe("<" + strings.Join(u.URLs, ">, <") + ">")
			if !slices.Equal(again.URLs, u.URLs) {
				t.Fatalf("written out and read again: %q, was %q", again.URLs, u.URLs)
			}
		}
	})
}
