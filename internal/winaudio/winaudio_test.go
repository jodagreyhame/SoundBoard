package winaudio

import (
	"testing"
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

// TestUTF16PtrToString round-trips Go strings through a NUL-terminated UTF-16
// buffer, the same shape IMMDevice::GetId and PKEY_Device_FriendlyName hand
// back. No COM is involved.
func TestUTF16PtrToString(t *testing.T) {
	cases := []string{
		"",
		"CABLE Output (VB-Audio Virtual Cable)",
		"{0.0.1.00000000}.{b3f60002-1234-5678-9abc-def012345678}",
		"Microphone (Réaltek © Audio)", // non-ASCII
	}
	for _, want := range cases {
		u16 := stringToUTF16WithNul(want)
		got := utf16PtrToString(&u16[0])
		if got != want {
			t.Fatalf("round-trip mismatch: got %q want %q", got, want)
		}
	}

	// A nil pointer must yield the empty string, not panic.
	if got := utf16PtrToString(nil); got != "" {
		t.Fatalf("nil pointer: got %q want empty", got)
	}
}

// stringToUTF16WithNul builds a NUL-terminated UTF-16 slice (always at least the
// terminator) so &slice[0] is a valid pointer even for the empty string.
func stringToUTF16WithNul(s string) []uint16 {
	out := make([]uint16, 0, len(s)+1)
	for _, r := range s {
		if r <= 0xFFFF {
			out = append(out, uint16(r))
		} else {
			// Surrogate pair for runes outside the BMP (not exercised here, but
			// keeps the helper correct).
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		}
	}
	return append(out, 0)
}

// TestOwnsCOMInit pins the CoInitializeEx HRESULT classification: S_OK and
// S_FALSE both add a refcount we must balance with CoUninitialize, while
// RPC_E_CHANGED_MODE does not. go-ole returns S_FALSE as a non-nil *OleError, so
// a naive `err == nil` check would leak one COM init per call on the (common)
// already-initialized-same-apartment path.
func TestOwnsCOMInit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"S_OK", nil, true},
		{"S_FALSE", ole.NewError(sFalse), true},
		{"RPC_E_CHANGED_MODE", ole.NewError(rpcEChangedMode), false},
		{"other failure (E_FAIL)", ole.NewError(0x80004005), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownsCOMInit(tc.err); got != tc.want {
				t.Fatalf("ownsCOMInit(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPolicyConfigVtableLayout pins the byte offset of SetDefaultEndpoint in the
// IPolicyConfig vtable. This is the single most fragile fact in the package: the
// interface is undocumented, and an off-by-one slot would silently call the
// wrong method (e.g. SetPropertyValue) on the live audio policy service. After
// the three IUnknown slots, SetDefaultEndpoint is the 11th interface method,
// i.e. vtable index 13 — 13*8 = 104 bytes on amd64.
func TestPolicyConfigVtableLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("offset invariant is amd64-specific")
	}
	var v iPolicyConfigVtbl
	off := unsafe.Offsetof(v.SetDefaultEndpoint)
	if off != 13*8 {
		t.Fatalf("SetDefaultEndpoint at offset %d, want %d (vtable slot 13)", off, 13*8)
	}
	// SetEndpointVisibility must be the very next slot.
	if got := unsafe.Offsetof(v.SetEndpointVisibility); got != 14*8 {
		t.Fatalf("SetEndpointVisibility at offset %d, want %d", got, 14*8)
	}
}

// TestEnumeratorVtableLayout pins the documented MMDevice vtable slots used by
// the enumeration path (EnumAudioEndpoints=3, GetDefaultAudioEndpoint=4).
func TestEnumeratorVtableLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("offset invariant is amd64-specific")
	}
	var v immDeviceEnumeratorVtbl
	if got := unsafe.Offsetof(v.EnumAudioEndpoints); got != 3*8 {
		t.Fatalf("EnumAudioEndpoints offset %d, want %d", got, 3*8)
	}
	if got := unsafe.Offsetof(v.GetDefaultAudioEndpoint); got != 4*8 {
		t.Fatalf("GetDefaultAudioEndpoint offset %d, want %d", got, 4*8)
	}

	var d immDeviceVtbl
	if got := unsafe.Offsetof(d.GetID); got != 5*8 {
		t.Fatalf("IMMDevice.GetId offset %d, want %d", got, 5*8)
	}
	if got := unsafe.Offsetof(d.OpenPropertyStore); got != 4*8 {
		t.Fatalf("IMMDevice.OpenPropertyStore offset %d, want %d", got, 4*8)
	}
}

// TestFriendlyNamePropertyKey pins PKEY_Device_FriendlyName
// {a45c254e-df1c-4efd-8020-67d146a850e0},14 — the wrong PID or GUID would read a
// different property (e.g. the device description) instead of the endpoint name.
func TestFriendlyNamePropertyKey(t *testing.T) {
	want := ole.GUID{
		Data1: 0xa45c254e, Data2: 0xdf1c, Data3: 0x4efd,
		Data4: [8]byte{0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0},
	}
	if pkeyDeviceFriendlyName.fmtid != want {
		t.Fatalf("PKEY fmtid = %+v, want %+v", pkeyDeviceFriendlyName.fmtid, want)
	}
	if pkeyDeviceFriendlyName.pid != 14 {
		t.Fatalf("PKEY pid = %d, want 14", pkeyDeviceFriendlyName.pid)
	}
}
