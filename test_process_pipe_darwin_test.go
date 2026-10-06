// A kqueue can report pipe type but must never become an owned test endpoint.
package server

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Use our own kqueue to pin the runtime-handle rejection without touching Go's.
func TestTestProcessPipeRejectsKqueueWithoutClosing(t *testing.T) {
	fd, err := unix.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	for _, access := range []TestProcessPipeAccess{TestProcessPipeRead, TestProcessPipeWrite} {
		if err := ValidateTestProcessPipe(uintptr(fd), access); err == nil {
			t.Fatalf("kqueue accepted for access %d", access)
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			t.Fatalf("rejection closed kqueue: %v", err)
		}
	}
}
