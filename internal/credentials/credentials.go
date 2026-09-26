// Package credentials stores and resolves the OAuth refresh token.
//
// Resolution order:
//  1. GMAIL_REFRESH_TOKEN in the environment (CI, automation).
//  2. The OS keyring — Secret Service on Linux, Keychain on macOS,
//     Credential Manager on Windows — keyed by the profile name.
//  3. A file under the profile directory, restricted to the current
//     account (0600, or an ACL on Windows), written only when the
//     keyring was unavailable at login, and warned about on every use.
//
// A missing keyring entry falls through quietly. A broken keyring (no
// session bus, no secret service) also falls through, so a headless
// machine still works, and its error is reported if nothing else is
// found.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/mmedum/google-mail-mcp/internal/fileperm"
)

// ServiceName is the keyring service identifier.
const ServiceName = "google-mail-mcp"

// EnvVar is the environment override.
const EnvVar = "GMAIL_REFRESH_TOKEN"

// Source identifies where a token came from.
type Source string

// Source values.
const (
	SourceEnv     Source = "env"
	SourceKeyring Source = "keyring"
	SourceFile    Source = "file"
)

// ErrNotFound means no token is stored anywhere.
var ErrNotFound = errors.New("credentials: no refresh token found; run `google-mail-mcp login`")

// ErrKeyringSilent is what a keyring that answers "nothing" looks like
// when the profile says a token was stored in it.
//
// The two are not the same and the advice differs. A missing token is
// fixed by logging in. A keyring that has the token and will not hand it
// over — locked, or a session bus that cannot reach the daemon — is
// fixed by unlocking it, and logging in again writes a second token
// beside the first without addressing the cause.
var ErrKeyringSilent = errors.New("credentials: the profile records a refresh token in the OS keyring and the " +
	"keyring returned nothing. It is more likely locked or unreachable than empty: unlock it and try again. " +
	"`google-mail-mcp login` writes a fresh token, which works around this rather than fixing it")

// Backend is the keyring contract. Tests substitute an in-memory one.
type Backend interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, account string) (string, error) { return keyring.Get(service, account) }
func (osKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}
func (osKeyring) Delete(service, account string) error { return keyring.Delete(service, account) }

// OSKeyring returns the production keyring backend.
func OSKeyring() Backend { return osKeyring{} }

// IsKeyringNotFound reports whether err is the keyring's "no entry".
func IsKeyringNotFound(err error) bool { return errors.Is(err, keyring.ErrNotFound) }

// Store resolves and saves the refresh token for one profile.
type Store struct {
	Profile  string
	Keyring  Backend
	FilePath string
	Env      func(string) string
	// Warn receives human-readable warnings; the plaintext fallback is
	// announced through it on every use rather than once at login.
	Warn func(string)
	// ExpectKeyring says the profile records that a token was saved to
	// the keyring. It changes only the error: with it, a keyring that
	// answers "nothing" is reported as a keyring that will not answer
	// rather than as a token that was never there.
	ExpectKeyring bool
}

type tokenFile struct {
	RefreshToken string    `json:"refresh_token"`
	SavedAt      time.Time `json:"saved_at"`
}

// FileProtection says what the file fallback's permissions actually
// achieve on this platform, so a warning never names a protection the
// platform does not provide.
//
// It used to be unconditional: every warning said "mode 0600", which was
// false on Windows, where Go's modes do not map to ACLs and the file
// landed readable by any account on the machine. The file is restricted
// by an explicit ACL there now, and this says so.
func FileProtection() string { return fileperm.Describe() }

func (s *Store) warn(msg string) {
	if s.Warn != nil {
		s.Warn(msg)
	}
}

func (s *Store) env(k string) string {
	if s.Env != nil {
		return s.Env(k)
	}
	return os.Getenv(k)
}

// Resolve returns the refresh token and where it came from.
func (s *Store) Resolve() (string, Source, error) {
	if v := s.env(EnvVar); v != "" {
		return v, SourceEnv, nil
	}
	return s.ResolveStored()
}

// ResolveStored returns the token from the keyring or the file, ignoring
// the environment override: it is the token logout can revoke and delete.
func (s *Store) ResolveStored() (string, Source, error) {
	var keyringErr error
	if s.Keyring != nil {
		tok, err := s.Keyring.Get(ServiceName, s.Profile)
		switch {
		case err == nil && tok != "":
			return tok, SourceKeyring, nil
		case err != nil && !IsKeyringNotFound(err):
			keyringErr = err
		}
	}
	if s.FilePath != "" {
		tok, err := s.readFile()
		if err == nil && tok != "" {
			s.warn(fmt.Sprintf("refresh token read from the plaintext file %s (%s); "+
				"an OS keyring would hold it better", s.FilePath, FileProtection()))
			return tok, SourceFile, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
	}
	if keyringErr != nil {
		return "", "", fmt.Errorf("%w (keyring error: %w)", ErrNotFound, keyringErr)
	}
	if s.ExpectKeyring {
		return "", "", ErrKeyringSilent
	}
	return "", "", ErrNotFound
}

// Save stores the token in the keyring, or in the file when the keyring
// fails, and reports where it landed.
func (s *Store) Save(token string) (Source, error) {
	if token == "" {
		return "", errors.New("credentials: refusing to store an empty token")
	}
	var keyringErr error
	if s.Keyring != nil {
		err := s.Keyring.Set(ServiceName, s.Profile, token)
		if err == nil {
			_ = s.removeFile() // one source of truth
			return SourceKeyring, nil
		}
		keyringErr = err
	}
	if s.FilePath == "" {
		if keyringErr != nil {
			return "", fmt.Errorf("credentials: keyring unavailable and no file fallback configured: %w", keyringErr)
		}
		return "", errors.New("credentials: no token store configured")
	}
	if err := s.writeFile(token); err != nil {
		return "", err
	}
	if keyringErr != nil {
		s.warn(fmt.Sprintf("keyring unavailable (%v); refresh token saved in plaintext at %s (%s)",
			keyringErr, s.FilePath, FileProtection()))
	}
	return SourceFile, nil
}

// Delete removes the token from every store. Missing entries are fine.
func (s *Store) Delete() error {
	var errs []error
	if s.Keyring != nil {
		if err := s.Keyring.Delete(ServiceName, s.Profile); err != nil && !IsKeyringNotFound(err) {
			errs = append(errs, fmt.Errorf("credentials: keyring delete: %w", err))
		}
	}
	if err := s.removeFile(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Store) readFile() (string, error) {
	data, err := os.ReadFile(s.FilePath)
	if err != nil {
		return "", err
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return "", fmt.Errorf("credentials: parse %s: %w", s.FilePath, err)
	}
	return tf.RefreshToken, nil
}

func (s *Store) writeFile(token string) error {
	if err := os.MkdirAll(filepath.Dir(s.FilePath), 0o700); err != nil {
		return fmt.Errorf("credentials: create %s: %w", filepath.Dir(s.FilePath), err)
	}
	// Writing the refresh token is what this function is for. It lands
	// in a 0600 file, only when the keyring was unavailable, and every
	// read of it warns (§10).
	data, err := json.Marshal(tokenFile{RefreshToken: token, SavedAt: time.Now().UTC()}) //nolint:gosec // the token file, by design
	if err != nil {
		return fmt.Errorf("credentials: encode token file: %w", err)
	}
	tmp := s.FilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("credentials: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.FilePath); err != nil {
		return fmt.Errorf("credentials: replace %s: %w", s.FilePath, err)
	}
	// After the rename, not before: on Windows the access list is a
	// property of the file at its final path, and a temporary file's
	// list does not survive being moved into place.
	return fileperm.RestrictToOwner(s.FilePath)
}

func (s *Store) removeFile() error {
	if s.FilePath == "" {
		return nil
	}
	if err := os.Remove(s.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credentials: remove %s: %w", s.FilePath, err)
	}
	return nil
}
