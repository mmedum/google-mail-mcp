package gmail

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The wire carries 64-bit ids as JSON strings; a numeric decode would
// lose precision above 2^53 and fail outright on a string.
func TestSixtyFourBitFieldsAreStrings(t *testing.T) {
	in := `{"id":"0000feed00000001","historyId":"18446744073709551615","internalDate":"1772442000000","sizeEstimate":2048,
	"payload":{"partId":"","mimeType":"text/plain","headers":[{"name":"Subject","value":"=?UTF-8?Q?x?="}],"body":{"size":3,"data":"YWJj"}}}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if h, ok := ParseHistoryID(m.HistoryID); !ok || h != 18446744073709551615 {
		t.Fatalf("historyId = %q %v", m.HistoryID, ok)
	}
	if ms, ok := m.InternalDateMillis(); !ok || ms != 1772442000000 {
		t.Fatalf("internalDate = %d %v", ms, ok)
	}
	if m.SizeEstimate != 2048 || m.Payload.Body.Data != "YWJj" || m.Payload.Headers[0].Name != "Subject" {
		t.Fatalf("decoded %+v", m)
	}
	if _, ok := (&Message{}).InternalDateMillis(); ok {
		t.Fatal("empty internalDate parsed")
	}
	if _, ok := ParseHistoryID("x"); ok {
		t.Fatal("bad history id parsed")
	}
}

// Every tag is the discovery document's name. This table is the
// document's spelling for the fields whose Go name differs in case.
func TestTagsMatchDiscovery(t *testing.T) {
	cases := []struct {
		v    any
		want []string
	}{
		{Message{}, []string{"id", "threadId", "labelIds", "snippet", "historyId", "internalDate", "sizeEstimate", "payload", "raw", "classificationLabelValues"}},
		{MessagePart{}, []string{"partId", "mimeType", "filename", "headers", "body", "parts"}},
		{MessagePartBody{}, []string{"attachmentId", "size", "data"}},
		{Label{}, []string{"id", "name", "type", "messageListVisibility", "labelListVisibility", "messagesTotal", "messagesUnread", "threadsTotal", "threadsUnread", "color"}},
		{VacationSettings{}, []string{"enableAutoReply", "responseSubject", "responseBodyPlainText", "responseBodyHtml", "restrictToContacts", "restrictToDomain", "startTime", "endTime"}},
		{BatchModifyMessagesRequest{}, []string{"ids", "addLabelIds", "removeLabelIds"}},
		{ListFiltersResponse{}, []string{"filter"}},
		{History{}, []string{"id", "messages", "messagesAdded", "messagesDeleted", "labelsAdded", "labelsRemoved"}},
	}
	for _, c := range cases {
		rt := reflect.TypeOf(c.v)
		var got []string
		for i := range rt.NumField() {
			tag := rt.Field(i).Tag.Get("json")
			for j := range len(tag) {
				if tag[j] == ',' {
					tag = tag[:j]
					break
				}
			}
			got = append(got, tag)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s tags = %v, want %v", rt.Name(), got, c.want)
		}
	}
}
