//go:build !linux

// Custody publication currently requires Linux no-replace rename and pinned
// no-follow directory descriptors. Other platforms refuse before file access.
package strecovery

import (
	"context"
	"errors"
	"os"
)

// Do not weaken custody checks on a platform lacking the supported syscall path.
func openPrivatePath(string, bool) (*os.File, error) {
	return nil, errors.New("recovery custody requires Linux")
}

// No caller may substitute ordinary path-following child opens.
func openPrivateChild(*os.File, string) (*os.File, error) {
	return nil, errors.New("recovery custody requires Linux")
}

// Unsupported custody reads have no file-system effects.
func readOpened(context.Context, *os.File, int) ([]byte, error) {
	return nil, errors.New("recovery custody requires Linux")
}

// Config and archive admission use the same fail-closed platform boundary.
func readPrivateFile(context.Context, string, int) ([]byte, error) {
	return nil, errors.New("recovery custody requires Linux")
}

// An unsupported store never becomes an empty successful source.
func readStore(context.Context, StoreSource, Limits) ([]StoreFile, error) {
	return nil, errors.New("recovery custody requires Linux")
}

// Directory preflight cannot be replaced by an unbounded listing.
func directoryNames(*os.File, int) ([]string, error) {
	return nil, errors.New("recovery custody requires Linux")
}

// Unsupported publication must not create partial custody files.
func publishFile(context.Context, *os.File, string, []byte) (bool, error) {
	return false, errors.New("recovery custody requires Linux")
}

// No unsupported process may claim the restoration ownership lock.
func lockDirectory(*os.File) (func(), error) {
	return nil, errors.New("recovery custody requires Linux")
}
