package main

import (
	"os"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/mmedum/google-mail-mcp/v2/internal/credentials"
)

// TestMain makes it impossible for a test in this package to reach the
// real OS keyring. A test can redirect the config directory and the
// environment and cannot redirect the keyring, and `logout` revokes the
// grant at Google and deletes the token: without this, `go test` would
// sign the person running it out of their own account.
//
// Installed for the whole package, because a rule each test has to
// remember is a rule the next test forgets.
func TestMain(m *testing.M) {
	keyringBackend = &packageKeyring{items: map[string]string{}}
	os.Exit(m.Run())
}

// packageKeyring is the only keyring a test in this package can see.
type packageKeyring struct {
	mu    sync.Mutex
	items map[string]string
}

func (k *packageKeyring) Get(service, account string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *packageKeyring) Set(service, account, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+"/"+account] = secret
	return nil
}

func (k *packageKeyring) Delete(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"/"+account)
	return nil
}

func (k *packageKeyring) reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items = map[string]string{}
}

// TestTheRealKeyringIsUnreachableFromTests is the decoy: it fails if
// TestMain stops installing the fake, rather than letting a refactor
// quietly return to deleting real credentials.
func TestTheRealKeyringIsUnreachableFromTests(t *testing.T) {
	if _, ok := keyringBackend.(*packageKeyring); !ok {
		t.Fatalf("keyringBackend is %T, not the package fake; a test here could revoke a real refresh token", keyringBackend)
	}
	if _, ok := credentials.OSKeyring().(*packageKeyring); ok {
		t.Fatal("credentials.OSKeyring() returns the test fake; production would have no keyring")
	}
}
