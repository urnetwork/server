package controller

// Deterministic RPC-boundary tests for release settlement's bounded fleet
// binding snapshot. The public Subtensor EVM endpoint meters physical HTTP
// requests, so this surface must not regress to one finalized-head/read pair
// per provider.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/urfoundation/sn/v2026/stabi"
)

type stBindingBatchRPC struct {
	t           *testing.T
	address     common.Address
	finalized   uint64
	epoch       uint64
	startBlock  uint64
	closeBlock  uint64
	malformedID byte
	malformedAt uint64
	errorID     byte
	errorAt     uint64
	stateAt     func([16]byte, uint64) *StFleetBindingState

	stateLock      sync.Mutex
	batchSizes     []int
	batchBlocks    []uint64
	finalizedCalls int
}

func stBindingFixtureState(clientId [16]byte) *StFleetBindingState {
	var fleetId, hotkey, clientKey, commitmentHash [32]byte
	fleetId[0], hotkey[0] = clientId[0], clientId[0]^0xff
	clientKey[0], clientKey[1] = clientId[0], 0xaa
	commitmentHash[0], commitmentHash[1] = clientId[0], 0x55
	return &StFleetBindingState{
		Active: clientId[0]%2 == 0, FleetId: fleetId, Hotkey: hotkey, ClientKey: clientKey, CommitmentHash: commitmentHash,
		Generation: uint64(clientId[0]) + 100, ValidFrom: 7, ValidTo: 77,
		Uid: uint16(clientId[0]) + 20,
	}
}

func (self *stBindingBatchRPC) snapshot() ([]int, []uint64, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]int(nil), self.batchSizes...), append([]uint64(nil), self.batchBlocks...), self.finalizedCalls
}

// ServeHTTP reverses batch responses to prove that JSON-RPC ids, rather than
// response position, preserve each client's binding. It also validates the
// canonical block selector and exact bindingAt calldata on every element.
func (self *stBindingBatchRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	self.t.Helper()
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read binding RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var call stEpochRPCRequest
		if json.Unmarshal(body, &call) != nil {
			self.t.Errorf("invalid single binding RPC request: %s", body)
			return
		}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getBlockByNumber":
			self.stateLock.Lock()
			self.finalizedCalls++
			self.stateLock.Unlock()
			result = stBlockBatchResult(self.finalized)
		default:
			self.t.Errorf("unexpected single binding RPC method %s", call.Method)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
		return
	}

	var calls []stEpochRPCRequest
	if json.Unmarshal(body, &calls) != nil || len(calls) == 0 || len(calls) > stMaximumEVMRPCBatchCalls {
		self.t.Errorf("invalid binding RPC batch: %s", body)
		return
	}
	coordinator := stabi.NewSTCoordinator()
	selector := coordinator.PackBindingAt([16]byte{}, new(big.Int))[:4]
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		self.t.Errorf("parse coordinator ABI: %v", err)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		var callArgs struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		var blockTag string
		if call.Method != "eth_call" || len(call.Params) != 2 || json.Unmarshal(call.Params[0], &callArgs) != nil || json.Unmarshal(call.Params[1], &blockTag) != nil {
			self.t.Errorf("invalid binding batch element %d", index)
			return
		}
		calldata, decodeErr := hexutil.Decode(callArgs.Data)
		block, blockErr := hexutil.DecodeUint64(blockTag)
		if decodeErr != nil || blockErr != nil || len(calldata) != 68 || !bytes.Equal(calldata[:4], selector) || !strings.EqualFold(callArgs.To, self.address.Hex()) || (block != self.startBlock && block != self.closeBlock) {
			self.t.Errorf("invalid binding call %d to=%s block=%s data=%s error=%v", index, callArgs.To, blockTag, callArgs.Data, decodeErr)
			return
		}
		if index == 0 {
			self.stateLock.Lock()
			self.batchSizes = append(self.batchSizes, len(calls))
			self.batchBlocks = append(self.batchBlocks, block)
			self.stateLock.Unlock()
		}
		if new(big.Int).SetBytes(calldata[36:68]).Uint64() != self.epoch {
			self.t.Errorf("binding call %d epoch mismatch", index)
			return
		}
		var clientId [16]byte
		copy(clientId[:], calldata[4:20])
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if self.errorID != 0 && clientId[0] == self.errorID && (self.errorAt == 0 || block == self.errorAt) {
			response["error"] = map[string]any{"code": -32000, "message": "binding unavailable"}
		} else if self.malformedID != 0 && clientId[0] == self.malformedID && (self.malformedAt == 0 || block == self.malformedAt) {
			response["result"] = "0x01"
		} else {
			state := stBindingFixtureState(clientId)
			if self.stateAt != nil {
				state = self.stateAt(clientId, block)
			}
			if state == nil {
				self.t.Errorf("nil binding fixture state for client %x at block %d", clientId, block)
				return
			}
			record := stabi.STCoordinatorBindingRecord{
				FleetId: state.FleetId, Hotkey: state.Hotkey, ClientKey: state.ClientKey, CommitmentHash: state.CommitmentHash,
				Generation: state.Generation, ValidFromEpoch: state.ValidFrom, ValidToEpoch: state.ValidTo,
				CleanedAtEpoch: state.CleanedAtEpoch, Uid: state.Uid, Cleaned: state.Cleaned,
			}
			encoded, packErr := parsed.Methods["bindingAt"].Outputs.Pack(state.Active, record)
			if packErr != nil {
				self.t.Errorf("pack binding result %d: %v", index, packErr)
				return
			}
			response["result"] = "0x" + hex.EncodeToString(encoded)
		}
		responses[len(calls)-1-index] = response
	}
	if err := json.NewEncoder(writer).Encode(responses); err != nil {
		self.t.Errorf("encode binding batch response: %v", err)
	}
}

func newStBindingBatchClient(t testing.TB, fixture *stBindingBatchRPC) *CoreStClient {
	t.Helper()
	client := newStBlockBatchClient(t, fixture)
	client.cfg.ContractAddress = fixture.address
	client.coordinator = stabi.NewSTCoordinator()
	return client
}

// A release-sized operator snapshot must use exactly one finalized-head read
// and three physical eth_call batches per boundary for 120 clients, preserving
// order even when the endpoint reverses every JSON-RPC response array.
func TestCoreStClientBindingsAtUsesOneFinalizedHeadAndBoundedBatches(t *testing.T) {
	fixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 41, startBlock: 700, closeBlock: 800,
	}
	client := newStBindingBatchClient(t, fixture)
	clientIds := make([][16]byte, 120)
	for index := range clientIds {
		clientIds[index][0] = byte(index + 1)
	}
	bindings, err := client.BindingsAt(context.Background(), clientIds, fixture.epoch, fixture.startBlock, fixture.closeBlock)
	if err != nil {
		t.Fatal(err)
	}
	batchSizes, batchBlocks, finalizedCalls := fixture.snapshot()
	wantSizes := []int{50, 50, 20, 50, 50, 20}
	wantBlocks := []uint64{fixture.startBlock, fixture.startBlock, fixture.startBlock, fixture.closeBlock, fixture.closeBlock, fixture.closeBlock}
	if !slices.Equal(batchSizes, wantSizes) || !slices.Equal(batchBlocks, wantBlocks) || finalizedCalls != 1 || len(bindings) != len(clientIds) {
		t.Fatalf("batch sizes/blocks/finalized calls/bindings=%v/%v/%d/%d, want %v/%v/1/%d", batchSizes, batchBlocks, finalizedCalls, len(bindings), wantSizes, wantBlocks, len(clientIds))
	}
	for index, clientId := range clientIds {
		if want := stBindingFixtureState(clientId); *bindings[index] != *want {
			t.Fatalf("binding %d=%+v, want %+v", index, bindings[index], want)
		}
	}
}

// A fleet which loses its runtime UID before close still received head-tier
// emission earlier in the epoch, so the boundary OR must keep it excluded.
func TestCoreStClientBindingsAtKeepsActiveStartPrunedCloseExcluded(t *testing.T) {
	fixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 41, startBlock: 700, closeBlock: 800,
	}
	fixture.stateAt = func(clientId [16]byte, block uint64) *StFleetBindingState {
		state := stBindingFixtureState(clientId)
		state.Active = block == fixture.startBlock
		return state
	}
	client := newStBindingBatchClient(t, fixture)
	bindings, err := client.BindingsAt(context.Background(), [][16]byte{{2}}, fixture.epoch, fixture.startBlock, fixture.closeBlock)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0] == nil || !bindings[0].Active || bindings[0].Generation != 102 {
		t.Fatalf("pruned close binding=%+v, want start identity retained and active", bindings)
	}
	batchSizes, batchBlocks, finalizedCalls := fixture.snapshot()
	if !slices.Equal(batchSizes, []int{1, 1}) || !slices.Equal(batchBlocks, []uint64{700, 800}) || finalizedCalls != 1 {
		t.Fatalf("boundary reads=%v/%v finalized=%d", batchSizes, batchBlocks, finalizedCalls)
	}
}

// A client inactive at both boundaries is no longer head-paid and therefore
// remains available for the following epoch's ordinary pool payout path.
func TestCoreStClientBindingsAtInactiveThroughoutAllowsNextEpochFallback(t *testing.T) {
	fixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 42, startBlock: 801, closeBlock: 900,
		stateAt: func(clientId [16]byte, _ uint64) *StFleetBindingState {
			state := stBindingFixtureState(clientId)
			state.Active = false
			return state
		},
	}
	client := newStBindingBatchClient(t, fixture)
	bindings, err := client.BindingsAt(context.Background(), [][16]byte{{2}}, fixture.epoch, fixture.startBlock, fixture.closeBlock)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0] == nil || bindings[0].Active {
		t.Fatalf("inactive boundary binding=%+v, want pool fallback", bindings)
	}
}

// Two active reads for the same client and epoch cannot legitimately identify
// different signed bindings; accepting either would let an endpoint substitute
// the payout-exclusion generation.
func TestCoreStClientBindingsAtRejectsDivergentActiveRecords(t *testing.T) {
	fixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 41, startBlock: 700, closeBlock: 800,
	}
	fixture.stateAt = func(clientId [16]byte, block uint64) *StFleetBindingState {
		state := stBindingFixtureState(clientId)
		state.Active = true
		if block == fixture.closeBlock {
			state.CommitmentHash[31]++
		}
		return state
	}
	client := newStBindingBatchClient(t, fixture)
	bindings, err := client.BindingsAt(context.Background(), [][16]byte{{2}}, fixture.epoch, fixture.startBlock, fixture.closeBlock)
	if err == nil || bindings != nil || !strings.Contains(err.Error(), "divergent active records") {
		t.Fatalf("divergent active records accepted: bindings=%v error=%v", bindings, err)
	}
}

// Active=true with a record outside the queried epoch is an impossible ABI
// combination and cannot be used as conservative exclusion evidence.
func TestCoreStClientBindingsAtRejectsImpossibleActiveRecord(t *testing.T) {
	fixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 41, startBlock: 700, closeBlock: 800,
		stateAt: func(clientId [16]byte, _ uint64) *StFleetBindingState {
			state := stBindingFixtureState(clientId)
			state.Active = true
			state.ValidFrom = 42
			return state
		},
	}
	client := newStBindingBatchClient(t, fixture)
	bindings, err := client.BindingsAt(context.Background(), [][16]byte{{2}}, fixture.epoch, fixture.startBlock, fixture.closeBlock)
	if err == nil || bindings != nil || !strings.Contains(err.Error(), "impossible record") {
		t.Fatalf("impossible active record accepted: bindings=%v error=%v", bindings, err)
	}
}

// Empty input still proves finality, while malformed ABI and element errors
// fail the complete snapshot rather than silently unexcluding a head miner.
func TestCoreStClientBindingsAtEmptyMalformedAndErrorFailClosed(t *testing.T) {
	emptyFixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 41, startBlock: 700, closeBlock: 800,
	}
	emptyClient := newStBindingBatchClient(t, emptyFixture)
	bindings, err := emptyClient.BindingsAt(context.Background(), nil, emptyFixture.epoch, emptyFixture.startBlock, emptyFixture.closeBlock)
	batchSizes, _, finalizedCalls := emptyFixture.snapshot()
	if err != nil || len(bindings) != 0 || len(batchSizes) != 0 || finalizedCalls != 1 {
		t.Fatalf("empty bindings=%v error=%v batches=%v finalized=%d", bindings, err, batchSizes, finalizedCalls)
	}
	for _, testCase := range []struct {
		name        string
		malformedID byte
		errorID     byte
	}{
		{name: "malformed ABI", malformedID: 2},
		{name: "element RPC error", errorID: 2},
	} {
		fixture := &stBindingBatchRPC{
			t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
			finalized: 999, epoch: 41, startBlock: 700, closeBlock: 800,
			malformedID: testCase.malformedID, malformedAt: 800, errorID: testCase.errorID, errorAt: 800,
		}
		client := newStBindingBatchClient(t, fixture)
		bindings, err := client.BindingsAt(context.Background(), [][16]byte{{1}, {2}, {3}}, fixture.epoch, fixture.startBlock, fixture.closeBlock)
		if err == nil || bindings != nil {
			t.Fatalf("%s response accepted: bindings=%v error=%v", testCase.name, bindings, err)
		}
	}
}

// Reversed windows fail before any RPC; a close past the finalized head fails
// after exactly the one proof read and before either boundary batch.
func TestCoreStClientBindingsAtRejectsInvalidBounds(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		startBlock         uint64
		closeBlock         uint64
		wantFinalizedCalls int
	}{
		{name: "reversed", startBlock: 801, closeBlock: 800, wantFinalizedCalls: 0},
		{name: "unfinalized close", startBlock: 800, closeBlock: 1000, wantFinalizedCalls: 1},
	} {
		fixture := &stBindingBatchRPC{
			t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
			finalized: 999, epoch: 41, startBlock: testCase.startBlock, closeBlock: testCase.closeBlock,
		}
		client := newStBindingBatchClient(t, fixture)
		bindings, err := client.BindingsAt(context.Background(), [][16]byte{{2}}, fixture.epoch, fixture.startBlock, fixture.closeBlock)
		batchSizes, _, finalizedCalls := fixture.snapshot()
		if err == nil || bindings != nil || len(batchSizes) != 0 || finalizedCalls != testCase.wantFinalizedCalls {
			t.Fatalf("%s bounds accepted: bindings=%v error=%v batches=%v finalized=%d", testCase.name, bindings, err, batchSizes, finalizedCalls)
		}
	}
}
