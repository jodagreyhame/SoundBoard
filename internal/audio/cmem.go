package audio

/*
#include <stdlib.h>
*/
import "C"
import "unsafe"

// cFree releases memory allocated by C.CBytes (used by malgo's
// DeviceID.Pointer(), which calls C.CBytes and never frees it). C.CBytes uses
// the C allocator, so it must be released with the matching C.free.
func cFree(p unsafe.Pointer) {
	C.free(p)
}
