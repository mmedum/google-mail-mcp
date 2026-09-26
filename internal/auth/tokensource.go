package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// ErrReauthorize means the stored refresh token no longer works: it was
// revoked, it expired (seven days for a client in Testing status,
// §2.11), or Google invalidated it as the oldest of too many. Only a new
// login fixes it; nothing the server retries will.
var ErrReauthorize = errors.New("auth: the stored credentials are no longer valid; run `google-mail-mcp login`")

// TokenSource returns a caching token source backed by the refresh
// token. timeout bounds each refresh; zero means DefaultHTTPTimeout.
//
// A refusal Google will repeat forever comes back wrapping
// ErrReauthorize, so the API client can say "log in again" rather than
// "try again".
func TokenSource(ctx context.Context, cfg *oauth2.Config, refreshToken string, timeout time.Duration) oauth2.TokenSource {
	ctx = boundHTTP(ctx, timeout)
	return reauthorizing{oauth2.ReuseTokenSource(nil, cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}))}
}

type reauthorizing struct{ src oauth2.TokenSource }

func (r reauthorizing) Token() (*oauth2.Token, error) {
	tok, err := r.src.Token()
	if err == nil {
		return tok, nil
	}
	if isInvalidGrant(err) {
		return nil, ErrReauthorize
	}
	// The response body is dropped from the text: it can quote the
	// request. The status is enough to tell an outage from a refusal.
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		return nil, fmt.Errorf("auth: could not refresh the access token: the token endpoint answered %d", re.Response.StatusCode)
	}
	return nil, fmt.Errorf("auth: could not refresh the access token: %w", withoutURL(err))
}

// isInvalidGrant recognizes the refusal that only signing in again
// fixes. Google reports it as invalid_grant.
func isInvalidGrant(err error) bool {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode == "invalid_grant" {
			return true
		}
		if re.Response != nil && re.Response.StatusCode == http.StatusBadRequest {
			return strings.Contains(string(re.Body), "invalid_grant")
		}
	}
	return strings.Contains(err.Error(), "invalid_grant")
}

// NoCredentials is the token source of a server started before anyone
// logged in. The server still starts, so tools/list and the schema dump
// work, and every call answers that login is needed.
type NoCredentials struct{ Reason error }

// Token always fails, naming why there are no credentials.
func (n NoCredentials) Token() (*oauth2.Token, error) {
	if n.Reason == nil {
		return nil, ErrReauthorize
	}
	return nil, fmt.Errorf("%w: %w", ErrReauthorize, n.Reason)
}
