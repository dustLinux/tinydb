//go:build !cgo

package store

// purgeNative is a no-op without cgo (native allocator handle unavailable).
func purgeNative() {}
