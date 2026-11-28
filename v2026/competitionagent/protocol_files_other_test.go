//go:build !linux

// Unsupported execution platforms must not start a partial transfer or create
// an output tree while retaining compile-time access to signed envelopes.
package competitionagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Any attempted stream access is a deterministic failure, not a timer race.
type unsupportedProtocolIo struct{}

func (unsupportedProtocolIo) Read(content []byte) (int, error) {
	panic("unsupported file transport read peer data")
}

func (unsupportedProtocolIo) Write(content []byte) (int, error) {
	panic("unsupported file transport wrote peer data")
}

// The explicit platform refusal does not read even a zero-length manifest's
// stream or create a destination supplied by its caller.
func TestUnsupportedFileTransportRefusesBeforeIoAndCreation(t *testing.T) {
	limits := DefaultLimits()
	destination := filepath.Join(t.TempDir(), "must-not-exist")
	if err := WriteFiles(unsupportedProtocolIo{}, destination, nil, limits.InputBytes, limits); !errors.Is(err, errFileTransportUnsupported) {
		t.Fatal("unsupported send did not refuse", err)
	}
	if err := ReadFiles(unsupportedProtocolIo{}, destination, nil, limits.ArtifactBytes, limits); !errors.Is(err, errFileTransportUnsupported) {
		t.Fatal("unsupported receive did not refuse", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("unsupported receive touched its destination", err)
	}
}
