package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestCompletedTransferBalanceRetentionSpoolCapacityRefusesBeforeDeletion(t *testing.T) {
	observer := &retentionOwnerTrace{}
	retentionOwnerFixture(t, 300, 20, observer, func(t testing.TB, fixtureCtx, ctx context.Context, network server.Id, spoolDir string) {
		recovered := server.HandleError(func() {
			removeCompletedTransferBalanceBatchesWithByteLimit(ctx, server.NowUtc().Add(-7*24*time.Hour), 256*int64(len(server.Id{})))
		})
		err, _ := recovered.(error)
		observer.mu.Lock()
		deletes, commits, overlapped := observer.deletes, observer.commits, observer.overlapped
		observer.mu.Unlock()
		if !errors.Is(err, errCompletedTransferBalanceSpoolCapacity) || deletes != 0 || commits != 0 || overlapped {
			t.Fatalf("spool capacity failure escaped into deletion: panic_type=%T deletes=%d commits=%d overlap=%t", recovered, deletes, commits, overlapped)
		}
		requireRetentionOwnerTraceClosed(t, observer, spoolDir)
		requireRetentionOwnerBalances(t, fixtureCtx, network, 320, 300)
	})
}
