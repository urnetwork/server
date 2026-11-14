// Pins the one fresh-read authority after an in-sweep disputed settlement fails.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The read is owned by one exact settlement phase. Missing/finalized/raced
// state, extra errors and late cancellation cannot donate partial authority.
func TestForceCloseDisputeRejectionVerificationBoundary(t *testing.T) {
	other := errors.New("synthetic verification database failure")
	verified := &forceCloseNonfinalError{disputed: true}
	cases := []struct {
		name          string
		settleErr     error
		verification  error
		cancelBefore  bool
		cancelInRead  bool
		wantRead      int
		wantAuthority bool
	}{
		{name: "fresh-disputed", settleErr: errContractInsufficientEscrow, verification: verified, wantRead: 1, wantAuthority: true},
		{name: "wrapped-exact-guard", settleErr: fmt.Errorf("synthetic context: %w", errContractInsufficientEscrow), verification: verified, wantRead: 1, wantAuthority: true},
		{name: "missing-row", settleErr: errContractInsufficientEscrow, verification: errors.New("synthetic row disappeared"), wantRead: 1},
		{name: "finalized-by-peer", settleErr: errContractInsufficientEscrow, wantRead: 1},
		{name: "no-longer-disputed", settleErr: errContractInsufficientEscrow, verification: &forceCloseNonfinalError{disputed: false}, wantRead: 1},
		{name: "read-failure", settleErr: errContractInsufficientEscrow, verification: other, wantRead: 1},
		{name: "read-canceled", settleErr: errContractInsufficientEscrow, verification: context.Canceled, wantRead: 1},
		{name: "read-deadline", settleErr: errContractInsufficientEscrow, verification: context.DeadlineExceeded, wantRead: 1},
		{name: "mixed-verifier", settleErr: errContractInsufficientEscrow, verification: errors.Join(verified, other), wantRead: 1},
		{name: "untyped-verifier", settleErr: errContractInsufficientEscrow, verification: errors.New(verified.Error()), wantRead: 1},
		{name: "mixed-settlement", settleErr: errors.Join(errContractInsufficientEscrow, other), verification: verified},
		{name: "untyped-guard", settleErr: errors.New(errContractInsufficientEscrow.Error()), verification: verified},
		{name: "ordinary-database-error", settleErr: other, verification: verified},
		{name: "already-canceled", settleErr: errContractInsufficientEscrow, verification: verified, cancelBefore: true},
		{name: "canceled-during-read", settleErr: errContractInsufficientEscrow, verification: verified, cancelInRead: true, wantRead: 1},
		{name: "healthy-no-extra-read"},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		if c.cancelBefore {
			cancel()
		}
		reads := 0
		err := finishForceCloseDisputeSettlement(ctx, c.settleErr, func() error {
			reads++
			if c.cancelInRead {
				cancel()
			}
			return c.verification
		})
		if reads != c.wantRead || isForceCloseAccountingRejection(nil, nil, err) != c.wantAuthority {
			t.Errorf("%s: read count or phase-owned authority changed", c.name)
		}
		if c.settleErr != nil && !errors.Is(err, c.settleErr) {
			t.Errorf("%s: original settlement error was hidden", c.name)
		}
		if c.wantRead != 0 && c.verification != nil && !errors.Is(err, c.verification) {
			t.Errorf("%s: independent verification error was hidden", c.name)
		}
		if (c.cancelBefore || c.cancelInRead) && !errors.Is(err, context.Canceled) {
			t.Errorf("%s: cancellation was lost", c.name)
		}
		if c.wantAuthority {
			if isForceCloseAccountingRejection(nil, nil, errors.Join(err, other)) ||
				isForceCloseAccountingRejection(other, nil, err) ||
				isForceCloseAccountingRejection(nil, other, err) {
				t.Errorf("%s: extra close/quarantine/cleanup failure gained authority", c.name)
			}
		}
		cancel()
	}
}
