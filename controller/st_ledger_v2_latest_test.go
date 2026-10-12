package controller

// st_ledger_v2_latest_test.go — a send's eth_call and eth_estimateGas run at
// the latest block and the transaction executes in a later one (the R
// battery), so the v2 driver sends a close or commit only while the block
// after the latest is inside its window (VERSION2-FINAL.md §8.1).

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func requireLedgerV2RetryAt(t testing.TB, outcome *StPublishOutcome, blocks uint64) {
	t.Helper()
	want := server.NowUtc().Add(time.Duration(blocks) * 12 * time.Second)
	if outcome.RetryAt.Before(want.Add(-time.Minute)) || want.Add(time.Minute).Before(outcome.RetryAt) {
		t.Fatalf("retry at %s, want about %s", outcome.RetryAt, want)
	}
}

// SV-1 (latest block): the fake, like the chain, refuses a close executed
// after its window. A close whose window ends at or before the latest block
// is held until the finalized head passes the window, and is then missed; a
// close whose window ends one block later is sent, and so is the commit, both
// executing in the window's last block.
func TestStLedgerV2DriverHoldsACloseAtTheLatestBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a := ledgerV2Coldkey(1)
		f.chain.emission[5], f.chain.emission[6] = 1000, 1000
		f.builder.earners[5], f.builder.earners[6] = [][32]byte{a}, [][32]byte{a}
		windowEnd := f.chain.end(5) + f.chain.window

		f.chain.head, f.chain.latest = windowEnd-3, windowEnd
		if _, err := f.chain.LedgerV2Close(f.ctx, 5, 1); err == nil || err.Error() != "InvalidWindow" {
			t.Fatalf("a close executed after its window: %v", err)
		}
		f.takeLog()
		for _, latest := range []uint64{windowEnd, windowEnd + 2} {
			f.chain.latest = latest
			outcome := f.run(StLedgerV2StepEpoch, 5, 0, 0)
			requireLedgerV2Outcome(t, outcome, model.StPublishStatusSkipped, true, fmt.Sprintf("the close window ends at block %d and the latest block is %d", windowEnd, latest))
			requireLedgerV2RetryAt(t, outcome, 4)
		}
		if log, publishes := f.takeLog(), f.publishes(5); len(log) != 0 || len(publishes) != 0 || f.chain.lastFunded != 4 {
			t.Fatalf("a held close sent %q or recorded %v", log, publishes)
		}
		f.chain.head = windowEnd + 1
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusMissed, false, fmt.Sprintf("close window closed at block %d", windowEnd))
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertMissedClose + " epoch 5"}) || len(f.takeLog()) != 0 {
			t.Fatalf("alerts %v", *f.alerts)
		}

		windowEnd = f.chain.end(6) + f.chain.window
		f.chain.head, f.chain.latest = windowEnd-3, windowEnd-1
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		if log := f.takeLog(); !slices.Contains(log, "close 6") || log[len(log)-1] != "commit 6 prev 0" || f.chain.lastFunded != 6 || f.chain.pending == nil || f.chain.pending.Epoch != 6 {
			t.Fatalf("calls %q pending %+v", log, f.chain.pending)
		}
	})
}

// SV-1 (latest block): a commit whose window ends at the latest block when the
// build is done is held, unsent, until the finalized head passes the window,
// and is then missed.
func TestStLedgerV2DriverHoldsACommitAtTheLatestBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		f.chain.emission[5] = 1000
		f.builder.earners[5] = [][32]byte{ledgerV2Coldkey(1)}
		windowEnd := f.chain.end(5) + f.chain.window
		f.chain.head = f.chain.end(5) + 10
		f.builder.onBuild = func() { f.chain.latest = windowEnd }
		outcome := f.run(StLedgerV2StepEpoch, 5, 0, 0)
		requireLedgerV2Outcome(t, outcome, model.StPublishStatusSkipped, true, fmt.Sprintf("the commit window ends at block %d and the latest block is %d", windowEnd, windowEnd))
		requireLedgerV2RetryAt(t, outcome, f.chain.window-9)
		log := f.takeLog()
		if !slices.Contains(log, "close 5") || slices.ContainsFunc(log, func(call string) bool { return strings.HasPrefix(call, "commit") }) || f.chain.pending != nil {
			t.Fatalf("calls %q pending %+v", log, f.chain.pending)
		}
		if got := f.publishes(5); !reflect.DeepEqual(got, []string{"v2close confirmed"}) {
			t.Fatalf("publishes %v", got)
		}
		f.builder.onBuild = nil
		f.chain.head = windowEnd + 1
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusMissed, false, fmt.Sprintf("commit window closed at block %d", windowEnd))
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertMissedCommitWindow + " epoch 5"}) || f.chain.pending != nil {
			t.Fatalf("alerts %v pending %+v", *f.alerts, f.chain.pending)
		}
	})
}

// SV-1 (latest block): a failed read of the latest block is retried before the
// close and before the commit, and neither is sent until it succeeds.
func TestStLedgerV2DriverRetriesTheLatestBlockRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		f.chain.emission[5] = 1000
		f.builder.earners[5] = [][32]byte{ledgerV2Coldkey(1)}
		f.chain.head = f.chain.end(5) + 10
		f.chain.failNext("latest", errors.New("latest unreadable"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "latest unreadable")
		if log := f.takeLog(); len(log) != 0 {
			t.Fatalf("calls %q", log)
		}
		f.chain.fail["latest"] = []error{nil, errors.New("latest unreadable at the commit")}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "latest unreadable at the commit")
		if log := f.takeLog(); !slices.Contains(log, "close 5") || slices.ContainsFunc(log, func(call string) bool { return strings.HasPrefix(call, "commit") }) {
			t.Fatalf("calls %q", log)
		}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
		if log := f.takeLog(); log[len(log)-1] != "commit 5 prev 0" || f.chain.pending == nil {
			t.Fatalf("calls %q", log)
		}
	})
}

// SV-1 (latest block): the client reads the latest block's number, which runs
// ahead of the finalized head; an endpoint that fails the read, a canceled
// read and a client without an endpoint fail.
func TestStLedgerV2ClientReadsTheLatestBlock(t *testing.T) {
	ctx := context.Background()
	fixture := newLedgerV2RPC(t)
	client, _, _ := newLedgerV2RPCClient(t, fixture)
	fixture.latestBlock = fixture.finalizedBlock + 3
	if latest, err := client.LedgerV2LatestBlock(ctx); err != nil || latest != 103 {
		t.Fatalf("latest %d %v", latest, err)
	}
	fixture.latestUnavailable = true
	if latest, err := client.LedgerV2LatestBlock(ctx); latest != 0 || err == nil || !strings.HasPrefix(err.Error(), "latest head: ") || !strings.Contains(err.Error(), "latest block unavailable") {
		t.Fatalf("a failed read: %d %v", latest, err)
	}
	fixture.latestUnavailable = false
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.LedgerV2LatestBlock(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	offline := &CoreStClient{cfg: ledgerV2TestConfig()}
	if _, err := offline.LedgerV2LatestBlock(ctx); err == nil || err.Error() != "latest head: st: read endpoint or operation is absent" {
		t.Fatalf("read without an endpoint: %v", err)
	}
}
