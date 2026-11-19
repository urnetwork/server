//go:build !linux

package nginx_test

// The parity container is linux; elsewhere the harness's in-container test
// skips before it would add an address (parityInContainer).

import (
	"errors"
	"net/netip"
)

// Adds an address on linux only.
func addParityLoopbackAddr(addr netip.Addr) error {
	return errors.New("the parity harness adds addresses only on linux")
}
