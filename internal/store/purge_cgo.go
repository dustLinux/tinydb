//go:build cgo

package store

/*
#include <malloc.h>
// bionic hides the declaration behind __BIONIC_AVAILABILITY_GUARD(26),
// which cgo's preprocessor run does not satisfy; the symbol itself is in
// libc since API 26 and M_PURGE_ALL (above) is an unconditional macro.
extern int mallopt(int __option, int __value);
*/
import "C"

// purgeNative asks the C allocator to release cached free pages now.
// Used after burst phases (import/backup) to keep RSS within the RAM budget.
func purgeNative() {
	C.mallopt(C.M_PURGE_ALL, 0)
}
