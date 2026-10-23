// Synthetic subnet chain fixtures for the §22.6 through §22.9 family readers.
// A test node answers the exact relayed json rpc batches through HostStdin;
// contract handlers decode and encode with the pinned abi bindings, so tests
// exercise the real request rendering, canonical re-encoding and decoders.
// Every identity is synthetic; nothing reaches a socket, shell or chain.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/ss58"
)

// A contract view handler: outputs, or the name of a custom error to revert with.
type subnetReplayTestHandler func(method string, args []any) (outputs []any, revert string)

// One synthetic contract.
type subnetReplayTestContract struct {
	contract *abi.ABI
	handle   subnetReplayTestHandler
	deployed uint64
}

// A synthetic archive node with canonical evm blocks, contracts and logs.
type subnetReplayTestNode struct {
	t           *testing.T
	stateLock   sync.Mutex
	chainId     uint64
	genesis     common.Hash
	blocks      map[uint64]common.Hash
	finalized   uint64
	contracts   map[common.Address]*subnetReplayTestContract
	logs        map[common.Address][]types.Log
	native      func(method string, params []json.RawMessage) (any, *subnetReplayRpcFailure)
	balances    map[common.Address]*big.Int
	nonces      map[common.Address]uint64
	failMethods map[string]bool
	exchanges   int
	calls       []string
	// Runs before each exchange is answered; a test can reorg or fail here.
	beforeExchange func(exchange int)
	settings       *SubnetReplaySettings
	rpcPort        int
}

func newSubnetReplayTestNode(t *testing.T, chainId uint64, genesis common.Hash, finalized uint64) *subnetReplayTestNode {
	node := &subnetReplayTestNode{
		t: t, chainId: chainId, genesis: genesis, blocks: map[uint64]common.Hash{}, finalized: finalized,
		contracts: map[common.Address]*subnetReplayTestContract{}, logs: map[common.Address][]types.Log{},
		failMethods: map[string]bool{}, rpcPort: 9944,
		balances: map[common.Address]*big.Int{}, nonces: map[common.Address]uint64{},
	}
	for number := uint64(0); number <= finalized+16; number++ {
		node.blocks[number] = subnetReplayTestBlockHash(number, 0)
	}
	return node
}

// Distinct synthetic evm hashes per height and fork generation.
func subnetReplayTestBlockHash(number uint64, generation byte) common.Hash {
	var hash common.Hash
	hash[0] = 0xe0 | generation
	hash[1] = 0x01
	big.NewInt(0).SetUint64(number + 1).FillBytes(hash[24:])
	return hash
}

func (self *subnetReplayTestNode) blockNumberOf(hash common.Hash) (uint64, bool) {
	for number, candidate := range self.blocks {
		if candidate == hash {
			return number, true
		}
	}
	return 0, false
}

// The synthetic source's stdin seam: the command must be the exact relay.
func (self *subnetReplayTestNode) relay(command string, stdin string) (string, error) {
	self.stateLock.Lock()
	expected := subnetReplayRelayCommand(self.rpcPort, self.settings)
	self.stateLock.Unlock()
	if command != expected {
		self.t.Fatalf("relay command departed from the fixed loopback relay")
	}
	self.stateLock.Lock()
	self.exchanges += 1
	exchange := self.exchanges
	hook := self.beforeExchange
	self.stateLock.Unlock()
	if hook != nil {
		hook(exchange)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var requests []struct {
		Jsonrpc string            `json:"jsonrpc"`
		Id      int               `json:"id"`
		Method  string            `json:"method"`
		Params  []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(stdin), &requests); err != nil {
		self.t.Fatalf("relay stdin is not a json rpc batch: %v", err)
	}
	responses := make([]map[string]any, 0, len(requests))
	for _, request := range requests {
		self.calls = append(self.calls, request.Method)
		result, failure := self.answer(request.Method, request.Params)
		if self.failMethods[request.Method] {
			result, failure = nil, &subnetReplayRpcFailure{Code: -32000, Message: "synthetic failure"}
		}
		// A nil result marshals as an explicit json null: valid absent state.
		member := map[string]any{"jsonrpc": "2.0", "id": request.Id}
		if failure != nil {
			member["error"] = failure
		} else {
			member["result"] = result
		}
		responses = append(responses, member)
	}
	encoded, err := json.Marshal(responses)
	if err != nil {
		self.t.Fatal(err)
	}
	return string(encoded), nil
}

func (self *subnetReplayTestNode) answer(method string, params []json.RawMessage) (any, *subnetReplayRpcFailure) {
	switch method {
	case "eth_chainId":
		return hexutil.EncodeUint64(self.chainId), nil
	case "chain_getBlockHash":
		var number uint64
		if len(params) != 1 || json.Unmarshal(params[0], &number) != nil {
			self.t.Fatalf("chain_getBlockHash params are invalid")
		}
		if number == 0 {
			return self.genesis.Hex(), nil
		}
		if self.native != nil {
			return self.native(method, params)
		}
		return nil, nil
	case "eth_getBlockByNumber":
		var tag string
		if len(params) != 2 || json.Unmarshal(params[0], &tag) != nil {
			self.t.Fatalf("eth_getBlockByNumber params are invalid")
		}
		number := self.finalized
		if tag != "finalized" {
			parsed, err := hexutil.DecodeUint64(tag)
			if err != nil {
				self.t.Fatalf("eth_getBlockByNumber tag is invalid")
			}
			number = parsed
		}
		// Like a real node, numbered reads also serve recent unfinalized blocks.
		hash, ok := self.blocks[number]
		if !ok {
			return nil, nil
		}
		return map[string]any{"number": hexutil.EncodeUint64(number), "hash": hash.Hex(), "parentHash": common.Hash{}.Hex()}, nil
	case "eth_call":
		return self.call(params)
	case "eth_getCode":
		var address common.Address
		var selector struct {
			BlockHash        common.Hash `json:"blockHash"`
			RequireCanonical bool        `json:"requireCanonical"`
		}
		if len(params) != 2 || json.Unmarshal(params[0], &address) != nil || json.Unmarshal(params[1], &selector) != nil || !selector.RequireCanonical {
			self.t.Fatalf("eth_getCode is not canonical hash pinned")
		}
		number, ok := self.blockNumberOf(selector.BlockHash)
		if !ok {
			return nil, &subnetReplayRpcFailure{Code: -32000, Message: "unknown block"}
		}
		contract := self.contracts[address]
		if contract == nil || number < contract.deployed {
			return "0x", nil
		}
		return "0x6080", nil
	case "eth_getBalance", "eth_getTransactionCount":
		var address common.Address
		var selector struct {
			BlockHash        common.Hash `json:"blockHash"`
			RequireCanonical bool        `json:"requireCanonical"`
		}
		if len(params) != 2 || json.Unmarshal(params[0], &address) != nil || json.Unmarshal(params[1], &selector) != nil || !selector.RequireCanonical {
			self.t.Fatalf("%s is not canonical hash pinned", method)
		}
		if _, ok := self.blockNumberOf(selector.BlockHash); !ok {
			return nil, &subnetReplayRpcFailure{Code: -32000, Message: "unknown block"}
		}
		if method == "eth_getTransactionCount" {
			return hexutil.EncodeUint64(self.nonces[address]), nil
		}
		balance, ok := self.balances[address]
		if !ok {
			balance = new(big.Int)
		}
		return hexutil.EncodeBig(balance), nil
	case "eth_getLogs":
		var filter struct {
			Address   common.Address `json:"address"`
			FromBlock string         `json:"fromBlock"`
			ToBlock   string         `json:"toBlock"`
		}
		if len(params) != 1 || json.Unmarshal(params[0], &filter) != nil {
			self.t.Fatalf("eth_getLogs params are invalid")
		}
		from, fromErr := hexutil.DecodeUint64(filter.FromBlock)
		to, toErr := hexutil.DecodeUint64(filter.ToBlock)
		if fromErr != nil || toErr != nil || self.finalized < to {
			self.t.Fatalf("eth_getLogs window is not a finalized numeric window")
		}
		selected := []types.Log{}
		for _, log := range self.logs[filter.Address] {
			if from <= log.BlockNumber && log.BlockNumber <= to {
				selected = append(selected, log)
			}
		}
		sort.SliceStable(selected, func(i, j int) bool {
			if selected[i].BlockNumber != selected[j].BlockNumber {
				return selected[i].BlockNumber < selected[j].BlockNumber
			}
			return selected[i].Index < selected[j].Index
		})
		return selected, nil
	default:
		if self.native != nil {
			return self.native(method, params)
		}
		self.t.Fatalf("unexpected relayed method %s", method)
		return nil, nil
	}
}

func (self *subnetReplayTestNode) call(params []json.RawMessage) (any, *subnetReplayRpcFailure) {
	var request struct {
		To    common.Address `json:"to"`
		Input hexutil.Bytes  `json:"input"`
	}
	var selector struct {
		BlockHash        common.Hash `json:"blockHash"`
		RequireCanonical bool        `json:"requireCanonical"`
	}
	if len(params) != 2 || json.Unmarshal(params[0], &request) != nil || json.Unmarshal(params[1], &selector) != nil {
		self.t.Fatalf("eth_call params are invalid")
	}
	if !selector.RequireCanonical {
		self.t.Fatalf("eth_call is not pinned with requireCanonical")
	}
	if _, ok := self.blockNumberOf(selector.BlockHash); !ok {
		return nil, &subnetReplayRpcFailure{Code: -32000, Message: "unknown block"}
	}
	contract := self.contracts[request.To]
	if contract == nil {
		return "0x", nil
	}
	method, err := contract.contract.MethodById(request.Input)
	if err != nil {
		self.t.Fatalf("eth_call selector is not in the pinned abi")
	}
	args, err := method.Inputs.Unpack(request.Input[4:])
	if err != nil {
		self.t.Fatalf("eth_call arguments do not decode")
	}
	outputs, revert := contract.handle(method.Name, args)
	if revert != "" {
		failure, ok := contract.contract.Errors[revert]
		if !ok {
			self.t.Fatalf("unknown synthetic revert %s", revert)
		}
		data, _ := json.Marshal(hexutil.Encode(failure.ID[:4]))
		return nil, &subnetReplayRpcFailure{Code: 3, Message: "execution reverted", Data: data}
	}
	encoded, err := method.Outputs.Pack(outputs...)
	if err != nil {
		self.t.Fatalf("synthetic %s outputs do not encode: %v", method.Name, err)
	}
	return hexutil.Encode(encoded), nil
}

// The synthetic SignalSource: the relay seam plus the existing Do seam.
type subnetReplayTestSource struct {
	*syntheticSource
	stateLock sync.Mutex
	node      *subnetReplayTestNode
	api       func(*http.Request) (*http.Response, error)
	apiCalls  int
}

func (self *subnetReplayTestSource) HostStdin(_ context.Context, _ HostSettings, command string, stdin string, _ time.Duration) (string, error) {
	if self.node == nil {
		return "", fmt.Errorf("synthetic relay unavailable")
	}
	return self.node.relay(command, stdin)
}

func (self *subnetReplayTestSource) Do(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	self.apiCalls += 1
	api := self.api
	self.stateLock.Unlock()
	if api == nil {
		return nil, fmt.Errorf("synthetic api unavailable")
	}
	return api(request)
}

// A visibly synthetic reviewed sn.yml; the chain, netuid and schema constants
// are the parser's required public mainnet values.
func subnetReplayTestPinBytes(genesis common.Hash, coordinator, vault common.Address, policyHash common.Hash) []byte {
	return []byte(strings.Join([]string{
		"schema: urnetwork-provider-payout-transition-v1",
		"cutoff_utc: 2026-01-01T00:00:00Z",
		"attribution: settled_contract_close_time",
		"legacy_usdc: finish_pre_cutoff_obligations",
		"mainnet:",
		"  profile: mainnet",
		"  chain_id: 964",
		"  genesis_hash: \"" + genesis.Hex() + "\"",
		"  netuid: 25",
		"  activation: reviewed",
		"  deployment_id: synthetic-replay-deployment",
		"  coordinator: \"" + strings.ToLower(coordinator.Hex()) + "\"",
		"  settlement_vault: \"" + strings.ToLower(vault.Hex()) + "\"",
		"  policy_hash: \"" + policyHash.Hex() + "\"",
		"  readiness_sha256: \"" + strings.Repeat("cd", 32) + "\"",
		"",
	}, "\n"))
}

var subnetReplayTestGenesis = common.HexToHash("0x" + strings.Repeat("ab", 32))
var subnetReplayTestPolicyHash = common.HexToHash("0x" + strings.Repeat("cd", 32))
var subnetReplayTestCoordinator = common.HexToAddress("0x00000000000000000000000000000000000c0001")
var subnetReplayTestVault = common.HexToAddress("0x00000000000000000000000000000000000c0002")
var subnetReplayTestEvidence = common.HexToAddress("0x00000000000000000000000000000000000c0003")

// Settings with one synthetic subtensor archive host and the reviewed pin.
func subnetReplayTestSettings(t *testing.T, node *subnetReplayTestNode) (SignalSettings, *subnetReplayTestSource) {
	t.Helper()
	source := &subnetReplayTestSource{syntheticSource: &syntheticSource{}, node: node}
	settings := syntheticSettings(source)
	settings.STConfigStatus = STConfigurationEnabled
	settings.VerificationEnabled = true
	settings.STDeploymentKey = "synthetic-replay-namespace"
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "subtensor-1", OverlayAddress: "198.51.100.40", Roles: []string{"subtensor"},
		Subtensor: &SubtensorHostSettings{
			PublicRPCURL: "https://rpc.synthetic.example.test", ExpectedChain: "Synthetic", ExpectedGenesisHash: subnetReplayTestGenesis.Hex(),
			ExpectedSpecName: "node-subtensor", ExpectedSpecVersion: 1, ExpectedTransactionVersion: 1, ExpectedEVMChainID: "0x3c4",
			Nodes: []SubtensorNodeSettings{{Name: "archive", SyncMode: "full", RPCPort: 9944, GatewayPort: 9944}},
		},
	})
	settings.SubnetDeployment = NewSubnetReplayPin(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash))
	if settings.SubnetDeployment.publicStatus() != subnetReplayPinReviewed {
		t.Fatalf("synthetic pin did not parse as reviewed: %s", settings.SubnetDeployment.publicStatus())
	}
	replay := DefaultSubnetReplaySettings()
	replay.Schema = subnetReplayEnrollmentSchema
	replay.ApiOrigins = []string{"https://first.replay.example.test", "https://second.replay.example.test"}
	settings.SubnetReplay = replay
	if node != nil {
		node.settings = replay
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("synthetic replay settings are invalid: %v", err)
	}
	return settings, source
}

// Finds every alert of one class.
func subnetReplayAlerts(alerts []Alert, class string) []Alert {
	selected := []Alert{}
	for _, alert := range alerts {
		if alert.Class == class {
			selected = append(selected, alert)
		}
	}
	return selected
}

// Requires an alert with exactly this class, target and frame.
func requireSubnetReplayAlert(t *testing.T, alerts []Alert, class, target, frame string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target && alert.Frame == frame {
			return alert
		}
	}
	identities := []string{}
	for _, alert := range alerts {
		identities = append(identities, alert.Class+"|"+alert.Target+"|"+alert.Frame)
	}
	t.Fatalf("no %s alert for %s frame %q; got %s", class, target, frame, strings.Join(identities, ", "))
	return Alert{}
}

// Every rendered alert must omit every synthetic hash, address and key.
func requireSubnetReplayRedacted(t *testing.T, alerts []Alert, forbidden ...string) {
	t.Helper()
	values := append([]string{
		subnetReplayTestGenesis.Hex(), strings.TrimPrefix(subnetReplayTestGenesis.Hex(), "0x"),
		strings.ToLower(subnetReplayTestCoordinator.Hex()), strings.ToLower(subnetReplayTestVault.Hex()),
		subnetReplayTestPolicyHash.Hex(), "synthetic-replay-deployment", "198.51.100.40",
	}, forbidden...)
	for _, alert := range alerts {
		markdown := strings.ToLower(alert.Markdown())
		for index, value := range values {
			if value != "" && strings.Contains(markdown, strings.ToLower(value)) {
				t.Fatalf("alert %s leaked forbidden identifier at fixture index %d", alert.Class, index)
			}
		}
	}
}

// Encodes one synthetic event with the pinned abi.
func subnetReplayTestLog(t *testing.T, contract *abi.ABI, address common.Address, name string, block uint64, index uint, tx common.Hash, values ...any) types.Log {
	t.Helper()
	event, ok := contract.Events[name]
	if !ok {
		t.Fatalf("unknown synthetic event %s", name)
	}
	if len(values) != len(event.Inputs) {
		t.Fatalf("synthetic event %s needs %d values", name, len(event.Inputs))
	}
	topics := []common.Hash{event.ID}
	data := []any{}
	for i, input := range event.Inputs {
		if !input.Indexed {
			data = append(data, values[i])
			continue
		}
		switch value := values[i].(type) {
		case *big.Int:
			topics = append(topics, common.BigToHash(value))
		case [32]byte:
			topics = append(topics, common.Hash(value))
		case common.Address:
			topics = append(topics, common.BytesToHash(value.Bytes()))
		case uint64:
			topics = append(topics, common.BigToHash(new(big.Int).SetUint64(value)))
		default:
			t.Fatalf("unsupported indexed synthetic value %T", value)
		}
	}
	encoded, err := event.Inputs.NonIndexed().Pack(data...)
	if err != nil {
		t.Fatalf("synthetic event %s data does not encode: %v", name, err)
	}
	return types.Log{
		Address: address, Topics: topics, Data: encoded, BlockNumber: block,
		TxHash: tx, Index: index, BlockHash: subnetReplayTestBlockHash(block, 0),
	}
}

// Synthetic transaction hashes.
func subnetReplayTestTx(value uint64) common.Hash {
	var hash common.Hash
	hash[0] = 0x7a
	big.NewInt(0).SetUint64(value).FillBytes(hash[16:])
	return hash
}

// Encodes a synthetic public key as a Bittensor ss58 hotkey for enrollment.
func subnetReplayTestSs58(t *testing.T, pubkey [32]byte) string {
	t.Helper()
	address, err := ss58.Encode(pubkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return address
}
