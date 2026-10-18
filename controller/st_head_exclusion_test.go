package controller

// Settlement v2 head exclusion reads bindingAt only for the FleetBound
// candidates and excludes exactly the providers v1's per-provider read
// excludes (VERSION2-FINAL.md §6.2, §8.2). Identities are synthetic.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A fake EVM endpoint for one coordinator's fleet bindings: FleetBound logs,
// bindingAt at the epoch's two boundary blocks and canonical block identities.
type stHeadExclusionRPC struct {
	t          testing.TB
	address    common.Address
	finalized  uint64
	epoch      uint64
	startBlock uint64
	closeBlock uint64
	logs       []types.Log
	logError   bool
	bindingAt  func(clientId [16]byte, block uint64) *StFleetBindingState

	stateLock    sync.Mutex
	logRanges    [][2]uint64
	bindingReads int
}

func (self *stHeadExclusionRPC) snapshot() ([][2]uint64, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][2]uint64(nil), self.logRanges...), self.bindingReads
}

func (self *stHeadExclusionRPC) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.logRanges, self.bindingReads = nil, 0
}

// The logs of one eth_getLogs filter: the inclusive range, the address and
// the first topic.
func (self *stHeadExclusionRPC) filterLogs(params []json.RawMessage) (any, error) {
	var filter struct {
		FromBlock string          `json:"fromBlock"`
		ToBlock   string          `json:"toBlock"`
		Address   json.RawMessage `json:"address"`
		Topics    [][]string      `json:"topics"`
	}
	if len(params) != 1 || json.Unmarshal(params[0], &filter) != nil {
		return nil, errors.New("invalid log filter")
	}
	from, fromErr := hexutil.DecodeUint64(filter.FromBlock)
	to, toErr := hexutil.DecodeUint64(filter.ToBlock)
	if fromErr != nil || toErr != nil || !strings.Contains(strings.ToLower(string(filter.Address)), strings.ToLower(self.address.Hex())) || len(filter.Topics) != 1 || len(filter.Topics[0]) != 1 || !strings.EqualFold(filter.Topics[0][0], stFleetBoundTopic.Hex()) {
		return nil, fmt.Errorf("unexpected log filter %+v", filter)
	}
	self.stateLock.Lock()
	self.logRanges = append(self.logRanges, [2]uint64{from, to})
	self.stateLock.Unlock()
	logs := []types.Log{}
	for _, log := range self.logs {
		if from <= log.BlockNumber && log.BlockNumber <= to {
			logs = append(logs, log)
		}
	}
	return logs, nil
}

// One bindingAt eth_call at a boundary block.
func (self *stHeadExclusionRPC) call(params []json.RawMessage) (any, error) {
	var callArgs struct {
		To   string `json:"to"`
		Data string `json:"data"`
	}
	var blockTag string
	if len(params) != 2 || json.Unmarshal(params[0], &callArgs) != nil || json.Unmarshal(params[1], &blockTag) != nil {
		return nil, errors.New("invalid eth_call")
	}
	calldata, decodeErr := hexutil.Decode(callArgs.Data)
	block, blockErr := hexutil.DecodeUint64(blockTag)
	selector := stabi.NewSTCoordinator().PackBindingAt([16]byte{}, new(big.Int))[:4]
	if decodeErr != nil || blockErr != nil || len(calldata) != 68 || !bytes.Equal(calldata[:4], selector) || !strings.EqualFold(callArgs.To, self.address.Hex()) || block != self.startBlock && block != self.closeBlock || new(big.Int).SetBytes(calldata[36:68]).Uint64() != self.epoch {
		return nil, fmt.Errorf("unexpected bindingAt call to=%s block=%s", callArgs.To, blockTag)
	}
	self.stateLock.Lock()
	self.bindingReads++
	self.stateLock.Unlock()
	var clientId [16]byte
	copy(clientId[:], calldata[4:20])
	state := self.bindingAt(clientId, block)
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	encoded, err := parsed.Methods["bindingAt"].Outputs.Pack(state.Active, stabi.STCoordinatorBindingRecord{
		FleetId: state.FleetId, Hotkey: state.Hotkey, ClientKey: state.ClientKey, CommitmentHash: state.CommitmentHash,
		Generation: state.Generation, ValidFromEpoch: state.ValidFrom, ValidToEpoch: state.ValidTo,
		CleanedAtEpoch: state.CleanedAtEpoch, Uid: state.Uid, Cleaned: state.Cleaned,
	})
	if err != nil {
		return nil, err
	}
	return "0x" + hex.EncodeToString(encoded), nil
}

func (self *stHeadExclusionRPC) answer(call stEpochRPCRequest) map[string]any {
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	var result any
	var err error
	switch call.Method {
	case "eth_chainId":
		result = "0x3b1"
	case "eth_getBlockByNumber":
		var selector string
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &selector) != nil {
			err = errors.New("invalid block selector")
			break
		}
		block := self.finalized
		if selector != "finalized" {
			block, err = hexutil.DecodeUint64(selector)
		}
		result = stBlockBatchResult(block)
	case "eth_getLogs":
		if self.logError {
			err = errors.New("logs unavailable")
			break
		}
		result, err = self.filterLogs(call.Params)
	case "eth_call":
		result, err = self.call(call.Params)
	default:
		err = fmt.Errorf("unexpected method %s", call.Method)
	}
	if err != nil {
		response["error"] = map[string]any{"code": -32000, "message": err.Error()}
	} else {
		response["result"] = result
	}
	return response
}

func (self *stHeadExclusionRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read head exclusion RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var call stEpochRPCRequest
		if json.Unmarshal(body, &call) != nil {
			self.t.Errorf("invalid head exclusion RPC request: %s", body)
			return
		}
		_ = json.NewEncoder(writer).Encode(self.answer(call))
		return
	}
	var calls []stEpochRPCRequest
	if json.Unmarshal(body, &calls) != nil || len(calls) == 0 || len(calls) > stMaximumEVMRPCBatchCalls {
		self.t.Errorf("invalid head exclusion RPC batch: %s", body)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[len(calls)-1-index] = self.answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

// A FleetBound log of the coordinator at a canonical block.
func stFleetBoundTestLog(t testing.TB, address common.Address, block uint64, index uint, clientId [16]byte, generation uint64, from uint64, through uint64) types.Log {
	t.Helper()
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["FleetBound"]
	data, err := event.Inputs.NonIndexed().Pack(uint16(20+clientId[0]), generation, from, through)
	if err != nil {
		t.Fatal(err)
	}
	var clientTopic common.Hash
	copy(clientTopic[:16], clientId[:])
	return types.Log{Address: address, Topics: []common.Hash{event.ID, clientTopic, {clientId[0], 0xf1}, {clientId[0], 0xf2}}, Data: data,
		BlockNumber: block, TxHash: common.Hash{byte(block >> 8), byte(block), byte(index)}, BlockHash: common.HexToHash(stBlockBatchHash(block)), Index: index}
}

// A binding record bindingAt returns for a client, active or not.
func stHeadExclusionBinding(clientId [16]byte, active bool, generation uint64) *StFleetBindingState {
	state := stBindingFixtureState(clientId)
	state.Active, state.Generation, state.ValidFrom, state.ValidTo = active, generation, 40, 60
	return state
}

func newStHeadExclusionClient(t testing.TB, fixture *stHeadExclusionRPC) *CoreStClient {
	t.Helper()
	client := newStBlockBatchClient(t, fixture)
	client.cfg.ContractAddress = fixture.address
	client.coordinator = stabi.NewSTCoordinator()
	return client
}

// GL-7, SV-6: the topic the scan filters on is STCoordinator's FleetBound.
func TestStFleetBoundTopicIsTheCoordinatorEvent(t *testing.T) {
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Events["FleetBound"].ID != stFleetBoundTopic || model.StFleetBoundEventKind != stabi.STCoordinatorFleetBoundEventName {
		t.Fatal("the FleetBound topic or kind differs from the coordinator ABI")
	}
}

// GL-7, SV-6: FleetBound logs decode into mirror rows with their exact fields;
// a removed log is dropped; an undecodable log, an RPC error and a client
// without the release coordinator each fail the range.
func TestCoreStClientFleetBoundEvents(t *testing.T) {
	address := common.HexToAddress("0x2000000000000000000000000000000000000002")
	clientId := [16]byte{7, 1}
	log := stFleetBoundTestLog(t, address, 120, 3, clientId, 9, 41, 59)
	removed := stFleetBoundTestLog(t, address, 121, 0, [16]byte{8}, 1, 41, 59)
	removed.Removed = true
	fixture := &stHeadExclusionRPC{t: t, address: address, finalized: 999, logs: []types.Log{log, removed}}
	client := newStHeadExclusionClient(t, fixture)
	events, err := client.FleetBoundEvents(context.Background(), 100, 199)
	if err != nil || len(events) != 1 {
		t.Fatal("FleetBound range", events, err)
	}
	var data model.StFleetBoundEventData
	if err := json.Unmarshal([]byte(events[0].DataJson), &data); err != nil {
		t.Fatal(err)
	}
	want := model.StFleetBoundEventData{ClientId: "0x07010000000000000000000000000000", FleetId: "0x07f1" + strings.Repeat("0", 60), Hotkey: "0x07f2" + strings.Repeat("0", 60),
		Uid: "27", Generation: "9", ValidFromEpoch: "41", ValidToEpoch: "59"}
	event := events[0]
	if data != want || event.Kind != model.StFleetBoundEventKind || event.BlockNumber != 120 || event.LogIndex != 3 || event.BlockHash != log.BlockHash.Hex() || event.TxHash != log.TxHash.Hex() || len(event.OriginalLog) == 0 {
		t.Fatalf("FleetBound event %+v data %+v", event, data)
	}
	if ranges, _ := fixture.snapshot(); !slices.Equal(ranges, [][2]uint64{{100, 199}}) {
		t.Fatal("FleetBound filter ranges", ranges)
	}
	undecodable := log
	undecodable.Data = undecodable.Data[:32]
	fixture.logs = []types.Log{undecodable}
	if events, err := client.FleetBoundEvents(context.Background(), 100, 199); events != nil || err == nil || !strings.Contains(err.Error(), "FleetBound log 120:3") {
		t.Fatal("an undecodable FleetBound log was accepted", events, err)
	}
	fixture.logError = true
	if events, err := client.FleetBoundEvents(context.Background(), 100, 199); events != nil || err == nil || !strings.Contains(err.Error(), "logs unavailable") {
		t.Fatal("a failed log read was accepted", events, err)
	}
	client.coordinator = nil
	if events, err := client.FleetBoundEvents(context.Background(), 100, 199); events != nil || err == nil || !strings.Contains(err.Error(), "release coordinator") {
		t.Fatal("a client without the release coordinator read FleetBound logs", events, err)
	}
}

// GL-7, SV-2, SV-6: on bindings active at the start only, the end only, both
// and neither, renewed, outside the epoch, bound after the close and bound for
// a client outside the census, the candidate read excludes exactly the
// providers, with exactly the bindings, that v1's read of every provider
// excludes, while reading bindingAt only for the candidates. A later read
// scans only the blocks after the scan's checkpoint.
func TestStHeadExcludedClientsEqualV1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		address := common.HexToAddress("0x2000000000000000000000000000000000000002")
		both, startOnly, closeOnly, neither, renewed, outside, afterClose, unobserved := [16]byte{11}, [16]byte{12}, [16]byte{13}, [16]byte{14}, [16]byte{15}, [16]byte{16}, [16]byte{17}, [16]byte{18}
		fixture := &stHeadExclusionRPC{t: t, address: address, finalized: 3_000, epoch: 50, startBlock: 2_000, closeBlock: 2_500,
			logs: []types.Log{
				stFleetBoundTestLog(t, address, 400, 0, both, 1, 40, 60),
				stFleetBoundTestLog(t, address, 1_200, 0, startOnly, 1, 45, 55),
				stFleetBoundTestLog(t, address, 1_200, 1, closeOnly, 1, 50, 50),
				stFleetBoundTestLog(t, address, 1_999, 0, neither, 1, 41, 70),
				stFleetBoundTestLog(t, address, 900, 0, renewed, 1, 30, 49),
				stFleetBoundTestLog(t, address, 2_100, 2, renewed, 2, 50, 80),
				stFleetBoundTestLog(t, address, 1_000, 0, outside, 1, 51, 60),
				stFleetBoundTestLog(t, address, 2_501, 0, afterClose, 1, 50, 60),
				stFleetBoundTestLog(t, address, 2_400, 0, unobserved, 1, 50, 60),
			},
		}
		fixture.bindingAt = func(clientId [16]byte, block uint64) *StFleetBindingState {
			switch clientId {
			case both:
				return stHeadExclusionBinding(clientId, true, 1)
			case startOnly:
				return stHeadExclusionBinding(clientId, block == fixture.startBlock, 1)
			case closeOnly:
				return stHeadExclusionBinding(clientId, block == fixture.closeBlock, 1)
			case renewed:
				return stHeadExclusionBinding(clientId, block == fixture.closeBlock, 2)
			case neither:
				return stHeadExclusionBinding(clientId, false, 1)
			}
			return &StFleetBindingState{}
		}
		client := newStHeadExclusionClient(t, fixture)
		cfg := &StConfig{ChainId: 945, ContractAddress: address, DeployBlock: 100}
		clientIds := [][16]byte{}
		for index := range 40 {
			clientIds = append(clientIds, [16]byte{byte(100 + index)})
		}
		clientIds = append(clientIds, both, startOnly, closeOnly, neither, renewed, outside, afterClose)

		v1, err := client.BindingsAt(ctx, clientIds, fixture.epoch, fixture.startBlock, fixture.closeBlock)
		if err != nil {
			t.Fatal(err)
		}
		_, v1Reads := fixture.snapshot()
		fixture.reset()
		excluded, err := stHeadExcludedClients(ctx, cfg, client, fixture.epoch, fixture.startBlock, fixture.closeBlock, clientIds)
		if err != nil {
			t.Fatal(err)
		}
		ranges, reads := fixture.snapshot()
		for index, clientId := range clientIds {
			if got := excluded[clientId]; v1[index].Active != (got != nil) || got != nil && *got != *v1[index] {
				t.Fatalf("client %x: v1 %+v, candidates %+v", clientId, v1[index], got)
			}
		}
		if want := []([16]byte){both, startOnly, closeOnly, renewed}; len(excluded) != len(want) {
			t.Fatalf("excluded %d providers, want %x", len(excluded), want)
		}
		// five candidates at two boundaries, where v1 reads every provider
		if v1Reads != 2*len(clientIds) || reads != 2*5 {
			t.Fatalf("bindingAt reads v1 %d, candidates %d", v1Reads, reads)
		}
		if !slices.Equal(ranges, [][2]uint64{{100, 1_099}, {1_100, 2_099}, {2_100, 2_500}}) || model.GetStFleetBoundNextBlock(ctx, cfg.DeploymentKey()) != 2_501 {
			t.Fatal("the FleetBound scan ranges or checkpoint", ranges)
		}
		// the next epoch scans only its new blocks, where the binding made
		// after the last close becomes a candidate
		fixture.reset()
		fixture.epoch, fixture.startBlock, fixture.closeBlock = 51, 2_500, 2_700
		excluded, err = stHeadExcludedClients(ctx, cfg, client, fixture.epoch, fixture.startBlock, fixture.closeBlock, clientIds)
		if ranges, reads := fixture.snapshot(); err != nil || !slices.Equal(ranges, [][2]uint64{{2_501, 2_700}}) || reads != 2*6 || len(excluded) != 3 || excluded[afterClose] != nil {
			t.Fatal("the next epoch rescanned or misread", ranges, reads, excluded, err)
		}
	})
}

// A stub chain whose FleetBound range, canonical hashes and bindings a test
// sets per case.
type stHeadExclusionStub struct {
	*stubStClient
	finalizedErr error
	events       []*model.StChainEvent
	eventsErr    error
	hashes       func(blocks []uint64) ([][32]byte, error)
	bindings     func(clientIds [][16]byte) ([]*StFleetBindingState, error)
}

func (self *stHeadExclusionStub) FinalizedHead(ctx context.Context) (uint64, [32]byte, time.Time, error) {
	if self.finalizedErr != nil {
		return 0, [32]byte{}, time.Time{}, self.finalizedErr
	}
	return self.stubStClient.FinalizedHead(ctx)
}

func (self *stHeadExclusionStub) FleetBoundEvents(context.Context, uint64, uint64) ([]*model.StChainEvent, error) {
	return self.events, self.eventsErr
}

func (self *stHeadExclusionStub) BlockHashes(ctx context.Context, blocks []uint64) ([][32]byte, error) {
	if self.hashes != nil {
		return self.hashes(blocks)
	}
	return self.stubStClient.BlockHashes(ctx, blocks)
}

func (self *stHeadExclusionStub) BindingsAt(ctx context.Context, clientIds [][16]byte, epoch uint64, startBlock uint64, closeBlock uint64) ([]*StFleetBindingState, error) {
	if self.bindings != nil {
		return self.bindings(clientIds)
	}
	return self.stubStClient.BindingsAt(ctx, clientIds, epoch, startBlock, closeBlock)
}

// A FleetBound mirror row the stub returns, at a block whose canonical hash
// the base stub reports.
func stHeadExclusionStubEvent(block uint64, clientId [16]byte) *model.StChainEvent {
	data, _ := json.Marshal(model.StFleetBoundEventData{ClientId: fmt.Sprintf("0x%x", clientId), ValidFromEpoch: "40", ValidToEpoch: "60"})
	return &model.StChainEvent{BlockNumber: block, BlockHash: common.Hash{1}.Hex(), LogIndex: 0, TxHash: common.Hash{2}.Hex(), Kind: model.StFleetBoundEventKind, DataJson: string(data)}
}

// GL-7, SV-6: each refusal of the FleetBound scan and the candidate read
// triggers exactly, and a refused range changes neither the mirror nor the
// checkpoint.
func TestStHeadExcludedClientsRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		state := &StEpochState{HeadBlock: 5_000}
		cfg := &StConfig{ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"), DeployBlock: 100}
		valid := stHeadExclusionStubEvent(150, [16]byte{1})
		outside, wrongKind, mismatched := *valid, *valid, *valid
		outside.BlockNumber = 50
		wrongKind.Kind = "Deposited"
		mismatched.BlockHash = common.Hash{3}.Hex()
		for _, refused := range []struct {
			label      string
			cfg        *StConfig
			startBlock uint64
			closeBlock uint64
			stub       *stHeadExclusionStub
			want       string
		}{
			{"reversed window", cfg, 300, 200, &stHeadExclusionStub{}, "block window [300,200] is invalid"},
			{"no deploy block", &StConfig{ChainId: 945, ContractAddress: cfg.ContractAddress}, 100, 200, &stHeadExclusionStub{}, "deploy_block must be nonzero"},
			{"finalized head", cfg, 100, 200, &stHeadExclusionStub{finalizedErr: errors.New("head unavailable")}, "head unavailable"},
			{"unfinalized close", cfg, 100, 5_001, &stHeadExclusionStub{}, "ahead of finalized head 5000"},
			{"log read", cfg, 100, 200, &stHeadExclusionStub{eventsErr: errors.New("logs unavailable")}, "logs unavailable"},
			{"nil event", cfg, 100, 200, &stHeadExclusionStub{events: []*model.StChainEvent{nil}}, "outside the requested range"},
			{"outside event", cfg, 100, 200, &stHeadExclusionStub{events: []*model.StChainEvent{&outside}}, "outside the requested range"},
			{"other kind", cfg, 100, 200, &stHeadExclusionStub{events: []*model.StChainEvent{&wrongKind}}, "outside the requested range"},
			{"canonical read", cfg, 100, 200, &stHeadExclusionStub{events: []*model.StChainEvent{valid}, hashes: func([]uint64) ([][32]byte, error) { return nil, errors.New("hashes unavailable") }}, "hashes unavailable"},
			{"canonical count", cfg, 100, 200, &stHeadExclusionStub{events: []*model.StChainEvent{valid}, hashes: func([]uint64) ([][32]byte, error) { return nil, nil }}, "returned 0 hashes, want 1"},
			{"canonical mismatch", cfg, 100, 200, &stHeadExclusionStub{events: []*model.StChainEvent{&mismatched}}, "block hash mismatch at 150"},
		} {
			refused.stub.stubStClient = newStubStClient(state)
			excluded, err := stHeadExcludedClients(ctx, refused.cfg, refused.stub, 50, refused.startBlock, refused.closeBlock, [][16]byte{{1}})
			if excluded != nil || err == nil || !strings.Contains(err.Error(), refused.want) {
				t.Fatalf("%s: excluded %v, error %v", refused.label, excluded, err)
			}
			if model.GetStFleetBoundNextBlock(ctx, cfg.DeploymentKey()) != 0 || len(model.GetStEvents(ctx, cfg.DeploymentKey(), 0, 5_000)) != 0 {
				t.Fatalf("%s: a refused range changed the mirror", refused.label)
			}
		}
		// the candidates' binding batch is refused whole
		for _, refused := range []struct {
			label    string
			bindings func([][16]byte) ([]*StFleetBindingState, error)
			want     string
		}{
			{"binding read", func([][16]byte) ([]*StFleetBindingState, error) { return nil, errors.New("bindings unavailable") }, "bindingAt batch: bindings unavailable"},
			{"binding count", func([][16]byte) ([]*StFleetBindingState, error) { return nil, nil }, "returned 0 rows for 1 clients"},
			{"nil binding", func([][16]byte) ([]*StFleetBindingState, error) { return []*StFleetBindingState{nil}, nil }, "row 0 for 01000000000000000000000000000000 is nil"},
		} {
			stub := &stHeadExclusionStub{stubStClient: newStubStClient(state), events: []*model.StChainEvent{valid}, bindings: refused.bindings}
			if excluded, err := stHeadExcludedClients(ctx, cfg, stub, 50, 100, 200, [][16]byte{{1}}); excluded != nil || err == nil || !strings.Contains(err.Error(), refused.want) {
				t.Fatalf("%s: excluded %v, error %v", refused.label, excluded, err)
			}
		}
		// a malformed mirrored FleetBound fails the candidate read
		malformed := stHeadExclusionStubEvent(250, [16]byte{2})
		malformed.DataJson = `{"client_id":"0x02","valid_from_epoch":"1","valid_to_epoch":"9"}`
		stub := &stHeadExclusionStub{stubStClient: newStubStClient(state), events: []*model.StChainEvent{malformed}}
		if excluded, err := stHeadExcludedClients(ctx, cfg, stub, 50, 100, 300, [][16]byte{{2}}); excluded != nil || err == nil || !strings.Contains(err.Error(), "FleetBound event 250:0") {
			t.Fatal("a malformed mirrored FleetBound was skipped", excluded, err)
		}
	})
}

// GL-7, SV-6: the configured entry point reads through the configured client,
// and refuses without configuration.
func TestStHeadExcludedClientsConfigured(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		SetStConfig(nil)
		SetStClient(nil)
		if excluded, err := StHeadExcludedClients(ctx, 50, 100, 200, [][16]byte{{1}}); excluded != nil || !errors.Is(err, errStNotConfigured) {
			t.Fatal("head exclusion ran without a configuration", excluded, err)
		}
		cfg := &StConfig{Enabled: true, ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"), DeployBlock: 100}
		stub := &stHeadExclusionStub{stubStClient: newStubStClient(&StEpochState{HeadBlock: 5_000}), events: []*model.StChainEvent{stHeadExclusionStubEvent(150, [16]byte{1})}}
		stub.bindings = func(clientIds [][16]byte) ([]*StFleetBindingState, error) {
			bindings := make([]*StFleetBindingState, len(clientIds))
			for index, clientId := range clientIds {
				bindings[index] = stHeadExclusionBinding(clientId, true, 3)
			}
			return bindings, nil
		}
		SetStConfig(cfg)
		SetStClient(stub)
		t.Cleanup(func() {
			SetStClient(nil)
			SetStConfig(nil)
		})
		excluded, err := StHeadExcludedClients(ctx, 50, 100, 200, [][16]byte{{1}, {2}})
		if err != nil || len(excluded) != 1 || excluded[[16]byte{1}] == nil || excluded[[16]byte{1}].Generation != 3 {
			t.Fatal("the configured head exclusion", excluded, err)
		}
	})
}
