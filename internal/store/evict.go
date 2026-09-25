package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	evictOnce  sync.Once
	codeRanges [][2]uintptr
)

// loadCodeRanges collects r-x mappings of our own executable from
// /proc/self/maps. Matching is done by inode (path form is unreliable on
// Android: /data/data is a symlink to /data/user/0 and maps may record
// either spelling), with the path kept as a fallback. Mappings never move
// after the process starts.
func loadCodeRanges() {
	var names []string
	var ino uint64
	if exe, err := os.Executable(); err == nil {
		names = append(names, exe)
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			names = append(names, r)
		}
		if fi, err := os.Stat(exe); err == nil {
			ino = uint64(fi.Sys().(*syscall.Stat_t).Ino)
		}
	}
	if len(names) == 0 && ino == 0 {
		return
	}
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || !strings.HasPrefix(f[1], "r-x") {
			continue
		}
		path := strings.TrimSuffix(strings.Join(f[5:], " "), " (deleted)")
		match := false
		if ino != 0 {
			if n, err := strconv.ParseUint(f[4], 10, 64); err == nil && n == ino && n != 0 {
				match = true
			}
		}
		if !match {
			for _, n := range names {
				if path == n {
					match = true
					break
				}
			}
		}
		if !match {
			continue
		}
		span := strings.SplitN(f[0], "-", 2)
		if len(span) != 2 {
			continue
		}
		start, e1 := strconv.ParseUint(span[0], 16, 64)
		end, e2 := strconv.ParseUint(span[1], 16, 64)
		if e1 != nil || e2 != nil || end <= start {
			continue
		}
		codeRanges = append(codeRanges, [2]uintptr{uintptr(start), uintptr(end)})
	}
}

// evictCode drops resident pages of our own r-x code segment back to the
// kernel. The mapping is file-backed and clean (code is never written
// in place), so every page faults back from the page cache on next use —
// exactly the way it was loaded the first time. Only r-x ranges are
// touched: rw- (relocated data) and r-- (COW-relocated rodata) must stay.
// Clean file pages are reclaimable by the kernel at any moment anyway, so
// this only front-loads what memory pressure would do on its own.
// Called after burst phases to keep RSS inside the RAM budget.
// Returns (ranges matched, first errno) for diagnostics.
func evictCode() (int, syscall.Errno) {
	evictOnce.Do(loadCodeRanges)
	for i, r := range codeRanges {
		_, _, errno := syscall.Syscall(syscall.SYS_MADVISE, r[0], r[1]-r[0], syscall.MADV_DONTNEED)
		if errno != 0 {
			return i, errno
		}
	}
	return len(codeRanges), 0
}
