//go:build !unix

package nativefeefixture

import (
	"errors"
	"os"
)

const (
	lockExclusive   = 1
	lockShared      = 2
	lockNonblocking = 4
)

func lockFile(string, int) (*os.File, error) {
	return nil, errors.New("the exports' cache needs flock")
}

func singleLink(os.FileInfo) bool { return false }
