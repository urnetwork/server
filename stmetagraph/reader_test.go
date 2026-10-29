// The reader against a synthetic native chain that serves the exact public
// Bittensor runtime 486 metadata recorded by the sibling sn repository
// (crv4/testdata) and SCALE-encoded storage whose keys come from that
// metadata, so the tests exercise the real key derivation, shape checks and
// decoders without a live chain. Identities and values are synthetic.
package stmetagraph

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

var testMetadataOnce = sync.OnceValues(func() ([]byte, error) {
	encoded, err := os.ReadFile("../../sn/crv4/testdata/runtime486-metadata.scale.gz.base64")
	if err != nil {
		return nil, err
	}
	compressed, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
})

func testMetadata(t *testing.T) ([]byte, *types.Metadata) {
	t.Helper()
	raw, err := testMetadataOnce()
	if err != nil {
		t.Fatalf("recorded runtime metadata is unavailable: %v", err)
	}
	meta := &types.Metadata{}
	if err := codec.Decode(raw, meta); err != nil {
		t.Fatal(err)
	}
	return raw, meta
}

var testGenesis = common.HexToHash("0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03")

func testBlockHash(number uint64, generation byte) common.Hash {
	var hash common.Hash
	hash[0], hash[1] = 0x0b, generation
	big.NewInt(0).SetUint64(number + 1).FillBytes(hash[24:])
	return hash
}

// A synthetic finalized native chain.
type testChain struct {
	t             *testing.T
	raw           []byte
	meta          *types.Metadata
	storage       map[string][]byte
	head          uint64
	hash          common.Hash
	genesis       common.Hash
	stateLock     sync.Mutex
	metadataReads int
	methods       []string
}

func newTestChain(t *testing.T) *testChain {
	raw, meta := testMetadata(t)
	head := uint64(7_000_000)
	return &testChain{t: t, raw: raw, meta: meta, storage: map[string][]byte{}, head: head, hash: testBlockHash(head, 1), genesis: testGenesis}
}

func (self *testChain) key(name string, args ...[]byte) string {
	self.t.Helper()
	item := items[name]
	key, err := types.CreateStorageKey(self.meta, item.pallet, item.name, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	return key.Hex()
}

func (self *testChain) set(name string, value any, args ...[]byte) {
	self.t.Helper()
	encoded, err := codec.Encode(value)
	if err != nil {
		self.t.Fatal(err)
	}
	self.storage[self.key(name, args...)] = encoded
}

func (self *testChain) setRaw(name string, raw []byte, args ...[]byte) {
	self.storage[self.key(name, args...)] = raw
}

func (self *testChain) answer(method string, args []any) (any, error) {
	self.stateLock.Lock()
	self.methods = append(self.methods, method)
	self.stateLock.Unlock()
	switch method {
	case "chain_getFinalizedHead":
		return self.hash.Hex(), nil
	case "chain_getBlockHash":
		number, _ := args[0].(uint64)
		if value, ok := args[0].(int); ok {
			number = uint64(value)
		}
		if number == 0 {
			return self.genesis.Hex(), nil
		}
		if number == self.head {
			return self.hash.Hex(), nil
		}
		return testBlockHash(number, 1).Hex(), nil
	case "chain_getHeader":
		return map[string]any{"number": hexutil.EncodeUint64(self.head)}, nil
	case "state_getRuntimeVersion":
		return map[string]any{"specName": "node-subtensor", "specVersion": 486}, nil
	case "state_getMetadata":
		self.stateLock.Lock()
		self.metadataReads++
		self.stateLock.Unlock()
		return hexutil.Encode(self.raw), nil
	case "state_getStorage":
		key, _ := args[0].(string)
		block, _ := args[1].(string)
		if block != self.hash.Hex() {
			return nil, fmt.Errorf("storage read at %s, not the checkpoint", block)
		}
		value, ok := self.storage[key]
		if !ok {
			return nil, nil
		}
		return hexutil.Encode(value), nil
	}
	return nil, fmt.Errorf("unexpected method %s", method)
}

func (self *testChain) BatchCallContext(ctx context.Context, batch []rpc.BatchElem) error {
	for i := range batch {
		value, err := self.answer(batch[i].Method, batch[i].Args)
		if err != nil {
			batch[i].Error = err
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, batch[i].Result); err != nil {
			return err
		}
	}
	return nil
}

func le16(value uint16) []byte {
	return littleEndian16(value)
}

type testWeight struct {
	Uid   types.U16
	Value types.U16
}

// An eight-UID subnet: UIDs 1 and 5 hold validator permits.
func (self *testChain) subnet(netuid uint16) {
	n := le16(netuid)
	self.set("SubnetworkN", types.U16(8), n)
	self.set("SubnetEpochIndex", types.U64(51234), n)
	self.set("Tempo", types.U16(360), n)
	self.set("ActivityCutoff", types.U16(5000), n)
	self.set("Incentive", []types.U16{0, 0, 30000, 0, 0, 0, 0, 35535}, n)
	self.set("LastUpdate", []types.U64{0, 6_999_900, 0, 0, 0, 6_990_000, 0, 0}, n)
	self.set("ValidatorPermit", []types.Bool{false, true, false, false, false, true, false, false}, n)
	self.set("ValidatorTrust", []types.U16{0, 65535, 0, 0, 0, 1000, 0, 0}, n)
	for uid := range 8 {
		self.set("Keys", types.AccountID{byte(0x40 + uid), 0x01}, n, le16(uint16(uid)))
		self.set("BlockAtRegistration", types.U64(1000+uid), n, le16(uint16(uid)))
	}
	self.set("Weights", []testWeight{{Uid: 2, Value: 65535}, {Uid: 7, Value: 30000}}, n, le16(1))
	self.set("Weights", []testWeight{{Uid: 169, Value: 14563}}, n, le16(5))
}

func testCensusOptions() CensusOptions {
	return CensusOptions{IdentityUids: []uint16{3, 9}, MaxValidatorRows: 64, MaxResponseBytes: 100_000}
}

// The census decodes every vector, the holders of the permitted and requested
// UIDs below SubnetworkN and the permitted rows, under the budget; the
// runtime metadata is fetched once and kept for the runtime.
func TestReadCensusAtTheRecordedRuntime(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	reader := NewReader()
	census, err := reader.ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions())
	if err != nil {
		t.Fatal(err)
	}
	if census.Checkpoint.Number != chain.head || census.Checkpoint.Hash != chain.hash || census.Checkpoint.SpecVersion != 486 {
		t.Fatalf("checkpoint %+v", census.Checkpoint)
	}
	if census.SubnetworkN != 8 || census.SubnetEpochIndex != 51234 || census.Tempo != 360 || census.ActivityCutoff != 5000 {
		t.Fatalf("subnet %+v", census)
	}
	if !slices.Equal(census.Incentive, []uint16{0, 0, 30000, 0, 0, 0, 0, 35535}) || census.LastUpdate[1] != 6_999_900 || !census.ValidatorPermit[5] || census.ValidatorTrust[5] != 1000 {
		t.Fatalf("vectors %+v", census)
	}
	if len(census.Identities) != 3 {
		t.Fatalf("identities %+v, want UIDs 1, 3 and 5", census.Identities)
	}
	for _, uid := range []uint16{1, 3, 5} {
		if identity := census.Identities[uid]; identity.Hotkey != [32]byte{byte(0x40 + uid), 0x01} || identity.BlockAtRegistration != 1000+uint64(uid) {
			t.Fatalf("uid %d identity %+v", uid, identity)
		}
	}
	if !census.ValidatorRowsComplete || len(census.ValidatorRows) != 2 || census.ValidatorRows[0].Uid != 1 ||
		!slices.Equal(census.ValidatorRows[0].Row, []WeightEntry{{Uid: 2, Value: 65535}, {Uid: 7, Value: 30000}}) ||
		!slices.Equal(census.ValidatorRows[1].Row, []WeightEntry{{Uid: 169, Value: 14563}}) {
		t.Fatalf("rows %+v", census.ValidatorRows)
	}
	if census.ResponseBytes <= 0 || 100_000 < census.ResponseBytes {
		t.Fatalf("response bytes %d", census.ResponseBytes)
	}
	if _, err := reader.ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions()); err != nil || chain.metadataReads != 1 {
		t.Fatal("a second census of the same runtime refetched the metadata", chain.metadataReads, err)
	}
}

// An absent value takes the runtime's declared default: an unset activity
// cutoff reads 5000 and an unset tempo 360, as the chain applies them.
func TestReadCensusAppliesRuntimeDefaults(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	delete(chain.storage, chain.key("ActivityCutoff", le16(25)))
	delete(chain.storage, chain.key("Tempo", le16(25)))
	census, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions())
	if err != nil || census.ActivityCutoff != 5000 || census.Tempo != 360 {
		t.Fatal("defaults", census, err)
	}
}

// The cheap index read decodes SubnetEpochIndex at the checkpoint.
func TestSubnetEpochIndexAtTheCheckpoint(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	reader := NewReader()
	checkpoint, used, err := reader.ReadCheckpoint(context.Background(), chain, testGenesis, 8_000)
	if err != nil || checkpoint.Number != chain.head || used <= 0 {
		t.Fatal(checkpoint, used, err)
	}
	index, used, err := reader.SubnetEpochIndex(context.Background(), chain, checkpoint, 25, 8_000)
	if err != nil || index != 51234 || used <= 0 || 1_000 < used {
		t.Fatal("index", index, used, err)
	}
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.Reason != reason || ErrorReason(err) != reason {
		t.Fatalf("error %v, want reason %s", err, reason)
	}
}

// A runtime that changes an item's shape is refused before any decode.
func TestReadCensusRefusesAChangedShape(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	original := items["Incentive"]
	changed := original
	changed.value = "Vec<prim6>"
	items["Incentive"] = changed
	defer func() { items["Incentive"] = original }()
	_, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions())
	requireReason(t, err, ReasonStorageShape)
	_, meta := testMetadata(t)
	if refusal := VerifyShapes(meta); refusal != "Incentive changed shape" {
		t.Fatalf("refusal %q", refusal)
	}
}

// Every reviewed item has its shape in both recorded runtimes 482 and 486.
func TestReviewedShapesHoldInRecordedRuntimes(t *testing.T) {
	for _, runtime := range []string{"482", "486"} {
		encoded, err := os.ReadFile("../../sn/crv4/testdata/runtime" + runtime + "-metadata.scale.gz.base64")
		if err != nil {
			t.Fatal(err)
		}
		compressed, _ := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
		reader, _ := gzip.NewReader(bytes.NewReader(compressed))
		raw, _ := io.ReadAll(reader)
		meta := &types.Metadata{}
		if err := codec.Decode(raw, meta); err != nil {
			t.Fatal(err)
		}
		if refusal := VerifyShapes(meta); refusal != "" {
			t.Fatalf("runtime %s: %s", runtime, refusal)
		}
	}
}

// Vectors that disagree with the subnet's size are refused.
func TestReadCensusRefusesInconsistentVectors(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	chain.set("Incentive", []types.U16{0, 0, 30000, 0, 0, 0, 0}, le16(25))
	_, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions())
	requireReason(t, err, ReasonVectorsInconsistent)
}

// A value with trailing bytes is malformed, not truncated into a decode.
func TestReadCensusRefusesTrailingBytes(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	chain.setRaw("SubnetEpochIndex", []byte{1, 0, 0, 0, 0, 0, 0, 0, 9}, le16(25))
	_, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions())
	requireReason(t, err, ReasonStorageMalformed)
}

// A census that would read past its response budget stops.
func TestReadCensusStopsAtItsBudget(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	options := testCensusOptions()
	options.MaxResponseBytes = 600
	_, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, options)
	requireReason(t, err, ReasonBudget)
	options.MaxResponseBytes = 0
	_, err = NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, options)
	requireReason(t, err, ReasonBudget)
}

// Rows past the row bound are not read, and the census says so.
func TestReadCensusBoundsValidatorRows(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	options := testCensusOptions()
	options.MaxValidatorRows = 1
	census, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, options)
	if err != nil || census.ValidatorRowsComplete || len(census.ValidatorRows) != 1 || census.ValidatorRows[0].Uid != 1 {
		t.Fatal("row bound", census, err)
	}
}

// Rows read the first UIDs first and stop at the budget between batches, so
// many permitted validators shorten the rows instead of failing the census.
func TestReadCensusRowsStopAtTheBudget(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	options := testCensusOptions()
	options.FirstRowUids = []uint16{5, 4}
	census, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, options)
	if err != nil || len(census.ValidatorRows) != 2 || census.ValidatorRows[0].Uid != 5 || census.ValidatorRows[1].Uid != 1 {
		t.Fatal("first rows", census, err)
	}
	// a budget that holds the vectors, holders and recheck but no row batch
	rowless := options
	rowless.MaxValidatorRows = 0
	census, err = NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, rowless)
	if err != nil || len(census.ValidatorRows) != 0 {
		t.Fatal("row-less census", census, err)
	}
	options.MaxResponseBytes = census.ResponseBytes + recheckReserve
	census, err = NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, options)
	if err != nil || census.ValidatorRowsComplete || len(census.ValidatorRows) != 0 {
		t.Fatal("rows past the budget", census, err)
	}
}

// An endpoint on another chain is refused before any storage read.
func TestReadCensusRefusesAnotherChain(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	chain.genesis = common.HexToHash("0x01")
	_, err := NewReader().ReadCensus(context.Background(), chain, testGenesis, 25, testCensusOptions())
	requireReason(t, err, ReasonChainIdentity)
	if slices.Contains(chain.methods, "state_getStorage") {
		t.Fatal("storage was read from another chain")
	}
}

// A checkpoint that is no longer canonical after the reads is refused.
func TestReadCensusRefusesAReorgedCheckpoint(t *testing.T) {
	chain := newTestChain(t)
	chain.subnet(25)
	_, err := NewReader().ReadCensus(context.Background(), &forkingChain{testChain: chain}, testGenesis, 25, testCensusOptions())
	requireReason(t, err, ReasonCheckpointNotCanonic)
}

// Answers the head's height with another block's hash.
type forkingChain struct {
	*testChain
}

func (self *forkingChain) BatchCallContext(ctx context.Context, batch []rpc.BatchElem) error {
	if err := self.testChain.BatchCallContext(ctx, batch); err != nil {
		return err
	}
	for i := range batch {
		if batch[i].Method != "chain_getBlockHash" || len(batch[i].Args) != 1 {
			continue
		}
		if number, ok := batch[i].Args[0].(uint64); ok && number == self.head {
			encoded, _ := json.Marshal(testBlockHash(self.head, 2).Hex())
			if err := json.Unmarshal(encoded, batch[i].Result); err != nil {
				return err
			}
		}
	}
	return nil
}
