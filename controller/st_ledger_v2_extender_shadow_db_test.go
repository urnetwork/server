package controller

// st_ledger_v2_extender_shadow_db_test.go — the SN25 extender pool before E_x
// with the real database and object store (sn WHITEPAPER.md §8.6.7): an epoch
// built and recorded under the rule in force gets its shadow report from the
// census captured at its end, which the validator's recompute approves, without
// a fixed input or a wallet resolution written; the offline replay of the same
// epoch finds its rows differ by exactly the pooled shares; and an epoch the
// amendment covers publishes its version 2 statement's extender pool from the
// committed component and its recorded parts.

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The shadow run's chain: the pool's state and vault2's fundedThrough.
type stExtenderShadowTestChain struct {
	state  StLedgerV2State
	funded map[uint64]uint256.Int
}

func (self *stExtenderShadowTestChain) LedgerV2State(context.Context, uint64) (*StLedgerV2State, error) {
	state := self.state
	return &state, nil
}

func (self *stExtenderShadowTestChain) LedgerV2FundedThrough(_ context.Context, _ uint64, epoch uint64) (uint256.Int, error) {
	return self.funded[epoch], nil
}

// The extender pool fixture of TestStLedgerV2BuilderBuildsTheExtenderPool:
// in epochs 1 and 2 a same-network contract its network funded, receiver 500
// and the extender's hop 500, and own's 1,000 bytes elsewhere; in epoch 2 the
// same unfunded, receiver 300 and the extender 300, and 200 bytes the
// measurement network paid for. The extender's record and one of the receiver,
// which has no coldkey, are known before epoch 2.
type stExtenderPoolScenario struct {
	f                          *ledgerV2BuildFixture
	own, ext, receiver         server.Id
	ownKey, extKey             [32]byte
	input                      *StLedgerV2BuildInput
	measurementNetwork, origin server.Id
}

func newStExtenderPoolScenario(t testing.TB, amendmentFrom uint64) *stExtenderPoolScenario {
	f := newLedgerV2BuildFixture(t)
	settings := model.DefaultVerifySettings()
	settings.EgressHashKey = []byte("sn25 extender census test key")
	SetVerifySettings(settings)
	t.Cleanup(func() { SetVerifySettings(nil) })
	s := &stExtenderPoolScenario{f: f, measurementNetwork: server.NewId()}
	if amendmentFrom != 0 {
		line, err := amendment.NewExtenders(f.cfg.ChainId, uint16(f.cfg.Netuid), f.cfg.NoId, amendmentFrom).Line()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.cfg.Amendments.Encode([]string{line}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.cfg.ExtenderPool.Encode(map[string][]string{"measurement_network_ids": {s.measurementNetwork.String()}}); err != nil {
		t.Fatal(err)
	}
	s.own, s.ext, s.receiver = server.NewId(), server.NewId(), server.NewId()
	ownNetwork, extNetwork, other, measured := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	s.origin = server.NewId()
	f.consents.providerChain(t, s.own, ownNetwork, [2]uint64{11, 1})
	f.consents.providerChain(t, s.ext, extNetwork, [2]uint64{16, 1})
	f.consents.flush(t)
	s.ownKey, s.extKey = f.consents.public(t, 11), f.consents.public(t, 16)
	created := ledgerV2BuildTime(150)
	for _, record := range []struct {
		client, network server.Id
		ip              string
	}{{s.ext, extNetwork, "198.51.100.9"}, {s.receiver, s.origin, "203.0.113.7"}} {
		extenderId := server.NewId()
		model.Testing_CreateNetworkExtender(f.ctx, &model.NetworkExtender{
			ExtenderId: extenderId, NetworkId: record.network, ClientId: record.client, PublicKey: extenderId.Bytes(), CreateTime: created,
			TcpPort: 443, UdpPort: 443, DnsPort: 53, DnsTld: "ur.xyz.", CountryCode: "us", Active: true,
		}, []*model.NetworkExtenderAddress{{ExtenderId: extenderId, IpVersion: 4, Ip: netip.MustParseAddr(record.ip), Carriers: []string{"tcp"}, ActivateTime: created, Active: true}})
	}
	one, two := ledgerV2BuildAuthority(1), ledgerV2BuildAuthority(2)
	for _, authority := range []*StPayoutEpochAuthority{one, two} {
		f.pooledContract(authority.StartTime.Add(time.Minute), s.origin, &s.origin, ledgerV2BuildUsage{s.receiver, s.origin, 500}, ledgerV2BuildUsage{s.ext, extNetwork, 500})
		f.pooledContract(authority.StartTime.Add(2*time.Minute), other, &other, ledgerV2BuildUsage{s.own, ownNetwork, 1000})
	}
	f.pooledContract(two.StartTime.Add(3*time.Minute), s.origin, nil, ledgerV2BuildUsage{s.receiver, s.origin, 300}, ledgerV2BuildUsage{s.ext, extNetwork, 300})
	f.pooledContract(two.StartTime.Add(4*time.Minute), s.measurementNetwork, &s.measurementNetwork, ledgerV2BuildUsage{measured, s.measurementNetwork, 200})
	s.input = &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_500, nil), ledgerV2Covered(2, 2_300, nil)}}
	return s
}

// Before E_x: epoch 2, built and recorded under the rule in force, gets the
// shadow of what §8.6 would have paid: Z = the receiver's funded 500, the
// unfunded 300 and the measurement network's 200 left out, the extender eligible
// alone in its unit, Z' = 500 below W_k = 1,800, and from the epoch's 2,300 rao
// own 1,000 and the extender's coldkey 1,300, where the rows as recorded pay
// own 1,278 and the extender 1,022. The validator's recompute approves it; the
// run records no fixed input and retains no wallet resolution; and it is built
// once. The offline replay of the same epoch reconciles exactly.
func TestStLedgerV2ExtenderShadowAndReplayBeforeTheAmendment(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		s := newStExtenderPoolScenario(t, 0)
		f := s.f
		build, err := (stLedgerV2Builder{}).Build(f.ctx, s.input)
		if err != nil {
			t.Fatal(err)
		}
		f.publish(s.input, build, 0)
		if stLedgerV2ExtenderShadowDue(f.ctx, f.cfg, 2) {
			t.Fatal("a shadow is due before the epoch's census is captured")
		}
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("census capture %+v", outcome)
		}
		if !stLedgerV2ExtenderShadowDue(f.ctx, f.cfg, 2) {
			t.Fatal("the shadow is not due once the census is captured")
		}
		chain := &stExtenderShadowTestChain{state: StLedgerV2State{Registered: true, FirstEpoch: 1, LastFundedEpoch: 1},
			funded: map[uint64]uint256.Int{1: *uint256.NewInt(1_500), 2: *uint256.NewInt(3_800)}}
		// the epoch's close is not final yet
		if outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 2); !outcome.Retry || !strings.Contains(outcome.Reason, "not closed yet") {
			t.Fatalf("a shadow before the close %+v", outcome)
		}
		chain.state.LastFundedEpoch = 2
		rowsBefore := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 2)
		// wallet resolutions are append-only; the shadow adds none
		resolutions := func() int {
			var count int
			server.Db(f.ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM st_payout_wallet_resolution`).Scan(&count))
			})
			return count
		}
		resolutionsBefore := resolutions()
		outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 2)
		if outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("shadow %+v", outcome)
		}
		recorded := model.GetStLedgerV2ExtenderShadow(f.ctx, f.cfg.DeploymentKey(), 1, 2)
		if recorded == nil || !strings.Contains(outcome.Reason, recorded.ReportRef) || stLedgerV2ExtenderShadowDue(f.ctx, f.cfg, 2) {
			t.Fatalf("recorded %+v after %+v", recorded, outcome)
		}
		if published, err := GetStLedgerV2Object(f.ctx, recorded.ReportRef); err != nil || string(published) != string(recorded.Report) {
			t.Fatalf("the published report %v", err)
		}
		var report stExtenderShadowReport
		if err := json.Unmarshal(recorded.Report, &report); err != nil {
			t.Fatal(err)
		}
		pool := report.ExtenderPool
		if pool.ExtenderPoolBytes != 500 || pool.IntraNetworkBytes != 500 || pool.IntraPlatformBytes != 0 || pool.ExtenderPoolWeightBytes != 500 ||
			pool.EligibleExtenders != 1 || pool.ExtenderUnits != 1 || pool.ExtenderPoolContracts != 1 || pool.ExtenderPoolPayers != 1 ||
			report.LeftOut.UnfundedBytes != 300 || report.LeftOut.MeasurementBytes != 200 || report.EligibleProviderBytes != 1_800 || report.FundedThrough["2"] != "2300" {
			t.Fatalf("report %s", recorded.Report)
		}
		amounts := map[string][2]string{}
		for _, entry := range report.Coldkeys {
			amounts[entry.Coldkey] = [2]string{entry.Recorded, entry.Shadow}
		}
		hex := func(key [32]byte) string { return stExtenderShadowHex(key[:]) }
		if len(amounts) != 2 || amounts[hex(s.ownKey)] != [2]string{"1278", "1000"} || amounts[hex(s.extKey)] != [2]string{"1022", "1300"} {
			t.Fatalf("amounts %v", amounts)
		}
		in, err := stExtenderShadowVerifyInput(recorded.Report, ledgerV2StoreFetcher{})
		if err != nil {
			t.Fatal(err)
		}
		in.MaxFetchBytes, in.Heads = ledger.DefaultMaxFetchBytes, ledgerV2Heads{2: {}}
		if result, err := ledger.Verify(f.ctx, *in); err != nil || result.Check != ledger.CheckNone {
			t.Fatalf("the recompute of the shadow: %+v, %v", result, err)
		}
		// nothing fixed, nothing retained
		rowsAfter := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 2)
		for epoch := range rowsBefore {
			if !rowsAfter[epoch].Extenders.Equal(rowsBefore[epoch].Extenders) || strings.Join(rowsAfter[epoch].ProviderChunks, ",") != strings.Join(rowsBefore[epoch].ProviderChunks, ",") {
				t.Fatalf("the shadow changed epoch %d's record", epoch)
			}
		}
		if after := resolutions(); after != resolutionsBefore {
			t.Fatalf("the shadow retained wallet resolutions: %d before, %d after", resolutionsBefore, after)
		}
		// built once
		if again := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 2); again.Status != model.StPublishStatusSkipped || !strings.Contains(again.Reason, recorded.ReportRef) {
			t.Fatalf("a second shadow %+v", again)
		}

		// the offline replay: the receiver's 800 and the measurement network's
		// 200 left the rows, exactly the pooled shares
		replay, err := StLedgerV2ExtenderReplay(f.ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !replay.Exact || replay.PooledBytes != 1_000 || replay.ExtenderPoolBytes != 500 || replay.UnfundedBytes != 300 || replay.MeasurementBytes != 200 ||
			replay.RecordedBytes != 2_800 || replay.AmendedBytes != 1_800 || len(replay.Mismatches) != 0 {
			t.Fatalf("replay %+v", replay)
		}
		if _, err := StLedgerV2ExtenderReplay(f.ctx, 3); err == nil {
			t.Fatal("a replay of an epoch with no recorded rows")
		}
	})
}

// From E_x, an epoch's version 2 statement carries what the committed component
// holds, Z, its census and share-row chunks, N, M and Z', with the parts its rows
// recorded; a component that is not the recorded one cannot be attested. And an
// epoch the amendment covers has no shadow.
func TestStLedgerV2StatementExtenderPoolFromTheCommittedComponent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		s := newStExtenderPoolScenario(t, 2)
		f := s.f
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("census capture %+v", outcome)
		}
		build, err := (stLedgerV2Builder{}).Build(f.ctx, s.input)
		if err != nil {
			t.Fatal(err)
		}
		f.publish(s.input, build, 0)
		records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 2, 2)
		if got := records[2].Extenders; got == nil || got.Bytes != 500 || got.IntraNetwork != 500 || got.IntraPlatform != 0 || got.Contracts != 1 || got.Payers != 1 {
			t.Fatalf("the recorded component %+v", got)
		}
		header, err := ledger.DecodeHeader(build.Header)
		if err != nil {
			t.Fatal(err)
		}
		component := header.Epochs[1].Extenders
		pool, committed, err := stLedgerV2StatementExtenderPool(f.ctx, f.cfg, 2, component)
		if err != nil {
			t.Fatal(err)
		}
		if pool.ExtenderPoolBytes != 500 || pool.IntraNetworkBytes != 500 || pool.ExtenderPoolWeightBytes != 500 || pool.EligibleExtenders != 1 || pool.ExtenderUnits != 1 ||
			pool.ExtenderPoolContracts != 1 || pool.ExtenderPoolPayers != 1 || pool.ExtenderCensus[0] != component.Census[0].String() || pool.ExtenderChunks[0] != component.Rows[0].String() {
			t.Fatalf("statement pool %+v", pool)
		}
		if committed.Weight != 500 || committed.Eligible != 1 || committed.Units != 1 {
			t.Fatalf("committed %+v", committed)
		}
		other := *component
		other.Bytes++
		if _, _, err := stLedgerV2StatementExtenderPool(f.ctx, f.cfg, 2, &other); !errors.Is(err, errStLedgerV2UsageHeader) {
			t.Fatalf("another component: %v", err)
		}
		if _, _, err := stLedgerV2StatementExtenderPool(f.ctx, f.cfg, 1, component); !errors.Is(err, errStLedgerV2UsageHeader) {
			t.Fatalf("an epoch recorded without a component: %v", err)
		}
		// the covered epoch has no shadow
		chain := &stExtenderShadowTestChain{state: StLedgerV2State{Registered: true, FirstEpoch: 1, LastFundedEpoch: 2}, funded: map[uint64]uint256.Int{1: *uint256.NewInt(1_500), 2: *uint256.NewInt(3_800)}}
		if outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 2); outcome.Status != model.StPublishStatusSkipped || !strings.Contains(outcome.Reason, "covered") || stLedgerV2ExtenderShadowDue(f.ctx, f.cfg, 2) {
			t.Fatalf("a shadow of a covered epoch %+v", outcome)
		}
		if model.GetStLedgerV2ExtenderShadow(f.ctx, f.cfg.DeploymentKey(), 1, 2) != nil {
			t.Fatal("a covered epoch's shadow was recorded")
		}
		// and its replay is refused: it was built with a component
		if _, err := StLedgerV2ExtenderReplay(f.ctx, 2); err == nil {
			t.Fatal("a replay of an epoch built with a component")
		}
	})
}
