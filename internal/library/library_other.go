//go:build !windows

package library

import (
	"fmt"
	"os"
	"path/filepath"
)

// platformDocumentsDir is the non-Windows stand-in.
//
// SoundBoard is Windows-only by construction (WASAPI routing, IPolicyConfig, an
// embedded Windows DLL), so this exists purely so `go build ./...` and the unit
// tests for the path logic work off-Windows. There is no known-folder concept to
// consult here, so the conventional location is used directly.
func platformDocumentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory unavailable: %w", err)
	}
	return filepath.Join(home, "Documents"), nil
}
