// Admission children share the parent package's descriptor preflight.
package work

import (
	"errors"
	"os"

	"github.com/urnetwork/server"
)

// Only a qualified inherited write endpoint can acquire an os.File owner.
func privateLoadInheritedWritePipe(fd uintptr) (*os.File, error) {
	if err := server.ValidateTestProcessPipe(fd, server.TestProcessPipeWrite); err != nil {
		return nil, errors.New("child report descriptor is not an owned write pipe")
	}
	pipe := os.NewFile(fd, "private-load-reports")
	if pipe == nil {
		return nil, errors.New("child report descriptor is not an owned write pipe")
	}
	return pipe, nil
}
