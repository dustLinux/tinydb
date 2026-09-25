//go:build cgo

package store

/*
#include <stdlib.h>

#if defined(__ANDROID__)
// bionic отдаёт mallopt только с API 26, а NDK-сисроут для более старых
// API вообще не содержит этого символа (ошибка линковки в CI). Поэтому:
// без malloc.h (там декларация за __BIONIC_AVAILABILITY_GUARD), слабая
// ссылка + проверка на NULL: на старых устройствах шаг просто пропускается.
// Значение M_PURGE_ALL совпадает с bionic'овским (джеймэллок, Android).
#ifndef M_PURGE_ALL
#define M_PURGE_ALL (-100)
#endif
int (*webdb_mallopt)(int, int) __attribute__((weak));
static inline void webdb_purge(void) {
	if (webdb_mallopt) {
		webdb_mallopt(M_PURGE_ALL, 0);
	}
}
#elif defined(__linux__)
#include <malloc.h>
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
