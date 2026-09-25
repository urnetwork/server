// Local filesystem capacity errors retain the same precise classification as
// configured quota refusals; joined authorization failures remain distinct.
package server

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
)

// Path wrappers must not erase an exact operating-system capacity cause.
func TestBlobCapacityErrorLocalOperatingSystemCauses(t *testing.T) {
	for _, err := range []error{syscall.ENOSPC, syscall.EDQUOT, &os.PathError{Op: "write", Path: "synthetic-staging-file", Err: syscall.ENOSPC}, &os.LinkError{Op: "link", Old: "synthetic-stage", New: "synthetic-object", Err: syscall.EDQUOT}, errors.Join(syscall.ENOSPC, ErrBlobCapacityExceeded)} {
		if !IsBlobCapacityError(err) {
			t.Fatalf("local capacity cause lost actionable classification: %v", err)
		}
	}
	for _, err := range []error{syscall.EACCES, errors.New("no space left on device"), &os.PathError{Op: "write", Path: "synthetic-staging-file", Err: syscall.EIO}, errors.Join(syscall.ENOSPC, os.ErrPermission), errors.Join(syscall.EDQUOT, context.Canceled)} {
		if IsBlobCapacityError(err) {
			t.Fatalf("noncapacity or mixed local failure was hidden: %v", err)
		}
	}
}
