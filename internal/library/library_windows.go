//go:build windows

package library

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// platformDocumentsDir asks Windows for the Documents known folder.
//
// Joining os.UserHomeDir() with "Documents" is wrong on a large fraction of
// real machines: OneDrive's "Back up your folders" (on by default on many OEM
// Windows 11 installs, and mandatory under some tenants) redirects Documents to
// %USERPROFILE%\OneDrive\Documents, and Folder Redirection GPO can move it to a
// network share. Guessing the path would silently read and write somewhere the
// user's Explorer never shows, which looks exactly like "my clips vanished".
//
// Only the on-disk name is fixed — since Vista it is always "Documents", with
// localisation handled by desktop.ini display names — so no locale handling is
// needed here, just the real path.
func platformDocumentsDir() (string, error) {
	p, err := windows.KnownFolderPath(windows.FOLDERID_Documents, windows.KF_FLAG_DEFAULT)
	if err == nil && p != "" {
		return p, nil
	}

	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return "", fmt.Errorf("known folder lookup failed (%v) and home directory unavailable: %w", err, homeErr)
	}

	guess := filepath.Join(home, "Documents")
	log.Printf("library: Documents known-folder lookup failed (%v); falling back to %s, which will be wrong if Documents is redirected to OneDrive or a network share", err, guess)
	return guess, nil
}
