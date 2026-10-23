// A synthetic native chain for the §22.6 weight reader. It serves the exact
// public Bittensor runtime 482 metadata recorded by the sibling sn repository
// and SCALE-encoded storage whose keys come from that metadata, so tests
// exercise the real key derivation, shape verification and decoders.
package monitor

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// The recorded runtime 482 metadata, read once per test binary.
func subnetWeightTestMetadata(t *testing.T) ([]byte, *types.Metadata) {
	t.Helper()
	encoded, err := os.ReadFile("../../sn/crv4/testdata/runtime482-metadata.scale.gz.base64")
	if err != nil {
		t.Fatalf("recorded runtime metadata is unavailable: %v", err)
	}
	compressed, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	meta := &types.Metadata{}
	if err := codec.Decode(raw, meta); err != nil {
		t.Fatal(err)
	}
	return raw, meta
}

// Synthetic validator and escrow hotkeys; never production identities.
var subnetWeightTestEnrolled = [32]byte{0x71, 0x01}
var subnetWeightTestCensus = [32]byte{0x72, 0x02}
var subnetWeightTestEscrow = [32]byte{0xe5}

type subnetWeightTestChain struct {
	t           *testing.T
	raw         []byte
	meta        *types.Metadata
	storage     map[string][]byte
	number      uint64
	hash        common.Hash
	specVersion uint32
	metadataGet int
}

func subnetWeightTestNativeHash(number uint64, generation byte) common.Hash {
	var hash common.Hash
	hash[0] = 0x0a
	hash[1] = generation
	big.NewInt(0).SetUint64(number + 1).FillBytes(hash[24:])
	return hash
}

func (self *subnetWeightTestChain) set(name string, value any, args ...[]byte) {
	self.t.Helper()
	item := subnetWeightItems[name]
	key, err := types.CreateStorageKey(self.meta, item.pallet, item.name, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	encoded, err := codec.Encode(value)
	if err != nil {
		self.t.Fatal(err)
	}
	self.storage[key.Hex()] = encoded
}

func (self *subnetWeightTestChain) remove(name string, args ...[]byte) {
	item := subnetWeightItems[name]
	key, err := types.CreateStorageKey(self.meta, item.pallet, item.name, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	delete(self.storage, key.Hex())
}

// A healthy subnet of eight UIDs: the enrolled validator at UID 0 and a
// census validator at UID 1 apply fresh, masked rows; one enrolled commit is
// pending inside its reveal window.
func newHealthySubnetWeightChain(t *testing.T, number uint64) *subnetWeightTestChain {
	raw, meta := subnetWeightTestMetadata(t)
	chain := &subnetWeightTestChain{t: t, raw: raw, meta: meta, storage: map[string][]byte{}, number: number, hash: subnetWeightTestNativeHash(number, 0), specVersion: 482}
	netuid := subnetWeightLittleEndian16(25)
	chain.set("SubnetworkN", types.U16(8), netuid)
	chain.set("Tempo", types.U16(99), netuid)
	chain.set("RevealPeriodEpochs", types.U64(1), netuid)
	chain.set("ValidatorPermit", []types.Bool{true, true, false, false, false, false, false, false}, netuid)
	chain.set("LastUpdate", []types.U64{types.U64(number - 50), types.U64(number - 50), 0, 0, 0, 0, 0, 0}, netuid)
	chain.set("Dividends", []types.U16{100, 200, 0, 0, 0, 0, 0, 0}, netuid)
	chain.set("ValidatorTrust", []types.U16{60000, 50000, 0, 0, 0, 0, 0, 0}, netuid)
	chain.set("LastStoredRound", types.U64(1000))
	chain.set("Keys", subnetWeightTestEnrolled, netuid, subnetWeightLittleEndian16(0))
	chain.set("Keys", subnetWeightTestCensus, netuid, subnetWeightLittleEndian16(1))
	chain.set("Uids", types.U16(0), netuid, subnetWeightTestEnrolled[:])
	chain.set("Uids", types.U16(1), netuid, subnetWeightTestCensus[:])
	chain.set("Uids", types.U16(7), netuid, subnetWeightTestEscrow[:])
	chain.set("Weights", []subnetWeightPair{{Uid: 2, Value: 30000}, {Uid: 3, Value: 65535}}, netuid, subnetWeightLittleEndian16(0))
	chain.set("Weights", []subnetWeightPair{{Uid: 4, Value: 65535}}, netuid, subnetWeightLittleEndian16(1))
	chain.commits(subnetWeightTestCommit(subnetWeightTestEnrolled, number-10, 1500))
	return chain
}

func subnetWeightTestCommit(account [32]byte, block uint64, round uint64) subnetWeightCommit {
	return subnetWeightCommit{Account: account, CommitBlock: types.U64(block), Ciphertext: types.Bytes{0xc1, 0xc2}, RevealRound: types.U64(round)}
}

// Replaces every pending commit with one queue under a synthetic epoch key.
func (self *subnetWeightTestChain) commits(queue ...subnetWeightCommit) {
	self.t.Helper()
	prefix, err := subnetWeightPrefix(self.meta, "TimelockedWeightCommits", subnetWeightLittleEndian16(25))
	if err != nil {
		self.t.Fatal(err)
	}
	for key := range self.storage {
		if strings.HasPrefix(key, prefix) {
			delete(self.storage, key)
		}
	}
	if len(queue) == 0 {
		return
	}
	epoch := make([]byte, 8)
	epoch[0] = 14
	self.set("TimelockedWeightCommits", queue, subnetWeightLittleEndian16(25), epoch)
}

// Answers the native methods; evm methods stay with the base node.
func (self *subnetWeightTestChain) answer(method string, params []json.RawMessage) (any, *subnetReplayRpcFailure) {
	block := func(index int) {
		var hash common.Hash
		if len(params) <= index || json.Unmarshal(params[index], &hash) != nil || hash != self.hash {
			self.t.Fatalf("%s is not pinned to the finalized native block", method)
		}
	}
	switch method {
	case "chain_getFinalizedHead":
		return self.hash.Hex(), nil
	case "chain_getBlockHash":
		var number uint64
		if json.Unmarshal(params[0], &number) != nil {
			self.t.Fatal("chain_getBlockHash number is invalid")
		}
		if number == self.number {
			return self.hash.Hex(), nil
		}
		return subnetWeightTestNativeHash(number, 0).Hex(), nil
	case "chain_getHeader":
		block(0)
		return map[string]any{"number": hexutil.EncodeUint64(self.number), "parentHash": common.Hash{}.Hex()}, nil
	case "state_getRuntimeVersion":
		block(0)
		return map[string]any{"specName": "node-subtensor", "specVersion": self.specVersion, "transactionVersion": 1, "stateVersion": 1}, nil
	case "state_getMetadata":
		block(0)
		self.metadataGet += 1
		return hexutil.Encode(self.raw), nil
	case "state_getStorage":
		block(1)
		var key string
		if json.Unmarshal(params[0], &key) != nil {
			self.t.Fatal("state_getStorage key is invalid")
		}
		value, ok := self.storage[key]
		if !ok {
			return nil, nil
		}
		return hexutil.Encode(value), nil
	case "state_getKeysPaged":
		block(3)
		var prefix, start string
		var count int
		if json.Unmarshal(params[0], &prefix) != nil || json.Unmarshal(params[1], &count) != nil || json.Unmarshal(params[2], &start) != nil {
			self.t.Fatal("state_getKeysPaged params are invalid")
		}
		keys := []string{}
		for key := range self.storage {
			if strings.HasPrefix(key, prefix) && start < key {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		if count < len(keys) {
			keys = keys[:count]
		}
		return keys, nil
	}
	self.t.Fatalf("unexpected native method %s", method)
	return nil, nil
}

// A ready weights environment over the healthy subnet and settlement chain.
func newSubnetWeightsFixture(t *testing.T) (*subnetWeightTestChain, *subnetReplayTestNode, SignalSettings) {
	t.Helper()
	settlement := newHealthySubnetSettlementChain(t)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, settlement.finalizedBlock())
	settlement.install(node)
	chain := newHealthySubnetWeightChain(t, settlement.finalizedBlock())
	node.native = chain.answer
	settings, _ := subnetReplayTestSettings(t, node)
	settings.SubnetReplay.Validators = []string{subnetReplayTestSs58(t, subnetWeightTestEnrolled)}
	settings.SubnetReplay.MaximumWeightLimitU16 = 50000
	return chain, node, settings
}

// Only the two fixed replay gaps are expected when healthy.
func requireSubnetWeightsOnlyReplayGaps(t *testing.T, alerts []Alert) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == "subnet-monitor-coverage" && (alert.Frame == "decision-replay" || alert.Frame == "yuma-replay") {
			continue
		}
		t.Fatalf("healthy weights alerted: %s %s %s %s", alert.Class, alert.Target, alert.Frame, alert.Observed)
	}
}
