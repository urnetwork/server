//go:build unix

// Native no-follow and nonblocking flags preserve regular-file admission.
package competitionagent

import (
	"os"
	"syscall"
)

// The confined root retains ancestor ownership; the leaf must not be followed.
func openProtocolFile(directory *os.Root, name string) (*os.File, error) {
	return directory.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
