//go:build windows

package fileperm_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/mmedum/google-mail-mcp/internal/fileperm"
)

// TestRestrictToOwnerSetsAProtectedACL reads the access list back rather
// than trusting the call that set it.
//
// 0600 is a no-op on Windows: the first CI run there found the token
// file at 0666, readable by every account on the machine, while its
// warnings said "mode 0600". What replaced it has to be
// checked the way Windows describes access, not the way Unix does.
func TestRestrictToOwnerSetsAProtectedACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fileperm.RestrictToOwner(path); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("reading the file's security descriptor: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("reading the descriptor's control bits: %v", err)
	}
	// Without SE_DACL_PROTECTED the entries inherited from the parent
	// directory are still in force, and a grant added below them widens
	// access rather than replacing it.
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("the access list still inherits from its parent, so it restricts nothing")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("reading the access list: %v", err)
	}
	if dacl == nil {
		t.Fatal("no access list on the file, which means everyone has access")
	}
	if dacl.AceCount != 1 {
		t.Fatalf("the access list has %d entries; only this account should be on it", dacl.AceCount)
	}
}
