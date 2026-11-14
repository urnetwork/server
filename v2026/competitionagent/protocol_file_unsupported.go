//go:build !unix

// File export requires a native no-follow, nonblocking regular-file boundary.
package competitionagent

import "os"

// Unsupported platforms cannot replace the protected open with an ordinary one.
func openProtocolFile(_ *os.Root, _ string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}
