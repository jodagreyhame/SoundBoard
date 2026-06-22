package setup

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// cableDownloadURLs are the official VB-Audio direct download URLs for the
// VB-CABLE driver pack, newest first. The current release at build time is
// Pack45; Pack43 is kept as a fallback in case VB-Audio rotates the "current"
// file. Both are served from VB-Audio's own CDN (download.vb-audio.com) — never
// a mirror — and both were confirmed reachable (HTTP 200, ~1.3 MB zip).
var cableDownloadURLs = []string{
	"https://download.vb-audio.com/Download_CABLE/VBCABLE_Driver_Pack45.zip",
	"https://download.vb-audio.com/Download_CABLE/VBCABLE_Driver_Pack43.zip",
}

// setupExeName is the 64-bit silent installer inside the driver pack zip. We
// run it with "-i -h": -i installs the driver, -h runs headless (no GUI). A
// reboot is typically required before the CABLE Input/Output endpoints appear.
const setupExeName = "VBCABLE_Setup_x64.exe"

// downloadTimeout bounds the whole download; the zip is ~1.3 MB so this is
// generous. installWait bounds how long we wait for the elevated installer.
const (
	downloadTimeout = 90 * time.Second
	installWait     = 5 * time.Minute
)

// ErrInstallDeclined means the user dismissed the UAC elevation prompt, so the
// installer never ran. Surfaced distinctly so the UI can say "you need to
// approve the admin prompt" rather than a generic failure.
var ErrInstallDeclined = errors.New("setup: VB-CABLE install declined at the UAC prompt")

// ErrNoNetwork means the driver pack could not be downloaded from any official
// URL (offline, DNS failure, server error).
var ErrNoNetwork = errors.New("setup: could not download VB-CABLE (no network or server unreachable)")

// installCable downloads the VB-CABLE driver pack, extracts the x64 setup, and
// runs it elevated and silently. It blocks until the installer exits or ctx is
// cancelled. The caller must NOT assume the endpoints exist on return — Windows
// often needs a reboot before the virtual cable enumerates.
func installCable(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	tmp, err := os.MkdirTemp("", "soundboard-vbcable-")
	if err != nil {
		return fmt.Errorf("setup: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "vbcable.zip")
	if err := downloadAny(ctx, cableDownloadURLs, zipPath); err != nil {
		return err
	}

	exePath, err := extractSetup(zipPath, tmp)
	if err != nil {
		return err
	}

	return runElevatedAndWait(ctx, exePath, "-i -h")
}

// downloadAny tries each URL in order and writes the first successful response
// body to dst. Network/server failures across all URLs collapse to ErrNoNetwork
// with the last underlying error wrapped for diagnostics.
func downloadAny(ctx context.Context, urls []string, dst string) error {
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	var lastErr error
	for _, url := range urls {
		if err := downloadOne(dctx, url, dst); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("%w: %v", ErrNoNetwork, lastErr)
}

// downloadOne fetches url into dst, failing on any non-2xx status.
func downloadOne(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// extractSetup unzips destDir and returns the path to VBCABLE_Setup_x64.exe.
// It guards against zip-slip by rejecting entries that escape destDir.
func extractSetup(zipPath, destDir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("setup: open zip: %w", err)
	}
	defer zr.Close()

	var setupPath string
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		// Clean the entry name; reject path traversal.
		clean := filepath.Clean(filepath.FromSlash(zf.Name))
		if strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || strings.Contains(clean, ".."+string(os.PathSeparator)) {
			continue
		}
		out := filepath.Join(destDir, clean)
		if !strings.HasPrefix(out, filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue // escapes destDir
		}
		if err := extractFile(zf, out); err != nil {
			return "", err
		}
		if strings.EqualFold(filepath.Base(out), setupExeName) {
			setupPath = out
		}
	}
	if setupPath == "" {
		return "", fmt.Errorf("setup: %s not found in driver pack", setupExeName)
	}
	return setupPath, nil
}

// extractFile writes a single zip entry to dst, creating parent dirs.
func extractFile(zf *zip.File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("setup: open %s in zip: %w", zf.Name, err)
	}
	defer rc.Close()

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil { //nolint:gosec // size bounded by trusted VB-Audio zip
		return fmt.Errorf("setup: extract %s: %w", zf.Name, err)
	}
	return nil
}

// --- Elevated launch via ShellExecuteExW("runas") ---------------------------

var (
	modshell32          = syscall.NewLazyDLL("shell32.dll")
	modkernel32         = syscall.NewLazyDLL("kernel32.dll")
	procShellExecuteExW = modshell32.NewProc("ShellExecuteExW")
	procWaitForSingle   = modkernel32.NewProc("WaitForSingleObject")
	procGetExitCode     = modkernel32.NewProc("GetExitCodeProcess")
	procCloseHandle     = modkernel32.NewProc("CloseHandle")
)

// shellExecuteInfoW mirrors SHELLEXECUTEINFOW (shellapi.h) on amd64. Field order
// and sizes match the documented struct exactly; cbSize must be set to the Go
// struct size before the call.
type shellExecuteInfoW struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIconOrMon   uintptr
	hProcess     uintptr
}

const (
	seeMaskNoCloseProcess = 0x00000040 // hProcess receives the process handle
	seeMaskNoAsync        = 0x00000100 // complete synchronously before returning
	seeMaskFlagNoUI       = 0x00000400 // suppress our own error dialogs (UAC still shows)
	swHide                = 0          // SW_HIDE — the installer runs headless anyway
	infinite              = 0xFFFFFFFF
	waitObject0           = 0x00000000
	waitTimeout           = 0x00000102
	errorCancelled        = 1223 // ERROR_CANCELLED — UAC prompt declined
)

// runElevatedAndWait launches exePath with the given argument string elevated
// (UAC "runas") and waits up to installWait (or ctx) for it to finish. UAC
// decline returns ErrInstallDeclined.
func runElevatedAndWait(ctx context.Context, exePath, args string) error {
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, err := syscall.UTF16PtrFromString(exePath)
	if err != nil {
		return fmt.Errorf("setup: encode exe path: %w", err)
	}
	params, err := syscall.UTF16PtrFromString(args)
	if err != nil {
		return fmt.Errorf("setup: encode args: %w", err)
	}
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(exePath))

	info := shellExecuteInfoW{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		lpDirectory:  dir,
		nShow:        swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		// ShellExecuteExW returned FALSE; callErr holds GetLastError().
		if errno, ok := callErr.(syscall.Errno); ok && uintptr(errno) == errorCancelled {
			return ErrInstallDeclined
		}
		return fmt.Errorf("setup: ShellExecuteExW(runas) failed: %w", callErr)
	}
	if info.hProcess == 0 {
		// Elevation succeeded but no waitable process handle (rare); we cannot
		// confirm completion, so report success — the install was launched.
		return nil
	}
	defer procCloseHandle.Call(info.hProcess)

	return waitProcess(ctx, info.hProcess)
}

// waitProcess blocks until the process handle signals, ctx is cancelled, or
// installWait elapses, polling in short slices so ctx cancellation is honoured.
func waitProcess(ctx context.Context, hProcess uintptr) error {
	deadline := time.Now().Add(installWait)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("setup: install cancelled: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("setup: timed out waiting for VB-CABLE installer")
		}
		// Wait in 500 ms slices so ctx cancellation stays responsive.
		r, _, _ := procWaitForSingle.Call(hProcess, uintptr(500))
		if r == waitObject0 {
			return checkExitCode(hProcess)
		}
		if r != waitTimeout {
			return fmt.Errorf("setup: WaitForSingleObject returned 0x%x", r)
		}
	}
}

// checkExitCode reads the installer's exit code. VBCABLE_Setup returns 0 on a
// clean install; any non-zero code is surfaced (the install may still need a
// reboot, which is not an error here).
func checkExitCode(hProcess uintptr) error {
	var code uint32
	r, _, _ := procGetExitCode.Call(hProcess, uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		return nil // could not read code; assume launched OK
	}
	if code != 0 {
		return fmt.Errorf("setup: VB-CABLE installer exited with code %d", code)
	}
	return nil
}
