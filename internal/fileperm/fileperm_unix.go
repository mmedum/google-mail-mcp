//go:build !windows

// Package fileperm restricts a file to the account that wrote it: mode
// 0600 on Unix, an explicit access list on Windows, where a mode only
// sets the read-only attribute and protects nothing.
package fileperm

import (
	"fmt"
	"os"
)

// RestrictToOwner reasserts owner-only permissions after the file is in
// place. The write already asked for 0600; this catches a file an
// earlier version, a umask or a restore left wider.
func RestrictToOwner(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("fileperm: restricting %s to its owner: %w", path, err)
	}
	return nil
}

// Describe says what the restriction achieves here, for the warning
// that quotes it. On Unix a mode is the whole story.
func Describe() string { return "mode 0600" }
