package gapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestStreamDataDecodesWhereverTheFieldSits(t *testing.T) {
	content := []byte("attachment bytes \x00\xff with a tail")
	padded := base64.URLEncoding.EncodeToString(content)
	raw := base64.RawURLEncoding.EncodeToString(content)
	for name, body := range map[string]string{
		"data last":           `{"size":34,"data":"` + padded + `"}`,
		"data first":          `{"data":"` + padded + `","size":34}`,
		"unpadded":            `{"size":34,"data":"` + raw + `"}`,
		"spaced":              "{ \"attachmentId\" : \"x\",\n \"data\" :\t\"" + padded + "\" }",
		"nested field before": `{"extra":{"a":[1,{"b":"}"}]},"data":"` + padded + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			// One byte at a time, so nothing depends on how the body is split.
			n, err := streamData(iotest.OneByteReader(strings.NewReader(body)), &out)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(content)) || !bytes.Equal(out.Bytes(), content) {
				t.Fatalf("wrote %d bytes %q; want %q", n, out.Bytes(), content)
			}
		})
	}
}

func TestStreamDataRefusesWhatIsNotAnAttachment(t *testing.T) {
	for name, body := range map[string]string{
		"not an object":  `["data"]`,
		"no data field":  `{"size":3}`,
		"escaped string": `{"data":"QUJD\u0044"}`,
		"standard alpha": `{"data":"ab+/"}`,
		"not a string":   `{"data":12}`,
		"bad quantum":    `{"data":"QUJDR"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := streamData(strings.NewReader(body), io.Discard)
			var e *Error
			if !errors.As(err, &e) || e.Class != ClassUnavailable || !errors.Is(err, errNotAttachment) {
				t.Fatalf("err = %v; want an unavailable not-an-attachment error", err)
			}
		})
	}
}

// A body cut inside the data string is a failed read, not a malformed
// answer: it comes back unclassified so the client retries it.
func TestStreamDataReportsACutBodyAsARead(t *testing.T) {
	_, err := streamData(strings.NewReader(`{"data":"QUJD`), io.Discard)
	if err == nil || errors.As(err, new(*Error)) {
		t.Fatalf("err = %v; want an unclassified read error", err)
	}
}

func TestStreamDataStopsAtTheCap(t *testing.T) {
	// Encode in quanta so the body is valid base64 of the whole.
	chunk := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{'a'}, 3<<10))
	body := io.MultiReader(strings.NewReader(`{"data":"`),
		&repeatReader{s: chunk, n: MaxAttachmentBytes/(3<<10) + 2}, strings.NewReader(`"}`))
	n, err := streamData(body, io.Discard)
	if c, ok := ClassOf(err); !ok || c != ClassInvalid {
		t.Fatalf("err = %v; want invalid", err)
	}
	if n != MaxAttachmentBytes+1 {
		t.Fatalf("wrote %d bytes; want the cap plus the one that proved it", n)
	}
}

type repeatReader struct {
	s   string
	n   int
	cur *strings.Reader
}

func (r *repeatReader) Read(p []byte) (int, error) {
	for {
		if r.cur != nil && r.cur.Len() > 0 {
			return r.cur.Read(p)
		}
		if r.n == 0 {
			return 0, io.EOF
		}
		r.n--
		r.cur = strings.NewReader(r.s)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestStreamDataReportsAWriteFailure(t *testing.T) {
	_, err := streamData(strings.NewReader(`{"data":"QUJD"}`), failWriter{})
	if c, ok := ClassOf(err); !ok || c != ClassUnavailable || !strings.Contains(err.Error(), "could not be written") {
		t.Fatalf("err = %v", err)
	}
}

// A body cut mid-stream is read again, and the consumer starts over: the
// second attempt's bytes are the whole file, not appended to the first.
func TestDownloadAttachmentRetriesACutBodyFromTheStart(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 2000))
	body := `{"size":20000,"data":"` + base64.URLEncoding.EncodeToString(content) + `"}`
	cut := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body[:len(body)/2])
		// Returning with the declared length unwritten closes the
		// connection mid-body.
	}
	f := newFake(t, cut, reply(http.StatusOK, body))
	c, _ := client(t, f)
	var out bytes.Buffer
	resets := 0
	n, err := c.DownloadAttachment(t.Context(), "m1", "att", func() (io.Writer, error) {
		resets++
		out.Reset()
		return &out, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resets != 2 || f.count() != 2 {
		t.Fatalf("%d resets over %d requests; want 2 and 2", resets, f.count())
	}
	if n != int64(len(content)) || !bytes.Equal(out.Bytes(), content) {
		t.Fatalf("wrote %d bytes; want the whole %d once", n, len(content))
	}
}

// An answer larger than the JSON cap still streams: the cap bounds an
// envelope held in memory, and an attachment is never held.
func TestDownloadAttachmentIsNotBoundByTheResponseCap(t *testing.T) {
	size := maxResponseBytes + 1<<20
	content := bytes.Repeat([]byte{0xab}, size)
	body := `{"data":"` + base64.RawURLEncoding.EncodeToString(content) + `"}`
	f := newFake(t, reply(http.StatusOK, body))
	c, _ := client(t, f)
	sum := &countWriter{}
	n, err := c.DownloadAttachment(t.Context(), "m1", "att", func() (io.Writer, error) { return sum, nil })
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(size) || sum.n != int64(size) {
		t.Fatalf("wrote %d (%d seen); want %d", n, sum.n, size)
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func TestAStreamFailureFromTheConsumerIsFinal(t *testing.T) {
	f := newFake(t, reply(http.StatusOK, `{"data":"QUJD"}`))
	c, _ := client(t, f)
	_, err := c.DownloadAttachment(t.Context(), "m1", "att", func() (io.Writer, error) {
		return nil, Errf(ClassUnavailable, "the file could not be reset")
	})
	if c, _ := ClassOf(err); c != ClassUnavailable || f.count() != 1 {
		t.Fatalf("err = %v after %d requests; want one unavailable", err, f.count())
	}
}

func TestOnlyAGetStreams(t *testing.T) {
	c := New(Options{})
	err := c.Stream(t.Context(), createDraft, func(io.Reader) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "only a GET streams") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnErrorAnswerIsNotStreamed(t *testing.T) {
	f := newFake(t, reply(http.StatusNotFound, googleErr(404, "notFound", "Requested entity was not found.")))
	c, _ := client(t, f)
	called := false
	_, err := c.DownloadAttachment(t.Context(), "m1", "att", func() (io.Writer, error) {
		called = true
		return io.Discard, nil
	})
	if classOf(t, err) != ClassNotFound || called {
		t.Fatalf("err = %v, consumer called %v", err, called)
	}
}

// A body that stops before the data string is a failed read too, not a
// malformed answer: the client retries it.
func TestStreamDataReportsAnEarlyCutAsARead(t *testing.T) {
	for _, body := range []string{``, `{"size":`, `{"size":12,`, `{"size":12,"da`} {
		_, err := streamData(strings.NewReader(body), io.Discard)
		if err == nil || errors.As(err, new(*Error)) {
			t.Errorf("body %q: err = %v; want an unclassified read error", body, err)
		}
	}
}

// A stream is bounded by idle time, not total time: a slow body that
// keeps arriving outlasts the timeout, and one that stops does not.
func TestAStreamOutlastsTheTimeoutWhileItProgresses(t *testing.T) {
	content := bytes.Repeat([]byte("slow"), 64)
	encoded := base64.RawURLEncoding.EncodeToString(content)
	trickle := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":"`)
		for i := 0; i < len(encoded); i += 32 {
			_, _ = io.WriteString(w, encoded[i:min(i+32, len(encoded))])
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = io.WriteString(w, `"}`)
	}
	f := newFake(t, trickle)
	c, _ := client(t, f, func(o *Options) { o.Timeout = 100 * time.Millisecond; o.MaxRetries = -1 })
	var out bytes.Buffer
	start := time.Now()
	if _, err := c.DownloadAttachment(t.Context(), "m1", "att", func() (io.Writer, error) { out.Reset(); return &out, nil }); err != nil {
		t.Fatalf("a stream that kept arriving for %v failed: %v", time.Since(start), err)
	}
	if time.Since(start) < 100*time.Millisecond || !bytes.Equal(out.Bytes(), content) {
		t.Fatalf("took %v and wrote %d bytes; want longer than the timeout and the whole body", time.Since(start), out.Len())
	}

	stall := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":"QUJD`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	f = newFake(t, stall)
	c, _ = client(t, f, func(o *Options) { o.Timeout = 100 * time.Millisecond; o.MaxRetries = -1 })
	_, err := c.DownloadAttachment(t.Context(), "m1", "att", func() (io.Writer, error) { return io.Discard, nil })
	if cl, _ := ClassOf(err); cl != ClassUnavailable {
		t.Fatalf("a stalled stream = %v; want unavailable", err)
	}
}
