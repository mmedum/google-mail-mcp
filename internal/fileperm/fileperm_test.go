package fileperm_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mmedum/google-mail-mcp/v2/internal/fileperm"
)

// TestRestrictToOwnerNarrowsAWideFile is the case the Unix path exists
// for: a file an earlier version, a umask or a restore left readable.
func TestRestrictToOwnerNarrowsAWideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fileperm.RestrictToOwner(path); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}
	if runtime.GOOS == "windows" {
		// A mode means nothing here; TestRestrictToOwnerSetsAProtectedACL
		// reads the access list instead.
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("mode %o is still readable by others", mode)
	}
}

func TestRestrictToOwnerReportsAMissingFile(t *testing.T) {
	err := fileperm.RestrictToOwner(filepath.Join(t.TempDir(), "nothing-here.json"))
	if err == nil {
		t.Fatal("restricting a file that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "fileperm:") {
		t.Fatalf("the error does not say which layer failed: %v", err)
	}
}

// TestDescribeNamesTheRealMechanism: the warnings quote this, and the
// whole defect was a warning naming a protection the platform did not
// provide.
func TestDescribeNamesTheRealMechanism(t *testing.T) {
	got := fileperm.Describe()
	want := "mode 0600"
	if runtime.GOOS == "windows" {
		want = "ACL"
	}
	if !strings.Contains(got, want) {
		t.Fatalf("Describe() = %q, which does not name %q", got, want)
	}
}
