// The startup reset keeps a fresh or recent pass and replaces a retained one;
// recovery keeps its ordinary RunOnce coalescing.
package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Each case is one retained args shape at a fixed instant.
func TestCloseExpiredContractsFreshPassArgsReplacesOnlyRetainedPasses(t *testing.T) {
	now := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	sweep := func(epoch time.Time) string {
		position := &model.ContractExpiryPosition{CreateTime: epoch.Add(-time.Hour), ContractId: server.NewId()}
		args, err := json.Marshal(&CloseExpiredContractsArgs{BlockSize: 1, Sweep: &model.ContractExpirySweepCursor{
			RecentAfter: epoch, HistoricalNext: true,
			Historical: &model.ContractExpiryCursor{ScanBefore: epoch, Open: position, DisputeDone: true},
		}})
		server.Raise(err)
		return string(args)
	}
	legacy := func(scanBefore time.Time) string {
		args, err := json.Marshal(&CloseExpiredContractsArgs{BlockSize: 1, Cursor: &model.ContractExpiryCursor{ScanBefore: scanBefore, DisputeDone: true}})
		server.Raise(err)
		return string(args)
	}
	freshWindowOnly, err := json.Marshal(&CloseExpiredContractsArgs{BlockSize: 1, Sweep: &model.ContractExpirySweepCursor{
		Fresh: &model.ContractExpiryCursor{ScanBefore: now.Add(-12 * time.Minute), DisputeDone: true},
	}})
	server.Raise(err)
	cases := []struct {
		name    string
		args    string
		replace bool
	}{
		{name: "fresh pass", args: `{"block_size":1,"block_index":0}`, replace: false},
		{name: "retained nine-day pass", args: sweep(now.Add(-9 * 24 * time.Hour)), replace: true},
		{name: "pass at the fence", args: sweep(now.Add(-closeExpiredContractsFreshPassFence)), replace: true},
		{name: "pass a sibling start began", args: sweep(now.Add(-5 * time.Minute)), replace: false},
		{name: "pass that visited only its fresh window", args: string(freshWindowOnly), replace: false},
		{name: "epoch after now", args: sweep(now.Add(time.Minute)), replace: false},
		{name: "retained legacy cursor", args: legacy(now.Add(-2 * time.Hour)), replace: true},
		{name: "recent legacy cursor", args: legacy(now.Add(-time.Minute)), replace: false},
		{name: "unreadable args", args: `{`, replace: true},
	}
	for _, c := range cases {
		replacement, replace := closeExpiredContractsFreshPassArgs(c.args, 0, now)
		if replace != c.replace {
			t.Errorf("%s: replace=%t, want %t", c.name, replace, c.replace)
		}
		if replace && replacement != `{"block_size":1,"block_index":0}` {
			t.Errorf("%s: replacement %s is not a fresh pass", c.name, replacement)
		}
	}
}

// An explicit recovery kick on an unleased retained row coalesces as before:
// the args and claim stay, only the wake generation moves.
func TestQueueContractExpiryRecoveryKeepsRetainedSweepArgs(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		epoch := server.NowUtc().Add(-9 * 24 * time.Hour)
		cursor := &model.ContractExpiryCursor{ScanBefore: epoch, DisputeDone: true,
			Open: &model.ContractExpiryPosition{CreateTime: epoch.Add(-time.Hour), ContractId: server.NewId()}}
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleCloseExpiredContractsPage(owner, tx, 0, false, cursor,
				&model.ContractExpirySweepCursor{Historical: cursor, RecentAfter: epoch, HistoricalNext: true})
		})
		key := task.RunOnce("close_expired_contracts_1_0").String()
		before := readExpiryRecoveryQueue(t, ctx)[key]
		if _, err := QueueContractExpiryRecovery(ctx); err != nil {
			t.Fatal(err)
		}
		after := readExpiryRecoveryQueue(t, ctx)[key]
		if after.id != before.id || after.args != before.args || after.claim != before.claim || after.generation != before.generation+1 {
			t.Fatal("recovery replaced the retained sweep instead of coalescing", after.args)
		}
	})
}
