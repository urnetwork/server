package controller

// st_ledger_v2_extender_relay_db_test.go — the SN25 extender pool's relay half
// with the real database and object store (sn WHITEPAPER.md §8.6.2, owner
// 2026-11-16): a covered epoch's build reads the bytes each census extender
// relayed, splits half of Z' by them and half by unit, publishes the relay rows
// and records their chunks with the component, so a rebuild from the records
// reproduces the root; a component recorded before the relay chunks rebuilds
// with none; and a shadow report splits its pool the same way.
//
// The defect (owner 2026-11-16, D1): the pool was split by unit alone, so an
// extender that relayed nothing took as much of Z' as one that carried the
// clients.

import (
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Names the provider clients of a contract's extender parties, created with the
// contract, for the contracts that closed at closed.
func (self *ledgerV2BuildFixture) extenderParties(closed time.Time, clientIds ...server.Id) {
	server.Tx(self.ctx, func(tx server.PgTx) {
		for _, clientId := range clientIds {
			tag := server.RaisePgResult(tx.Exec(self.ctx, `
				INSERT INTO contract_extender (contract_id,extender_id,party,client_id,network_id,create_time)
				SELECT contract_id,gen_random_uuid(),'source',$2,gen_random_uuid(),create_time FROM transfer_contract WHERE close_time = $1
			`, closed, clientId))
			if tag.RowsAffected() != 1 {
				self.t.Fatalf("%d contracts closed at %s", tag.RowsAffected(), closed)
			}
		}
	})
}

// The extender pool scenario with a second extender and relay bytes in epoch
// 2: the extender is a party of the funded same-network contract (its hop 500)
// and of the unfunded one (300, not counted), the receiver, a census client
// too, a party of the funded one (its share pooled, not counted), and a second
// extender, alone in another unit, a party of a funded cross-network contract
// with its hop 200 and a provider's 100 (that provider is no party). So R =
// 700, from 2 relaying: of Z' = 500, 250 by unit (125 each) and 250 by relay
// bytes (the extender 179 with the larger remainder, the second 71).
type stExtenderRelayScenario struct {
	*stExtenderPoolScenario
	ext2, ext2Network server.Id
	ext2Key           [32]byte
}

func newStExtenderRelayScenario(t testing.TB, amendmentFrom uint64) *stExtenderRelayScenario {
	s := &stExtenderRelayScenario{stExtenderPoolScenario: newStExtenderPoolScenario(t, amendmentFrom), ext2: server.NewId(), ext2Network: server.NewId()}
	f := s.f
	ext2Network, payer, provider, providerNetwork := s.ext2Network, server.NewId(), server.NewId(), server.NewId()
	f.consents.providerChain(t, s.ext2, ext2Network, [2]uint64{17, 1})
	f.consents.flush(t)
	s.ext2Key = f.consents.public(t, 17)
	created := ledgerV2BuildTime(150)
	extenderId := server.NewId()
	model.Testing_CreateNetworkExtender(f.ctx, &model.NetworkExtender{
		ExtenderId: extenderId, NetworkId: ext2Network, ClientId: s.ext2, PublicKey: extenderId.Bytes(), CreateTime: created,
		TcpPort: 443, UdpPort: 443, DnsPort: 53, DnsTld: "ur.xyz.", CountryCode: "us", Active: true,
	}, []*model.NetworkExtenderAddress{{ExtenderId: extenderId, IpVersion: 4, Ip: netip.MustParseAddr("192.0.2.77"), Carriers: []string{"tcp"}, ActivateTime: created, Active: true}})
	two := ledgerV2BuildAuthority(2)
	f.extenderParties(two.StartTime.Add(time.Minute), s.ext, s.receiver)
	f.extenderParties(two.StartTime.Add(3*time.Minute), s.ext)
	relayed := two.StartTime.Add(6 * time.Minute)
	f.pooledContract(relayed, payer, &payer, ledgerV2BuildUsage{s.ext2, ext2Network, 200}, ledgerV2BuildUsage{provider, providerNetwork, 100})
	f.extenderParties(relayed, s.ext2)
	return s
}

// A share row of an extender.
func stExtenderRelayShare(clientId server.Id, coldkey [32]byte, bytes uint64) protocol.LedgerUsageRow {
	return protocol.LedgerUsageRow{ClientID: [16]byte(clientId), Coldkey: coldkey, UsageBytes: bytes}
}

// From E_x = 2, epoch 2's build reads its relay bytes, publishes them as the
// component's relay rows and splits Z' = 500 half by relay bytes: the extender
// 125 + 179 and the second 125 + 71, where by unit alone each took 250. The
// relay chunks are recorded with the component, the validator approves the
// root, and the records rebuild it byte-identically. A component recorded
// before the relay chunks, null in its record, rebuilds by unit alone, as it
// was built.
func TestStLedgerV2BuilderSplitsTheExtenderPoolHalfByRelayBytes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		s := newStExtenderRelayScenario(t, 2)
		f := s.f
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed || !strings.Contains(outcome.Reason, "3 rows, 2 eligible extenders in 2 units") {
			t.Fatalf("census capture %+v", outcome)
		}
		build, err := (stLedgerV2Builder{}).Build(f.ctx, s.input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(s.input, build, 0)
		header, err := ledger.DecodeHeader(build.Header)
		if err != nil {
			t.Fatal(err)
		}
		component := header.Epochs[1].Extenders
		if component == nil || component.Bytes != 500 || len(component.Census) != 1 || len(component.Relay) != 1 || len(component.Rows) != 1 || header.Epochs[0].Extenders != nil {
			t.Fatalf("epoch 2 extenders %+v", component)
		}
		object := func(ref ledger.Ref) []byte {
			data, err := GetStLedgerV2Object(f.ctx, ref.String())
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		relay, err := ledger.DecodeRelayChunks([][]byte{object(component.Relay[0])})
		wantRelay := []ledger.RelayRow{{ClientID: [16]byte(s.ext), RelayBytes: 500}, {ClientID: [16]byte(s.ext2), RelayBytes: 200}}
		slices.SortFunc(wantRelay, func(a, b ledger.RelayRow) int { return server.Id(a.ClientID).Cmp(server.Id(b.ClientID)) })
		if err != nil || !slices.Equal(relay, wantRelay) {
			t.Fatalf("relay rows %+v %v, want %+v", relay, err, wantRelay)
		}
		shares, err := ledger.DecodeProviderChunks([][]byte{object(component.Rows[0])})
		wantShares := []protocol.LedgerUsageRow{stExtenderRelayShare(s.ext, s.extKey, 125+179), stExtenderRelayShare(s.ext2, s.ext2Key, 125+71)}
		slices.SortFunc(wantShares, func(a, b protocol.LedgerUsageRow) int { return server.Id(a.ClientID).Cmp(server.Id(b.ClientID)) })
		if err != nil || !reflect.DeepEqual(shares, wantShares) {
			t.Fatalf("share rows %+v %v, want %+v", shares, err, wantShares)
		}
		// recorded with the component, and published with the build
		records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 2)
		if got := records[2].Extenders; got == nil || !slices.Equal(got.RelayChunks, []string{component.Relay[0].String()}) || !got.Equal(build.Epochs[1].Extenders) {
			t.Fatalf("epoch 2's record %+v", got)
		}
		heads := ledgerV2Heads{1: {}, 2: {}}
		if result := f.auditFrom(s.input, root, heads, 2); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit from epoch 2: %s: %v", result.Check, result.Err)
		}

		// the records build the same root again, whatever relays later
		later, payer := ledgerV2BuildAuthority(2).StartTime.Add(7*time.Minute), server.NewId()
		f.pooledContract(later, payer, &payer, ledgerV2BuildUsage{s.ext2, s.ext2Network, 900})
		f.extenderParties(later, s.ext2)
		again := &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_500, records[1]), ledgerV2Covered(2, 2_300, records[2])}}
		rebuilt, err := (stLedgerV2Builder{}).Build(f.ctx, again)
		if err != nil {
			t.Fatal(err)
		}
		if string(rebuilt.Header) != string(build.Header) || !reflect.DeepEqual(rebuilt.Accounts, build.Accounts) || !reflect.DeepEqual(rebuilt.Amounts, build.Amounts) {
			t.Fatal("the recorded epochs were not rebuilt byte-identically")
		}

		// a component recorded before the relay chunks: built by unit alone
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `UPDATE st_ledger_v2_epoch_rows SET extender_relay_chunks = NULL WHERE epoch = 2`))
		})
		before := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 2, 2)[2]
		if before.Extenders == nil || before.Extenders.RelayChunks == nil || len(before.Extenders.RelayChunks) != 0 {
			t.Fatalf("a component recorded before the relay chunks %+v", before.Extenders)
		}
		byUnit, err := (stLedgerV2Builder{}).Build(f.ctx, &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_500, records[1]), ledgerV2Covered(2, 2_300, before)}})
		if err != nil {
			t.Fatal(err)
		}
		byUnitHeader, err := ledger.DecodeHeader(byUnit.Header)
		if err != nil {
			t.Fatal(err)
		}
		unitComponent := byUnitHeader.Epochs[1].Extenders
		if unitComponent == nil || len(unitComponent.Relay) != 0 || !slices.Equal(unitComponent.Census, component.Census) || len(unitComponent.Rows) != 1 {
			t.Fatalf("epoch 2 extenders by unit %+v", unitComponent)
		}
		if recordedByUnit := byUnit.Epochs[1].Extenders; recordedByUnit == nil || len(recordedByUnit.RelayChunks) != 0 {
			t.Fatalf("epoch 2's component by unit %+v", recordedByUnit)
		}
		wantUnit := []protocol.LedgerUsageRow{stExtenderRelayShare(s.ext, s.extKey, 250), stExtenderRelayShare(s.ext2, s.ext2Key, 250)}
		slices.SortFunc(wantUnit, func(a, b protocol.LedgerUsageRow) int { return server.Id(a.ClientID).Cmp(server.Id(b.ClientID)) })
		chunks := ledger.EncodeProviderChunks(wantUnit)
		if len(chunks) != 1 || ledger.RefOf(chunks[0]) != unitComponent.Rows[0] {
			t.Fatalf("share rows by unit are not %+v", wantUnit)
		}
		extAmount := func(build *StLedgerV2Build) uint64 {
			for _, amount := range build.Amounts {
				if amount.Epoch == 2 && amount.Coldkey == s.extKey {
					return amount.Amount.Uint64()
				}
			}
			return 0
		}
		if withRelay, unitOnly := extAmount(build), extAmount(byUnit); unitOnly == 0 || withRelay <= unitOnly {
			t.Fatalf("the relaying extender's epoch 2 amount %d with relay rows, %d by unit alone", withRelay, unitOnly)
		}
	})
}

// Before E_x = 4, epoch 2's shadow report reads the epoch's relay bytes and
// builds its shadow root with them: its header lists the relay rows, its share
// rows split Z' = 500 half by relay bytes, and the validator's recompute
// approves it.
func TestStLedgerV2ExtenderShadowSplitsHalfByRelayBytes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		s := newStExtenderRelayScenario(t, 4)
		f := s.f
		stExtenderShadowConcluded.Clear()
		t.Cleanup(stExtenderShadowConcluded.Clear)
		build, err := (stLedgerV2Builder{}).Build(f.ctx, s.input)
		if err != nil {
			t.Fatal(err)
		}
		f.publish(s.input, build, 0)
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("census capture %+v", outcome)
		}
		chain := &stExtenderShadowTestChain{state: StLedgerV2State{Registered: true, FirstEpoch: 1, LastFundedEpoch: 2},
			funded: map[uint64]uint256.Int{1: *uint256.NewInt(1_500), 2: *uint256.NewInt(3_800)}}
		settings := DefaultStLedgerV2ExtenderShadowSettings()
		settings.ReadSliceRetryDelay = time.Millisecond
		if outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 2, settings); outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("shadow %+v", outcome)
		}
		recorded := model.GetStLedgerV2ExtenderShadow(f.ctx, f.cfg.DeploymentKey(), 1, 2)
		if recorded == nil {
			t.Fatal("no shadow report")
		}
		in, err := stExtenderShadowVerifyInput(recorded.Report, ledgerV2StoreFetcher{})
		if err != nil {
			t.Fatal(err)
		}
		object := func(ref ledger.Ref) []byte {
			data, err := GetStLedgerV2Object(f.ctx, ref.String())
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		header, err := ledger.DecodeHeader(object(ledger.Ref(in.Pending.LedgerHash)))
		if err != nil {
			t.Fatal(err)
		}
		component := header.Epochs[0].Extenders
		if component == nil || component.Bytes != 500 || len(component.Relay) != 1 || len(component.Rows) != 1 {
			t.Fatalf("the shadow's component %+v", component)
		}
		relay, err := ledger.DecodeRelayChunks([][]byte{object(component.Relay[0])})
		if err != nil || len(relay) != 2 || relay[0].RelayBytes+relay[1].RelayBytes != 700 {
			t.Fatalf("the shadow's relay rows %+v %v", relay, err)
		}
		shares, err := ledger.DecodeProviderChunks([][]byte{object(component.Rows[0])})
		wantShares := []protocol.LedgerUsageRow{stExtenderRelayShare(s.ext, s.extKey, 125+179), stExtenderRelayShare(s.ext2, s.ext2Key, 125+71)}
		slices.SortFunc(wantShares, func(a, b protocol.LedgerUsageRow) int { return server.Id(a.ClientID).Cmp(server.Id(b.ClientID)) })
		if err != nil || !reflect.DeepEqual(shares, wantShares) {
			t.Fatalf("the shadow's share rows %+v %v, want %+v", shares, err, wantShares)
		}
		in.MaxFetchBytes, in.Heads = ledger.DefaultMaxFetchBytes, ledgerV2Heads{2: {}}
		if result, err := ledger.Verify(f.ctx, *in); err != nil || result.Check != ledger.CheckNone {
			t.Fatalf("the recompute of the shadow: %+v, %v", result, err)
		}
	})
}
