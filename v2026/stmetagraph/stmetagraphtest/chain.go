// Package stmetagraphtest serves a synthetic finalized native chain for tests
// of stmetagraph's callers. It answers with the exact public Bittensor runtime
// 486 metadata recorded by the sibling sn repository (crv4/testdata) and
// SCALE-encoded storage whose keys come from that metadata, so a caller's test
// exercises the real reader without a live chain. Values are synthetic.
package stmetagraphtest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// The recorded metadata, relative to a package directory one level below the
// server module.
const MetadataPath = "../../sn/crv4/testdata/runtime486-metadata.scale.gz.base64"

// The Finney genesis the recorded runtimes were read from.
var Genesis = common.HexToHash("0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03")

// BlockHash is the synthetic hash of a block number.
func BlockHash(number uint64) common.Hash {
	var hash common.Hash
	hash[0], hash[1] = 0x0c, 0x01
	big.NewInt(0).SetUint64(number + 1).FillBytes(hash[24:])
	return hash
}

// Chain is one synthetic finalized native chain.
type Chain struct {
	t       testing.TB
	raw     []byte
	meta    *types.Metadata
	storage map[string][]byte

	stateLock sync.Mutex
	head      uint64
	calls     map[string]int
	bytes     int
	fail      error
}

// NewChain reads the recorded runtime and starts at head.
func NewChain(t testing.TB, metadataPath string, head uint64) *Chain {
	t.Helper()
	encoded, err := os.ReadFile(metadataPath)
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
	return &Chain{t: t, raw: raw, meta: meta, storage: map[string][]byte{}, head: head, calls: map[string]int{}}
}

// U16 is the SCALE encoding of a storage key argument.
func U16(value uint16) []byte {
	return binary.LittleEndian.AppendUint16(nil, value)
}

// Set stores a SubtensorModule value under its metadata key.
func (self *Chain) Set(name string, value any, args ...[]byte) {
	self.t.Helper()
	key, err := types.CreateStorageKey(self.meta, "SubtensorModule", name, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	encoded, err := codec.Encode(value)
	if err != nil {
		self.t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.storage[key.Hex()] = encoded
}

// SetHead moves the finalized head.
func (self *Chain) SetHead(head uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.head = head
}

// Fail makes every exchange fail with err; nil restores the chain.
func (self *Chain) Fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fail = err
}

// Calls returns how many times a method was called.
func (self *Chain) Calls(method string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls[method]
}

// TotalCalls returns how many methods were called.
func (self *Chain) TotalCalls() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	total := 0
	for _, count := range self.calls {
		total += count
	}
	return total
}

// Weight is one applied weight of a Weights row.
type Weight struct {
	Uid   types.U16
	Value types.U16
}

// Subnet stores an n-UID subnet whose UID i holds hotkey {0x40+i, 0x01},
// registered at block 1000+i, with the given vectors and validator rows.
func (self *Chain) Subnet(netuid uint16, epochIndex uint64, incentive []uint16, lastUpdate []uint64, permits []bool, trust []uint16, rows map[uint16][]Weight) {
	n := U16(netuid)
	size := len(incentive)
	self.Set("SubnetworkN", types.U16(size), n)
	self.Set("SubnetEpochIndex", types.U64(epochIndex), n)
	self.Set("Tempo", types.U16(360), n)
	self.Set("ActivityCutoff", types.U16(5000), n)
	encodedIncentive, encodedTrust := make([]types.U16, size), make([]types.U16, size)
	encodedLastUpdate, encodedPermits := make([]types.U64, size), make([]types.Bool, size)
	for uid := range size {
		encodedIncentive[uid], encodedTrust[uid] = types.U16(incentive[uid]), types.U16(trust[uid])
		encodedLastUpdate[uid], encodedPermits[uid] = types.U64(lastUpdate[uid]), types.Bool(permits[uid])
		self.Set("Keys", types.AccountID(Hotkey(uint16(uid))), n, U16(uint16(uid)))
		self.Set("BlockAtRegistration", types.U64(1000+uid), n, U16(uint16(uid)))
	}
	self.Set("Incentive", encodedIncentive, n)
	self.Set("LastUpdate", encodedLastUpdate, n)
	self.Set("ValidatorPermit", encodedPermits, n)
	self.Set("ValidatorTrust", encodedTrust, n)
	for uid, row := range rows {
		self.Set("Weights", row, n, U16(uid))
	}
}

// Hotkey is the synthetic hotkey Subnet gives a UID.
func Hotkey(uid uint16) [32]byte {
	return [32]byte{byte(0x40 + uid), 0x01}
}

func (self *Chain) answer(method string, args []any) (any, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls[method]++
	if self.fail != nil {
		return nil, self.fail
	}
	hash := BlockHash(self.head)
	switch method {
	case "chain_getFinalizedHead":
		return hash.Hex(), nil
	case "chain_getBlockHash":
		switch number := args[0].(type) {
		case int:
			if number == 0 {
				return Genesis.Hex(), nil
			}
			return BlockHash(uint64(number)).Hex(), nil
		case uint64:
			return BlockHash(number).Hex(), nil
		}
		return nil, fmt.Errorf("block number %v", args[0])
	case "chain_getHeader":
		return map[string]any{"number": hexutil.EncodeUint64(self.head)}, nil
	case "state_getRuntimeVersion":
		return map[string]any{"specName": "node-subtensor", "specVersion": 486}, nil
	case "state_getMetadata":
		return hexutil.Encode(self.raw), nil
	case "state_getStorage":
		key, _ := args[0].(string)
		if block, _ := args[1].(string); block != hash.Hex() {
			return nil, fmt.Errorf("storage read at %s, not the head", block)
		}
		value, ok := self.storage[key]
		if !ok {
			return nil, nil
		}
		return hexutil.Encode(value), nil
	}
	return nil, fmt.Errorf("unexpected method %s", method)
}

// BatchCallContext answers one batch, as the st gateway does.
func (self *Chain) BatchCallContext(ctx context.Context, batch []rpc.BatchElem) error {
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
