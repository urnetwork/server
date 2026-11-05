// Framing and replies own native stream deadlines, not abandoned I/O goroutines.
// A request lifetime is distinct from the detached evaluation's durable clock.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// Non-file blocking transports must expose real native deadline semantics.
// Close is used only for parent cancellation; a local stage timeout does not
// race a completed I/O operation by closing the stream from a watchdog.
type controlDeadlineReader interface {
	io.ReadCloser
	SetReadDeadline(time.Time) error
}

// The same contract preserves a completed write even if its goroutine resumes
// after the deadline. No asynchronous writer retains a buffer or descriptor.
type controlDeadlineWriter interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

// Memory streams cannot block. File streams own a pollable duplicate; a caller
// retains its original descriptor and any independent evaluation lifetime.
type controlReadStream struct {
	reader      io.Reader
	setDeadline func(time.Time) error
	cancelClose io.Closer
	owned       io.Closer
}

// Only production pipes, deadline-capable streams and finite memory input are
// accepted before parsing an unauthenticated frame. Unknown readers fail closed.
func newControlReadStream(reader io.Reader) (*controlReadStream, error) {
	self := &controlReadStream{reader: reader}
	if file, ok := reader.(*os.File); ok {
		owned, err := duplicateControlFile(file, false)
		if err != nil {
			return nil, err
		}
		self.reader, self.owned, self.cancelClose, self.setDeadline = owned, owned, owned, owned.SetReadDeadline
	} else if stream, ok := reader.(controlDeadlineReader); ok {
		self.cancelClose, self.setDeadline = stream, stream.SetReadDeadline
	} else {
		nilInput := false
		switch input := reader.(type) {
		case *bytes.Buffer:
			nilInput = input == nil
		case *bytes.Reader:
			nilInput = input == nil
		case *strings.Reader:
			nilInput = input == nil
		default:
			return nil, errors.New("control header input must be finite or support native read deadlines")
		}
		if nilInput {
			return nil, errors.New("control header input must not be a nil finite stream")
		}
	}
	if self.setDeadline != nil {
		if err := self.setDeadline(time.Time{}); err != nil {
			_ = self.Close()
			return nil, errors.New("control header input cannot interrupt a pending read")
		}
	}
	return self, nil
}

// Only a privately duplicated file is normally closed. Borrowed native streams
// are closed on explicit parent cancellation, never after healthy framing.
func (self *controlReadStream) Close() error {
	if self.owned == nil {
		return nil
	}
	return self.owned.Close()
}

// The header allowance is independent from the later signed body allowance.
// Clearing this native deadline cannot reset the submission's absolute clock.
func (self *controlReadStream) WithDeadline(ctx context.Context, allowance time.Duration, consume func(io.Reader) error) error {
	return withControlDeadline(ctx, allowance, self.setDeadline, self.cancelClose, func() error {
		return consume(&contextReader{ctx: ctx, reader: self.reader})
	})
}

// Replies use the same exact stream across framing and file stages, with an
// independent native allowance for each. No stage closes a healthy successor.
type controlWriteStream struct {
	writer      io.Writer
	setDeadline func(time.Time) error
	cancelClose io.Closer
	owned       io.Closer
}

// Validation happens before dispatch, so an unsupported output cannot cause a
// durable start whose acknowledgement was never writable by this RPC owner.
func newControlWriteStream(writer io.Writer) (*controlWriteStream, error) {
	self := &controlWriteStream{writer: writer}
	if file, ok := writer.(*os.File); ok {
		owned, err := duplicateControlFile(file, true)
		if err != nil {
			return nil, err
		}
		self.writer, self.owned, self.cancelClose, self.setDeadline = owned, owned, owned, owned.SetWriteDeadline
	} else if stream, ok := writer.(controlDeadlineWriter); ok {
		self.cancelClose, self.setDeadline = stream, stream.SetWriteDeadline
	} else {
		memory, finite := writer.(*bytes.Buffer)
		if finite && memory == nil {
			return nil, errors.New("control reply output must not be a nil finite stream")
		}
		if !finite && writer != io.Discard {
			return nil, errors.New("control reply output must be finite or support native write deadlines")
		}
	}
	if self.setDeadline != nil {
		if err := self.setDeadline(time.Time{}); err != nil {
			_ = self.Close()
			return nil, errors.New("control reply output cannot interrupt a pending write")
		}
	}
	return self, nil
}

// This does not close a caller-owned connection after a successful operation.
func (self *controlWriteStream) Close() error {
	if self.owned == nil {
		return nil
	}
	return self.owned.Close()
}

// File transfer receives its complete allowance, not the framing deadline's
// remainder. The coordinator still separately enforces the original job clock.
func (self *controlWriteStream) WithDeadline(ctx context.Context, allowance time.Duration, produce func(io.Writer) error) error {
	return withControlDeadline(ctx, allowance, self.setDeadline, self.cancelClose, func() error {
		return produce(controlContextWriter{ctx: ctx, writer: self.writer})
	})
}

// Only parent cancellation owns a close callback. Native deadlines decide
// actual I/O timeout; a late watchdog cannot undo an already-complete write.
func withControlDeadline(parent context.Context, allowance time.Duration, setDeadline func(time.Time) error, closer io.Closer, operation func() error) error {
	if allowance <= 0 || operation == nil {
		return errors.New("control I/O requires a positive owner-local allowance")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	if setDeadline != nil {
		deadline := time.Now().Add(allowance)
		if limit, present := parent.Deadline(); present && limit.Before(deadline) {
			deadline = limit
		}
		if err := setDeadline(deadline); err != nil {
			return err
		}
	}
	closed := make(chan struct{})
	stopClose := func() bool { return true }
	if closer != nil {
		stopClose = context.AfterFunc(parent, func() {
			_ = closer.Close()
			close(closed)
		})
	}
	err := operation()
	if !stopClose() {
		<-closed
	}
	var clearErr error
	if setDeadline != nil {
		clearErr = setDeadline(time.Time{})
	}
	if err == nil {
		if parent.Err() != nil {
			// Native I/O already acknowledged completion; explicit withdrawal may
			// close the fd afterward, but cannot undo those completed bytes.
			return nil
		}
		return clearErr
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

// Cancellation is checked before each finite write while native deadlines
// retain the physical completion boundary of an in-progress pipe/socket write.
type controlContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

// No helper goroutine can outlive this synchronous write or retain its buffer.
func (self controlContextWriter) Write(content []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	return self.writer.Write(content)
}
