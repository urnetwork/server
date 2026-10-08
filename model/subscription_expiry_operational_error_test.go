// Operational close failures preserve financial ownership without hiding causes.
package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Exercise the irreversible boundary, including wrappers and independent joins.
// A recognized operational cause must keep every other cause and cleanup error.
func TestForceCloseOperationalFailuresNeverQuarantine(t *testing.T) {
	causes := []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"db-context", server.DbContextDoneError},
		{"closed-connection", pgconn.ErrConnClosed},
		{"transaction-closed", pgx.ErrTxClosed},
		{"commit-rollback", pgx.ErrTxCommitRollback},
		{"eof", io.EOF},
		{"partial-reply", io.ErrUnexpectedEOF},
		{"network-closed", net.ErrClosed},
		{"network-operation", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("lost reply")}},
		{"dns", &net.DNSError{Err: "lookup unavailable", Name: "synthetic.invalid"}},
		{"syscall-wrapped", &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}},
	}
	config, err := pgconn.ParseConfig("postgres://synthetic@127.0.0.1/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("synthetic refused connection")
	}
	_, connectErr := pgconn.ConnectConfig(context.Background(), config)
	if _, ok := connectErr.(*pgconn.ConnectError); !ok {
		t.Fatalf("dial-only fixture did not produce the actual typed connect error: %T", connectErr)
	}
	causes = append(causes, struct {
		name string
		err  error
	}{"connect", connectErr})
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
		syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		causes = append(causes, struct {
			name string
			err  error
		}{fmt.Sprintf("errno-%d", errno), errno})
	}
	for _, code := range []string{"08000", "08006", "08007", "25P02", "25P03", "25P04", "40001", "40003", "40P01",
		"53000", "53100", "53200", "53300", "53400", "54000", "54001", "55000", "55P03",
		"57014", "57P01", "57P02", "57P03", "57P04", "57P05", "58000", "58030", "XX000", "XX001"} {
		causes = append(causes, struct {
			name string
			err  error
		}{code, &pgconn.PgError{Code: code, Message: "synthetic operational failure"}})
	}
	cleanupFailure := errors.New("independent terminal verification failed")
	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			for _, closeErr := range []error{tc.err, fmt.Errorf("continuation: %w", tc.err),
				errors.Join(errContractInsufficientEscrow, tc.err), errors.Join(tc.err, errContractAlreadySettled)} {
				quarantines, cleanups := 0, 0
				got := finishForceCloseContract(closeErr, func() error {
					quarantines++
					return nil
				}, func() error {
					cleanups++
					return cleanupFailure
				})
				if quarantines != 0 || cleanups != 1 || !errors.Is(got, closeErr) || !errors.Is(got, cleanupFailure) {
					t.Fatalf("operational cause changed ownership or lost phase errors: quarantines=%d cleanups=%d", quarantines, cleanups)
				}
				if isForceCloseAccountingRejection(closeErr, nil, &forceCloseNonfinalError{disputed: true}) ||
					isForceCloseQuarantinedAccountingRejection(closeErr, false, nil, cleanupFailure) {
					t.Fatal("operational failure gained accounting-only progress authority")
				}
			}
		})
	}
}

type forceCloseOperationalSpoof struct{}

func (*forceCloseOperationalSpoof) Error() string { return "context deadline exceeded" }
func (*forceCloseOperationalSpoof) Is(error) bool { return true }
func (*forceCloseOperationalSpoof) As(target any) bool {
	if pg, ok := target.(**pgconn.PgError); ok {
		*pg = &pgconn.PgError{Code: "53200"}
		return true
	}
	return false
}

// Keep malformed-data policy unchanged. Error text and custom Is/As methods
// cannot make a malformed error impersonate an operational failure.
func TestForceCloseMalformedFailuresRetainQuarantine(t *testing.T) {
	quarantineFailure := errors.New("independent quarantine failure")
	cleanupFailure := errors.New("independent cleanup failure")
	for _, cause := range []error{errContractInsufficientEscrow,
		&pgconn.PgError{Code: "P0001", Message: "synthetic malformed escrow"},
		&pgconn.PgError{Code: "22003", Message: "synthetic malformed data"},
		errors.New(context.DeadlineExceeded.Error()), &forceCloseOperationalSpoof{}} {
		quarantines, cleanups := 0, 0
		got := finishForceCloseContract(cause, func() error {
			quarantines++
			return quarantineFailure
		}, func() error {
			cleanups++
			return cleanupFailure
		})
		if quarantines != 1 || cleanups != 1 || !errors.Is(got, quarantineFailure) || !errors.Is(got, cleanupFailure) {
			t.Fatal("malformed fallback or independent error propagation changed")
		}
	}
}

type forceCloseCauseCycle struct{}

func (self *forceCloseCauseCycle) Error() string { return "synthetic cyclic cause" }
func (self *forceCloseCauseCycle) Unwrap() error { return self }

type forceCloseCauseBranches struct{ causes []error }

func (*forceCloseCauseBranches) Error() string        { return "synthetic cause branches" }
func (self *forceCloseCauseBranches) Unwrap() []error { return self.causes }

func forceCloseDeepCause(cause error, depth int) error {
	for range depth {
		cause = fmt.Errorf("synthetic wrapper: %w", cause)
	}
	return cause
}

// An unseen cause never donates permission for an irreversible fallback. A
// bounded scan must also protect later accounting classifiers from cycles.
func TestForceCloseIncompleteCauseGraphNeverQuarantines(t *testing.T) {
	wide := make([]error, 128)
	for index := range wide {
		wide[index] = errors.New("synthetic ordinary failure")
	}
	wide = append(wide, context.DeadlineExceeded)
	var typedNil *forceCloseCauseBranches
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deep-deadline", forceCloseDeepCause(context.DeadlineExceeded, 33)},
		{"deep-malformed", forceCloseDeepCause(&pgconn.PgError{Code: "P0001"}, 33)},
		{"wide-hidden-deadline", errors.Join(wide...)},
		{"cycle", &forceCloseCauseCycle{}},
		{"empty", &forceCloseCauseBranches{}},
		{"nil-branch", &forceCloseCauseBranches{causes: []error{errContractInsufficientEscrow, nil}}},
		{"typed-nil", typedNil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quarantines, cleanups := 0, 0
			cleanupFailure := errors.New("independent verification failure")
			got := finishForceCloseContract(tc.err, func() error {
				quarantines++
				return nil
			}, func() error {
				cleanups++
				return cleanupFailure
			})
			// errors.Is itself is unbounded on a cycle. Inspect the returned
			// root join's actual children to prove both phases were preserved.
			joined, ok := got.(interface{ Unwrap() []error })
			if !ok || len(joined.Unwrap()) != 2 || joined.Unwrap()[0] != tc.err || joined.Unwrap()[1] != cleanupFailure ||
				quarantines != 0 || cleanups != 1 {
				t.Fatal("incomplete cause graph changed custody or lost phase errors", quarantines, cleanups)
			}
			if isForceCloseAccountingRejection(tc.err, nil, &forceCloseNonfinalError{disputed: true}) ||
				isForceCloseQuarantinedAccountingRejection(tc.err, true, nil, nil) || isOnlyContractAlreadySettled(tc.err) {
				t.Fatal("incomplete graph gained terminal or accounting authority")
			}
		})
	}
}

func TestForceCloseSentinelAuthorityRequiresCompleteSingleCause(t *testing.T) {
	for _, expected := range []error{errContractAlreadySettled, errContractInsufficientEscrow, errTransferBalanceOwnershipBusy} {
		if !isOnlyContractError(expected, expected) || !isOnlyContractError(forceCloseDeepCause(expected, 32), expected) {
			t.Fatal("complete exact sentinel lost single-cause authority")
		}
		for _, cause := range []error{forceCloseDeepCause(expected, 33), errors.Join(expected),
			&forceCloseCauseCycle{}, &forceCloseCauseBranches{causes: []error{expected, nil}}, &forceCloseOperationalSpoof{}} {
			if isOnlyContractError(cause, expected) {
				t.Fatal("incomplete, multiple, or spoofed cause gained exact sentinel authority")
			}
		}
	}
}
