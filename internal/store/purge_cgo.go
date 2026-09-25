//go:build cgo

package store

/*
#include <stdlib.h>

#if defined(__ANDROID__) || defined(__linux__)
#include <malloc.h>
#ifdef __ANDROID__
// bionic hides the declaration behind __BIONIC_AVAILABILITY_GUARD(26),
// which cgo's preprocessor run does not satisfy; the symbol itself is in
// libc since API 26 and M_PURGE_ALL is an unconditional macro there.
extern int mallopt(int __option, int __value);
#endif
#ifndef M_PURGE_ALL
// M_PURGE_ALL (-100) is bionic-specific; glibc treats unknown mallopt
// options as a no-op, so the call is harmless outside Android too.
#define M_PURGE_ALL (-100)
#endif
static inline void webdb_purge(void) { mallopt(M_PURGE_ALL, 0); }
#else
// No mallopt on this platform (e.g. macOS): keep the hook a no-op.
static inline void webdb_purge(void) { (void)0; }
#endif
*/
import "C"

// purgeNative asks the C allocator to release cached free pages now.
// Used after burst phases (import/backup) to keep RSS within the RAM budget.
// No-op where the platform has no mallopt (macOS); the rest of the purge
// chain (shrink_memory, FreeOSMemory) still runs everywhere.
func purgeNative() {
	C.webdb_purge()
}
