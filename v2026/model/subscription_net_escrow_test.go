package model

// The redis net-escrow counter mirrors a postgres-durable reservation. The
// reservation is committed in the escrow tx; the mirror is updated afterwards
// in a post. If the mirror update is bound to the caller's request context, a
// client that disconnects in that window desyncs the counter permanently:
// downward on a lost create (over-reporting available balance, seen as a
// negative residue), upward on a lost settle (hiding balance, the
// "insufficient balance" lockup).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// TestNetEscrowMirrorSurvivesCallerCancel requires the mirror to match the
// committed reservation once the create call returns, even though the caller's
// context is cancelled the moment it does.
//
// Today the mirror update runs synchronously inside the create (RunPosts before
// return), so this holds trivially. It is a guard, not a regression test: making
// the mirror update asynchronous or binding it to the caller's context would
// reopen the window where postgres holds a reservation the counter never
// recorded, and the eventual settle then decrements past zero (a negative
// residue, which over-reports available balance).
func TestNetEscrowMirrorSurvivesCallerCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024)
		contractByteCount := ByteCount(4 * 1024 * 1024)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			UsdToNanoCents(10.00),
			"net-escrow-cancel",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		_, err = RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		// the caller goes away as soon as the request returns: the contract is
		// committed, the mirror update is still outstanding
		cancelCtx, cancel := context.WithCancel(ctx)
		transferEscrow, err := CreateTransferEscrow(
			cancelCtx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractByteCount,
		)
		cancel()
		connect.AssertEqual(t, err, nil)

		// the reservation is durable in postgres, so the mirror must reach the
		// same total. poll: the mirror update is asynchronous by design.
		var netEscrow ByteCount
		deadline := time.Now().Add(10 * time.Second)
		for {
			netEscrow = ByteCount(0)
			for _, balance := range transferEscrow.Balances {
				netEscrow += Testing_NetEscrowByteCount(ctx, balance.BalanceId)
			}
			if netEscrow == contractByteCount {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf(
					"net escrow mirror = %d, want %d: the mirror update was lost with the caller's context, so the settle will decrement a reservation the counter never recorded (negative residue)",
					netEscrow,
					contractByteCount,
				)
			}
			select {
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
}
