package model

// Deposit hotkey allocation (mmm/BTCPAY.md §18.2): every account's hotkey is
// derived at the next global derivation index, and an advisory lock
// serializes allocations so that two accounts never derive the same index.
// The lock was taken inside a repeatable read transaction: the statement that
// waits for the lock takes the transaction's snapshot when it starts, so an
// allocation that waited behind another read the index table as it was
// before the other committed, derived the index the other had just used, and
// failed on the table's unique indexes; only the transaction wrapper's rerun
// of a unique violation recovered it. The allocation reads the indexes taken
// only once it holds the lock.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// gammaAllocationTestWaitForLockWaiter waits until a session of this database
// waits for the deposit hotkey allocation lock: the advisory lock's bigint key
// is classid (high 32 bits) and objid (low 32 bits), objsubid 1.
func gammaAllocationTestWaitForLockWaiter(ctx context.Context) (returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("observe the allocation lock waiter: %v", recovered)
		}
	}()
	key := int64(gammaDepositHotkeyAllocationLock)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		waiting := false
		server.Db(ctx, func(conn server.PgConn) {
			returnErr = conn.QueryRow(
				ctx,
				`
					SELECT EXISTS (
						SELECT 1
						FROM pg_locks
						WHERE locktype = 'advisory'
						  AND NOT granted
						  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
						  AND classid::bigint = $1
						  AND objid::bigint = $2
						  AND objsubid = 1
					)
				`,
				key>>32,
				key&0xffffffff,
			).Scan(&waiting)
		})
		if returnErr != nil {
			return fmt.Errorf("observe the allocation lock waiter: %w", returnErr)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no allocation entered the allocation lock wait: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// gammaAllocationTestDerivations records the indexes an allocation derives.
type gammaAllocationTestDerivations struct {
	lock    sync.Mutex
	indexes []int64
}

func (self *gammaAllocationTestDerivations) add(index int64) int {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.indexes = append(self.indexes, index)
	return len(self.indexes)
}

func (self *gammaAllocationTestDerivations) list() []int64 {
	self.lock.Lock()
	defer self.lock.Unlock()
	return slices.Clone(self.indexes)
}

func gammaAllocationTestAddress(index int64) string {
	return fmt.Sprintf("synthetic-hotkey-%d", index)
}

// Two first allocations at once. The first holds the allocation lock while it
// derives index 0; the second starts then, and waits for the lock. Once the
// first commits, the second reads index 0 as taken and derives index 1, once:
// it never derives an index another allocation holds, and never meets the
// unique index.
func TestGammaDepositHotkeyAllocationAfterAWaitReadsTheIndexJustTaken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		firstNetworkId := server.NewId()
		secondNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, firstNetworkId, "synthetic-gamma-allocation-first", server.NewId())
		Testing_CreateNetwork(ctx, secondNetworkId, "synthetic-gamma-allocation-second", server.NewId())
		now := server.NowUtc()

		firstDerivations := &gammaAllocationTestDerivations{}
		firstDeriving := make(chan struct{})
		releaseFirst := make(chan struct{})
		var firstHotkey *GammaDepositHotkey
		firstDone := runPaymentModelTest(func() error {
			hotkey, err := AllocateGammaDepositHotkey(ctx, firstNetworkId, func(index int64) (string, error) {
				if firstDerivations.add(index) == 1 {
					// the first attempt holds the lock until the second waits
					close(firstDeriving)
					select {
					case <-releaseFirst:
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}
				return gammaAllocationTestAddress(index), nil
			}, now)
			firstHotkey = hotkey
			return err
		})
		select {
		case <-firstDeriving:
		case <-ctx.Done():
			close(releaseFirst)
			t.Fatalf("the first allocation never derived: %v", awaitPaymentModelTest(firstDone))
		}

		secondDerivations := &gammaAllocationTestDerivations{}
		var secondHotkey *GammaDepositHotkey
		secondDone := runPaymentModelTest(func() error {
			hotkey, err := AllocateGammaDepositHotkey(ctx, secondNetworkId, func(index int64) (string, error) {
				secondDerivations.add(index)
				return gammaAllocationTestAddress(index), nil
			}, now)
			secondHotkey = hotkey
			return err
		})

		waitErr := gammaAllocationTestWaitForLockWaiter(ctx)
		close(releaseFirst)
		firstErr := awaitPaymentModelTest(firstDone)
		secondErr := awaitPaymentModelTest(secondDone)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if firstErr != nil {
			t.Fatalf("the first allocation: %v", firstErr)
		}
		if secondErr != nil {
			t.Fatalf("the second allocation: %v", secondErr)
		}

		connect.AssertEqual(t, firstDerivations.list(), []int64{0})
		if derived := secondDerivations.list(); !slices.Equal(derived, []int64{1}) {
			t.Fatalf("the second allocation derived indexes %v; want [1]: it read the index table from before the first allocation committed, derived the index the first had just taken, and met the unique index", derived)
		}
		connect.AssertEqual(t, firstHotkey.DerivationIndex, int64(0))
		connect.AssertEqual(t, secondHotkey.DerivationIndex, int64(1))
		connect.AssertEqual(t, secondHotkey.Hotkey, gammaAllocationTestAddress(1))
		hotkeys := GetGammaDepositHotkeysByAddresses(ctx, []string{gammaAllocationTestAddress(0), gammaAllocationTestAddress(1)})
		connect.AssertEqual(t, len(hotkeys), 2)
		connect.AssertEqual(t, hotkeys[gammaAllocationTestAddress(0)].NetworkId, firstNetworkId)
		connect.AssertEqual(t, hotkeys[gammaAllocationTestAddress(1)].NetworkId, secondNetworkId)
	})
}

// A failed derivation writes nothing and leaves the index free for the next
// allocation.
func TestGammaDepositHotkeyAllocationFailedDerivationTakesNoIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gamma-allocation-failed", server.NewId())
		now := server.NowUtc()
		deriveErr := errors.New("synthetic derivation failure")
		hotkey, err := AllocateGammaDepositHotkey(ctx, networkId, func(index int64) (string, error) {
			return "", deriveErr
		}, now)
		connect.AssertEqual(t, errors.Is(err, deriveErr), true)
		connect.AssertEqual(t, hotkey == nil, true)
		connect.AssertEqual(t, len(GetGammaDepositHotkeys(ctx, networkId)), 0)

		hotkey, err = AllocateGammaDepositHotkey(ctx, networkId, func(index int64) (string, error) {
			return gammaAllocationTestAddress(index), nil
		}, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, hotkey.DerivationIndex, int64(0))
	})
}
