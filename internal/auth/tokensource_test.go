package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func tokenServer(t *testing.T, status int, body string) *oauth2.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
}

func TestTokenSourceRefreshes(t *testing.T) {
	cfg := tokenServer(t, http.StatusOK, `{"access_token":"at","token_type":"Bearer","expires_in":3600}`)
	tok, err := TokenSource(context.Background(), cfg, "rt", time.Second).Token()
	if err != nil || tok.AccessToken != "at" {
		t.Fatalf("Token = %v, %v", tok, err)
	}
}

func TestARevokedRefreshTokenAsksForANewLogin(t *testing.T) {
	cfg := tokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	_, err := TokenSource(context.Background(), cfg, "rt", time.Second).Token()
	if !errors.Is(err, ErrReauthorize) {
		t.Fatalf("err = %v, want ErrReauthorize", err)
	}
}

func TestATransientFailureIsNotAReauthorize(t *testing.T) {
	cfg := tokenServer(t, http.StatusServiceUnavailable, `quoted request body`)
	_, err := TokenSource(context.Background(), cfg, "rt", time.Second).Token()
	if err == nil || errors.Is(err, ErrReauthorize) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "quoted request body") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	// A transport failure keeps no URL.
	cfg = &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://127.0.0.1:1/token?secret=x"}}
	_, err = TokenSource(context.Background(), cfg, "rt", time.Second).Token()
	if err == nil || strings.Contains(err.Error(), "secret=x") {
		t.Fatalf("transport err = %v", err)
	}
}

func TestIsInvalidGrant(t *testing.T) {
	if !isInvalidGrant(errors.New("oauth2: invalid_grant")) {
		t.Error("text form missed")
	}
	re := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, Body: []byte(`{"error":"invalid_grant"}`)}
	if !isInvalidGrant(re) {
		t.Error("body form missed")
	}
	if isInvalidGrant(&oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, Body: []byte(`{}`)}) {
		t.Error("a plain 400 read as invalid_grant")
	}
}

func TestNoCredentials(t *testing.T) {
	if _, err := (NoCredentials{}).Token(); !errors.Is(err, ErrReauthorize) {
		t.Errorf("err = %v", err)
	}
	cause := errors.New("no refresh token found")
	_, err := NoCredentials{Reason: cause}.Token()
	if !errors.Is(err, ErrReauthorize) || !errors.Is(err, cause) {
		t.Errorf("err = %v", err)
	}
}

func TestWithoutURLFailsClosed(t *testing.T) {
	if got := withoutURL(errors.New("Get \"https://x?access_token=y\"")); strings.Contains(got.Error(), "access_token") {
		t.Errorf("withoutURL = %v", got)
	}
}
