//go:build !linux

package monitor

import (
	"context"
	"errors"
	"io"
)

// Shared native ownership requires Linux process generations and cgroup v2.
func newSharedSshAdmission(context.Context, string, []string) (sshAdmissionBackend, error) {
	return nil, errors.New("shared SSH admission requires Linux")
}

func RunSshAdmissionLease(context.Context, string, io.Reader, io.Writer) error {
	return errors.New("shared SSH admission requires Linux")
}
