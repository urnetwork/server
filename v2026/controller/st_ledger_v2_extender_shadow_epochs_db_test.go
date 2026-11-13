package controller

// st_ledger_v2_extender_shadow_epochs_db_test.go — which epochs run the SN25
// extender pool's live shadow, with the real database (sn WHITEPAPER.md
// §8.6.7): the two before E_x and no other, by the amendments the server holds,
// whether configured, stored from the published file, or published since.
//
// The defect (server CPUOPT5.md, top item #1): a live shadow was planned for
// every epoch with a captured census and rows recorded without an extender
// component, so epoch 4's step re-read the epoch's contracts on Main's primary
// every 1–2 hours and never finished, and epoch 5 would have joined it.

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/urfoundation/sn/v2026/amendment"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A shadow run's chain that counts its reads.
type stExtenderShadowCountingChain struct {
	stExtenderShadowTestChain
	reads atomic.Int64
}

func (self *stExtenderShadowCountingChain) LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error) {
	self.reads.Add(1)
	return self.stExtenderShadowTestChain.LedgerV2State(ctx, noId)
}

func (self *stExtenderShadowCountingChain) LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error) {
	self.reads.Add(1)
	return self.stExtenderShadowTestChain.LedgerV2FundedThrough(ctx, noId, epoch)
}

// Records each epoch as its census capture and live root leave it before a
// shadow: an empty census, and rows without an extender component.
func stExtenderShadowRecordEpochs(t testing.TB, f *ledgerV2BuildFixture, epochs ...uint64) {
	t.Helper()
	for _, epoch := range epochs {
		model.CaptureStLedgerV2ExtenderCensus(f.ctx, f.cfg.DeploymentKey(), f.cfg.NoId, epoch, model.EncodeStExtenderCensus(nil), 0, server.NowUtc())
		if err := model.RecordStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), f.cfg.NoId, epoch, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// Each epoch's planned shadow, by epoch.
func stExtenderShadowPlanned(f *ledgerV2BuildFixture, epochs ...uint64) map[uint64]bool {
	planned := map[uint64]bool{}
	for _, epoch := range epochs {
		planned[epoch] = stLedgerV2ExtenderShadowDue(f.ctx, f.cfg, epoch)
	}
	return planned
}

// With E_x = 8, of epochs 4 to 7, each with its census captured and its rows
// recorded without an extender component, only 6 and 7 are planned. A step for
// 4 or 5, as an earlier binary queued for epoch 4, concludes with no retry and
// no report, before it reads the chain; one for 6 goes on, to wait for the
// epoch's close.
func TestStLedgerV2ExtenderShadowPlansOnlyTheTwoEpochsBeforeTheFirstEpoch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		stExtenderShadowConcluded.Clear()
		t.Cleanup(stExtenderShadowConcluded.Clear)
		line, err := amendment.NewExtenders(f.cfg.ChainId, uint16(f.cfg.Netuid), f.cfg.NoId, 8).Line()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.cfg.Amendments.Encode([]string{line}); err != nil {
			t.Fatal(err)
		}
		stExtenderShadowRecordEpochs(t, f, 4, 5, 6, 7)
		if planned := stExtenderShadowPlanned(f, 4, 5, 6, 7); planned[4] || planned[5] || !planned[6] || !planned[7] {
			t.Fatalf("planned %v, want epochs 6 and 7 only", planned)
		}
		chain := &stExtenderShadowCountingChain{stExtenderShadowTestChain: stExtenderShadowTestChain{
			state: StLedgerV2State{Registered: true, FirstEpoch: 1, LastFundedEpoch: 5}, funded: map[uint64]uint256.Int{}}}
		for _, epoch := range []uint64{4, 5} {
			outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, epoch, DefaultStLedgerV2ExtenderShadowSettings())
			if outcome.Status != model.StPublishStatusSkipped || outcome.Retry || !strings.Contains(outcome.Reason, "first epoch 8") {
				t.Fatalf("epoch %d: %+v", epoch, outcome)
			}
			if model.GetStLedgerV2ExtenderShadow(f.ctx, f.cfg.DeploymentKey(), f.cfg.NoId, epoch) != nil {
				t.Fatalf("epoch %d's shadow was recorded", epoch)
			}
		}
		if reads := chain.reads.Load(); reads != 0 {
			t.Fatalf("the steps of epochs 4 and 5 read the chain %d times", reads)
		}
		if outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 6, DefaultStLedgerV2ExtenderShadowSettings()); !outcome.Retry || !strings.Contains(outcome.Reason, "not closed yet") {
			t.Fatalf("epoch 6: %+v", outcome)
		}
		if planned := stExtenderShadowPlanned(f, 4, 5, 6, 7); planned[4] || planned[5] || !planned[6] || !planned[7] {
			t.Fatalf("planned after the steps %v", planned)
		}
	})
}

// Before an entry names E_x, the deployment's plan says which epochs run in
// shadow. A published entry that moves the start to epoch 9 ends a step the
// plan admitted for epoch 6 once the step reads the published file, before the
// epoch's records or the chain; that read stores the file, and the sync, which
// reads only the stored copy, then plans 7 and 8. The sync never fetches.
func TestStLedgerV2ExtenderShadowFollowsTheEntryThatFixesTheFirstEpoch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		stExtenderShadowConcluded.Clear()
		t.Cleanup(stExtenderShadowConcluded.Clear)
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		var served atomic.Value
		served.Store("sn: 7\n")
		var fetches atomic.Int64
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fetches.Add(1)
			w.Write([]byte(served.Load().(string)))
		}))
		t.Cleanup(site.Close)
		t.Cleanup(Testing_SetStAmendments(site.URL+"/amendments.yml", crypto.PubkeyToAddress(key.PublicKey), f.cfg.ChainId, uint16(f.cfg.Netuid)))
		previousSink, previousPlan := stLedgerV2AlertSink, stExtenderPoolPlannedFirstEpoch
		alerts := []string{}
		stLedgerV2AlertSink = func(name, subject, message string) { alerts = append(alerts, name+": "+message) }
		stExtenderPoolPlannedFirstEpoch = func(chainId uint64, netuid uint64, noId uint64) (uint64, bool) {
			return 8, chainId == f.cfg.ChainId && netuid == f.cfg.Netuid && noId == f.cfg.NoId
		}
		t.Cleanup(func() {
			stLedgerV2AlertSink, stExtenderPoolPlannedFirstEpoch = previousSink, previousPlan
			stLedgerV2Alerted.Clear()
		})
		stExtenderShadowRecordEpochs(t, f, 5, 6, 7, 8)
		if planned := stExtenderShadowPlanned(f, 5, 6, 7, 8); planned[5] || !planned[6] || !planned[7] || planned[8] {
			t.Fatalf("as planned %v, want epochs 6 and 7", planned)
		}
		entry := amendment.NewExtenders(f.cfg.ChainId, uint16(f.cfg.Netuid), f.cfg.NoId, 9)
		if err := entry.Sign(key); err != nil {
			t.Fatal(err)
		}
		line, err := entry.Line()
		if err != nil {
			t.Fatal(err)
		}
		served.Store("sn: 7\nsigned_amendments:\n  - line: \"" + line + "\"\n    signature: \"0x" + hex.EncodeToString(entry.Signature) + "\"\n")
		chain := &stExtenderShadowCountingChain{stExtenderShadowTestChain: stExtenderShadowTestChain{
			state: StLedgerV2State{Registered: true, FirstEpoch: 1, LastFundedEpoch: 8}, funded: map[uint64]uint256.Int{}}}
		outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 6, DefaultStLedgerV2ExtenderShadowSettings())
		if outcome.Status != model.StPublishStatusSkipped || outcome.Retry || !strings.Contains(outcome.Reason, "first epoch 9 (as the amendments name it)") {
			t.Fatalf("epoch 6 after the start moved: %+v", outcome)
		}
		if reads := chain.reads.Load(); reads != 0 || fetches.Load() != 1 || len(alerts) != 0 {
			t.Fatalf("%d chain reads, %d fetches, alerts %v", reads, fetches.Load(), alerts)
		}
		if len(model.GetStAmendments(f.ctx, f.cfg.DeploymentKey())) == 0 {
			t.Fatal("the published file was not stored")
		}
		stExtenderShadowConcluded.Clear()
		if planned := stExtenderShadowPlanned(f, 5, 6, 7, 8); planned[5] || planned[6] || !planned[7] || !planned[8] {
			t.Fatalf("from the stored entry %v, want epochs 7 and 8", planned)
		}
		if fetches.Load() != 1 {
			t.Fatalf("the sync fetched the published file: %d fetches", fetches.Load())
		}
	})
}
