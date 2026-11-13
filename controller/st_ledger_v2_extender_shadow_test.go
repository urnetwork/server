package controller

// st_ledger_v2_extender_shadow_test.go — the SN25 extender pool's shadow
// report, offline replay, metrics and version 2 usage statement, as pure
// arithmetic over sn WHITEPAPER.md §8.6.5's worked example: one epoch of one
// pool with A = 1,000 α = 10^12 rao.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urfoundation/sn/demand/usage"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const stExtenderExampleEpoch = 6

func stExtenderColdkey(name byte) [32]byte {
	return [32]byte{name}
}

func stExtenderClient(n byte) [16]byte {
	return [16]byte{0x10, n}
}

// The worked example (§8.6.5) as an epoch's inputs:
//   - provider usage by coldkey: a 6.0 TB and b 2.5 TB cross-network; c 0.5 TB
//     cross-network plus 0.4 TB served to its own network's clients; a provider
//     without a wallet 1.0 TB;
//   - 600,000,000,001 bytes of intra-platform traffic, held by a platform
//     endpoint with no wallet;
//   - the census: x1 (coldkey d) and x2 (coldkey b) in unit u1, x3 (coldkey e)
//     alone in u2, whose hash sorts after u1's, and x4 without a wallet.
//
// recorded are the rows the rule in force counts, every share provider usage;
// amended the rows under §8.6, c's same-network and the endpoint's shares
// pooled.
func stExtenderExample() (recorded []protocol.LedgerUsageRow, amended []protocol.LedgerUsageRow, pool *model.StEpochExtenderPool, census []ledger.CensusRow) {
	a, b, c, nowallet, endpoint := stExtenderClient(1), stExtenderClient(2), stExtenderClient(3), stExtenderClient(4), stExtenderClient(5)
	recorded = []protocol.LedgerUsageRow{
		{ClientID: a, Coldkey: stExtenderColdkey('a'), UsageBytes: 6_000_000_000_000},
		{ClientID: b, Coldkey: stExtenderColdkey('b'), UsageBytes: 2_500_000_000_000},
		{ClientID: c, Coldkey: stExtenderColdkey('c'), UsageBytes: 900_000_000_000},
		{ClientID: nowallet, UsageBytes: 1_000_000_000_000},
		{ClientID: endpoint, UsageBytes: 600_000_000_001},
	}
	amended = []protocol.LedgerUsageRow{
		{ClientID: a, Coldkey: stExtenderColdkey('a'), UsageBytes: 6_000_000_000_000},
		{ClientID: b, Coldkey: stExtenderColdkey('b'), UsageBytes: 2_500_000_000_000},
		{ClientID: c, Coldkey: stExtenderColdkey('c'), UsageBytes: 500_000_000_000},
		{ClientID: nowallet, UsageBytes: 1_000_000_000_000},
	}
	pool = &model.StEpochExtenderPool{IntraPlatform: 600_000_000_001, IntraNetwork: 400_000_000_000, Contracts: 7, Payers: 3,
		ClientIdPooledBytes: map[server.Id]model.ByteCount{server.Id(c): 400_000_000_000, server.Id(endpoint): 600_000_000_001}}
	unit := func(n byte) [32]byte { return [32]byte{0xee, n} }
	census = []ledger.CensusRow{
		{ExtenderID: [16]byte{1}, ClientID: stExtenderClient(0x21), Coldkey: stExtenderColdkey('d'), Unit: unit(1), Family: ledger.UnitIPv4, Flags: ledger.CensusActive},
		{ExtenderID: [16]byte{2}, ClientID: stExtenderClient(0x22), Coldkey: stExtenderColdkey('b'), Unit: unit(1), Family: ledger.UnitIPv4, Flags: ledger.CensusActive},
		{ExtenderID: [16]byte{3}, ClientID: stExtenderClient(0x23), Coldkey: stExtenderColdkey('e'), Unit: unit(2), Family: ledger.UnitIPv4, Flags: ledger.CensusActive},
		{ExtenderID: [16]byte{4}, ClientID: stExtenderClient(0x24), Unit: unit(3), Family: ledger.UnitIPv4, Flags: ledger.CensusActive},
	}
	return recorded, amended, pool, census
}

func stExtenderExampleInput(t *testing.T) *stExtenderShadowInput {
	t.Helper()
	recorded, amended, pool, census := stExtenderExample()
	if err := ledger.CheckCensusRows(census); err != nil {
		t.Fatal(err)
	}
	chunks := make([]string, 0)
	for _, chunk := range ledger.EncodeProviderChunks(recorded) {
		chunks = append(chunks, ledger.RefOf(chunk).String())
	}
	return &stExtenderShadowInput{ChainId: 964, Netuid: 25, NoId: 1, Epoch: stExtenderExampleEpoch, Funding: *uint256.NewInt(1_000_000_000_000),
		Rows: amended, Pool: pool, Recorded: recorded, RecordedChunks: chunks, Census: census, PlatformEndpoints: 1}
}

// The shadow of the worked example pays exactly the whitepaper's table: under
// §8.6, a 600 α, b 275 α (its provider row and x2's share row), c 50 α, d (x1)
// 25 α and e (x3) 50 α; under the rule in force, a 638.297872340 α, b
// 265.957446809 α and c 95.744680851 α. Its report carries Z's parts, Z' = Z
// below the cap, N = 3 in M = 2 units and W_k = 9 TB, and the validator's
// recompute, ledger.Verify with the epoch as E_x, approves it.
func TestStExtenderShadowReportOfTheWorkedExample(t *testing.T) {
	ctx := context.Background()
	report, shadow, err := stExtenderShadowBuild(ctx, stExtenderExampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	var decoded stExtenderShadowReport
	if err := json.Unmarshal(report, &decoded); err != nil {
		t.Fatal(err)
	}
	pool := decoded.ExtenderPool
	if decoded.Schema != stExtenderShadowSchema || decoded.Epoch != stExtenderExampleEpoch || decoded.ExtendersFrom != stExtenderExampleEpoch || decoded.FirstEpoch != stExtenderExampleEpoch {
		t.Fatalf("report %s", report)
	}
	if pool.IntraPlatformBytes != 600_000_000_001 || pool.IntraNetworkBytes != 400_000_000_000 || pool.ExtenderPoolBytes != 1_000_000_000_001 ||
		pool.ExtenderPoolWeightBytes != 1_000_000_000_001 || pool.EligibleExtenders != 3 || pool.ExtenderUnits != 2 ||
		pool.ExtenderPoolContracts != 7 || pool.ExtenderPoolPayers != 3 || len(pool.ExtenderCensus) != 1 || len(pool.ExtenderChunks) != 1 {
		t.Fatalf("extender pool %+v", pool)
	}
	if decoded.EligibleProviderBytes != 9_000_000_000_000 || decoded.PlatformEndpoints != 1 || decoded.FundedThrough["6"] != "1000000000000" {
		t.Fatalf("report %s", report)
	}
	want := map[[32]byte][2]string{
		stExtenderColdkey('a'): {"638297872340", "600000000000"},
		stExtenderColdkey('b'): {"265957446809", "275000000000"},
		stExtenderColdkey('c'): {"95744680851", "50000000000"},
		stExtenderColdkey('d'): {"0", "25000000000"},
		stExtenderColdkey('e'): {"0", "50000000000"},
	}
	if len(decoded.Coldkeys) != len(want) {
		t.Fatalf("coldkeys %+v", decoded.Coldkeys)
	}
	for i, entry := range decoded.Coldkeys {
		var coldkey [32]byte
		copy(coldkey[:], common.FromHex(entry.Coldkey))
		if amounts, ok := want[coldkey]; !ok || entry.Recorded != amounts[0] || entry.Shadow != amounts[1] {
			t.Fatalf("coldkey %d %+v, want %v", i, entry, amounts)
		}
		if i > 0 && entry.Coldkey <= decoded.Coldkeys[i-1].Coldkey {
			t.Fatal("coldkeys are not in ascending order")
		}
	}
	if decoded.Pending.Total != "1000000000000" || len(decoded.RecordedChunks) != 1 {
		t.Fatalf("report %s", report)
	}

	// the validator's recompute approves the shadow root from the report alone
	in, err := stExtenderShadowVerifyInput(report, shadow.Objects)
	if err != nil {
		t.Fatal(err)
	}
	in.MaxFetchBytes = ledger.DefaultMaxFetchBytes
	result, err := ledger.Verify(ctx, *in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Check != ledger.CheckNone {
		t.Fatalf("the recompute refused the shadow: %s: %v", result.Check, result.Err)
	}
	// and refuses one whose funding was misreported, or whose component is not
	// the one its epoch as E_x calls for
	tampered := strings.Replace(string(report), `"funded_through":{"6":"1000000000000"}`, `"funded_through":{"6":"1000000000001"}`, 1)
	if in, err := stExtenderShadowVerifyInput([]byte(tampered), shadow.Objects); err != nil {
		t.Fatal(err)
	} else {
		in.MaxFetchBytes = ledger.DefaultMaxFetchBytes
		if result, err := ledger.Verify(ctx, *in); err != nil || result.Check == ledger.CheckNone || result.Check == ledger.CheckFetch {
			t.Fatalf("a misreported funding passed: %+v, %v", result, err)
		}
	}
	if in, err := stExtenderShadowVerifyInput(report, shadow.Objects); err != nil {
		t.Fatal(err)
	} else {
		in.MaxFetchBytes = ledger.DefaultMaxFetchBytes
		in.ExtendersFrom = stExtenderExampleEpoch + 1
		if result, err := ledger.Verify(ctx, *in); err != nil || result.Check != ledger.CheckExtenders || !errors.Is(result.Err, ledger.ErrExtendersUnexpected) {
			t.Fatalf("a component outside the amendment passed: %+v, %v", result, err)
		}
	}
	for name, edit := range map[string]func(string) string{
		"another schema": func(s string) string {
			return strings.Replace(s, stExtenderShadowSchema, "urnetwork-extender-shadow-v0", 1)
		},
		"another E_x":   func(s string) string { return strings.Replace(s, `"extenders_from":6`, `"extenders_from":7`, 1) },
		"a stray field": func(s string) string { return strings.Replace(s, `{"schema"`, `{"surprise":1,"schema"`, 1) },
	} {
		if _, err := stExtenderShadowVerifyInput([]byte(edit(string(report))), shadow.Objects); err == nil {
			t.Fatalf("%s: read as a shadow report", name)
		}
	}
	// the same inputs build the same bytes, so a retry publishes nothing new
	again, _, err := stExtenderShadowBuild(ctx, stExtenderExampleInput(t))
	if err != nil || string(again) != string(report) {
		t.Fatalf("a rebuild differs: %v", err)
	}
}

// With no eligible extender the shadow drops Z: no weight and no share rows,
// and every coldkey's amount is what its amended rows alone earn.
func TestStExtenderShadowWithNoEligibleExtender(t *testing.T) {
	ctx := context.Background()
	input := stExtenderExampleInput(t)
	input.Census = []ledger.CensusRow{input.Census[3]}
	report, shadow, err := stExtenderShadowBuild(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded stExtenderShadowReport
	if err := json.Unmarshal(report, &decoded); err != nil {
		t.Fatal(err)
	}
	if pool := decoded.ExtenderPool; pool.ExtenderPoolBytes != 1_000_000_000_001 || pool.ExtenderPoolWeightBytes != 0 || pool.EligibleExtenders != 0 || pool.ExtenderUnits != 0 || len(pool.ExtenderChunks) != 0 {
		t.Fatalf("extender pool %+v", pool)
	}
	// ΣW = 9 TB: a 666.666666667, b 277.777777778, c 55.555555555 α
	shadowAmounts := map[string]string{}
	for _, entry := range decoded.Coldkeys {
		shadowAmounts[entry.Coldkey] = entry.Shadow
	}
	hex := func(name byte) string { coldkey := stExtenderColdkey(name); return "0x" + common.Bytes2Hex(coldkey[:]) }
	if shadowAmounts[hex('a')] != "666666666667" || shadowAmounts[hex('b')] != "277777777778" || shadowAmounts[hex('c')] != "55555555555" {
		t.Fatalf("amounts %v", shadowAmounts)
	}
	in, err := stExtenderShadowVerifyInput(report, shadow.Objects)
	if err != nil {
		t.Fatal(err)
	}
	in.MaxFetchBytes = ledger.DefaultMaxFetchBytes
	if result, err := ledger.Verify(ctx, *in); err != nil || result.Check != ledger.CheckNone {
		t.Fatalf("%+v, %v", result, err)
	}
}

// The cap: a Z above the epoch's eligible provider usage enters as W_k, so the
// extenders take exactly half of the pool.
func TestStExtenderShadowCapsZAtTheEligibleUsage(t *testing.T) {
	input := stExtenderExampleInput(t)
	input.Pool = &model.StEpochExtenderPool{IntraNetwork: 20_000_000_000_000, Contracts: 1, Payers: 1}
	report, _, err := stExtenderShadowBuild(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded stExtenderShadowReport
	if err := json.Unmarshal(report, &decoded); err != nil {
		t.Fatal(err)
	}
	if pool := decoded.ExtenderPool; pool.ExtenderPoolBytes != 20_000_000_000_000 || pool.ExtenderPoolWeightBytes != 9_000_000_000_000 {
		t.Fatalf("extender pool %+v", pool)
	}
	var extenders uint256.Int
	for _, entry := range decoded.Coldkeys {
		// d and e hold only share rows; b holds both, so take its share part
		switch entry.Coldkey {
		case "0x" + common.Bytes2Hex(func() []byte { k := stExtenderColdkey('d'); return k[:] }()), "0x" + common.Bytes2Hex(func() []byte { k := stExtenderColdkey('e'); return k[:] }()):
			amount, err := uint256.FromDecimal(entry.Shadow)
			if err != nil {
				t.Fatal(err)
			}
			extenders.Add(&extenders, amount)
		}
	}
	// Z' = 9 TB over ΣW = 18 TB: u1 4.5 TB (x1 2.25, x2 2.25), u2 4.5 TB (x3):
	// d 125 α and e 250 α
	if extenders.Dec() != "375000000000" {
		t.Fatalf("d and e took %s rao", extenders.Dec())
	}
}

// The offline replay of the worked example's epoch: the recorded rows exceed
// the amended rows by exactly the pooled shares, c's same-network 0.4 TB and
// the platform endpoint's 600,000,000,001 bytes, and nothing else moves.
func TestStExtenderReplayOfTheWorkedExample(t *testing.T) {
	recorded, amended, pool, _ := stExtenderExample()
	report := stExtenderReplayCompare(4, recorded, amended, pool)
	if !report.Exact || len(report.Mismatches) != 0 || report.PooledBytes != 1_000_000_000_001 || report.PooledOutsideRows != 0 ||
		report.RecordedBytes != 11_000_000_000_001 || report.AmendedBytes != 10_000_000_000_000 || report.PooledClients != 2 {
		t.Fatalf("replay %+v", report)
	}
	// unfunded and measurement shares leave the rows too, and are pooled bytes
	unfunded := *pool
	unfunded.IntraNetwork, unfunded.UnfundedBytes, unfunded.UnfundedContracts = 300_000_000_000, 100_000_000_000, 1
	if report := stExtenderReplayCompare(4, recorded, amended, &unfunded); !report.Exact || report.ExtenderPoolBytes != 900_000_000_001 || report.PooledBytes != 1_000_000_000_001 {
		t.Fatalf("replay %+v", report)
	}
	// a row that moved by anything but its pooled shares is a mismatch
	moved := append([]protocol.LedgerUsageRow{}, amended...)
	moved[2].UsageBytes = 500_000_000_001
	if report := stExtenderReplayCompare(4, recorded, moved, pool); report.Exact || len(report.Mismatches) != 1 || report.Mismatches[0].ClientId != server.Id(stExtenderClient(3)).String() {
		t.Fatalf("replay %+v", report)
	}
	// so is a coldkey or head flag that moved
	rekeyed := append([]protocol.LedgerUsageRow{}, amended...)
	rekeyed[0].Coldkey = stExtenderColdkey('z')
	if report := stExtenderReplayCompare(4, recorded, rekeyed, pool); report.Exact || len(report.Mismatches) != 1 {
		t.Fatalf("replay %+v", report)
	}
	flagged := append([]protocol.LedgerUsageRow{}, amended...)
	flagged[1].HeadExcluded = true
	if report := stExtenderReplayCompare(4, recorded, flagged, pool); report.Exact || len(report.Mismatches) != 1 {
		t.Fatalf("replay %+v", report)
	}
	// and a pooled client with no row either way, outside a roster's universe,
	// is counted apart
	outside := *pool
	outside.ClientIdPooledBytes = map[server.Id]model.ByteCount{server.Id(stExtenderClient(3)): 400_000_000_000, server.Id(stExtenderClient(5)): 600_000_000_001, server.Id(stExtenderClient(9)): 7}
	outside.IntraNetwork += 7
	if report := stExtenderReplayCompare(4, recorded, amended, &outside); !report.Exact || report.PooledOutsideRows != 7 {
		t.Fatalf("replay %+v", report)
	}
}

// The metrics carry Z and its parts, Z', W_k, N, M and the extenders' share of
// the pool weight, by phase, and nothing that names a client.
func TestStExtenderPoolMetrics(t *testing.T) {
	_, _, pool, census := stExtenderExample()
	split, err := ledger.SplitExtenderPool(pool.Bytes(), []protocol.LedgerUsageRow{{ClientID: stExtenderClient(1), Coldkey: stExtenderColdkey('a'), UsageBytes: 3_000_000_000_000}}, census)
	if err != nil {
		t.Fatal(err)
	}
	stExtenderPoolMetrics("shadow", 6, pool, split, 3_000_000_000_000, 1)
	for part, want := range map[string]float64{"pool": 1_000_000_000_001, "intra_platform": 600_000_000_001, "intra_network": 400_000_000_000, "weight": 1_000_000_000_001, "eligible_provider": 3_000_000_000_000} {
		if got := testutil.ToFloat64(stExtenderPoolBytes.WithLabelValues("shadow", part)); got != want {
			t.Fatalf("%s: %v, want %v", part, got, want)
		}
	}
	for count, want := range map[string]float64{"eligible": 3, "units": 2, "contracts": 7, "payers": 3, "platform_endpoints": 1} {
		if got := testutil.ToFloat64(stExtenderPoolCounts.WithLabelValues("shadow", count)); got != want {
			t.Fatalf("%s: %v, want %v", count, got, want)
		}
	}
	if got := testutil.ToFloat64(stExtenderPoolWeightShare.WithLabelValues("shadow")); got != 1_000_000_000_001.0/4_000_000_000_001.0 {
		t.Fatalf("weight share %v", got)
	}
	if got := testutil.ToFloat64(stExtenderPoolEpoch.WithLabelValues("shadow")); got != 6 {
		t.Fatalf("epoch %v", got)
	}
	stExtenderCensusMetrics(7, census)
	if got := testutil.ToFloat64(stExtenderPoolCounts.WithLabelValues("census", "eligible")); got != 3 {
		t.Fatalf("census eligible %v", got)
	}
	if stExtenderWeightShare(0, 0) != 0 || stExtenderWeightShare(5, 5) != 0.5 {
		t.Fatal("weight share")
	}
}

// An epoch with an extender component publishes a version 2 statement under
// epoch-usage-v2, whose total holds all of Z; the statement validators will
// check against the header passes those checks. Every other epoch's statement
// stays version 1.
func TestStLedgerV2SealsAVersion2StatementForAnExtenderComponent(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &StConfig{ArtifactKey: key, Netuid: 25, ChainId: 964, DeploymentId: "sn25-test", NoId: 1,
		ContractAddress: common.HexToAddress("0x1"), VaultV2: common.HexToAddress("0x2")}
	authority := &StPayoutEpochAuthority{Epoch: 8, PolicyHash: [32]byte{3}, EndTime: time.Date(2026, 12, 4, 18, 25, 0, 0, time.UTC)}
	authority.Start.Block, authority.Start.Hash = 9_600_028, [32]byte{4}
	authority.End.Block, authority.End.Hash = 9_650_428, [32]byte{5}
	_, amended, pool, census := stExtenderExample()
	split, err := ledger.SplitExtenderPool(pool.Bytes(), amended, census)
	if err != nil {
		t.Fatal(err)
	}
	censusChunks, shareChunks := ledger.EncodeCensusChunks(census), ledger.EncodeProviderChunks(split.Rows)
	header := &ledger.HeaderExtenders{Bytes: pool.Bytes(), Census: []ledger.Ref{ledger.RefOf(censusChunks[0])}, Rows: []ledger.Ref{ledger.RefOf(shareChunks[0])}}
	committed, err := usage.CommittedExtenders(header, census, split.Rows)
	if err != nil {
		t.Fatal(err)
	}
	extenderPool, err := usage.NewExtenderPool(uint64(pool.IntraPlatform), uint64(pool.IntraNetwork), pool.Contracts, pool.Payers, committed)
	if err != nil {
		t.Fatal(err)
	}
	providerBytes, count, err := usage.SumRows(amended)
	if err != nil {
		t.Fatal(err)
	}
	total, err := usage.TotalUsage(providerBytes, pool.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	chunks := []string{ledger.RefOf(ledger.EncodeProviderChunks(amended)[0]).String()}
	statement, payload, envelope, err := stLedgerV2SealStatement(cfg, &stLedgerV2StatementInput{Epoch: 8, Authority: authority, Chunks: chunks, TotalBytes: total, Rows: count, Users: 11, Extenders: extenderPool})
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != usage.EvidenceKindV2 || statement.Schema != usage.SchemaV2 || statement.TotalUsageBytes != 11_000_000_000_001 {
		t.Fatalf("published %s as %s with %d bytes", statement.Schema, envelope.Kind, statement.TotalUsageBytes)
	}
	decoded, err := usage.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.Verify(crypto.PubkeyToAddress(key.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if err := decoded.CheckExtenders(committed); err != nil {
		t.Fatal(err)
	}
	if err := decoded.CheckSums(providerBytes, count); err != nil {
		t.Fatal(err)
	}
	// the deposit sizes from the total, Z included
	usageBytes, users, err := stDepositStatementUsage(decoded, cfg, 8, authority)
	if err != nil || usageBytes != 11_000_000_000_001 || users != 11 {
		t.Fatalf("deposit usage %d bytes and %d users, %v", usageBytes, users, err)
	}
	// without a component the statement is version 1
	v1, _, v1Envelope, err := stLedgerV2SealStatement(cfg, &stLedgerV2StatementInput{Epoch: 8, Authority: authority, Chunks: chunks, TotalBytes: providerBytes, Rows: count, Users: 11})
	if err != nil {
		t.Fatal(err)
	}
	if v1Envelope.Kind != usage.EvidenceKind || v1.Schema != usage.Schema || v1.ExtenderPool != nil {
		t.Fatalf("published %s as %s", v1.Schema, v1Envelope.Kind)
	}
}
