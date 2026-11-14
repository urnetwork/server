package controller

// st_ledger_v2_extender_shadow_epochs_test.go — which epochs run the SN25
// extender pool's live shadow (sn WHITEPAPER.md §8.6.7): the two before E_x,
// epochs 6 and 7 for mainnet's E_x = 8, and no other. Pure: the amendments are
// served and stored through the seams of st_ledger_v2_amendments_test.go, and
// every read of PostgreSQL panics.
//
// The defect (server CPUOPT5.md, top item #1): a live shadow was planned for
// every epoch with a captured census, so epoch 4's step, which re-reads the
// epoch's 7 days of contracts in one snapshot on Main's primary, ran from
// 2026-11-12 20:30Z for 30 minutes every 1–2 hours and never finished, and
// epoch 5 would have joined it at its boundary.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The chain of a shadow step that must not read it.
type stExtenderShadowTripwireChain struct {
	calls atomic.Int64
}

func (self *stExtenderShadowTripwireChain) LedgerV2State(context.Context, uint64) (*StLedgerV2State, error) {
	self.calls.Add(1)
	return &StLedgerV2State{}, nil
}

func (self *stExtenderShadowTripwireChain) LedgerV2FundedThrough(context.Context, uint64, uint64) (uint256.Int, error) {
	self.calls.Add(1)
	return uint256.Int{}, nil
}

// Forgets the conclusions of this process's shadow steps, now and after the
// test.
func clearStExtenderShadowConcluded(t *testing.T) {
	t.Helper()
	stExtenderShadowConcluded.Clear()
	t.Cleanup(stExtenderShadowConcluded.Clear)
}

// The epochs of 1 to 12 that run live in shadow by the amendments cfg holds.
func stExtenderShadowLiveEpochsOf(t *testing.T, cfg *StConfig) string {
	t.Helper()
	file, fallback, err := stLedgerV2HeldAmendments(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	live := []string{}
	for epoch := uint64(1); epoch <= 12; epoch++ {
		if stExtenderShadowNotLive(cfg, file, fallback, epoch) == "" {
			live = append(live, fmt.Sprint(epoch))
		}
	}
	return strings.Join(live, ",")
}

// The step an earlier binary queued for epoch 4 ends at once on mainnet, and so
// would one for epoch 5 or any earlier epoch: before E_x = 8 only epochs 6 and
// 7 run live in shadow. Each concludes, with no retry, before it reads
// PostgreSQL, a chain view or the published amendments, and the sync plans
// none of them.
func TestStLedgerV2ExtenderShadowEpochsBeforeTheShadowRunsConcludeWithoutReading(t *testing.T) {
	site := newStAmendmentTestSite(t)
	// the published file names no entry yet; the entry is signed during epoch 7
	site.publish(t)
	cfg := stAmendmentTestConfig(t, "enabled: false\n")
	clearStExtenderShadowConcluded(t)
	chain := &stExtenderShadowTripwireChain{}
	ctx := server.WithoutPostgres(context.Background())
	for _, epoch := range []uint64{4, 5, 1, 3} {
		outcome := func() *StPublishOutcome {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("epoch %d's shadow step read before it concluded: %v", epoch, r)
				}
			}()
			return stLedgerV2RunExtenderShadow(ctx, cfg, chain, epoch, DefaultStLedgerV2ExtenderShadowSettings())
		}()
		if outcome.Status != model.StPublishStatusSkipped || outcome.Retry || !strings.Contains(outcome.Reason, "first epoch 8") {
			t.Fatalf("epoch %d: %+v", epoch, outcome)
		}
		if stLedgerV2ExtenderShadowDue(ctx, cfg, epoch) {
			t.Fatalf("epoch %d's shadow is planned", epoch)
		}
	}
	if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
		t.Fatalf("%d PostgreSQL acquisitions", attempts)
	}
	if calls := chain.calls.Load(); calls != 0 {
		t.Fatalf("%d chain reads", calls)
	}
	if fetches := site.fetches.Load(); fetches != 0 {
		t.Fatalf("%d fetches of the published amendments", fetches)
	}
	if len(site.alerts) != 0 {
		t.Fatalf("alerts %v", site.alerts)
	}
}

// E_x, and so the two epochs before it, is the first epoch an extender pool
// entry puts the amendment in force, signed or configured, and before any is
// held, the deployment's plan: 8 on mainnet (chain 964, sn 25, no 1). A
// signed entry that moves the start moves the shadow runs with it; a configured
// line stands in before the first signed entry, as amendment.File.At answers;
// a stored copy that does not verify is none. A deployment with neither an
// entry nor a plan has no live shadow.
func TestStExtenderShadowRunsAreTheTwoEpochsBeforeTheFirstEpoch(t *testing.T) {
	if firstEpoch, planned := stExtenderPoolPlannedFirstEpoch(964, 25, 1); !planned || firstEpoch != 8 {
		t.Fatalf("mainnet's plan: %d, %v", firstEpoch, planned)
	}
	for _, other := range [][3]uint64{{964, 25, 2}, {964, 7, 1}, {945, 25, 1}, {945, 350, 1}} {
		if _, planned := stExtenderPoolPlannedFirstEpoch(other[0], other[1], other[2]); planned {
			t.Fatalf("a plan for chain %d sn %d no %d", other[0], other[1], other[2])
		}
	}
	site := newStAmendmentTestSite(t)
	mainnet := stAmendmentTestConfig(t, "enabled: false\n")
	if got := stExtenderShadowLiveEpochsOf(t, mainnet); got != "6,7" {
		t.Fatalf("mainnet as planned: %s", got)
	}
	// the copy a build stored of a file whose signed entry starts at epoch 9
	site.publish(t, 9)
	site.store.raw = site.served.Load().([]byte)
	if got := stExtenderShadowLiveEpochsOf(t, mainnet); got != "7,8" {
		t.Fatalf("the start moved to epoch 9: %s", got)
	}
	configured := stAmendmentTestConfig(t, "enabled: false\namendments:\n  - \""+stAmendmentTestLine(t, 8)+"\"\n")
	if got := stExtenderShadowLiveEpochsOf(t, configured); got != "6,7" {
		t.Fatalf("a line configured from epoch 8 before the entry from 9: %s", got)
	}
	// a stored copy signed by another key is none
	other, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	site.key = other
	site.publish(t, 10)
	site.store.raw = site.served.Load().([]byte)
	if got := stExtenderShadowLiveEpochsOf(t, mainnet); got != "6,7" {
		t.Fatalf("a stored copy that does not verify: %s", got)
	}
	unpinned := stAmendmentTestConfig(t, "enabled: false\n")
	unpinned.ChainId, unpinned.Netuid = 945, 350
	if got := stExtenderShadowLiveEpochsOf(t, unpinned); got != "" {
		t.Fatalf("a deployment with no plan and no entry: %s", got)
	}
	line := strings.Replace(strings.Replace(stAmendmentTestLine(t, 8), "chain 964", "chain 945", 1), "sn 25", "sn 350", 1)
	unpinned = stAmendmentTestConfig(t, "enabled: false\namendments:\n  - \""+line+"\"\n")
	unpinned.ChainId, unpinned.Netuid = 945, 350
	if got := stExtenderShadowLiveEpochsOf(t, unpinned); got != "6,7" {
		t.Fatalf("a deployment with a line configured from epoch 8: %s", got)
	}
	if site.fetches.Load() != 0 {
		t.Fatal("the held amendments fetched the published file")
	}
}

// Every epoch that is not one of the two before E_x says why it has no live
// shadow: an earlier one has the offline replay, one from a named E_x is built
// live, and one from a planned E_x that no entry names yet waits for the entry
// that moves the start.
func TestStExtenderShadowNotLiveSaysWhy(t *testing.T) {
	site := newStAmendmentTestSite(t)
	mainnet := stAmendmentTestConfig(t, "enabled: false\n")
	held := func() string {
		t.Helper()
		file, fallback, err := stLedgerV2HeldAmendments(context.Background(), mainnet)
		if err != nil {
			t.Fatal(err)
		}
		return stExtenderShadowNotLive(mainnet, file, fallback, 8) + " | " + stExtenderShadowNotLive(mainnet, file, fallback, 5)
	}
	if got := held(); !strings.Contains(got, "not before the extender pool's planned first epoch 8, which no amendment names yet") || !strings.Contains(got, "epoch 5 has none; an earlier epoch has the offline replay") {
		t.Fatalf("as planned: %s", got)
	}
	site.publish(t, 8)
	site.store.raw = site.served.Load().([]byte)
	if got := held(); !strings.Contains(got, "epoch 8 is covered by the extender pool amendment from epoch 8") || !strings.Contains(got, "as the amendments name it") {
		t.Fatalf("as named: %s", got)
	}
	unplanned := stAmendmentTestConfig(t, "enabled: false\n")
	unplanned.NoId = 2
	file, fallback, err := stLedgerV2HeldAmendments(context.Background(), unplanned)
	if err != nil {
		t.Fatal(err)
	}
	// the stored entry is operator 1's
	if got := stExtenderShadowNotLive(unplanned, file, fallback, 7); !strings.Contains(got, "none is planned") {
		t.Fatalf("another operator: %s", got)
	}
}

// An epoch whose shadow read keeps running past its bound is given up once:
// one alert, its timed-out attempts in the shadow phase's gauge, and the sync
// plans the epoch no more, before reading anything of it.
func TestStLedgerV2GiveUpExtenderShadowAlertsOnce(t *testing.T) {
	site := newStAmendmentTestSite(t)
	site.publish(t)
	cfg := stAmendmentTestConfig(t, "enabled: false\n")
	previousCfg, previousClient := stConfigInstance, stClientInstance
	SetStConfig(cfg)
	SetStClient(newStubStClient(&StEpochState{Epoch: 8}))
	t.Cleanup(func() {
		stConfigInstance, stClientInstance = previousCfg, previousClient
	})
	clearStExtenderShadowConcluded(t)
	ctx := server.WithoutPostgres(context.Background())
	for range 2 {
		StLedgerV2GiveUpExtenderShadow(6, 2, 110*time.Minute)
	}
	alerts := 0
	for _, alert := range site.alerts {
		if strings.HasPrefix(alert, StLedgerV2AlertExtenderShadow+": ") && strings.Contains(alert, "epoch 6's extender shadow read ran past 1h50m0s in 2 attempts") {
			alerts++
		}
	}
	if alerts != 1 || len(site.alerts) != 1 {
		t.Fatalf("alerts %v", site.alerts)
	}
	if got := testutil.ToFloat64(stExtenderPoolCounts.WithLabelValues("shadow", "timed_out_attempts")); got != 2 {
		t.Fatalf("timed-out attempts gauge %v", got)
	}
	if stLedgerV2ExtenderShadowDue(ctx, cfg, 6) || server.PacketPostgresAttempts(ctx) != 0 {
		t.Fatalf("a given-up epoch is planned, or read: %d PostgreSQL acquisitions", server.PacketPostgresAttempts(ctx))
	}
}
