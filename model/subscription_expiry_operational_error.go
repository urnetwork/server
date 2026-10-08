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
	"github.com/urnetwork/server"
)

// Only concrete causes grant this exception; matching error text or a custom
// Is/As method cannot change the quarantine policy. A known operational cause
// in a join still prevents irreversible fallback, while every cause is returned
// to the caller. This neither retries a transaction nor declares it rolled back:
// lost transport replies also cover uncertain commits owned by durable state.
func isForceCloseOperationalError(err error) bool {
	for _, cause := range server.InspectErrorCauses(err).Nodes {
		switch cause.Err {
		case context.Canceled, context.DeadlineExceeded, server.DbContextDoneError,
			pgx.ErrTxClosed, pgx.ErrTxCommitRollback,
			pgconn.ErrConnClosed, io.EOF, io.ErrUnexpectedEOF, net.ErrClosed,
			syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
			syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return true
		}
		switch value := cause.Err.(type) {
		case *net.OpError:
			if value != nil {
				return true
			}
		case *net.DNSError:
			if value != nil {
				return true
			}
		case *pgconn.ConnectError:
			if value != nil {
				return true
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
					return true
				}
			}
		}
	}
	return false
}
