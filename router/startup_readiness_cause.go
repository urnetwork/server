// Startup read failures retain physical causes. A bounded error-tree inspection
// separates temporary dependency loss from configuration and schema refusals.
package router

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Database and Redis adapters raise their original errors through panic. Keep
// that error chain; an untyped panic remains an opaque, nonretryable failure.
func startupReadinessReadError(component string, recovered any) error {
	if err, ok := recovered.(error); ok {
		return fmt.Errorf("%s: %w", component, err)
	}
	return fmt.Errorf("%s: %v", component, recovered)
}

// Only complete trees of typed unavailable read causes can retry. Cancellation,
// local files, malformed responses and migration contradictions stay closed.
func RetryableStartupReadinessError(err error) (retryable bool) {
	defer func() {
		if recover() != nil {
			retryable = false
		}
	}()
	nodes := 0
	readCause := false
	var inspect func(error, int) bool
	inspect = func(cause error, depth int) bool {
		nodes++
		if cause == nil || depth > 32 || nodes > 256 {
			return false
		}
		if _, local := cause.(*os.PathError); local {
			return false
		}
		if cause == context.Canceled {
			return false
		}
		if cause == server.DbContextDoneError {
			// This legacy marker carries no retry authority by itself. The
			// complete tree still needs an actual unavailable read cause.
			return true
		}
		if cause == context.DeadlineExceeded || cause == io.EOF || cause == io.ErrUnexpectedEOF ||
			cause == net.ErrClosed || cause == syscall.ECONNREFUSED || cause == syscall.ECONNRESET ||
			cause == syscall.EPIPE || cause == syscall.ETIMEDOUT || cause == syscall.EHOSTUNREACH ||
			cause == syscall.ENETUNREACH || cause == syscall.EADDRNOTAVAIL || cause == pgconn.ErrConnClosed ||
			cause == redis.ErrPoolTimeout || cause == redis.ErrPoolExhausted {
			readCause = true
			return true
		}
		if database, ok := cause.(*pgconn.PgError); ok {
			switch database.Code {
			case "08000", "08001", "08003", "08006", "08007", "40001", "40P01", "53300", "57014", "57P01", "57P02", "57P03":
				readCause = true
				return true
			default:
				return false
			}
		}
		if joined, ok := cause.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !inspect(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := cause.(interface{ Unwrap() error }); ok {
			if child := wrapped.Unwrap(); child != nil {
				return inspect(child, depth+1)
			}
		}
		if response, ok := cause.(redis.Error); ok {
			code, _, _ := strings.Cut(response.Error(), " ")
			switch code {
			case "LOADING", "CLUSTERDOWN", "TRYAGAIN", "MASTERDOWN", "READONLY", "BUSY":
				readCause = true
				return true
			default:
				return false
			}
		}
		if network, ok := cause.(net.Error); ok {
			unavailable := network.Timeout() || network.Temporary()
			readCause = readCause || unavailable
			return unavailable
		}
		return false
	}
	return inspect(err, 0) && readCause
}
