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

	"github.com/urfoundation/sn/stabi"
)

type stBindingBatchRPC struct {
	t           *testing.T
	address     common.Address
	finalized   uint64
	epoch       uint64
	malformedID byte
	errorID     byte

	stateLock      sync.Mutex
	batchSizes     []int
	finalizedCalls int
}

func stBindingFixtureState(clientId [16]byte) *StFleetBindingState {
	var fleetId, hotkey [32]byte
	fleetId[0], hotkey[0] = clientId[0], clientId[0]^0xff
	return &StFleetBindingState{
		Active: clientId[0]%2 == 0, FleetId: fleetId, Hotkey: hotkey,
		Generation: uint64(clientId[0]) + 100, ValidFrom: 7, ValidTo: 77,
		Uid: uint16(clientId[0]) + 20,
	}
}

func (self *stBindingBatchRPC) snapshot() ([]int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]int(nil), self.batchSizes...), self.finalizedCalls
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
	self.stateLock.Lock()
	self.batchSizes = append(self.batchSizes, len(calls))
	self.stateLock.Unlock()
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
		if decodeErr != nil || len(calldata) != 68 || !bytes.Equal(calldata[:4], selector) || !strings.EqualFold(callArgs.To, self.address.Hex()) || blockTag != hexutil.EncodeUint64(self.finalized) {
			self.t.Errorf("invalid binding call %d to=%s block=%s data=%s error=%v", index, callArgs.To, blockTag, callArgs.Data, decodeErr)
			return
		}
		if new(big.Int).SetBytes(calldata[36:68]).Uint64() != self.epoch {
			self.t.Errorf("binding call %d epoch mismatch", index)
			return
		}
		var clientId [16]byte
		copy(clientId[:], calldata[4:20])
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		switch clientId[0] {
		case self.errorID:
			if self.errorID != 0 {
				response["error"] = map[string]any{"code": -32000, "message": "binding unavailable"}
				break
			}
			fallthrough
		default:
			if self.malformedID != 0 && clientId[0] == self.malformedID {
				response["result"] = "0x01"
				break
			}
			state := stBindingFixtureState(clientId)
			record := stabi.STCoordinatorBindingRecord{
				FleetId: state.FleetId, Hotkey: state.Hotkey, Generation: state.Generation,
				ValidFromEpoch: state.ValidFrom, ValidToEpoch: state.ValidTo, Uid: state.Uid,
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
// and three physical eth_call batches for 120 clients, preserving order even
// when the endpoint reverses every JSON-RPC response array.
func TestCoreStClientBindingsAtUsesOneFinalizedHeadAndBoundedBatches(t *testing.T) {
	fixture := &stBindingBatchRPC{
		t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		finalized: 999, epoch: 41,
	}
	client := newStBindingBatchClient(t, fixture)
	clientIds := make([][16]byte, 120)
	for index := range clientIds {
		clientIds[index][0] = byte(index + 1)
	}
	bindings, err := client.BindingsAt(context.Background(), clientIds, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	batchSizes, finalizedCalls := fixture.snapshot()
	if !slices.Equal(batchSizes, []int{50, 50, 20}) || finalizedCalls != 1 || len(bindings) != len(clientIds) {
		t.Fatalf("batch sizes/finalized calls/bindings=%v/%d/%d, want [50 50 20]/1/%d", batchSizes, finalizedCalls, len(bindings), len(clientIds))
	}
	for index, clientId := range clientIds {
		if want := stBindingFixtureState(clientId); *bindings[index] != *want {
			t.Fatalf("binding %d=%+v, want %+v", index, bindings[index], want)
		}
	}
}

// Empty, ABI-malformed, and element-error responses are adjacent public-RPC
// cases: zero work must consume no endpoint call, while incomplete evidence
// must fail the entire settlement snapshot rather than silently unexclude a
// head miner.
func TestCoreStClientBindingsAtEmptyAndMalformedResponsesFailClosed(t *testing.T) {
	if bindings, err := (&CoreStClient{}).BindingsAt(context.Background(), nil, 1); err != nil || len(bindings) != 0 {
		t.Fatalf("empty bindings=%v error=%v", bindings, err)
	}
	for _, testCase := range []struct {
		name        string
		malformedID byte
		errorID     byte
	}{
		{name: "malformed ABI", malformedID: 2},
		{name: "element RPC error", errorID: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := &stBindingBatchRPC{
				t: t, address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
				finalized: 999, epoch: 41, malformedID: testCase.malformedID, errorID: testCase.errorID,
			}
			client := newStBindingBatchClient(t, fixture)
			if bindings, err := client.BindingsAt(context.Background(), [][16]byte{{1}, {2}, {3}}, fixture.epoch); err == nil || bindings != nil {
				t.Fatalf("incomplete binding snapshot accepted: bindings=%v error=%v", bindings, err)
			}
		})
	}
}
