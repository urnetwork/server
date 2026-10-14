package controller

// st_ledger_v2_rebuild_withdraw_test.go — the operator's withdrawal of a
// rebuild request (VERSION2-FINAL.md §6.4 fixed inputs, §8.6): the driver
// treats a withdrawn epoch as if it had never been requested, and the
// withdrawal is refused without a pending request, for an epoch unknown to
// the deployment's pool, and when rows rebuilt for the request win a race.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

func ledgerV2EpochRecord(f *ledgerV2Fixture, epoch uint64) model.StLedgerV2EpochRows {
	return *model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, epoch, epoch)[epoch]
}

// Commits epoch 5's root, then records the operator's request to rebuild
// epoch 5 and earners that rows built again would list instead. It returns
// epoch 5's record before the request.
func ledgerV2RequestEpoch5Rebuild(t testing.TB, f *ledgerV2Fixture) model.StLedgerV2EpochRows {
	t.Helper()
	f.chain.emission[5], f.chain.emission[6] = 1000, 1000
	f.builder.earners[5], f.builder.earners[6] = [][32]byte{ledgerV2Coldkey(1)}, [][32]byte{ledgerV2Coldkey(1)}
	f.chain.head = f.chain.end(5) + 10
	requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
	recorded := ledgerV2EpochRecord(f, 5)
	if err := StLedgerV2RebuildEpoch(f.ctx, 5, "rebuilder@example"); err != nil {
		t.Fatal(err)
	}
	f.builder.earners[5] = [][32]byte{ledgerV2Coldkey(2)}
	return recorded
}

// Records rows for epochs in a deployment's pool, each with a pending rebuild
// request.
func ledgerV2RequestedRows(t testing.TB, f *ledgerV2Fixture, key model.StDeploymentKey, noId uint64, epochs ...uint64) {
	t.Helper()
	for _, epoch := range epochs {
		if err := model.RecordStLedgerV2EpochRows(f.ctx, key, noId, epoch, []string{startifact.LedgerObjectRef(fmt.Appendf(nil, "rows of epoch %d", epoch))}); err != nil {
			t.Fatal(err)
		}
		if requested, err := model.RequestStLedgerV2EpochRebuild(f.ctx, key, noId, epoch, "rebuilder@example"); !requested || err != nil {
			t.Fatalf("request %s pool %d epoch %d: %t %v", key, noId, epoch, requested, err)
		}
	}
}

func requireLedgerV2WithdrawRefused(t testing.TB, f *ledgerV2Fixture, epoch uint64, reason string) {
	t.Helper()
	requestedBy, err := StLedgerV2WithdrawRebuild(f.ctx, epoch, "withdrawer@example")
	if requestedBy != "" || !errors.Is(err, ErrStLedgerV2RebuildWithdrawRefused) || err.Error() != ErrStLedgerV2RebuildWithdrawRefused.Error()+": "+reason {
		t.Fatalf("withdraw epoch %d: %q %v, want refused: %s", epoch, requestedBy, err, reason)
	}
}

// §6.4 (fixed inputs), §8.6, SV-1: withdrawing epoch 5's rebuild request
// restores the epoch's record, and the driver treats the epoch as if it had
// never been requested: epoch 6's root, built while epoch 5's root is short of
// its quorum, lists epoch 5's recorded rows rather than rows built again, and
// the driver then finalizes it.
func TestStLedgerV2WithdrawRebuildLetsTheDriverFinalizeTheRecordedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		recorded := ledgerV2RequestEpoch5Rebuild(t, f)
		if requestedBy, err := StLedgerV2WithdrawRebuild(f.ctx, 5, "withdrawer@example"); requestedBy != "rebuilder@example" || err != nil {
			t.Fatalf("withdraw: %q %v", requestedBy, err)
		}
		if got := ledgerV2EpochRecord(f, 5); !reflect.DeepEqual(got, recorded) {
			t.Fatalf("withdrawn record %+v, recorded %+v", got, recorded)
		}

		f.chain.head = f.chain.end(6) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		input, build := f.builder.inputs[1], f.builder.builds[1]
		if len(input.Epochs) != 2 || input.Epochs[0].Rows == nil || !reflect.DeepEqual(*input.Epochs[0].Rows, recorded) || input.Epochs[1].Rows != nil {
			t.Fatalf("epoch 6 input %+v %+v", input.Epochs[0], input.Epochs[1])
		}
		if got := ledgerV2EpochRecord(f, 5); !reflect.DeepEqual(build.Epochs[0].ProviderChunks, recorded.ProviderChunks) || !reflect.DeepEqual(got, recorded) {
			t.Fatalf("epoch 5 listed as %+v, recorded as %+v", build.Epochs[0], got)
		}
		f.chain.head, f.chain.approvals = f.chain.pending.CommittedBlock+f.chain.challenge, 1
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		if len(f.chain.roots) != 1 || f.chain.roots[0].Epoch != 6 || f.chain.roots[0].LedgerHash != sha256.Sum256(build.Header) || f.chain.pending != nil {
			t.Fatalf("roots %+v pending %+v", f.chain.roots, f.chain.pending)
		}
	})
}

// §6.4 (fixed inputs), §8.6, SV-1: a withdrawal that lands while the driver
// builds the requested epoch's rows again wins: recording those rows is
// refused, so the step is retried, and the retry lists the epoch's recorded
// rows as if the request had never been made.
func TestStLedgerV2DriverDropsRowsRebuiltForAWithdrawnRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		recorded := ledgerV2RequestEpoch5Rebuild(t, f)
		f.builder.onBuild = func() {
			f.builder.onBuild = nil
			if _, err := StLedgerV2WithdrawRebuild(f.ctx, 5, "withdrawer@example"); err != nil {
				t.Errorf("withdraw during the build: %v", err)
			}
		}
		f.chain.head = f.chain.end(6) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusFailed, true, model.ErrStLedgerV2EpochRowsFixed.Error()+": epoch 5")
		if rebuilt := f.builder.builds[1]; f.builder.inputs[1].Epochs[0].Rows != nil || reflect.DeepEqual(rebuilt.Epochs[0].ProviderChunks, recorded.ProviderChunks) {
			t.Fatalf("epoch 5 was not built again: %+v", rebuilt.Epochs[0])
		}
		if got := ledgerV2EpochRecord(f, 5); !reflect.DeepEqual(got, recorded) || f.chain.pending.Epoch != 5 {
			t.Fatalf("epoch 5 record %+v, pending %+v", got, f.chain.pending)
		}

		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		input, build := f.builder.inputs[2], f.builder.builds[2]
		if input.Epochs[0].Rows == nil || !reflect.DeepEqual(*input.Epochs[0].Rows, recorded) || !reflect.DeepEqual(build.Epochs[0].ProviderChunks, recorded.ProviderChunks) || f.chain.pending.Epoch != 6 {
			t.Fatalf("epoch 5 input %+v listed as %+v, pending %+v", input.Epochs[0], build.Epochs[0], f.chain.pending)
		}
	})
}

// §8.6: a withdrawal is refused, changing nothing, for an epoch without a
// pending rebuild request: rows never requested, a null declaration never
// requested, and rows whose request was withdrawn already.
func TestStLedgerV2WithdrawRebuildRefusesAnEpochWithoutAPendingRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		if err := model.RecordStLedgerV2EpochRows(f.ctx, key, 1, 5, []string{}); err != nil {
			t.Fatal(err)
		}
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 6, "operator@example"); err != nil {
			t.Fatal(err)
		}
		ledgerV2RequestedRows(t, f, key, 1, 7)
		if _, err := StLedgerV2WithdrawRebuild(f.ctx, 7, "withdrawer@example"); err != nil {
			t.Fatal(err)
		}
		before := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 7)
		for _, epoch := range []uint64{5, 6, 7} {
			requireLedgerV2WithdrawRefused(t, f, epoch, fmt.Sprintf("epoch %d has no pending rebuild request", epoch))
		}
		if got := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 7); len(got) != 3 || !reflect.DeepEqual(got, before) {
			t.Fatalf("a refused withdrawal changed a record: %+v %+v %+v", got[5], got[6], got[7])
		}
	})
}

// §6.4 (fixed inputs), §8.6: rows rebuilt for a request end it, over the
// epoch's rows and over its null declaration alike, so a withdrawal after them
// is refused, finding no pending request, and the rebuilt rows stand. A
// declaration never ends a request: it is refused over requested rows and
// changes nothing over a requested declaration.
func TestStLedgerV2WithdrawRebuildRefusesRowsAlreadyRebuilt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		ledgerV2RequestedRows(t, f, key, 1, 5)
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 6, "operator@example"); err != nil {
			t.Fatal(err)
		}
		if err := StLedgerV2RebuildEpoch(f.ctx, 6, "rebuilder@example"); err != nil {
			t.Fatal(err)
		}
		requested := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 6)
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 5, "operator@example"); !errors.Is(err, model.ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("declaration over requested rows: %v", err)
		}
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 6, "operator@example"); err != nil {
			t.Fatal(err)
		}
		if got := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 6); !reflect.DeepEqual(got, requested) {
			t.Fatalf("a declaration changed a requested record: %+v %+v", got[5], got[6])
		}

		rebuilt := []string{startifact.LedgerObjectRef([]byte("rebuilt rows"))}
		for _, epoch := range []uint64{5, 6} {
			if err := model.RecordStLedgerV2EpochRows(f.ctx, key, 1, epoch, rebuilt); err != nil {
				t.Fatal(err)
			}
		}
		for _, epoch := range []uint64{5, 6} {
			requireLedgerV2WithdrawRefused(t, f, epoch, fmt.Sprintf("epoch %d has no pending rebuild request", epoch))
			if got := ledgerV2EpochRecord(f, epoch); !reflect.DeepEqual(got.ProviderChunks, rebuilt) || got.Null() || got.RebuildRequestedBy != "" {
				t.Fatalf("epoch %d record %+v", epoch, got)
			}
		}
	})
}

// §8.6: a withdrawal is refused without settlement v2, and for an epoch with
// nothing recorded in the configured deployment's pool: one recorded and
// requested only in another pool and another deployment, one recorded
// nowhere, and one recorded only in a deployment other than the configured
// one. Every request stays.
func TestStLedgerV2WithdrawRebuildRefusesAnUnknownEpochOrDeployment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key, other := f.cfg.DeploymentKey(), model.StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		ledgerV2RequestedRows(t, f, key, 1, 5)
		ledgerV2RequestedRows(t, f, key, 2, 6)
		ledgerV2RequestedRows(t, f, other, 1, 6)
		requests := func() []string {
			return []string{
				model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 5)[5].RebuildRequestedBy,
				model.GetStLedgerV2EpochRows(f.ctx, key, 2, 6, 6)[6].RebuildRequestedBy,
				model.GetStLedgerV2EpochRows(f.ctx, other, 1, 6, 6)[6].RebuildRequestedBy,
			}
		}

		for _, epoch := range []uint64{6, 7} {
			requireLedgerV2WithdrawRefused(t, f, epoch, fmt.Sprintf("epoch %d has no recorded provider rows in pool 1 of deployment %s", epoch, key))
		}
		unknown := *f.cfg
		unknown.ContractAddress = common.HexToAddress("0x2000000000000000000000000000000000000005")
		SetStConfig(&unknown)
		requireLedgerV2WithdrawRefused(t, f, 5, "epoch 5 has no recorded provider rows in pool 1 of deployment 945:0x2000000000000000000000000000000000000005")
		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(&off)
		if requestedBy, err := StLedgerV2WithdrawRebuild(f.ctx, 5, "withdrawer@example"); requestedBy != "" || !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("v2 off: %q %v", requestedBy, err)
		}
		if got := requests(); !reflect.DeepEqual(got, []string{"rebuilder@example", "rebuilder@example", "rebuilder@example"}) {
			t.Fatalf("requests %q", got)
		}
	})
}

// §8.6: like the request, a withdrawal must name who makes it; one that does
// not is refused and leaves the request.
func TestStLedgerV2WithdrawRebuildRequiresAWithdrawer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		ledgerV2RequestedRows(t, f, f.cfg.DeploymentKey(), 1, 5)
		if requestedBy, err := StLedgerV2WithdrawRebuild(f.ctx, 5, ""); requestedBy != "" || err == nil || err.Error() != "st ledger v2: a rebuild withdrawal must name who makes it" {
			t.Fatalf("anonymous withdrawal: %q %v", requestedBy, err)
		}
		if got := ledgerV2EpochRecord(f, 5); got.RebuildRequestedBy != "rebuilder@example" {
			t.Fatalf("epoch 5 record %+v", got)
		}
	})
}

// Runs fn on its own goroutine, its panic as its error.
func ledgerV2Go(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- fn()
	}()
	return done
}

// Waits until n sessions of the test database wait on a lock.
func ledgerV2AwaitLockWaits(t testing.TB, f *ledgerV2Fixture, n int) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting))
		})
		if waiting == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait on a lock, want %d", waiting, n)
		}
	}
}

// Races the withdrawal of epoch 5's pending rebuild request against recording
// rows rebuilt for it. A held lock on the epoch's record queues the first of
// the two, then the second, on PostgreSQL's row lock; the withdrawal reads the
// request before it queues. The lock is then released.
func ledgerV2RaceWithdrawal(t testing.TB, f *ledgerV2Fixture, rebuilt []string, rebuildFirst bool) (recordErr error, requestedBy string, withdrawErr error) {
	t.Helper()
	key := f.cfg.DeploymentKey()
	record := func() <-chan error {
		return ledgerV2Go(func() error {
			return model.RecordStLedgerV2EpochRows(f.ctx, key, 1, 5, rebuilt)
		})
	}
	withdraw := func() <-chan error {
		return ledgerV2Go(func() (err error) {
			requestedBy, err = StLedgerV2WithdrawRebuild(f.ctx, 5, "withdrawer@example")
			return err
		})
	}
	var recorded, withdrawn <-chan error
	server.Db(f.ctx, func(conn server.PgConn) {
		held, err := conn.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		server.RaisePgResult(held.Exec(f.ctx, `SELECT 1 FROM st_ledger_v2_epoch_rows WHERE deployment_key = $1 AND no_id = 1 AND epoch = 5 FOR UPDATE`, string(key)))
		if rebuildFirst {
			recorded = record()
			ledgerV2AwaitLockWaits(t, f, 1)
			withdrawn = withdraw()
		} else {
			withdrawn = withdraw()
			ledgerV2AwaitLockWaits(t, f, 1)
			recorded = record()
		}
		ledgerV2AwaitLockWaits(t, f, 2)
		server.Raise(held.Rollback(f.ctx))
	})
	// the withdrawal sets requestedBy before it finishes
	recordErr, withdrawErr = <-recorded, <-withdrawn
	return recordErr, requestedBy, withdrawErr
}

// §6.4 (fixed inputs), §8.6: a withdrawal races rows rebuilt for its request,
// and the rebuilt rows are recorded first, ending the request after the
// withdrawal read it: the withdrawal's compare-and-set finds the request gone
// and it is refused, and the rebuilt rows stand.
func TestStLedgerV2WithdrawRebuildLosesARaceToARebuild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		ledgerV2RequestedRows(t, f, f.cfg.DeploymentKey(), 1, 5)
		rebuilt := []string{startifact.LedgerObjectRef([]byte("rebuilt rows"))}
		recordErr, requestedBy, withdrawErr := ledgerV2RaceWithdrawal(t, f, rebuilt, true)
		if recordErr != nil {
			t.Fatalf("rebuilt rows: %v", recordErr)
		}
		reason := ErrStLedgerV2RebuildWithdrawRefused.Error() + ": epoch 5's rows were rebuilt, or its request changed, while it was being withdrawn"
		if requestedBy != "" || !errors.Is(withdrawErr, ErrStLedgerV2RebuildWithdrawRefused) || withdrawErr.Error() != reason {
			t.Fatalf("withdrawal: %q %v", requestedBy, withdrawErr)
		}
		if got := ledgerV2EpochRecord(f, 5); !reflect.DeepEqual(got.ProviderChunks, rebuilt) || got.RebuildRequestedBy != "" {
			t.Fatalf("epoch 5 record %+v", got)
		}
	})
}

// §6.4 (fixed inputs), §8.6: the same race with the withdrawal first: it
// withdraws the request, so the rebuilt rows are refused, and the epoch's rows
// stand as recorded before the request.
func TestStLedgerV2WithdrawRebuildWinsARaceWithARebuild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		ledgerV2RequestedRows(t, f, key, 1, 5)
		recorded := ledgerV2EpochRecord(f, 5)
		recorded.RebuildRequestedBy = ""
		recordErr, requestedBy, withdrawErr := ledgerV2RaceWithdrawal(t, f, []string{startifact.LedgerObjectRef([]byte("rebuilt rows"))}, false)
		if requestedBy != "rebuilder@example" || withdrawErr != nil {
			t.Fatalf("withdrawal: %q %v", requestedBy, withdrawErr)
		}
		if !errors.Is(recordErr, model.ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("rebuilt rows: %v", recordErr)
		}
		if got := ledgerV2EpochRecord(f, 5); !reflect.DeepEqual(got, recorded) {
			t.Fatalf("epoch 5 record %+v, recorded %+v", got, recorded)
		}
	})
}
