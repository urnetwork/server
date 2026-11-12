package controller

// st_ledger_v2_extenders_db_test.go — the SN25 extender pool through the
// settlement v2 build with the real database and object store (sn
// WHITEPAPER.md §8.6.2, §8.6.7): the configured amendment line, the census
// captured by its usage step, the amended epoch reader, the build, the
// recorded component, the validator's audit at E_x and the rebuild from the
// records.

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// pooledContract retains one settled contract closed at closed, whose usage
// origin is its source on originNetwork, paid by payer (nil for none), with
// the providers' usage.
func (self *ledgerV2BuildFixture) pooledContract(closed time.Time, originNetwork server.Id, payer *server.Id, providers ...ledgerV2BuildUsage) {
	providers = slices.Clone(providers)
	slices.SortFunc(providers, func(a ledgerV2BuildUsage, b ledgerV2BuildUsage) int { return a.clientId.Cmp(b.clientId) })
	entries, total := []string{}, int64(0)
	for _, provider := range providers {
		entries = append(entries, fmt.Sprintf(`{"client_id":%q,"network_id":%q,"byte_count":%d}`, provider.clientId.String(), provider.networkId.String(), provider.bytes))
		total += provider.bytes
	}
	usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[%s]}`, total, strings.Join(entries, ","))
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source,payer_network_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true,$10)`, server.NewId(), originNetwork, server.NewId(), server.NewId(), server.NewId(), total, closed.Add(-time.Hour), closed, usage, payer))
	})
}

// auditFrom runs the validator's audit as audit does, for a pool whose
// extender amendment is in force from extendersFrom (0 for none).
func (self *ledgerV2BuildFixture) auditFrom(input *StLedgerV2BuildInput, root *StLedgerV2FinalizedRoot, heads ledgerV2Heads, extendersFrom uint64) *ledger.Result {
	self.t.Helper()
	in := ledger.VerifyInput{NoID: input.NoId, FirstEpoch: input.Epochs[0].Epoch, Seq: root.Index + 1, FundedThrough: map[uint64]uint256.Int{},
		Pending: ledger.Root{Epoch: root.Epoch, Root: root.Root, Total: root.Total, LeafCount: root.LeafCount, LedgerHash: root.LedgerHash},
		Fetcher: ledgerV2StoreFetcher{}, Custody: ledger.NewMemoryCustody(), Heads: heads, MaxFetchBytes: ledger.DefaultMaxFetchBytes, ExtendersFrom: extendersFrom}
	var funded uint256.Int
	for _, covered := range input.Epochs {
		funded.Add(&funded, &covered.Funding)
		in.FundedThrough[covered.Epoch] = funded
	}
	result, err := ledger.Verify(self.ctx, in)
	if err != nil {
		self.t.Fatal(err)
	}
	return result
}

// From E_x = 2, set by the configured amendment line of a deployment that
// publishes no amendments, an epoch's same-network shares of the contracts a
// balance funded are its extender pool bytes Z: the epoch's provider rows
// lose them, its census captured by the usage step lists the operator's
// extender records, and the build splits Z, capped at the eligible usage,
// over the eligible extenders' units. The unfunded and the measurement
// network's same-network shares are in neither. Epoch 1, before E_x, is
// built exactly as before. The epoch's component is recorded with its rows;
// the validator approves the root only with the amendment in force from the
// same epoch; and a rebuild from the records is byte-identical.
func TestStLedgerV2BuilderBuildsTheExtenderPool(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		// the census keys units as the head's egress prefix, under verify.yml's
		// key, which the test vault does not hold
		settings := model.DefaultVerifySettings()
		settings.EgressHashKey = []byte("sn25 extender census test key")
		SetVerifySettings(settings)
		t.Cleanup(func() { SetVerifySettings(nil) })
		line, err := amendment.NewExtenders(f.cfg.ChainId, uint16(f.cfg.Netuid), f.cfg.NoId, 2).Line()
		if err != nil {
			t.Fatal(err)
		}
		measurementNetwork := server.NewId()
		if err := f.cfg.Amendments.Encode([]string{line}); err != nil {
			t.Fatal(err)
		}
		if err := f.cfg.ExtenderPool.Encode(map[string][]string{"measurement_network_ids": {measurementNetwork.String()}}); err != nil {
			t.Fatal(err)
		}

		own, ext, receiver, measured := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		ownNetwork, extNetwork, origin := server.NewId(), server.NewId(), server.NewId()
		f.consents.providerChain(t, own, ownNetwork, [2]uint64{11, 1})
		f.consents.providerChain(t, ext, extNetwork, [2]uint64{16, 1})
		f.consents.flush(t)
		ownKey, extKey := f.consents.public(t, 11), f.consents.public(t, 16)
		// the extender's record, and one of a client without a payout coldkey,
		// known before epoch 2
		created := ledgerV2BuildTime(150)
		for _, record := range []struct {
			client, network server.Id
			ip              string
		}{{ext, extNetwork, "198.51.100.9"}, {receiver, origin, "203.0.113.7"}} {
			extenderId := server.NewId()
			model.Testing_CreateNetworkExtender(f.ctx, &model.NetworkExtender{
				ExtenderId: extenderId, NetworkId: record.network, ClientId: record.client, PublicKey: extenderId.Bytes(), CreateTime: created,
				TcpPort: 443, UdpPort: 443, DnsPort: 53, DnsTld: "ur.xyz.", CountryCode: "us", Active: true,
			}, []*model.NetworkExtenderAddress{{ExtenderId: extenderId, IpVersion: 4, Ip: netip.MustParseAddr(record.ip), Carriers: []string{"tcp"}, ActivateTime: created, Active: true}})
		}

		one, two := ledgerV2BuildAuthority(1), ledgerV2BuildAuthority(2)
		other := server.NewId()
		for _, authority := range []*StPayoutEpochAuthority{one, two} {
			// a same-network contract its network funded, with the extender's hop
			f.pooledContract(authority.StartTime.Add(time.Minute), origin, &origin, ledgerV2BuildUsage{receiver, origin, 500}, ledgerV2BuildUsage{ext, extNetwork, 500})
			f.pooledContract(authority.StartTime.Add(2*time.Minute), other, &other, ledgerV2BuildUsage{own, ownNetwork, 1000})
		}
		// the same unfunded, and one the measurement network paid
		f.pooledContract(two.StartTime.Add(3*time.Minute), origin, nil, ledgerV2BuildUsage{receiver, origin, 300}, ledgerV2BuildUsage{ext, extNetwork, 300})
		f.pooledContract(two.StartTime.Add(4*time.Minute), measurementNetwork, &measurementNetwork, ledgerV2BuildUsage{measured, measurementNetwork, 200})

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_500, nil), ledgerV2Covered(2, 2_300, nil)}}
		if _, err := (stLedgerV2Builder{}).Build(f.ctx, input); !errors.Is(err, errStLedgerV2ExtenderCensusMissing) {
			t.Fatalf("a build of an epoch the amendment covers before its census: %v", err)
		}
		if !stLedgerV2ExtenderCensusDue(f.ctx, f.cfg, 2) {
			t.Fatal("the census is not due before its capture")
		}
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed || !strings.Contains(outcome.Reason, "2 rows, 1 eligible extenders in 1 units") {
			t.Fatalf("census capture %+v", outcome)
		}
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusSkipped || stLedgerV2ExtenderCensusDue(f.ctx, f.cfg, 2) {
			t.Fatalf("a second census capture %+v", outcome)
		}

		build, err := (stLedgerV2Builder{}).Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		// written as the driver writes it, so its objects are in the store
		root := f.publish(input, build, 0)
		var zero [32]byte
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(ledgerV2Row(own, ownKey, 1000, false), ledgerV2Row(ext, extKey, 500, false), ledgerV2Row(receiver, zero, 500, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 1 rows %+v\nwant %+v", got, want)
		}
		if got, want := f.rows(build.Epochs[1]), ledgerV2SortedRows(ledgerV2Row(own, ownKey, 1000, false), ledgerV2Row(ext, extKey, 800, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 2 rows %+v\nwant %+v", got, want)
		}
		header, err := ledger.DecodeHeader(build.Header)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(build.Header), `"`+ledger.SchemaV3+`"`) || header.Epochs[0].Extenders != nil {
			t.Fatalf("header %s, epoch 1 extenders %+v", build.Header, header.Epochs[0].Extenders)
		}
		component := header.Epochs[1].Extenders
		if component == nil || component.Bytes != 500 || len(component.Census) != 1 || len(component.Rows) != 1 {
			t.Fatalf("epoch 2 extenders %+v", component)
		}
		if got := build.Epochs[1].Extenders; got == nil || got.Bytes != 500 || !slices.Equal(got.CensusChunks, []string{component.Census[0].String()}) || build.Epochs[0].Extenders != nil {
			t.Fatalf("built components %+v, %+v", build.Epochs[0].Extenders, got)
		}
		// the extender's coldkey takes its provider row and its share
		want := map[string]uint64{fmt.Sprintf("1 %x", ownKey[:4]): 1000, fmt.Sprintf("1 %x", extKey[:4]): 500, fmt.Sprintf("2 %x", ownKey[:4]): 1000, fmt.Sprintf("2 %x", extKey[:4]): 1300}
		if got := ledgerV2Amounts(build.Amounts); !reflect.DeepEqual(got, want) {
			t.Fatalf("amounts %v, want %v", got, want)
		}

		object := func(ref ledger.Ref) []byte {
			data, err := GetStLedgerV2Object(f.ctx, ref.String())
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		census, err := ledger.DecodeCensusChunks([][]byte{object(component.Census[0])})
		if err != nil || len(census) != 2 {
			t.Fatalf("census %+v %v", census, err)
		}
		for _, row := range census {
			switch server.Id(row.ClientID) {
			case ext:
				if !row.Eligible() || row.Coldkey != extKey || row.Family != ledger.UnitIPv4 || row.Unit != model.VerifyEgressIpHashWithSettings(netip.MustParseAddr("198.51.100.9"), VerifySettings()) {
					t.Fatalf("the extender's census row %+v", row)
				}
			case receiver:
				if row.Eligible() || row.Flags != ledger.CensusActive || row.Coldkey != zero {
					t.Fatalf("the census row of a client without a coldkey %+v", row)
				}
			default:
				t.Fatalf("census row %+v", row)
			}
		}
		shares, err := ledger.DecodeProviderChunks([][]byte{object(component.Rows[0])})
		if err != nil || !reflect.DeepEqual(shares, []protocol.LedgerUsageRow{{ClientID: [16]byte(ext), Coldkey: extKey, UsageBytes: 500}}) {
			t.Fatalf("share rows %+v %v", shares, err)
		}
		records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 2)
		if len(records) != 2 || records[1].Extenders != nil || !records[2].Extenders.Equal(build.Epochs[1].Extenders) {
			t.Fatalf("records %+v, %+v", records[1], records[2])
		}
		if captured := model.GetStLedgerV2ExtenderCensus(f.ctx, f.cfg.DeploymentKey(), 1, 2); captured == nil || string(captured.Census) != string(object(component.Census[0])) {
			t.Fatal("the published census is not the captured one")
		}

		heads := ledgerV2Heads{1: {}, 2: {}}
		if result := f.auditFrom(input, root, heads, 2); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit from epoch 2: %s: %v", result.Check, result.Err)
		}
		for from, want := range map[uint64]error{0: ledger.ErrExtendersUnexpected, 1: ledger.ErrExtendersMissing, 3: ledger.ErrExtendersUnexpected} {
			if result := f.auditFrom(input, root, heads, from); result.Check != ledger.CheckExtenders || !errors.Is(result.Err, want) || result.Action() != ledger.ActionRefuse {
				t.Fatalf("audit from epoch %d: %s: %v, want %v", from, result.Check, result.Err, want)
			}
		}

		// the records build the same root again, whatever arrives later
		f.pooledContract(two.StartTime.Add(5*time.Minute), origin, &origin, ledgerV2BuildUsage{receiver, origin, 900})
		again := &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_500, records[1]), ledgerV2Covered(2, 2_300, records[2])}}
		rebuilt, err := (stLedgerV2Builder{}).Build(f.ctx, again)
		if err != nil {
			t.Fatal(err)
		}
		if string(rebuilt.Header) != string(build.Header) || !reflect.DeepEqual(rebuilt.Accounts, build.Accounts) || !reflect.DeepEqual(rebuilt.Amounts, build.Amounts) {
			t.Fatal("the recorded epochs were not rebuilt byte-identically")
		}
	})
}
