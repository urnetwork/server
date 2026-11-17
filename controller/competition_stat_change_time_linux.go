//go:build linux

// The inode change time is the custody clock exact competition records compare
// before and after authentication. Linux names it Ctim.
package controller

import (
	"syscall"
)

// Whether two stats of one file carry the same inode change time.
func statChangeTimeEqual(a *syscall.Stat_t, b *syscall.Stat_t) bool {
	return a.Ctim == b.Ctim
}
