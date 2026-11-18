//go:build !linux

package filecap

import (
	"errors"
)

// security.capability is a linux attribute: elsewhere nothing can be set or read.
var errNotLinux = errors.New("file capabilities are a linux attribute")

// Refuses: file capabilities are linux's.
func SetFileCapability(path string, capability FileCapability) error {
	return errNotLinux
}

// Refuses: file capabilities are linux's.
func ReadFileCapability(path string) ([]byte, error) {
	return nil, errNotLinux
}

// Refuses: SO_MARK is linux's.
func ProbeSoMark() error {
	return errNotLinux
}
