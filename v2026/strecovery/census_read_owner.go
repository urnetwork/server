// Census observations preserve their original causes without displaying
// connection credentials. Only pure transient read failures admit continuation.
package strecovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// This wrapper keeps diagnostics bounded to independently selected source and
// stage names. Error inspection can still reach the original system/DB cause.
type CensusReadError struct {
	Source string
	Stage  string
	Cause  error
}

// Private credentials and database error detail never enter this projection.
func (self *CensusReadError) Error() string {
	return fmt.Sprintf("recovery census source %q %s failed", self.Source, self.Stage)
}

// Original cancellation, transport and database error identity remains intact.
func (self *CensusReadError) Unwrap() error { return self.Cause }

// One finite owner covers connection, snapshot counts, row scans and completion.
// Close has a separate joined cleanup owner and cannot make partial data usable.
func censusReadOwner(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, errors.New("census read owner context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	if timeout < 60*time.Second || timeout > 900*time.Second {
		return nil, nil, errors.New("census read owner must be within 60..900 seconds")
	}
	owner, cancel := context.WithTimeout(ctx, timeout)
	return owner, cancel, nil
}

// Every leaf must be retryable. Cancellation, bad credentials, malformed rows,
// input contradiction and mixed hard/soft errors never become transient reads.
func CensusReadUnavailable(err error) bool {
	if err == nil {
		return false
	}
	remaining := 64
	var visit func(error, int) bool
	visit = func(cause error, depth int) bool {
		remaining--
		if cause == nil || remaining < 0 || depth > 16 || cause == context.Canceled {
			return false
		}
		if joined, ok := cause.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := cause.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap(), depth+1)
		}
		if postgres, ok := cause.(*pgconn.PgError); ok {
			switch postgres.Code {
			case "08000", "08001", "08003", "08006", "08007", "40001", "40P01", "53300", "55P03", "57014", "57P01", "57P02", "57P03":
				return true
			default:
				return false
			}
		}
		if cause == context.DeadlineExceeded || cause == io.EOF || cause == io.ErrUnexpectedEOF || cause == os.ErrClosed {
			return true
		}
		if code, ok := cause.(syscall.Errno); ok {
			switch code {
			case syscall.EIO, syscall.EAGAIN, syscall.EINTR, syscall.EBUSY, syscall.ETIMEDOUT, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EPIPE:
				return true
			}
		}
		if network, ok := cause.(net.Error); ok {
			return network.Timeout() || network.Temporary()
		}
		return false
	}
	return visit(err, 0)
}
