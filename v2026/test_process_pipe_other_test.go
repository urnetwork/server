//go:build !unix

// Unsupported platforms reject the protocol without probing numeric handles.
package server

import "testing"

// Neither direction can qualify when anonymous inheritance is unavailable.
func TestTestProcessPipeUnsupportedPlatformRefuses(t *testing.T) {
	for _, access := range []TestProcessPipeAccess{TestProcessPipeRead, TestProcessPipeWrite} {
		if err := ValidateTestProcessPipe(3, access); err == nil {
			t.Fatalf("unsupported descriptor accepted for access %d", access)
		}
	}
}
