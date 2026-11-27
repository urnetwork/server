package controller

// The epoch 8 late-binding window on the release client's bindingAt reads
// (sn protocol/fleet_late_binding_epoch8.go): the endpoint answers bindingAt
// at any block as the coordinator does, from each client's binding versions.

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The SN25 mainnet coordinator, whose epoch 8 has the window; a literal, so
// the test reads the same against the code from before the window.
var stLateBindingCoordinator = common.HexToAddress("0xC18925925E2B7bb9059b7d696b8c92762AE86406")

// One binding version: the block it landed in and its signed validity.
type stLateBindingVersion struct {
	landed, generation, validFrom, validTo uint64
}

type stLateBindingRPC struct {
	t         *testing.T
	finalized uint64
	versions  map[[16]byte][]stLateBindingVersion
}

func (self *stLateBindingRPC) bindingAt(client [16]byte, epoch, block uint64) (bool, stabi.STCoordinatorBindingRecord) {
	versions := self.versions[client]
	for i := len(versions) - 1; 0 <= i; i-- {
		version := versions[i]
		if block < version.landed || epoch < version.validFrom {
			continue
		}
		return epoch <= version.validTo, stabi.STCoordinatorBindingRecord{FleetId: [32]byte{0xf1}, Hotkey: [32]byte{0xf2}, ClientKey: [32]byte{0xf3}, CommitmentHash: [32]byte{0xf4}, Generation: version.generation, ValidFromEpoch: version.validFrom, ValidToEpoch: version.validTo, Uid: 9}
	}
	return false, stabi.STCoordinatorBindingRecord{}
}

func (self *stLateBindingRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read late binding RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var call stEpochRPCRequest
		if json.Unmarshal(body, &call) != nil {
			self.t.Errorf("invalid late binding RPC request: %s", body)
			return
		}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3c4"
		case "eth_getBlockByNumber":
			result = stBlockBatchResult(self.finalized)
		default:
			self.t.Errorf("unexpected late binding RPC method %s", call.Method)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
		return
	}
	var calls []stEpochRPCRequest
	if json.Unmarshal(body, &calls) != nil || len(calls) == 0 {
		self.t.Errorf("invalid late binding RPC batch: %s", body)
		return
	}
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
			self.t.Errorf("invalid late binding batch element %d", index)
			return
		}
		calldata, decodeErr := hexutil.Decode(callArgs.Data)
		block, blockErr := hexutil.DecodeUint64(blockTag)
		if decodeErr != nil || blockErr != nil || len(calldata) != 68 || !strings.EqualFold(callArgs.To, stLateBindingCoordinator.Hex()) || self.finalized < block {
			self.t.Errorf("invalid late binding call %d to=%s block=%s", index, callArgs.To, blockTag)
			return
		}
		var client [16]byte
		copy(client[:], calldata[4:20])
		active, record := self.bindingAt(client, new(big.Int).SetBytes(calldata[36:68]).Uint64(), block)
		encoded, err := parsed.Methods["bindingAt"].Outputs.Pack(active, record)
		if err != nil {
			self.t.Errorf("pack late binding result %d: %v", index, err)
			return
		}
		responses[index] = map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x" + hex.EncodeToString(encoded)}
	}
	if err := json.NewEncoder(writer).Encode(responses); err != nil {
		self.t.Errorf("encode late binding batch response: %v", err)
	}
}

var (
	stLateBindingLate   = [16]byte{0x01}
	stLateBindingLast   = [16]byte{0x02}
	stLateBindingBefore = [16]byte{0x03}
	stLateBindingAfter  = [16]byte{0x04}
	stLateBindingNormal = [16]byte{0x05}
)

// Bindings landing at 9,600,029 and 9,615,060 are in the window; one landing
// at 9,600,027 is before epoch 8 and one at 9,615,061 is past the deadline,
// both signed from epoch 9; one bound in epoch 7 from epoch 8 is ordinary.
func newStLateBindingClient(t *testing.T, finalized uint64) *CoreStClient {
	t.Helper()
	fixture := &stLateBindingRPC{t: t, finalized: finalized, versions: map[[16]byte][]stLateBindingVersion{
		stLateBindingLate:   {{landed: 9_600_029, generation: 1, validFrom: 9, validTo: 20}},
		stLateBindingLast:   {{landed: 9_615_060, generation: 1, validFrom: 9, validTo: 20}},
		stLateBindingBefore: {{landed: 9_600_027, generation: 1, validFrom: 9, validTo: 20}},
		stLateBindingAfter:  {{landed: 9_615_061, generation: 1, validFrom: 9, validTo: 20}},
		stLateBindingNormal: {{landed: 9_560_000, generation: 1, validFrom: 8, validTo: 19}},
	}}
	client := newStBlockBatchClient(t, fixture)
	client.cfg.ChainId, client.cfg.ContractAddress = 964, stLateBindingCoordinator
	client.coordinator = stabi.NewSTCoordinator()
	return client
}

// Head exclusion for epoch 8, at its two boundaries, counts the bindings that
// landed in the window as active, and only them, so the epoch 8 ledger, the
// extender and capture the flag censuses and the draw leave them out of the
// provider pool as the validator's audit expects.
func TestCoreStClientBindingsAtCountsEpoch8LateBindings(t *testing.T) {
	client := newStLateBindingClient(t, 9_650_500)
	clients := [][16]byte{stLateBindingLate, stLateBindingLast, stLateBindingBefore, stLateBindingAfter, stLateBindingNormal}
	bindings, err := client.BindingsAt(t.Context(), clients, 8, 9_600_028, 9_650_428)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, true, false, false, true}
	for index, binding := range bindings {
		if binding.Active != want[index] {
			t.Fatalf("client %x: active %t, want %t (%+v)", clients[index][0], binding.Active, want[index], binding)
		}
		if binding.Active && (8 < binding.ValidFrom || binding.ValidTo < 8) {
			t.Fatalf("client %x: an active record outside epoch 8: %+v", clients[index][0], binding)
		}
	}
	// epoch 9 reads are the coordinator's own
	nine, err := client.BindingsAt(t.Context(), clients, 9, 9_650_428, 9_650_428)
	if err != nil {
		t.Fatal(err)
	}
	for index, binding := range nine {
		if !binding.Active || binding.ValidFrom != []uint64{9, 9, 9, 9, 8}[index] {
			t.Fatalf("client %x at epoch 9: %+v", clients[index][0], binding)
		}
	}
}

// GET /sn/head's fallback read counts a window binding for epoch 8 from the
// block it lands in.
func TestCoreStClientBindingAtCountsAnEpoch8LateBinding(t *testing.T) {
	client := newStLateBindingClient(t, 9_600_100)
	late, err := client.BindingAt(t.Context(), stLateBindingLate, 8)
	if err != nil || !late.Active || late.ValidFrom != 8 || late.Generation != 1 {
		t.Fatalf("the window binding at block 9,600,100: %+v %v", late, err)
	}
	before, err := client.BindingAt(t.Context(), stLateBindingBefore, 8)
	if err != nil || before.Active {
		t.Fatalf("a binding from before epoch 8 at block 9,600,100: %+v %v", before, err)
	}
}

// The verify cohort holds headroom for members of fleets bound from the next
// epoch. A window binding counts from epoch 8, so at epoch 8 it is a current
// member, not an upcoming one; a binding signed from epoch 9 outside the
// window is upcoming.
func TestSnUpcomingClaimedMembersSkipsEpoch8Window(t *testing.T) {
	version := func(client byte, block uint64, window bool) *model.SnFleetBindingVersion {
		return &model.SnFleetBindingVersion{ClientId: server.Id{client}, Generation: 1, FleetId: [32]byte{client}, Hotkey: [32]byte{client, 0xf2}, Uid: uint16(client),
			ValidFromEpoch: 9, SignedValidToEpoch: 20, ValidToEpoch: 20, BoundBlock: block, Epoch8Window: window}
	}
	versions := []*model.SnFleetBindingVersion{version(1, 9_600_029, true), version(2, 9_615_061, false)}
	if upcoming := snUpcomingClaimedMembers(versions, 8, 7); upcoming != 1 {
		t.Fatalf("upcoming members at epoch 8: %d, want 1", upcoming)
	}
}
