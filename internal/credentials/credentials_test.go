package credentials_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/mmedum/google-mail-mcp/v2/internal/credentials"
)

// fakeKeyring is an in-memory Backend. failGet/failSet make it behave
// like a locked or unreachable one.
type fakeKeyring struct {
	items   map[string]string
	failGet error
	failSet error
}

func newFake() *fakeKeyring { return &fakeKeyring{items: map[string]string{}} }

func (f *fakeKeyring) Get(service, account string) (string, error) {
	if f.failGet != nil {
		return "", f.failGet
	}
	v, ok := f.items[service+"/"+account]
	if !ok {
		return "", errNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(service, account, secret string) error {
	if f.failSet != nil {
		return f.failSet
	}
	f.items[service+"/"+account] = secret
	return nil
}

func (f *fakeKeyring) Delete(service, account string) error {
	delete(f.items, service+"/"+account)
	return nil
}

// errNotFound is the keyring's own "no entry", not a lookalike.
//
// It has to be the real sentinel: the store tells a missing entry apart
// from a keyring that will not answer by matching this exact error, and
// a plain errors.New here would be classed as a transport failure and
// take the wrong branch. Substituting a stand-in made this file assert
// the opposite of what it meant on the first run.
var errNotFound = keyring.ErrNotFound

func store(t *testing.T, kr credentials.Backend, expectKeyring bool) (*credentials.Store, *[]string) {
	t.Helper()
	var warnings []string
	s := &credentials.Store{
		Profile:       "default",
		Keyring:       kr,
		FilePath:      filepath.Join(t.TempDir(), "token.json"),
		Env:           func(string) string { return "" },
		Warn:          func(m string) { warnings = append(warnings, m) },
		ExpectKeyring: expectKeyring,
	}
	return s, &warnings
}

func TestSaveAndResolveFromKeyring(t *testing.T) {
	kr := newFake()
	s, _ := store(t, kr, false)
	src, err := s.Save("refresh-abc")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if src != credentials.SourceKeyring {
		t.Fatalf("Save reported %q, want keyring", src)
	}
	tok, src, err := s.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "refresh-abc" || src != credentials.SourceKeyring {
		t.Fatalf("Resolve = %q from %q", tok, src)
	}
}

func TestEnvironmentWins(t *testing.T) {
	kr := newFake()
	s, _ := store(t, kr, false)
	if _, err := s.Save("from-keyring"); err != nil {
		t.Fatal(err)
	}
	s.Env = func(k string) string {
		if k == credentials.EnvVar {
			return "from-env"
		}
		return ""
	}
	tok, src, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "from-env" || src != credentials.SourceEnv {
		t.Fatalf("Resolve = %q from %q, want the environment to win", tok, src)
	}
	// ResolveStored ignores it, because that is what logout revokes.
	tok, src, err = s.ResolveStored()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "from-keyring" || src != credentials.SourceKeyring {
		t.Fatalf("ResolveStored = %q from %q, want the stored token", tok, src)
	}
}

// TestFileFallbackWarnsEveryTime: the plaintext file is a downgrade, and
// a warning at login only is a warning the user has forgotten by the
// time it matters.
func TestFileFallbackWarnsEveryTime(t *testing.T) {
	kr := newFake()
	kr.failSet = errors.New("no session bus")
	s, warnings := store(t, kr, false)

	src, err := s.Save("refresh-abc")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if src != credentials.SourceFile {
		t.Fatalf("Save reported %q, want file", src)
	}
	if len(*warnings) == 0 {
		t.Fatal("saving to the plaintext file warned nobody")
	}

	kr.failGet = errors.New("no session bus")
	before := len(*warnings)
	if _, _, err := s.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(*warnings) == before {
		t.Fatal("reading the plaintext file did not warn")
	}
}

func TestFileIsOwnerOnly(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("file modes differ on Windows")
	}
	kr := newFake()
	kr.failSet = errors.New("unavailable")
	s, _ := store(t, kr, false)
	if _, err := s.Save("refresh-abc"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// A mode means nothing here — Go's modes do not map to ACLs, and
		// the file reads as 0666 whatever it was written with. The
		// access list is what restricts it, and TestFileIsRestrictedToOwner
		// reads that list back. All this holds is that the warning
		// describes the real mechanism.
		if note := credentials.FileProtection(); !strings.Contains(note, "ACL") {
			t.Fatalf("on Windows the file is restricted by an ACL, and the warning says %q", note)
		}
		return
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("token file mode %o is readable by others", mode)
	}
	if note := credentials.FileProtection(); note != "mode 0600" {
		t.Fatalf("the warning describes the file as %q", note)
	}
}

// TestSilentKeyringIsItsOwnError is the distinction that cost a sibling
// a debugging session: "log in again" is the wrong advice for a keyring
// that is locked rather than empty.
func TestSilentKeyringIsItsOwnError(t *testing.T) {
	kr := newFake() // empty, and Get returns a plain "not found"
	s, _ := store(t, kr, true)
	_, _, err := s.Resolve()
	if !errors.Is(err, credentials.ErrKeyringSilent) {
		t.Fatalf("Resolve = %v, want ErrKeyringSilent when the profile expects a keyring token", err)
	}

	s2, _ := store(t, newFake(), false)
	_, _, err = s2.Resolve()
	if !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("Resolve = %v, want ErrNotFound when nothing claims a token exists", err)
	}
}

func TestSaveRefusesAnEmptyToken(t *testing.T) {
	s, _ := store(t, newFake(), false)
	if _, err := s.Save(""); err == nil {
		t.Fatal("Save stored an empty token")
	}
}

func TestKeyringSaveDropsAStalePlaintextCopy(t *testing.T) {
	kr := newFake()
	kr.failSet = errors.New("unavailable")
	s, _ := store(t, kr, false)
	if _, err := s.Save("old"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.FilePath); err != nil {
		t.Fatalf("precondition: the file fallback was not written: %v", err)
	}

	kr.failSet = nil
	if _, err := s.Save("new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.FilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stale plaintext token survived a successful keyring save")
	}
}

func TestDeleteRemovesEverythingAndIsIdempotent(t *testing.T) {
	kr := newFake()
	kr.failSet = errors.New("unavailable")
	s, _ := store(t, kr, false)
	if _, err := s.Save("abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(s.FilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Delete left the plaintext file behind")
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete must be idempotent: %v", err)
	}
}

func TestOSKeyringIsTheProductionBackend(t *testing.T) {
	// Not exercised against the real keyring — a test that writes to a
	// maintainer's keyring is how a sibling's refresh token vanished.
	// This only asserts the constructor hands back something usable.
	if credentials.OSKeyring() == nil {
		t.Fatal("OSKeyring returned nil")
	}
	if !credentials.IsKeyringNotFound(keyring.ErrNotFound) {
		t.Fatal("IsKeyringNotFound does not recognize the keyring's own sentinel")
	}
	if credentials.IsKeyringNotFound(errors.New("something else")) {
		t.Fatal("IsKeyringNotFound matched an unrelated error")
	}
}

func TestResolveWithNoStoreConfigured(t *testing.T) {
	s := &credentials.Store{Profile: "default", Env: func(string) string { return "" }}
	if _, err := s.Save("abc"); err == nil {
		t.Fatal("Save succeeded with no keyring and no file path")
	}
	if _, _, err := s.Resolve(); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("Resolve = %v, want ErrNotFound", err)
	}
}

func TestCorruptTokenFileIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	kr := newFake()
	kr.failGet = errNotFound
	s := &credentials.Store{
		Profile: "default", Keyring: kr, FilePath: path,
		Env: func(string) string { return "" },
	}
	if _, _, err := s.Resolve(); err == nil {
		t.Fatal("a corrupt token file resolved successfully")
	}
}

func TestKeyringTransportFailureIsNotAMissingToken(t *testing.T) {
	kr := newFake()
	kr.failGet = errors.New("no session bus")
	s := &credentials.Store{
		Profile: "default", Keyring: kr,
		Env: func(string) string { return "" },
	}
	_, _, err := s.Resolve()
	if err == nil {
		t.Fatal("expected an error")
	}
	// It must carry the keyring's own reason, or the person is told to
	// log in when the real problem is a locked keyring.
	if !strings.Contains(err.Error(), "session bus") {
		t.Fatalf("the keyring failure was swallowed: %v", err)
	}
}
