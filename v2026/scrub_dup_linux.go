//go:build linux

package server

import "golang.org/x/sys/unix"

// dup2 points newfd at whatever oldfd refers to.
//
// linux/arm64 has no dup2 syscall at all -- it was dropped when dup3 was
// introduced -- so the portable spelling is dup3 with no flags, which is
// exactly dup2's behavior on every linux architecture.
func dup2(oldfd int, newfd int) error {
	return unix.Dup3(oldfd, newfd, 0)
}
