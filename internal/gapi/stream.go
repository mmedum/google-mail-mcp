package gapi

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxAttachmentBytes bounds one attachment written to disk. Gmail
// accepts at most 25 MB attached to a message it delivers and 35 MB
// through the API (§2.6); twice the larger leaves room for what another
// system delivered, and stops a body that never ends.
const MaxAttachmentBytes = 64 << 20

// errNotAttachment is a response that is not the MessagePartBody
// attachments.get documents. Google does not send one, so it is not
// retried.
var errNotAttachment = errors.New("the answer is not an attachment body")

// streamData copies the decoded "data" field of a MessagePartBody from
// r to w without holding it: the JSON is read token by token up to that
// field's key, and the string itself goes through a base64url decoder
// as it arrives. Other fields are skipped.
func streamData(r io.Reader, w io.Writer) (int64, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return 0, readOrMalformed(err)
	}
	if tok != json.Delim('{') {
		return 0, notAttachment(nil)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return 0, readOrMalformed(err)
		}
		if key, _ := tok.(string); key != "data" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, readOrMalformed(err)
			}
			continue
		}
		// The decoder has read the key and nothing after it; the rest of
		// its buffer, then the body, hold ": "…"".
		rest := bufio.NewReader(io.MultiReader(dec.Buffered(), r))
		if err := expectString(rest); err != nil {
			return 0, err
		}
		dataReader := &jsonBase64{r: rest}
		sink := &writeTracker{w: w}
		limited := &io.LimitedReader{R: base64.NewDecoder(base64.RawURLEncoding, dataReader), N: MaxAttachmentBytes + 1}
		n, err := io.Copy(sink, limited)
		switch {
		case dataReader.readErr != nil:
			// The connection failed mid-body: a transport failure, retried.
			return n, dataReader.readErr
		case sink.err != nil:
			// The writer is the caller's, and so is the words for its
			// failure; an unclassified one is still final.
			var e *Error
			if errors.As(sink.err, &e) {
				return n, e
			}
			return n, Wrap(ClassUnavailable, sink.err, "the attachment could not be written")
		case err != nil:
			return n, notAttachment(err)
		case n > MaxAttachmentBytes:
			return n, Errf(ClassInvalid, "the attachment is larger than %d MB, the most this server writes", MaxAttachmentBytes>>20)
		}
		return n, nil
	}
	// More is false at the end of the body as well as at the closing
	// brace; only the brace means the object ended without data.
	if _, err := dec.Token(); err != nil {
		return 0, readOrMalformed(err)
	}
	return 0, notAttachment(errors.New("no data field"))
}

// readOrMalformed tells a body that stopped arriving from one that
// arrived and is not JSON. The first is a failed read, returned
// unclassified so the client retries it as a GET; the second is final.
func readOrMalformed(err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax) && syntax.Error() == truncatedJSON:
		// encoding/json reports a body that ended mid-value as a syntax
		// error with this fixed text.
		return io.ErrUnexpectedEOF
	case errors.As(err, &syntax), errors.As(err, &typ):
		return notAttachment(err)
	}
	return err
}

const truncatedJSON = "unexpected end of JSON input"

func notAttachment(err error) *Error {
	if err == nil {
		err = errNotAttachment
	}
	return Wrap(ClassUnavailable, fmt.Errorf("%w: %w", errNotAttachment, err),
		"Google's answer to gmail.users.messages.attachments.get was not the attachment body this server expected")
}

// expectString consumes the colon after a key and the quote that opens
// its string value.
func expectString(r *bufio.Reader) error {
	for _, want := range []byte{':', '"'} {
		for {
			b, err := r.ReadByte()
			if err != nil {
				return err
			}
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				continue
			}
			if b != want {
				return notAttachment(fmt.Errorf("found %q where %q belongs", b, want))
			}
			break
		}
	}
	return nil
}

// errBadData is a character base64url never uses inside the data string.
var errBadData = errors.New("the data field holds a character base64url does not use")

// jsonBase64 reads a JSON string's content up to its closing quote,
// dropping base64 padding. base64url needs no JSON escape, so a
// backslash is refused rather than decoded.
type jsonBase64 struct {
	r       *bufio.Reader
	closed  bool
	readErr error
}

func (j *jsonBase64) Read(p []byte) (int, error) {
	if j.closed {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		b, err := j.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			j.readErr = err
			return n, err
		}
		switch {
		case b == '"':
			j.closed = true
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		case b == '=':
			continue
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '_':
			p[n] = b
			n++
		default:
			return n, errBadData
		}
	}
	return n, nil
}

// writeTracker remembers a failed write, to tell a full disk from a
// malformed answer when the copy stops.
type writeTracker struct {
	w   io.Writer
	err error
}

func (t *writeTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}
