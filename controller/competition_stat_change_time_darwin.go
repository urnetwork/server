//go:build darwin

// The inode change time is the custody clock exact competition records compare
// before and after authentication. Darwin names it Ctimespec, so bringyourctl
// still builds on an operator's Mac.
package controller

import (
	"syscall"
)

// Whether two stats of one file carry the same inode change time.
func statChangeTimeEqual(a *syscall.Stat_t, b *syscall.Stat_t) bool {
	return a.Ctimespec == b.Ctimespec
}
