//go:build windows

package server

import "testing"

// TestSoReusePortWindowsDoesNotSubstituteReuseAddr pins the safe Windows
// behavior: do not emulate Unix SO_REUSEPORT with Windows SO_REUSEADDR.
func TestSoReusePortWindowsDoesNotSubstituteReuseAddr(t *testing.T) {
	if err := SoReusePort("tcp", "127.0.0.1:0", nil); err != nil {
		t.Fatal(err)
	}
}
