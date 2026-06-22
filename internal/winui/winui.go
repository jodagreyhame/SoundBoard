// Package winui provides tiny native Windows helpers the tray needs for a
// visible setup experience: open a URL in the default browser and show
// information / yes-no message boxes. It uses user32.dll directly so there is no
// extra dependency and no console window flashes.
package winui

import (
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

// MessageBox flags (winuser.h).
const (
	mbOK          = 0x00000000
	mbYesNo       = 0x00000004
	mbIconInfo    = 0x00000040
	mbIconWarning = 0x00000030
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000

	idYes = 6
)

// OpenURL opens url in the user's default browser without flashing a console.
// rundll32 + url.dll is the standard shell handler for this.
func OpenURL(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// Info shows a modal information box with an OK button.
func Info(title, text string) {
	messageBox(title, text, mbOK|mbIconInfo|mbSetForeground|mbTopMost)
}

// Confirm shows a modal yes/no box and reports whether the user chose Yes.
func Confirm(title, text string) bool {
	return messageBox(title, text, mbYesNo|mbIconWarning|mbSetForeground|mbTopMost) == idYes
}

func messageBox(title, text string, flags uint32) int {
	textPtr, _ := syscall.UTF16PtrFromString(text)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	ret, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
	return int(ret)
}
