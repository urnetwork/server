package controller

// st_ledger_v2_prune_test.go — the SN25 prune-risk alert (VERSION2-FINAL.md
// §8.1, §17 dissolution row), SV-1 (alerts): runtime 475's dissolution order
// against fixtures, the alert once per subject, the throttled census read,
// the read against a substrate JSON-RPC fake serving storage captured from
// mainnet, every refusal of a malformed answer, and the sync turn's check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	stconn "github.com/urnetwork/server/st"
)

// pruneSubnet is a census subnet with a moving price in 2^-32 TAO per alpha.
func pruneSubnet(netuid uint16, registeredAt uint64, price uint64) stLedgerV2Subnet {
	return stLedgerV2Subnet{Netuid: netuid, RegisteredAt: registeredAt, MovingPrice: *uint256.NewInt(price)}
}

func pruneNetuids(subnets []stLedgerV2Subnet) []uint16 {
	netuids := []uint16{}
	for _, subnet := range subnets {
		netuids = append(netuids, subnet.Netuid)
	}
	return netuids
}

func pruneTestConfig(netuid uint64, positions uint64) *StConfig {
	cfg := ledgerV2TestConfig()
	cfg.Netuid, cfg.PruneAlertPositions = netuid, positions
	return cfg
}

// pruneRankCensus is seven subnets at block 10,000 under a limit of seven:
// subnets 1-5 at moving prices 10-50, registered at blocks 1-5, subnet 9
// immune at price 1, and SN25 registered at block 100 at the given price.
func pruneRankCensus(sn25 uint64) *stLedgerV2Census {
	census := &stLedgerV2Census{Block: 10_000, SubnetLimit: 7, ImmunityPeriod: 1_000}
	for i := uint64(1); i <= 5; i++ {
		census.Subnets = append(census.Subnets, pruneSubnet(uint16(i), i, 10*i))
	}
	census.Subnets = append(census.Subnets, pruneSubnet(9, 9_500, 1), pruneSubnet(25, 100, sn25))
	return census
}

// capturePruneAlertMessages records each alert as "name subject: message".
func capturePruneAlertMessages(t testing.TB) *[]string {
	alerts := &[]string{}
	sink := stLedgerV2AlertSink
	stLedgerV2AlertSink = func(name string, subject string, message string) {
		*alerts = append(*alerts, name+" "+subject+": "+message)
	}
	stLedgerV2Alerted.Clear()
	t.Cleanup(func() {
		stLedgerV2AlertSink = sink
		stLedgerV2Alerted.Clear()
	})
	return alerts
}

// SV-1 (alerts): the dissolution order skips immune subnets (immune before
// block RegisteredAt + ImmunityPeriod, a sum that saturates), orders by
// moving price, breaks a price tie by the earlier registration and an exact
// tie by storage order, the netuid's little-endian bytes.
func TestStLedgerV2PruneOrder(t *testing.T) {
	census := &stLedgerV2Census{Block: 1_000, ImmunityPeriod: 100, Subnets: []stLedgerV2Subnet{
		pruneSubnet(1, 10, 500),
		pruneSubnet(2, 901, 1),
		pruneSubnet(3, 900, 2),
		pruneSubnet(4, 20, 500),
		pruneSubnet(5, 30, 7),
		pruneSubnet(256, 30, 7),
		pruneSubnet(6, math.MaxUint64-5, 0),
		pruneSubnet(7, 1_001, 0),
	}}
	// Subnet 2 is immune until block 1,001 and subnet 3 until 1,000; subnets
	// 6 and 7 register after the block. Subnet 256 is 0x0001 in storage, so it
	// precedes subnet 5, 0x0500.
	if got := pruneNetuids(census.pruneOrder()); !slices.Equal(got, []uint16{3, 256, 5, 1, 4}) {
		t.Fatalf("order at block 1000 %v", got)
	}
	census.Block = 1_001
	if got := pruneNetuids(census.pruneOrder()); !slices.Equal(got, []uint16{2, 3, 256, 5, 1, 4}) {
		t.Fatalf("order at block 1001 %v", got)
	}
	// Subnet 6's immunity ends past 2^64 and saturates, so it stays immune.
	census.Block = math.MaxUint64 - 1
	if got := pruneNetuids(census.pruneOrder()); !slices.Equal(got, []uint16{7, 2, 3, 256, 5, 1, 4}) {
		t.Fatalf("order at block 2^64-2 %v", got)
	}
	if got := pruneNetuids(census.Subnets); !slices.Equal(got, []uint16{1, 2, 3, 4, 5, 256, 6, 7}) {
		t.Fatalf("the order reordered the census: %v", got)
	}
}

// SV-1 (alerts): a registration dissolves a subnet exactly when the added
// subnets and those still being dissolved reach the limit and the dissolving
// ones do not free a netuid for each queued registration.
func TestStLedgerV2CensusPrunesAtTheLimit(t *testing.T) {
	for _, test := range []struct {
		name                   string
		added                  int
		limit                  uint16
		cleanup, registrations uint64
		prunes                 bool
	}{
		{"below the limit", 3, 4, 0, 0, false},
		{"exactly at the limit", 4, 4, 0, 0, true},
		{"over a lowered limit", 5, 4, 0, 0, true},
		{"dissolving subnets fill the limit", 3, 4, 1, 1, true},
		{"a dissolving subnet frees a netuid", 3, 4, 1, 0, false},
		{"below the limit with dissolving subnets", 2, 4, 1, 1, false},
	} {
		census := &stLedgerV2Census{SubnetLimit: test.limit, CleanupQueue: test.cleanup, RegistrationQueue: test.registrations}
		for i := range test.added {
			census.Subnets = append(census.Subnets, pruneSubnet(uint16(i+1), 0, 1))
		}
		if got := census.prunes(); got != test.prunes {
			t.Fatalf("%s: prunes %t", test.name, got)
		}
	}
}

// SV-1 (alerts): the alert fires while SN25 ranks among the lowest
// PruneAlertPositions non-immune subnets, three by default, its subject
// naming the rank and whether a registration now dissolves a subnet; it
// fires when SN25 is not added, and never for an immune or lower-risk SN25.
func TestStLedgerV2PruneAlertRanks(t *testing.T) {
	limit := func(limit uint16) func(*stLedgerV2Census) {
		return func(census *stLedgerV2Census) { census.SubnetLimit = limit }
	}
	for _, test := range []struct {
		name      string
		positions uint64
		sn25      uint64
		change    func(*stLedgerV2Census)
		want      []string
	}{
		{"lowest at the limit", 0, 5, nil, []string{"v2_sn25_prune_risk netuid 25 rank 1 at the limit"}},
		{"lowest below the limit", 0, 5, limit(8), []string{"v2_sn25_prune_risk netuid 25 rank 1"}},
		{"third at the limit", 0, 25, nil, []string{"v2_sn25_prune_risk netuid 25 rank 3 at the limit"}},
		{"third below the limit", 0, 25, limit(8), []string{"v2_sn25_prune_risk netuid 25 rank 3"}},
		{"fourth", 0, 35, nil, []string{}},
		{"last", 0, 60, nil, []string{}},
		{"a price tie ranks the earlier registration first", 4, 30, nil, []string{"v2_sn25_prune_risk netuid 25 rank 4 at the limit"}},
		{"one position", 1, 15, nil, []string{}},
		{"two positions", 2, 15, nil, []string{"v2_sn25_prune_risk netuid 25 rank 2 at the limit"}},
		{"immune", 0, 5, func(census *stLedgerV2Census) { census.Subnets[6].RegisteredAt = 9_200 }, []string{}},
		{"dissolving subnets fill the limit", 0, 5, func(census *stLedgerV2Census) {
			census.SubnetLimit, census.CleanupQueue, census.RegistrationQueue = 8, 1, 1
		}, []string{"v2_sn25_prune_risk netuid 25 rank 1 at the limit"}},
		{"a dissolving subnet frees a netuid", 0, 5, func(census *stLedgerV2Census) {
			census.SubnetLimit, census.CleanupQueue = 8, 1
		}, []string{"v2_sn25_prune_risk netuid 25 rank 1"}},
		{"not added", 0, 5, func(census *stLedgerV2Census) { census.Subnets = census.Subnets[:6] }, []string{"v2_sn25_prune_risk netuid 25 not added"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			alerts := captureLedgerV2Alerts(t)
			census := pruneRankCensus(test.sn25)
			if test.change != nil {
				test.change(census)
			}
			stLedgerV2PruneAlert(pruneTestConfig(25, test.positions), census)
			if !reflect.DeepEqual(*alerts, test.want) {
				t.Fatalf("alerts %q, want %q", *alerts, test.want)
			}
		})
	}
}

// SV-1 (alerts): each subject alerts once per process and deployment; a step
// closer, a lower rank or the limit reached, is a new subject and alerts.
func TestStLedgerV2PruneAlertOncePerSubject(t *testing.T) {
	alerts := captureLedgerV2Alerts(t)
	cfg := pruneTestConfig(25, 0)
	check := func(cfg *StConfig, sn25 uint64, limit uint16) {
		census := pruneRankCensus(sn25)
		census.SubnetLimit = limit
		stLedgerV2PruneAlert(cfg, census)
	}
	check(cfg, 15, 8)
	check(cfg, 15, 8)
	check(cfg, 5, 8)
	check(cfg, 5, 7)
	check(cfg, 15, 8)
	check(cfg, 5, 8)
	check(cfg, 5, 7)
	other := pruneTestConfig(25, 0)
	other.ChainId = 964
	check(other, 15, 8)
	want := []string{
		"v2_sn25_prune_risk netuid 25 rank 2",
		"v2_sn25_prune_risk netuid 25 rank 1",
		"v2_sn25_prune_risk netuid 25 rank 1 at the limit",
		"v2_sn25_prune_risk netuid 25 rank 2",
	}
	if !reflect.DeepEqual(*alerts, want) {
		t.Fatalf("alerts %q, want %q", *alerts, want)
	}
}

// countingCensusReader is a v2 client that reads the census.
type countingCensusReader struct {
	StLedgerV2Client
	census *stLedgerV2Census
	err    error
	reads  int
}

func (self *countingCensusReader) ledgerV2Census(ctx context.Context) (*stLedgerV2Census, error) {
	self.reads++
	return self.census, self.err
}

// SV-1 (alerts): the census is read at most once per ten minutes per
// deployment and subnet, a failed read included, and a failed read returns
// its error; a client that cannot read the census is not checked.
func TestStLedgerV2PruneCheckThrottlesTheRead(t *testing.T) {
	resetLedgerV2Caches()
	t.Cleanup(resetLedgerV2Caches)
	alerts := captureLedgerV2Alerts(t)
	ctx, cfg := context.Background(), pruneTestConfig(25, 0)
	reader := &countingCensusReader{census: pruneRankCensus(5)}
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	check := func(cfg *StConfig, client StLedgerV2Client, at time.Duration, reads int, want string) {
		t.Helper()
		err := stLedgerV2CheckPrune(ctx, cfg, client, start.Add(at))
		if fmt.Sprint(err) != want || reader.reads != reads {
			t.Fatalf("at %s: error %v reads %d, want %s and %d", at, err, reader.reads, want, reads)
		}
	}
	interval := stLedgerV2PruneCheckInterval
	check(cfg, reader, 0, 1, "<nil>")
	check(cfg, reader, interval-time.Nanosecond, 1, "<nil>")
	check(cfg, reader, interval, 2, "<nil>")
	reader.err = errors.New("census refused")
	check(cfg, reader, 2*interval, 3, "subnet census: census refused")
	check(cfg, reader, 3*interval-time.Nanosecond, 3, "<nil>")
	reader.err = nil
	check(cfg, reader, 3*interval, 4, "<nil>")
	check(pruneTestConfig(26, 0), reader, 3*interval, 5, "<nil>")
	check(cfg, newFakeLedgerV2Chain(&[]string{}), 10*interval, 5, "<nil>")
	want := []string{"v2_sn25_prune_risk netuid 25 rank 1 at the limit", "v2_sn25_prune_risk netuid 26 not added"}
	if !reflect.DeepEqual(*alerts, want) {
		t.Fatalf("alerts %q, want %q", *alerts, want)
	}
}

// SV-1 (alerts): the census keys are SubtensorModule storage keys, a map key
// under its Identity hasher; these read the mainnet census fixture.
func TestStSubtensorStorageKeys(t *testing.T) {
	const pallet = "0x658faa385070e074c85bf6b568cf0555"
	for _, test := range []struct {
		item   string
		netuid []uint16
		want   string
	}{
		{"NetworksAdded", []uint16{25}, pallet + "0e30450fc4d507a846032a7fa65d9a43" + "1900"},
		{"NetworkRegisteredAt", []uint16{25}, pallet + "271d29b9b717ce3d8c571f1cbc180fa2" + "1900"},
		{"SubnetMechanism", []uint16{300}, pallet + "306afce653cf1dfd6333a3c30d8d347e" + "2c01"},
		{"SubnetMovingPrice", []uint16{25}, pallet + "1abf1b0f4fd14f7b72ee50f9d91d5915" + "1900"},
		{"SubnetLimit", nil, pallet + "a336e39c4086c4d5f43b7e6dff8a04f6"},
		{"NetworkImmunityPeriod", nil, pallet + "be4bea261cfe8a2b911e689c943bbd43"},
		{"DissolveCleanupQueue", nil, pallet + "1b3f4fb9309f90f6e26d5767a2118eca"},
		{"NetworkRegistrationQueue", nil, pallet + "7317bfa2e1d07976bf90ee457ab92d9c"},
	} {
		if got := hexutil.Encode(stSubtensorStorageKey(test.item, test.netuid...)); got != test.want {
			t.Fatalf("%s%v key %s, want %s", test.item, test.netuid, got, test.want)
		}
	}
}

// fakeSubtensorStorage serves the substrate JSON-RPC methods the census read
// uses, and eth_chainId for the endpoint dial, over one storage map at one
// finalized block.
type fakeSubtensorStorage struct {
	block   uint64
	hash    string
	storage map[string]string
	// Faults: a method's refusal, an unknown header, the number of change
	// sets per answer, and a key left unanswered.
	fail     map[string]error
	noHeader bool
	sets     int
	omit     string
	// The methods called, the paging start keys ("" for none), and the keys
	// of the last state_queryStorageAt.
	calls   []string
	starts  []string
	queried []string
}

func newFakeSubtensorStorage() *fakeSubtensorStorage {
	return &fakeSubtensorStorage{hash: "0x" + strings.Repeat("ab", 32), storage: map[string]string{}, fail: map[string]error{}, sets: 1}
}

// set stores an item's value, under a map key when netuid is given.
func (self *fakeSubtensorStorage) set(item string, value string, netuid ...uint16) {
	self.storage[hexutil.Encode(stSubtensorStorageKey(item, netuid...))] = value
}

func (self *fakeSubtensorStorage) called(method string, at string) error {
	self.calls = append(self.calls, method)
	if err := self.fail[method]; err != nil {
		return err
	}
	if at != self.hash {
		return fmt.Errorf("unknown block %s", at)
	}
	return nil
}

// client serves the fake over JSON-RPC and dials a core client to it.
func (self *fakeSubtensorStorage) client(t testing.TB) *CoreStClient {
	t.Helper()
	rpcServer := rpc.NewServer()
	for name, api := range map[string]any{"eth": fakeSubtensorEthApi{}, "chain": fakeSubtensorChainApi{self}, "state": fakeSubtensorStateApi{self}} {
		if err := rpcServer.RegisterName(name, api); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(rpcServer.Stop)
	return newStBlockBatchClient(t, rpcServer)
}

type fakeSubtensorEthApi struct{}

func (fakeSubtensorEthApi) ChainId() hexutil.Uint64 {
	return 945
}

type fakeSubtensorChainApi struct {
	fake *fakeSubtensorStorage
}

func (self fakeSubtensorChainApi) GetFinalizedHead() (string, error) {
	return self.fake.hash, self.fake.called("chain_getFinalizedHead", self.fake.hash)
}

type fakeSubtensorHeader struct {
	Number hexutil.Uint64 `json:"number"`
}

func (self fakeSubtensorChainApi) GetHeader(hash string) (*fakeSubtensorHeader, error) {
	if err := self.fake.called("chain_getHeader", hash); err != nil || self.fake.noHeader {
		return nil, err
	}
	return &fakeSubtensorHeader{Number: hexutil.Uint64(self.fake.block)}, nil
}

type fakeSubtensorStateApi struct {
	fake *fakeSubtensorStorage
}

// GetKeysPaged returns up to count keys with the prefix after start, in
// storage order.
func (self fakeSubtensorStateApi) GetKeysPaged(prefix string, count int, start *string, at string) ([]string, error) {
	if err := self.fake.called("state_getKeysPaged", at); err != nil {
		return nil, err
	}
	after := ""
	if start != nil {
		after = *start
	}
	self.fake.starts = append(self.fake.starts, after)
	keys := []string{}
	for key := range self.fake.storage {
		if strings.HasPrefix(key, prefix) && after < key {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys[:min(count, len(keys))], nil
}

type fakeSubtensorChangeSet struct {
	Block   string       `json:"block"`
	Changes [][2]*string `json:"changes"`
}

func (self fakeSubtensorStateApi) QueryStorageAt(keys []string, at string) ([]fakeSubtensorChangeSet, error) {
	if err := self.fake.called("state_queryStorageAt", at); err != nil {
		return nil, err
	}
	self.fake.queried = keys
	set := fakeSubtensorChangeSet{Block: at, Changes: [][2]*string{}}
	for _, key := range keys {
		if key == self.fake.omit {
			continue
		}
		var value *string
		if stored, ok := self.fake.storage[key]; ok {
			value = &stored
		}
		set.Changes = append(set.Changes, [2]*string{&key, value})
	}
	sets := []fakeSubtensorChangeSet{}
	for range self.fake.sets {
		sets = append(sets, set)
	}
	return sets, nil
}

// mainnetCensusStorage serves the SubtensorModule storage read from mainnet's
// entrypoint at finalized block 9,263,450
// (testdata/st_ledger_v2_mainnet_census.json): the census items' raw SCALE
// values, each subnet's as NetworksAdded, NetworkRegisteredAt,
// SubnetMechanism and SubnetMovingPrice, null when absent.
func mainnetCensusStorage(t testing.TB) *fakeSubtensorStorage {
	t.Helper()
	raw, err := os.ReadFile("testdata/st_ledger_v2_mainnet_census.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Block   uint64                `json:"block"`
		Hash    string                `json:"hash"`
		Globals map[string]*string    `json:"globals"`
		Subnets map[string][4]*string `json:"subnets"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	fake := newFakeSubtensorStorage()
	fake.block, fake.hash = fixture.Block, fixture.Hash
	for item, value := range fixture.Globals {
		if value != nil {
			fake.set(item, *value)
		}
	}
	for key, values := range fixture.Subnets {
		netuid, err := strconv.ParseUint(key, 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		for i, item := range []string{"NetworksAdded", "NetworkRegisteredAt", "SubnetMechanism", "SubnetMovingPrice"} {
			if values[i] != nil {
				fake.set(item, *values[i], uint16(netuid))
			}
		}
	}
	return fake
}

// SV-1 (alerts): the census read decodes mainnet's own storage. At block
// 9,263,450 the 128 added subnets fill the limit of 128, so a registration
// dissolves subnet 47, the lowest of 112 non-immune subnets, and SN25 ranks
// 75th; the alert fires once the positions reach that rank.
func TestStLedgerV2CensusReadsMainnetStorage(t *testing.T) {
	fake := mainnetCensusStorage(t)
	census, err := fake.client(t).ledgerV2Census(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if census.Block != 9_263_450 || census.SubnetLimit != 128 || census.ImmunityPeriod != 864_000 || census.CleanupQueue != 0 || census.RegistrationQueue != 0 || len(census.Subnets) != 128 || !census.prunes() {
		t.Fatalf("census at block %d: limit %d immunity %d queues %d/%d subnets %d", census.Block, census.SubnetLimit, census.ImmunityPeriod, census.CleanupQueue, census.RegistrationQueue, len(census.Subnets))
	}
	if got := pruneNetuids(census.Subnets); got[0] != 1 || got[127] != 128 || !slices.IsSorted(got) {
		t.Fatalf("subnets %v", got)
	}
	order := census.pruneOrder()
	if len(order) != 112 || !slices.Equal(pruneNetuids(order[:5]), []uint16{47, 72, 42, 106, 98}) || order[74].Netuid != 25 {
		t.Fatalf("order of %d: %v", len(order), pruneNetuids(order))
	}
	if !slices.Equal(fake.calls, []string{"chain_getFinalizedHead", "chain_getHeader", "state_getKeysPaged", "state_queryStorageAt"}) || len(fake.queried) != 4+128*4 {
		t.Fatalf("calls %v querying %d keys", fake.calls, len(fake.queried))
	}

	alerts := capturePruneAlertMessages(t)
	stLedgerV2PruneAlert(pruneTestConfig(25, 74), census)
	stLedgerV2PruneAlert(pruneTestConfig(25, 75), census)
	want := []string{"v2_sn25_prune_risk netuid 25 rank 75 at the limit: at block 9263450 subnet 25 ranks 75 of 112 non-immune subnets by moving price, lowest first (0.006301022 TAO per alpha, the lowest 0.001735119); 128 subnets are added and 0 dissolving under a limit of 128, so a registration now dissolves subnet 47"}
	if !reflect.DeepEqual(*alerts, want) {
		t.Fatalf("alerts %q\nwant %q", *alerts, want)
	}
}

// SV-1 (alerts): absent values read as the runtime's defaults (a limit of
// 128, an immunity of 1,296,000 blocks, empty queues, registration at block
// 0, a moving price of zero, the stable mechanism); a stable subnet's price is
// 1.0 whatever its moving price; a negative moving price reads as zero and
// ties an absent one; root and subnets NetworksAdded marks false are left
// out; queue lengths decode wider compact prefixes; and the NetworksAdded
// keys are read page by page.
func TestStLedgerV2CensusReadsStorageDefaults(t *testing.T) {
	fake := newFakeSubtensorStorage()
	fake.block = 2_000_000
	fake.set("NetworksAdded", "0x01", 0)
	fake.set("NetworksAdded", "0x00", 3)
	fake.set("NetworksAdded", "0x01", 4)
	fake.set("SubnetMovingPrice", "0x01000000000000000000000000000000", 4)
	for netuid, registeredAt := range map[uint16]string{5: "0x0a00000000000000", 6: "0x1400000000000000", 300: "0x1e00000000000000"} {
		fake.set("NetworksAdded", "0x01", netuid)
		fake.set("SubnetMechanism", "0x0100", netuid)
		fake.set("NetworkRegisteredAt", registeredAt, netuid)
	}
	fake.set("SubnetMovingPrice", "0x"+strings.Repeat("ff", 16), 5)
	fake.set("SubnetMovingPrice", "0x00000001000000000000000000000000", 300)
	fake.set("DissolveCleanupQueue", "0x0101")
	fake.set("NetworkRegistrationQueue", "0x02000100")
	for netuid := uint16(1_000); netuid < 2_100; netuid++ {
		fake.set("NetworksAdded", "0x00", netuid)
	}

	census, err := fake.client(t).ledgerV2Census(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := &stLedgerV2Census{Block: 2_000_000, SubnetLimit: 128, ImmunityPeriod: 1_296_000, CleanupQueue: 64, RegistrationQueue: 16_384, Subnets: []stLedgerV2Subnet{
		{Netuid: 4, MovingPrice: *uint256.NewInt(1 << 32)},
		{Netuid: 5, RegisteredAt: 10},
		{Netuid: 6, RegisteredAt: 20},
		{Netuid: 300, RegisteredAt: 30, MovingPrice: *uint256.NewInt(1 << 24)},
	}}
	if !reflect.DeepEqual(census, want) {
		t.Fatalf("census %+v\nwant %+v", census, want)
	}
	if got := pruneNetuids(census.pruneOrder()); !slices.Equal(got, []uint16{5, 6, 300, 4}) || census.prunes() {
		t.Fatalf("order %v prunes %t", got, census.prunes())
	}
	prefix := hexutil.Encode(stSubtensorStorageKey("NetworksAdded"))
	keys := []string{}
	for key := range fake.storage {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	if !slices.Equal(fake.starts, []string{"", keys[stLedgerV2CensusPageSize-1]}) || len(fake.queried) != 4+(len(keys)-1)*4 {
		t.Fatalf("pages after %q, querying %d keys", fake.starts, len(fake.queried))
	}
}

// SV-1 (alerts): every refusal of the census read, each through the core
// client's endpoint loop, which reports no complete observation.
func TestStLedgerV2CensusRefusesMalformedAnswers(t *testing.T) {
	refused := errors.New("refused")
	for _, test := range []struct {
		name   string
		change func(*fakeSubtensorStorage)
		want   string
	}{
		{"finalized head refused", func(f *fakeSubtensorStorage) { f.fail["chain_getFinalizedHead"] = refused }, "chain_getFinalizedHead: refused"},
		{"header refused", func(f *fakeSubtensorStorage) { f.fail["chain_getHeader"] = refused }, "chain_getHeader: refused"},
		{"no header", func(f *fakeSubtensorStorage) { f.noHeader = true }, "chain_getHeader: no header for 0xabab"},
		{"keys refused", func(f *fakeSubtensorStorage) { f.fail["state_getKeysPaged"] = refused }, "state_getKeysPaged: refused"},
		{"malformed key", func(f *fakeSubtensorStorage) {
			f.storage[hexutil.Encode(stSubtensorStorageKey("NetworksAdded"))+"010203"] = "0x01"
		}, "state_getKeysPaged: malformed NetworksAdded key"},
		{"query refused", func(f *fakeSubtensorStorage) { f.fail["state_queryStorageAt"] = refused }, "state_queryStorageAt: refused"},
		{"no change set", func(f *fakeSubtensorStorage) { f.sets = 0 }, "state_queryStorageAt: 0 change sets, want 1"},
		{"two change sets", func(f *fakeSubtensorStorage) { f.sets = 2 }, "state_queryStorageAt: 2 change sets, want 1"},
		{"unanswered key", func(f *fakeSubtensorStorage) { f.omit = hexutil.Encode(stSubtensorStorageKey("SubnetLimit")) }, "state_queryStorageAt: no answer for SubnetLimit[]"},
		{"not hex", func(f *fakeSubtensorStorage) { f.set("NetworkImmunityPeriod", "0xzz") }, "state_queryStorageAt: malformed NetworkImmunityPeriod[]"},
		{"short limit", func(f *fakeSubtensorStorage) { f.set("SubnetLimit", "0x80") }, "state_queryStorageAt: malformed SubnetLimit[]"},
		{"long registration", func(f *fakeSubtensorStorage) { f.set("NetworkRegisteredAt", "0x000000000000000000", 1) }, "state_queryStorageAt: malformed NetworkRegisteredAt[1]"},
		{"short mechanism", func(f *fakeSubtensorStorage) { f.set("SubnetMechanism", "0x01", 1) }, "state_queryStorageAt: malformed SubnetMechanism[1]"},
		{"short price", func(f *fakeSubtensorStorage) { f.set("SubnetMovingPrice", "0x01", 1) }, "state_queryStorageAt: malformed SubnetMovingPrice[1]"},
		{"NetworksAdded not a bool", func(f *fakeSubtensorStorage) { f.set("NetworksAdded", "0x02", 1) }, "state_queryStorageAt: malformed NetworksAdded[1]"},
		{"empty queue", func(f *fakeSubtensorStorage) { f.set("DissolveCleanupQueue", "0x") }, "state_queryStorageAt: malformed DissolveCleanupQueue[]"},
		{"queue length past 64 bits", func(f *fakeSubtensorStorage) { f.set("NetworkRegistrationQueue", "0x17"+strings.Repeat("ff", 9)) }, "state_queryStorageAt: malformed NetworkRegistrationQueue[]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeSubtensorStorage()
			fake.block = 100
			fake.set("NetworksAdded", "0x01", 1)
			test.change(fake)
			census, err := fake.client(t).ledgerV2Census(context.Background())
			if census != nil || err == nil || !strings.HasPrefix(err.Error(), "st: no complete RPC observation: ") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("census %+v error %v, want %q", census, err, test.want)
			}
		})
	}
}

// SV-1 (alerts): st.yml sets the alert's positions per profile, with no
// fallback between profiles, under keys the go-live check knows.
func TestStConfigSelectsPruneAlertPositions(t *testing.T) {
	var file stVaultFile
	if err := yaml.Unmarshal([]byte("v2_prune_alert_positions: 7\ntestnet-v2-prune-alert-positions: 5\n"), &file); err != nil {
		t.Fatal(err)
	}
	if file.PruneAlertPositions != 7 || file.TestnetPruneAlertPositions != 5 {
		t.Fatalf("decoded %d and %d", file.PruneAlertPositions, file.TestnetPruneAlertPositions)
	}
	f := releaseStVaultFile()
	f.PruneAlertPositions = 7
	if cfg, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil || cfg.PruneAlertPositions != 0 {
		t.Fatalf("testnet selected the mainnet positions: %v %+v", err, cfg)
	}
	f.TestnetPruneAlertPositions = 5
	if cfg, err := stConfigForProfile(stconn.ProfileTestnet, f, []string{"http://testnet"}); err != nil || cfg.PruneAlertPositions != 5 {
		t.Fatalf("testnet positions: %v %+v", err, cfg)
	}
	if cfg, err := stConfigForProfile(stconn.ProfileMainnet, f, []string{"http://mainnet"}); err != nil || cfg.PruneAlertPositions != 7 {
		t.Fatalf("mainnet positions: %v %+v", err, cfg)
	}
	keys := stVaultFileKeys()
	if !slices.Contains(keys, "v2_prune_alert_positions") || !slices.Contains(keys, "testnet-v2-prune-alert-positions") {
		t.Fatalf("loader keys %v", keys)
	}
}

// SV-1 (alerts): each sync turn checks the prune risk first, before vault2
// registers the pool too, at most once per ten minutes; a failed census read
// is logged and the turn plans its steps.
func TestStLedgerV2SyncChecksPruneRisk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		f.cfg.Netuid = 25
		reader := &countingCensusReader{StLedgerV2Client: f.chain, census: pruneRankCensus(5)}
		SetStLedgerV2Client(reader)
		f.chain.registered = false
		if steps := ledgerV2Sync(t, f, 6); len(steps) != 0 || reader.reads != 1 {
			t.Fatalf("unregistered pool: steps %v reads %d", steps, reader.reads)
		}
		want := []string{"v2_sn25_prune_risk netuid 25 rank 1 at the limit"}
		if !reflect.DeepEqual(*f.alerts, want) {
			t.Fatalf("alerts %q, want %q", *f.alerts, want)
		}

		f.chain.registered = true
		stLedgerV2Pools.Clear()
		f.chain.head = f.chain.end(5) + 10
		epoch5 := []StLedgerV2Step{{Kind: StLedgerV2StepEpoch, Epoch: 5}}
		if steps := ledgerV2Sync(t, f, 6); !reflect.DeepEqual(steps, epoch5) || reader.reads != 1 {
			t.Fatalf("throttled turn: steps %v reads %d", steps, reader.reads)
		}
		stLedgerV2PruneChecks.Clear()
		reader.err = errors.New("census refused")
		if steps := ledgerV2Sync(t, f, 6); !reflect.DeepEqual(steps, epoch5) || reader.reads != 2 {
			t.Fatalf("failed census: steps %v reads %d", steps, reader.reads)
		}
		if !reflect.DeepEqual(*f.alerts, want) {
			t.Fatalf("alerts %q, want %q", *f.alerts, want)
		}
	})
}
