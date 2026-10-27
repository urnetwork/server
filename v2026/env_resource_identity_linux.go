//go:build linux

// Resolution caching identifies entries by device, inode, size and the
// kernel-set modification and change times (see env_resource_cache.go).
package server

import (
	"os"
	"syscall"
)

// False when the platform stat is unavailable; the resolution is then not cached.
func resolverIdentityOf(info os.FileInfo) (resolverIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return resolverIdentity{}, false
	}
	return resolverIdentity{
		device:          uint64(stat.Dev),
		inode:           uint64(stat.Ino),
		mode:            info.Mode(),
		size:            info.Size(),
		modifyTimeNanos: stat.Mtim.Nano(),
		changeTimeNanos: stat.Ctim.Nano(),
	}, true
}
