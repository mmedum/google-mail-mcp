package mime

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
)

// cal wraps lines in a VCALENDAR with CRLF line ends.
func cal(lines ...string) []byte {
	return []byte(strings.Join(append(append([]string{"BEGIN:VCALENDAR", "VERSION:2.0"}, lines...), "END:VCALENDAR"), "\r\n") + "\r\n")
}

func event(lines ...string) []string {
	return append(append([]string{"BEGIN:VEVENT"}, lines...), "END:VEVENT")
}

func TestParseInvitation(t *testing.T) {
	long := strings.Repeat("é", maxICSText+10)
	for _, tc := range []struct {
		name string
		data []byte
		cs   string
		want Invitation
	}{
		{"a request in UTC",
			cal(append([]string{"METHOD:REQUEST"}, event("UID:aaaa-1@example.com", "SEQUENCE:2", "SUMMARY:Design review",
				"ORGANIZER;CN=Ada Quill:mailto:ada@example.com", "DTSTART:20260310T100000Z", "DTEND:20260310T110000Z")...)...), "",
			Invitation{Method: "REQUEST", UID: "aaaa-1@example.com", Sequence: 2, Summary: "Design review", Organizer: "ada@example.com",
				Start: "2026-03-10T10:00:00Z", End: "2026-03-10T11:00:00Z", Events: 1}},
		{"an IANA zone gives RFC 3339 with its offset",
			cal(event("UID:a", "DTSTART;TZID=Europe/Copenhagen:20260310T100000", "DTEND;TZID=Europe/Copenhagen:20260710T110000")...), "",
			Invitation{UID: "a", Start: "2026-03-10T10:00:00+01:00", End: "2026-07-10T11:00:00+02:00", TimeZone: "Europe/Copenhagen", Events: 1}},
		{"another zone name leaves the time as written",
			cal(event(`DTSTART;TZID="W. Europe Standard Time":20260310T100000`, `DTEND;TZID="W. Europe Standard Time":20260310T110000`)...), "",
			Invitation{Start: "20260310T100000", End: "20260310T110000", TimeZone: "W. Europe Standard Time", Events: 1}},
		{"Local is not a zone a sender can mean",
			cal(event("DTSTART;TZID=Local:20260310T100000")...), "",
			Invitation{Start: "20260310T100000", TimeZone: "Local", Events: 1}},
		{"a floating time is as written", cal(event("DTSTART:20260310T100000")...), "",
			Invitation{Start: "20260310T100000", Events: 1}},
		{"an all-day event is dates",
			cal(event("DTSTART;VALUE=DATE:20260310", "DTEND;VALUE=DATE:20260311")...), "",
			Invitation{Start: "2026-03-10", End: "2026-03-11", AllDay: true, Events: 1}},
		{"a date with no VALUE is a date", cal(event("DTSTART:20260310")...), "",
			Invitation{Start: "2026-03-10", AllDay: true, Events: 1}},
		{"an unreadable time is as written", cal(event("DTSTART:20261340T100000Z")...), "",
			Invitation{Start: "20261340T100000Z", Events: 1}},
		{"folded lines, with a fold inside a character and one after a tab",
			[]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:aaaa-\r\n 1@exam\n\tple.com\r\nSUMMARY:Caf\xc3\r\n \xa9 meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), "",
			Invitation{UID: "aaaa-1@example.com", Summary: "Café meeting", Events: 1}},
		{"escaped text", cal(event(`SUMMARY:Plan\, part 1\; notes\nand more \\ done \x`)...), "",
			Invitation{Summary: `Plan, part 1; notes and more \ done \x`, Events: 1}},
		{"quoted parameters hold colons, semicolons and commas",
			cal(event(`ORGANIZER;CN="Quill; Ada: lead, ops";X-A=b,"c:d":mailto:ada@example.com`,
				`DTSTART;X-B="x;y:z";TZID="Europe/Copenhagen",Other:20260310T100000`)...), "",
			Invitation{Organizer: "ada@example.com", Start: "2026-03-10T10:00:00+01:00", TimeZone: "Europe/Copenhagen", Events: 1}},
		{"an organizer that is not mailto is left out", cal(event("ORGANIZER:urn:uuid:0000")...), "", Invitation{Events: 1}},
		{"a mailto organizer is decoded and its query dropped",
			cal(event("ORGANIZER:MAILTO:ada%40example.com?subject=x")...), "", Invitation{Organizer: "ada@example.com", Events: 1}},
		{"a mailto with a name in it is left out", cal(event("ORGANIZER:mailto:Ada <ada@example.com>")...), "", Invitation{Events: 1}},
		{"properties of a time zone and an alarm are not the event's",
			cal(append([]string{"BEGIN:VTIMEZONE", "TZID:Europe/Copenhagen", "BEGIN:STANDARD", "DTSTART:19701025T030000",
				"END:STANDARD", "END:VTIMEZONE"}, event("BEGIN:VALARM", "SUMMARY:Alarm text", "UID:alarm", "END:VALARM",
				"SUMMARY:The event", "DTSTART:20260310T100000Z")...)...), "",
			Invitation{Summary: "The event", Start: "2026-03-10T10:00:00Z", Events: 1}},
		{"the first value of a property is kept", cal(event("SUMMARY:First", "SUMMARY:Second")...), "",
			Invitation{Summary: "First", Events: 1}},
		{"the series is described before an occurrence's change",
			cal(append(event("UID:s", "RECURRENCE-ID:20260317T100000Z", "SUMMARY:Moved", "SEQUENCE:4"),
				event("UID:s", "SUMMARY:Weekly", "DTSTART:20260310T100000Z", "SEQUENCE:3")...)...), "",
			Invitation{UID: "s", Sequence: 3, Summary: "Weekly", Start: "2026-03-10T10:00:00Z", Events: 2}},
		{"an occurrence alone is described with its recurrence id",
			cal(event("UID:s", "RECURRENCE-ID;TZID=Europe/Copenhagen:20260317T100000", "SUMMARY:Moved")...), "",
			Invitation{UID: "s", RecurrenceID: "2026-03-17T10:00:00+01:00", RecurrenceTimeZone: "Europe/Copenhagen", Summary: "Moved", Events: 1}},
		{"an end and a recurrence id in a zone of their own name it",
			cal(event(`DTSTART;TZID="W. Europe Standard Time":20260310T100000`, `DTEND;TZID="GTB Standard Time":20260310T120000`,
				`RECURRENCE-ID;TZID="Romance Standard Time":20260310T100000`)...), "",
			Invitation{Start: "20260310T100000", TimeZone: "W. Europe Standard Time", End: "20260310T120000",
				EndTimeZone: "GTB Standard Time", RecurrenceID: "20260310T100000", RecurrenceTimeZone: "Romance Standard Time", Events: 1}},
		{"an end in UTC after a zoned start",
			cal(event(`DTSTART;TZID="W. Europe Standard Time":20260310T100000`, "DTEND:20260310T110000Z")...), "",
			Invitation{Start: "20260310T100000", TimeZone: "W. Europe Standard Time", End: "2026-03-10T11:00:00Z", Events: 1}},
		{"a method RFC 5546 does not define is left out", cal("METHOD:X-OTHER"), "", Invitation{}},
		{"a method in any case", cal(append([]string{"METHOD:cancel"}, event("UID:a")...)...), "",
			Invitation{Method: "CANCEL", UID: "a", Events: 1}},
		{"a sequence that is not a count is 0", cal(event("SEQUENCE:-1")...), "", Invitation{Events: 1}},
		{"control and invisible characters", cal(event("SUMMARY:a\\nb\x01c\u202ed")...), "",
			Invitation{Summary: `a b c\u{202E}d`, Events: 1}},
		{"a summary is cut", cal(event("SUMMARY:" + long)...), "",
			Invitation{Summary: strings.Repeat("é", maxICSText) + "…", Events: 1}},
		{"an over-long UID is left out rather than cut", cal(event("UID:" + strings.Repeat("u", maxICSID+1))...), "",
			Invitation{Events: 1}},
		{"a charset other than UTF-8", cal(event("SUMMARY:Caf\xe9")...), "iso-8859-1", Invitation{Summary: "Café", Events: 1}},
		{"bare LF line ends", []byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:a\nEND:VEVENT\nEND:VCALENDAR\n"), "",
			Invitation{UID: "a", Events: 1}},
		{"an END closes what is left open inside it",
			cal(append(event("UID:a", "BEGIN:VALARM"), event("UID:b")...)...), "", Invitation{UID: "a", Events: 2}},
		{"components nested too deep are skipped",
			cal(event(append(append(strings.Split(strings.Repeat("BEGIN:X\n", 20), "\n")[:20], "SUMMARY:deep"),
				append(strings.Split(strings.Repeat("END:X\n", 20), "\n")[:20], "SUMMARY:shallow")...)...)...), "",
			Invitation{Summary: "shallow", Events: 1}},
		{"a calendar of no events", cal("METHOD:PUBLISH"), "", Invitation{Method: "PUBLISH"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseInvitation(tc.data, tc.cs)
			if got == nil {
				t.Fatal("no invitation")
			}
			if *got != tc.want {
				t.Errorf("got  %+v\nwant %+v", *got, tc.want)
			}
		})
	}
}

func TestParseInvitationRefuses(t *testing.T) {
	over := cal(append([]string{"METHOD:CANCEL"}, event("UID:a", "DESCRIPTION:"+strings.Repeat("x", maxICSBytes))...)...)
	for _, tc := range []struct {
		name, method string
		data         []byte
	}{
		{"not a calendar", "", []byte("hello\r\nMETHOD:REQUEST\r\n")},
		{"an event outside a calendar", "", []byte("BEGIN:VEVENT\r\nUID:a\r\nEND:VEVENT\r\n")},
		{"over the size cap, which still gives its method", "CANCEL", over},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, method := parseInvitation(tc.data, "")
			if got != nil || method != tc.method {
				t.Errorf("got %+v and method %q; want nil and %q", got, method, tc.method)
			}
		})
	}
}

// calendarPart is a calendar leaf of a payload: inline when data is set,
// else stored behind an attachment id.
func calendarPart(id, ct, data string, size int) gmail.MessagePart {
	p := gmail.MessagePart{PartID: id, MimeType: strings.SplitN(ct, ";", 2)[0], Filename: "invite.ics",
		Headers: []gmail.MessagePartHeader{{Name: "Content-Type", Value: ct}}, Body: &gmail.MessagePartBody{Size: int32(size)}}
	if data != "" {
		p.Body.Data = base64.RawURLEncoding.EncodeToString([]byte(data))
	} else {
		p.Body.AttachmentID = "ATT" + id
	}
	return p
}

// One calendar part Gmail stored apart is fetched per message: the
// first whose Content-Type does not state the method, else the first.
// Once it is read, nothing more is asked for.
func TestInvitationFetch(t *testing.T) {
	ics := string(cal(append([]string{"METHOD:REQUEST"}, event("UID:fetched@example.com")...)...))
	payload := func(parts ...gmail.MessagePart) *gmail.MessagePart {
		return &gmail.MessagePart{MimeType: "multipart/mixed",
			Headers: []gmail.MessagePartHeader{{Name: "Content-Type", Value: "multipart/mixed; boundary=b"}},
			Parts:   append([]gmail.MessagePart{{PartID: "0", MimeType: "text/plain", Body: &gmail.MessagePartBody{Size: 2, Data: "aGk"}}}, parts...)}
	}
	for _, tc := range []struct {
		name  string
		parts []gmail.MessagePart
		want  string // the part id fetched, or ""
	}{
		{"the one without a method",
			[]gmail.MessagePart{calendarPart("1", "text/calendar; method=REQUEST", "", 300), calendarPart("2", "application/ics", "", 300),
				calendarPart("3", "application/ics", "", 300)}, "2"},
		{"else the first", []gmail.MessagePart{calendarPart("1", "text/calendar; method=REQUEST", "", 300),
			calendarPart("2", "text/calendar; method=REQUEST", "", 300)}, "1"},
		{"an inline part does not stop a fetch", []gmail.MessagePart{calendarPart("1", "text/calendar; method=REQUEST", ics, len(ics)),
			calendarPart("2", "application/ics", "", 300)}, "2"},
		{"nothing over the size cap", []gmail.MessagePart{calendarPart("1", "application/ics", "", maxICSBytes+1)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := ParsePayload(payload(tc.parts...), nil)
			var got []string
			for _, r := range m.NeedsFetch {
				got = append(got, r.PartID)
			}
			if (tc.want == "" && len(got) != 0) || (tc.want != "" && (len(got) != 1 || got[0] != tc.want)) {
				t.Fatalf("fetches %v; want %q", got, tc.want)
			}
			if tc.want == "" {
				return
			}
			again, _ := ParsePayload(payload(tc.parts...), map[string][]byte{tc.want: []byte(ics)})
			if len(again.NeedsFetch) != 0 {
				t.Errorf("after the fetch it asks for %+v", again.NeedsFetch)
			}
			for _, a := range again.Attachments {
				if a.PartID == tc.want && (a.Invitation == nil || a.Invitation.UID != "fetched@example.com" || a.CalendarMethod == "UNKNOWN") {
					t.Errorf("the fetched part reads %+v with method %q", a.Invitation, a.CalendarMethod)
				}
			}
		})
	}
}

// The method comes from the Content-Type, else from the content's
// METHOD, which may carry parameters.
func TestCalendarMethod(t *testing.T) {
	for _, tc := range []struct{ ct, data, want string }{
		{"text/calendar; method=cancel", "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n", "CANCEL"},
		{"text/calendar", "BEGIN:VCALENDAR\r\nMETHOD;X-A=1:reply\r\nEND:VCALENDAR\r\n", "REPLY"},
		{"text/calendar", "BEGIN:VCALENDAR\r\nMETHOD:not a token\r\nEND:VCALENDAR\r\n", "UNKNOWN"},
		{"text/calendar", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nMETHOD:REQUEST\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", "UNKNOWN"},
	} {
		m := ParseRaw([]byte("Content-Type: " + tc.ct + "\r\nContent-Disposition: attachment\r\n\r\n" + tc.data))
		if len(m.Attachments) != 1 || m.Attachments[0].CalendarMethod != tc.want {
			t.Errorf("%q: %+v; want method %s", tc.data, m.Attachments, tc.want)
		}
	}
}

// FuzzParseInvitation holds what any part yields: no panic, every
// string one line of valid UTF-8 within its cap, a method from the
// fixed set, and times within their cap.
func FuzzParseInvitation(f *testing.F) {
	for _, s := range [][]byte{
		cal(append([]string{"METHOD:REQUEST"}, event("UID:a", "SUMMARY:x\\, y", "DTSTART;TZID=Europe/Copenhagen:20260310T100000")...)...),
		cal(event(`ORGANIZER;CN="a:b";X=1,2:mailto:a@example.com`, "DTSTART;VALUE=DATE:20260310", "RECURRENCE-ID:20260310T100000Z")...),
		[]byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nSUMMARY:a\n b\nEND:VALARM\nEND:VCALENDAR"),
		[]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART;TZID=\"\r\n x\":1\r\n"),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		inv, _ := parseInvitation(data, "")
		if inv == nil {
			return
		}
		for _, s := range []string{inv.UID, inv.Summary, inv.Organizer, inv.Start, inv.End, inv.RecurrenceID, inv.TimeZone} {
			if !utf8.ValidString(s) || strings.ContainsAny(s, "\r\n\t\x00") || hasInvisible(s) {
				t.Fatalf("not one clean line: %q", s)
			}
		}
		if len(inv.UID) > maxICSID || utf8.RuneCountInString(inv.Summary) > maxICSText+1 || inv.Sequence < 0 || inv.Events < 0 {
			t.Fatalf("over a cap: %+v", inv)
		}
		if inv.Method != "" && !slices.Contains([]string{"PUBLISH", "REQUEST", "REPLY", "ADD", "CANCEL", "REFRESH", "COUNTER", "DECLINECOUNTER"}, inv.Method) {
			t.Fatalf("method %q", inv.Method)
		}
		for _, s := range []string{inv.Start, inv.End, inv.RecurrenceID} {
			if len(s) > maxICSTime {
				t.Fatalf("time %q over the cap", s)
			}
		}
	})
}

// FuzzInvitationRoundTrip writes a summary and UID of visible characters
// as a sender would, escaped and folded every 75 octets even inside a
// character, and requires them back exactly.
func FuzzInvitationRoundTrip(f *testing.F) {
	f.Add("Design review, part 1; notes \\ done", "aaaa-1@example.com", 75)
	f.Add("Café ☕ 会议 مراجعة", "x", 1)
	f.Fuzz(func(t *testing.T, summary, uid string, width int) {
		if width < 1 || width > 200 || summary == "" || uid == "" {
			return
		}
		for _, s := range []string{summary, uid} {
			if !utf8.ValidString(s) || strings.TrimSpace(s) != s || hasInvisible(s) ||
				strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }) {
				return
			}
		}
		if utf8.RuneCountInString(summary) > maxICSText || len(uid) > maxICSID {
			return
		}
		esc := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`)
		written := string(cal(event("UID:"+esc.Replace(uid), "SUMMARY:"+esc.Replace(summary))...))
		var folded strings.Builder
		for line := range strings.SplitSeq(strings.TrimSuffix(written, "\r\n"), "\r\n") {
			for len(line) > width {
				folded.WriteString(line[:width] + "\r\n ")
				line = line[width:]
			}
			folded.WriteString(line + "\r\n")
		}
		inv, _ := parseInvitation([]byte(folded.String()), "utf-8")
		if inv == nil || inv.Summary != summary || inv.UID != uid {
			t.Fatalf("read back %+v from summary %q and uid %q", inv, summary, uid)
		}
	})
}
