package controller

// st_ctf_shadow_test.go — the shadow run (CAPTURETHEFLAG.md §11.2, step 6):
// it sends nothing, records no fixed input, and publishes a report that the
// validator's audit reproduces with ctf.Verify.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server/model"
)

// newCtfShadowFixture is a shadow driver whose provider pool funded epoch 7
// with 1,195.84 α, epoch 4's scale (§2.1).
func newCtfShadowFixture(t testing.TB) *ctfFixture {
	f := newCtfFixture(t, StCtfModeShadow)
	providers := f.chain.providers
	providers.lastFunded = 7
	providers.funded[6] = *uint256.NewInt(5_000_000_000_000)
	providers.funded[7] = *uint256.NewInt(5_000_000_000_000 + 1_195_838_935_720)
	f.census[7] = ctfTestCensusOf(t, 7, ctfTestCandidates()...)
	return f
}

func TestStCtfShadowPublishesAnAuditableReport(t *testing.T) {
	f := newCtfShadowFixture(t)
	end, window := f.chain.end(7), f.chain.window
	block := stCtfShadowCensusBlock(end, window)
	f.chain.times[block] = ctfTestCommitTime(1002)

	f.chain.head = end
	if got := f.sync(8); !reflect.DeepEqual(got, []string{"ctf_shadow 7"}) {
		t.Fatalf("planned %q", got)
	}
	// the census stand-in block is end(7) + w/2
	requireLedgerV2Outcome(t, f.run(StCtfStepShadow, 7), model.StPublishStatusSkipped, true, "is block")
	f.chain.head = block
	outcome := f.run(StCtfStepShadow, 7)
	requireLedgerV2Outcome(t, outcome, model.StPublishStatusConfirmed, false, "report sha256:")
	// nothing is sent, and nothing a live root would read is recorded
	requireCtfLog(t, f.takeLog())
	if len(f.chain.census) != 0 || len(f.records.fixed) != 0 || f.chain.pending != nil {
		t.Fatalf("the shadow run wrote: census %v fixed %v pending %v", f.chain.census, f.records.fixed, f.chain.pending)
	}

	ref, err := ledger.ParseRef(strings.TrimPrefix(outcome.Reason, "report "))
	if err != nil {
		t.Fatal(err)
	}
	report, err := GetStLedgerV2Object(f.ctx, ref.String())
	if err != nil {
		t.Fatal(err)
	}
	in, err := stCtfShadowVerifyInput(report, stCtfObjectFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	in.MaxFetchBytes = 1 << 30
	result, err := ctf.Verify(f.ctx, *in)
	if err != nil {
		t.Fatal(err)
	}
	requireCtfAudit(t, result)
	e := result.Epochs[0]
	if !e.Drawn || e.Round != 1002 || e.CensusBlock != block || len(e.Awards) != 6 || e.Funding.Uint64() != 119_583_893_572 {
		t.Fatalf("shadow epoch: drawn %t round %d block %d awards %d funding %s", e.Drawn, e.Round, e.CensusBlock, len(e.Awards), e.Funding.Dec())
	}

	var decoded map[string]any
	if err := json.Unmarshal(report, &decoded); err != nil {
		t.Fatal(err)
	}
	funding := decoded["funding"].(map[string]any)
	if decoded["schema"] != stCtfShadowSchema || decoded["pool_id"] != float64(ctfTestPoolId) || decoded["previous"] != nil ||
		funding["basis"] != stCtfShadowBasisProviderTenth || funding["provider_funding"] != "1195838935720" ||
		decoded["funded_through"].(map[string]any)["7"] != "119583893572" {
		t.Fatalf("report %s", report)
	}
	// the keys are in the documented order
	keys := []string{"schema", "chain_id", "netuid", "no_id", "pool_id", "epoch", "first_epoch", "carry_coldkey", "carry_contract", "previous", "pending", "funded_through", "funding", "epochs", "summary"}
	at := 0
	for _, key := range keys {
		next := strings.Index(string(report[at:]), `"`+key+`":`)
		if next < 0 {
			t.Fatalf("key %s out of order in %s", key, report)
		}
		at += next
	}

	// concluded: planned and run again, it changes nothing
	if got := f.sync(8); len(got) != 0 {
		t.Fatalf("planned again %q", got)
	}
	requireLedgerV2Outcome(t, f.run(StCtfStepShadow, 7), model.StPublishStatusConfirmed, false, outcome.Reason)
	if got := f.records.rows(7); !reflect.DeepEqual(got, []string{"ctfshadow confirmed"}) {
		t.Fatalf("rows %q", got)
	}
	requireCtfLog(t, f.takeLog())

	// a live step does nothing in shadow mode
	requireLedgerV2Outcome(t, f.run(StCtfStepCensus, 8), model.StPublishStatusSkipped, false, "not live")
	requireCtfLog(t, f.takeLog())
}

// The same inputs publish the same report: a retry, or another replica,
// changes nothing.
func TestStCtfShadowIsDeterministic(t *testing.T) {
	reports := []string{}
	for range 2 {
		f := newCtfShadowFixture(t)
		block := stCtfShadowCensusBlock(f.chain.end(7), f.chain.window)
		f.chain.times[block] = ctfTestCommitTime(1002)
		f.chain.head = block
		outcome := f.run(StCtfStepShadow, 7)
		requireLedgerV2Outcome(t, outcome, model.StPublishStatusConfirmed, false, "report sha256:")
		reports = append(reports, outcome.Reason)
	}
	if reports[0] != reports[1] {
		t.Fatalf("reports %q", reports)
	}
}

// A shadow epoch without a census until the next epoch ends is missed; one
// that funds nothing has no root; the CTF pool's own funding is used once
// vault2 captured the epoch for it.
func TestStCtfShadowEdges(t *testing.T) {
	f := newCtfShadowFixture(t)
	delete(f.census, 7)
	block := stCtfShadowCensusBlock(f.chain.end(7), f.chain.window)
	f.chain.times[block] = ctfTestCommitTime(1002)
	f.chain.head = block
	requireLedgerV2Outcome(t, f.run(StCtfStepShadow, 7), model.StPublishStatusSkipped, true, "is not built yet")
	f.chain.head = f.chain.end(8)
	requireLedgerV2Outcome(t, f.run(StCtfStepShadow, 7), model.StPublishStatusMissed, false, "no census of epoch 7")

	g := newCtfShadowFixture(t)
	g.chain.providers.funded[7] = g.chain.providers.funded[6]
	g.chain.times[block] = ctfTestCommitTime(1002)
	g.chain.head = block
	requireLedgerV2Outcome(t, g.run(StCtfStepShadow, 7), model.StPublishStatusSkipped, false, "funds nothing")

	// epoch 8 of a registered pool that vault2 captured: the pool's funding
	h := newCtfShadowFixture(t)
	h.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
	h.chain.emission[8] = 3_000_000_000
	h.chain.head = h.chain.end(8)
	if _, err := h.chain.LedgerV2Close(h.ctx, 8, ctfTestPoolId); err != nil {
		t.Fatal(err)
	}
	block8 := stCtfShadowCensusBlock(h.chain.end(8), h.chain.window)
	h.chain.times[block8] = ctfTestCommitTime(1003)
	h.chain.head = block8
	h.takeLog()
	outcome := h.run(StCtfStepShadow, 8)
	requireLedgerV2Outcome(t, outcome, model.StPublishStatusConfirmed, false, "report sha256:")
	requireCtfLog(t, h.takeLog())
	report, err := GetStLedgerV2Object(h.ctx, strings.TrimPrefix(outcome.Reason, "report "))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), `"funded_through":{"8":"3000000000"},"funding":{"basis":"pool","provider_funding":"0"}`) {
		t.Fatalf("report %s", report)
	}
}
