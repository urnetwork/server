// Operational failures leave billing ownership intact. They never establish
// malformed contract data or authorize expiry's terminal no-payout fallback.
package model

import (
	"context"
	"io"
	"net"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Irreversible fallback requires complete cause inspection. Incomplete graphs
// may hide an operational cause and retain ordinary failure without quarantine.
// Concrete causes, not text or custom Is/As, decide the operational exception.
// This neither retries a transaction nor declares it rolled back: lost replies
// include uncertain commits owned by durable state. Every cause is returned.
func forceCloseErrorAllowsQuarantine(err error) bool {
	causes := server.InspectErrorCauses(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	for _, cause := range causes.Nodes {
		switch cause.Err {
		case context.Canceled, context.DeadlineExceeded, server.DbContextDoneError,
			pgx.ErrTxClosed, pgx.ErrTxCommitRollback,
			pgconn.ErrConnClosed, io.EOF, io.ErrUnexpectedEOF, net.ErrClosed,
			syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
			syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return false
		}
		switch value := cause.Err.(type) {
		case *net.OpError:
			if value != nil {
				return false
			}
		case *net.DNSError:
			if value != nil {
				return false
			}
		case *pgconn.ConnectError:
			if value != nil {
				return false
			}
		case *pgconn.PgError:
			if value == nil {
				continue
			}
			// These SQLSTATE classes describe a connection, transaction,
			// resource/limit, prerequisite, operator, system, or internal
			// failure. None proves malformed escrow. Preserve the existing
			// P0001/application and data-exception fallback separately.
			if len(value.Code) == 5 {
				switch value.Code[:2] {
				case "08", "25", "40", "53", "54", "55", "57", "58", "XX":
					return false
				}
			}
		}
	}
	return true
}
