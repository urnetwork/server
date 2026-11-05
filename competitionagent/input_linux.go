// Private uploads never retain a scheduling/cleanup authority lock. Cancellation
// interrupts the actual stream read; no abandoned reader goroutine owns input.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// The only production stream is an SSH/local-exec stdin pipe. Duplication makes
// its close lifecycle private, and nonblocking registration gives os.File an
// interruptible poll owner rather than an uninterruptible syscall.Read thread.
func cancellableControlInput(reader io.Reader) (io.Reader, io.Closer, error) {
	if file, ok := reader.(*os.File); ok {
		owned, err := duplicateControlFile(file, false)
		return owned, owned, err
	}
	// These finite memory readers cannot block inside Read. Every other stream
	// must explicitly promise that Close interrupts its pending read, matching
	// net.Conn, io.PipeReader and the production pipe owner above.
	switch reader.(type) {
	case *bytes.Buffer, *bytes.Reader, *strings.Reader:
		return reader, io.NopCloser(strings.NewReader("")), nil
	}
	if closer, ok := reader.(io.ReadCloser); ok {
		return closer, closer, nil
	}
	return nil, nil, errors.New("subevaluation input must support interruptible close or finite memory reads")
}

// Input and output each own a distinct nonblocking duplicate; closing an RPC
// lease never closes the launcher-owned original or a detached evaluation.
func duplicateControlFile(file *os.File, writing bool) (*os.File, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var duplicateErr error
	if err := connection.Control(func(original uintptr) {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_GETFL, 0)
		if errno != 0 {
			duplicateErr = errno
			return
		}
		accessMode := int(flags) & syscall.O_ACCMODE
		if writing && accessMode == syscall.O_RDONLY || !writing && accessMode == syscall.O_WRONLY {
			duplicateErr = errors.New("control descriptor has the wrong I/O direction")
			return
		}
		duplicate, _, errno := syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_DUPFD_CLOEXEC, 0)
		if errno != 0 {
			duplicateErr = errno
			return
		}
		fd = int(duplicate)
	}); err != nil {
		return nil, err
	}
	if duplicateErr != nil {
		return nil, duplicateErr
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "subevaluation-control-stream"), nil
}

// The absolute submission deadline and fixed transfer ceiling both apply to
// input; reconnecting cannot grant an upload a fresh submission budget.
func (self *Agent) receiveStartInputs(parent context.Context, request Request, reader io.Reader, incoming string) error {
	ctx, cancel := context.WithTimeout(parent, self.settings.InputTransferTimeout)
	defer cancel()
	ctx, deadlineCancel := context.WithDeadline(ctx, request.Start.Identity.DeadlineAt)
	defer deadlineCancel()
	input, closer, err := cancellableControlInput(reader)
	if err != nil {
		return err
	}
	defer closer.Close()
	if file, ok := input.(*os.File); ok {
		deadline, _ := ctx.Deadline()
		if err := file.SetReadDeadline(deadline); err != nil {
			return errors.New("control input descriptor cannot interrupt a pending read")
		}
		defer file.SetReadDeadline(time.Time{})
	}
	closed := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() {
		_ = closer.Close()
		close(closed)
	})
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	err = ReadFiles(&contextReader{ctx: ctx, reader: input}, incoming, request.Start.Inputs, self.config.Limits.InputBytes, self.config.Limits)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
