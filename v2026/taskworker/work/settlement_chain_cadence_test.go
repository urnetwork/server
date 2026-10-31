// The settlement chains' cadence and empty-pass decisions, without a database.
package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A page with more work continues at once and a pass that found work re-arms at
// the active delay. A chain backs off straight to the idle delay at its second
// consecutive empty pass; the first only counts.
func TestSettlementChainBacksOffStraightToIdleDelay(t *testing.T) {
	settings := DefaultSettlementChainSettings()
	if settings.ActiveDelay != 2*time.Second || settings.IdleDelay != 30*time.Second || settings.EmptyPassesBeforeBackOff != 2 {
		t.Fatal("settlement chain cadence changed", settings.ActiveDelay, settings.IdleDelay, settings.EmptyPassesBeforeBackOff)
	}
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	// A quiet spell from a pass that found work: the count saturates where the
	// chain backs off, and the first pass that finds work resets it.
	emptyPasses := 0
	for _, c := range []struct {
		name      string
		more      bool
		empty     bool
		wantCount int
		want      time.Duration
	}{
		{name: "work", wantCount: 0, want: 2 * time.Second},
		{name: "more", more: true, wantCount: 0, want: 0},
		{name: "first_empty", empty: true, wantCount: 1, want: 2 * time.Second},
		{name: "second_empty", empty: true, wantCount: 2, want: 30 * time.Second},
		{name: "backed_off_empty", empty: true, wantCount: 2, want: 30 * time.Second},
		{name: "resumed", wantCount: 0, want: 2 * time.Second},
		{name: "empty_again", empty: true, wantCount: 1, want: 2 * time.Second},
	} {
		emptyPasses = settings.nextEmptyPasses(emptyPasses, c.empty)
		got := settings.nextRunAt(now, c.more, settings.backedOff(emptyPasses)).Sub(now)
		if emptyPasses != c.wantCount || got != c.want {
			t.Errorf("%s: empty passes %d and next run after %s, want %d and %s", c.name, emptyPasses, got, c.wantCount, c.want)
		}
	}
	// Below one, the first empty pass backs off.
	settings.EmptyPassesBeforeBackOff = 0
	if count := settings.nextEmptyPasses(0, true); count != 1 || !settings.backedOff(count) || settings.backedOff(0) {
		t.Fatal("a non-positive threshold did not back off at the first empty pass", count)
	}
	// A backed-off debit chain renews its mark on every idle run, before the
	// mark lapses, so producers keep kicking it for as long as it is idle.
	settings = DefaultSettlementChainSettings()
	if settings.IdleDelay+settings.MaxTime >= settings.Wake.MarkTtl {
		t.Fatal("an idle mark can lapse between two idle runs", settings.IdleDelay, settings.MaxTime, settings.Wake.MarkTtl)
	}
}

// Only a traversal from the start of the partition that met no balance is
// empty. A resumed traversal that reached the end wraps at the active delay, and
// any visited balance, including a busy or failed one, keeps the chain active.
func TestTransferDebitFlushEmptyOnlyForAnEmptyPartition(t *testing.T) {
	cursor := server.NewId()
	result := func(last *server.Id, balances int, more bool, failed int, busy int) *FlushTransferDebitsResult {
		return &FlushTransferDebitsResult{TransferDebitFlushResult: model.TransferDebitFlushResult{
			LastBalanceId: last, Balances: balances, More: more, Failed: failed, Busy: busy}}
	}
	for _, c := range []struct {
		name   string
		args   *FlushTransferDebitsArgs
		result *FlushTransferDebitsResult
		want   bool
	}{
		{name: "empty", args: &FlushTransferDebitsArgs{Shard: 3}, result: result(nil, 0, false, 0, 0), want: true},
		{name: "empty_backed_off", args: &FlushTransferDebitsArgs{Shard: 3, EmptyPasses: 2}, result: result(nil, 0, false, 0, 0), want: true},
		{name: "resumed_end", args: &FlushTransferDebitsArgs{Shard: 3, AfterBalanceId: &cursor}, result: result(nil, 0, false, 0, 0)},
		{name: "visited", args: &FlushTransferDebitsArgs{Shard: 3}, result: result(nil, 2, false, 0, 0)},
		{name: "busy", args: &FlushTransferDebitsArgs{Shard: 3}, result: result(&cursor, 1, false, 0, 1)},
		{name: "failed", args: &FlushTransferDebitsArgs{Shard: 3}, result: result(&cursor, 1, false, 1, 0)},
		{name: "more", args: &FlushTransferDebitsArgs{Shard: 3}, result: result(&cursor, 64, true, 0, 0)},
	} {
		if got := transferDebitFlushEmpty(c.args, c.result); got != c.want {
			t.Errorf("%s: empty=%t, want %t", c.name, got, c.want)
		}
	}
}
