//go:build windows

package server

import "syscall"

// SoReusePort is a no-op on Windows. Windows SO_REUSEADDR does not provide
// Unix SO_REUSEPORT semantics and can permit unrelated processes to bind the
// same address, so substituting it would weaken listener ownership. A single
// server instance still binds normally; overlapping listener handoff remains
// a Unix-only deployment feature.
func SoReusePort(_ string, _ string, _ syscall.RawConn) error {
	return nil
}
