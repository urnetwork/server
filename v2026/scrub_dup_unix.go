//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package server

import "syscall"

// dup2 points newfd at whatever oldfd refers to. The BSDs have dup2 directly.
func dup2(oldfd int, newfd int) error {
	return syscall.Dup2(oldfd, newfd)
}
