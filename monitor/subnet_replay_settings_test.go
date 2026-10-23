// Unit controls for the shared §22.6-§22.9 reader layer: the monitor.yml
// enrollment projection, settings generations, the reviewed pin and its
// deployment decision, archive endpoint selection, the fixed relay and stdin
// seam, the json rpc envelope, block-pinned views and reverts, log windows,
// the bounded api reader and the counterexample latch.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestSubnetReplayStrictMonitorYamlProjection(t *testing.T) {
	validator := subnetReplayTestSs58(t, [32]byte{0x31})
	raw := fmt.Sprintf(`subnet_replay:
  schema: urnetwork-monitor-subnet-replay-v1
  api_origins: ["https://one.replay.example.test", "https://two.replay.example.test"]
  replay_from_block: 1000
  evidence_from_block: 1001
  validators: [%q]
  maximum_weight_limit_u16: 50000
  anchor_deadline_blocks: 30
  writer_minimum_actions: 4
  writer_action_liability_wei: "1000000"
`, validator)
	parsed, err := decodeMonitorYaml([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	loaded := subnetReplaySettingsFromNode(parsed.SubnetReplay)
	if loaded == nil || loaded.enrollmentReason() != "" || loaded.ReplayFromBlock != 1000 || loaded.EvidenceFromBlock != 1001 || len(loaded.Validators) != 1 || loaded.MaximumWeightLimitU16 != 50000 || loaded.WriterActionLiabilityWei != "1000000" {
		t.Fatalf("real YAML projection lost enrolled values: %+v", loaded)
	}
	if loaded.budgetReason() != "" || loaded.Cadence != DefaultSubnetReplaySettings().Cadence {
		t.Fatal("enrollment replaced a default budget")
	}
	for _, invalid := range []string{
		"subnet_replay: null\n",
		"subnet_replay: private-value\n",
		"subnet_replay: {schema: wrong}\n",
		"subnet_replay: {extra_private_key: private-value}\n",
		"subnet_replay: &private_anchor {replay_from_block: 1}\n",
		"subnet_replay: {api_origins: [\"https://only.example.test\"]}\n",
		"subnet_replay: {api_origins: [\"http://a.example.test\", \"https://b.example.test\"]}\n",
		"subnet_replay: {validators: [\"not-an-address\"]}\n",
		fmt.Sprintf("subnet_replay: {validators: [%q, %q]}\n", validator, validator),
		"subnet_replay: {writer_action_liability_wei: \"0x10\"}\n",
		"subnet_replay: {replay_from_block: -1}\n",
		"subnet_replay: {cadence: 1s}\n",
	} {
		parsed, err := decodeMonitorYaml([]byte(invalid))
		if err != nil {
			continue
		}
		loaded := subnetReplaySettingsFromNode(parsed.SubnetReplay)
		if loaded == nil || loaded.enrollmentReason() == "" {
			t.Fatalf("present invalid enrollment disappeared or passed: %q", invalid)
		}
		if loaded.budgetReason() != "" {
			t.Fatal("an invalid enrollment lost its bounded defaults")
		}
	}
	parsed, err = decodeMonitorYaml([]byte("address_mode: overlay\n"))
	if err != nil || subnetReplaySettingsFromNode(parsed.SubnetReplay) != nil {
		t.Fatal("absent enrollment distinction changed")
	}
}

func TestSubnetReplaySettingsCloningAndGeneration(t *testing.T) {
	settings, _ := subnetReplayTestSettings(t, nil)
	current := settings.withDefaults()
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if equal, err := settings.SettingsGenerationCheck(context.Background(), settings); err != nil || !equal {
		t.Fatal("equal enrollment and pin changed generation")
	}
	current.SubnetReplay.ApiOrigins[0] = "https://changed.replay.example.test"
	if equal, err := settings.SettingsGenerationCheck(context.Background(), settings); err != nil || equal {
		t.Fatal("changed enrollment retained generation")
	}
	current = settings.withDefaults()
	current.SubnetDeployment = NewSubnetReplayPin(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, common.HexToAddress("0x00000000000000000000000000000000000c00ff"), subnetReplayTestPolicyHash))
	settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if equal, err := settings.SettingsGenerationCheck(context.Background(), settings); err != nil || equal {
		t.Fatal("changed reviewed pin retained generation")
	}
	captured := settings.withDefaults()
	env, err := newProbeEnv(captured)
	if err != nil {
		t.Fatal(err)
	}
	captured.SubnetReplay.ApiOrigins[0] = "https://mutated.replay.example.test"
	if env.cfg.subnetReplay.ApiOrigins[0] == captured.SubnetReplay.ApiOrigins[0] {
		t.Fatal("probe environment borrowed an enrollment slice")
	}
	if effectiveSubnetReplaySettings(nil).budgetReason() != "" {
		t.Fatal("absent settings lost their defaults")
	}
}

func TestSubnetReplayPinAndDeploymentDecision(t *testing.T) {
	reviewed := NewSubnetReplayPin(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash))
	if reviewed.publicStatus() != subnetReplayPinReviewed || reviewed.coordinator != subnetReplayTestCoordinator || reviewed.netuid != 25 || reviewed.settlementV2Declared {
		t.Fatalf("reviewed pin=%+v", reviewed)
	}
	blocked := NewSubnetReplayPin(bytes.Replace(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash), []byte("activation: reviewed"), []byte("activation: blocked"), 1))
	if blocked.publicStatus() != subnetReplayPinBlocked || blocked.coordinator != (common.Address{}) {
		t.Fatalf("blocked pin kept a deployment: %+v", blocked)
	}
	for _, invalid := range [][]byte{
		[]byte("not: [yaml"),
		bytes.Replace(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash), []byte("chain_id: 964"), []byte("chain_id: 945"), 1),
		bytes.Replace(subnetReplayTestPinBytes(subnetReplayTestGenesis, subnetReplayTestCoordinator, subnetReplayTestVault, subnetReplayTestPolicyHash), []byte("deployment_id: synthetic-replay-deployment"), []byte("deployment_id: \"\""), 1),
	} {
		if NewSubnetReplayPin(invalid).publicStatus() != subnetReplayPinInvalid {
			t.Fatal("an invalid sn.yml produced a usable pin")
		}
	}
	decide := func(pin *SubnetReplayPin, status STConfigurationStatus) subnetReplayDeployment {
		return subnetReplayDeploymentOf(&probeEnv{cfg: &monitorConfig{subnetReplayPin: pin, stConfigStatus: status}})
	}
	if decision := decide(reviewed, STConfigurationExplicitDisabled); decision.pin == nil || decision.notDeployed || decision.reason != "" {
		t.Fatal("a reviewed pin was not observed while the producer is disabled")
	}
	if decision := decide(nil, STConfigurationExplicitDisabled); !decision.notDeployed {
		t.Fatal("an explicit disable without a pin was not an expected absence")
	}
	if decision := decide(nil, STConfigurationEnabled); decision.reason != "deployment-pin-missing" {
		t.Fatalf("missing pin decision=%+v", decision)
	}
	if decision := decide(blocked, STConfigurationUnknown); decision.reason != "deployment-pin-blocked" {
		t.Fatalf("blocked pin decision=%+v", decision)
	}
	if decision := decide(&SubnetReplayPin{status: subnetReplayPinInvalid}, STConfigurationExplicitDisabled); decision.reason != "deployment-pin-invalid" {
		t.Fatal("an invalid pin became not deployed")
	}
}

func TestSubnetReplayEndpointSelectsThePinnedArchive(t *testing.T) {
	settings, _ := subnetReplayTestSettings(t, nil)
	other := settings.Hosts[2]
	other.Name = "subtensor-other"
	other.Subtensor = cloneSubtensorHostSettings(other.Subtensor)
	other.Subtensor.ExpectedGenesisHash = common.HexToHash("0x" + strings.Repeat("99", 32)).Hex()
	warp := settings.Hosts[2]
	warp.Name = "subtensor-warp"
	warp.Subtensor = cloneSubtensorHostSettings(warp.Subtensor)
	warp.Subtensor.Nodes = []SubtensorNodeSettings{{Name: "lite", SyncMode: "warp", RPCPort: 9945, GatewayPort: 9945}}
	settings.Hosts = append([]HostSettings{settings.Hosts[0], settings.Hosts[1], other, warp}, settings.Hosts[2])
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	target, port, reason := selectSubnetReplayEndpoint(env, env.cfg.subnetReplayPin)
	if reason != "" || target == nil || target.name != "subtensor-1" || port != 9944 {
		t.Fatalf("endpoint=%v port=%d reason=%q", target, port, reason)
	}
	settings.Hosts = settings.Hosts[:4]
	env, _ = newProbeEnv(settings.withDefaults())
	if _, _, reason := selectSubnetReplayEndpoint(env, env.cfg.subnetReplayPin); reason != "archive-endpoint-unconfigured" {
		t.Fatalf("no matching archive reason=%q", reason)
	}
}

// The relay command is fixed: integers only, the request on stdin, and a
// synthetic source without the stdin seam keeps refusing stdin.
func TestSubnetReplayRelayCommandAndStdinSeam(t *testing.T) {
	settings := DefaultSubnetReplaySettings()
	command := subnetReplayRelayCommand(9944, settings)
	for _, required := range []string{"--noproxy '*'", "--max-time 15", "--max-filesize 8388608", "--data-binary @-", "http://127.0.0.1:9944/"} {
		if !strings.Contains(command, required) {
			t.Fatalf("relay command lacks %q", required)
		}
	}
	runner := &sourceRunner{source: &syntheticSource{}}
	if _, err := runner.sshTimeout(context.Background(), &host{name: "subtensor-1"}, command, "{}", time.Second); err == nil {
		t.Fatal("a source without the stdin seam accepted stdin")
	}
	received := ""
	seam := &subnetReplayTestStdinSource{syntheticSource: &syntheticSource{}, receive: func(command, stdin string) { received = stdin }}
	runner = &sourceRunner{source: seam}
	if _, err := runner.sshTimeout(context.Background(), &host{name: "subtensor-1"}, command, `[{"id":0}]`, time.Second); err != nil || received != `[{"id":0}]` {
		t.Fatalf("stdin seam received %q err=%v", received, err)
	}
}

type subnetReplayTestStdinSource struct {
	*syntheticSource
	receive func(command, stdin string)
}

func (self *subnetReplayTestStdinSource) HostStdin(_ context.Context, _ HostSettings, command, stdin string, _ time.Duration) (string, error) {
	self.receive(command, stdin)
	return "", nil
}

// A relay whose response is fixed by the test.
type subnetReplayTestRelay struct {
	response []byte
	err      error
	requests [][]byte
}

func (self *subnetReplayTestRelay) exchange(_ context.Context, request []byte) ([]byte, error) {
	self.requests = append(self.requests, request)
	return self.response, self.err
}

func TestSubnetReplayRpcRejectsMalformedBatches(t *testing.T) {
	settings := DefaultSubnetReplaySettings()
	for _, c := range []struct {
		response string
		reason   string
	}{
		{response: `{"jsonrpc":"2.0","id":0,"result":"0x1"}`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"2.0","id":0,"result":"0x1"}] []`, reason: "rpc-response-malformed"},
		{response: `[]`, reason: "rpc-response-incomplete"},
		{response: `[{"jsonrpc":"2.0","id":0,"result":"0x1"},{"jsonrpc":"2.0","id":0,"result":"0x1"}]`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"2.0","id":5,"result":"0x1"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"2.0","result":"0x1"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"2.0","id":0,"result":"0x1","error":{"code":1,"message":"x"}},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"2.0","id":0},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"1.0","id":0,"result":"0x1"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, reason: "rpc-response-malformed"},
		{response: `[{"jsonrpc":"2.0","id":0,"result":7},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, reason: "rpc-result-malformed"},
	} {
		relay := &subnetReplayTestRelay{response: []byte(c.response)}
		rpc := newSubnetReplayRpc(relay, settings)
		var first, second *hexutil.Big
		err := rpc.call(context.Background(), &subnetReplayRpcCall{method: "eth_chainId", result: &first}, &subnetReplayRpcCall{method: "eth_chainId", result: &second})
		if subnetReplayUnknownReason(err) != c.reason {
			t.Fatalf("response %s gave %v, want %s", c.response, err, c.reason)
		}
	}
	oversized := *settings
	oversized.RpcMaximumResponseBytes = 8
	rpc := newSubnetReplayRpc(&subnetReplayTestRelay{response: []byte(`[{"jsonrpc":"2.0","id":0,"result":"0x1"}]`)}, &oversized)
	if err := rpc.call(context.Background(), &subnetReplayRpcCall{method: "eth_chainId"}); subnetReplayUnknownReason(err) != "rpc-response-oversized" {
		t.Fatalf("oversized response=%v", err)
	}
	private := errors.New("private-relay-cause 198.51.100.40")
	rpc = newSubnetReplayRpc(&subnetReplayTestRelay{err: private}, settings)
	err := rpc.call(context.Background(), &subnetReplayRpcCall{method: "eth_chainId"})
	if subnetReplayUnknownReason(err) != "rpc-relay-unavailable" || !errors.Is(err, private) || strings.Contains(err.Error(), "private") {
		t.Fatalf("relay failure lost or leaked its cause: %v", err)
	}
	budget := *settings
	budget.RpcMaximumExchanges = 1
	relay := &subnetReplayTestRelay{response: []byte(`[{"jsonrpc":"2.0","id":0,"result":"0x1"}]`)}
	rpc = newSubnetReplayRpc(relay, &budget)
	if err := rpc.call(context.Background(), &subnetReplayRpcCall{method: "eth_chainId"}); err != nil {
		t.Fatal(err)
	}
	if err := rpc.call(context.Background(), &subnetReplayRpcCall{method: "eth_chainId"}); subnetReplayUnknownReason(err) != "rpc-budget-exhausted" || len(relay.requests) != 1 {
		t.Fatal("the exchange budget did not stop a second exchange")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rpc = newSubnetReplayRpc(&subnetReplayTestRelay{}, settings)
	if err := rpc.call(ctx, &subnetReplayRpcCall{method: "eth_chainId"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call=%v", err)
	}
	var request []map[string]any
	if json.Unmarshal(relay.requests[0], &request) != nil || len(request) != 1 || request[0]["method"] != "eth_chainId" {
		t.Fatal("the relayed request is not a json rpc batch")
	}
}

func TestSubnetReplayViewsRefuseNoncanonicalOutputAndUnexpectedReverts(t *testing.T) {
	contracts, err := subnetSettlementAbis()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := subnetReplayEvmCheckpoint{number: 10, hash: common.HexToHash("0x01")}
	encoded, _ := contracts.vault.Methods["totalCaptured"].Outputs.Pack(big.NewInt(5))
	selector := contracts.coordinator.Errors["UnknownOperator"].ID
	revert, _ := json.Marshal(hexutil.Encode(selector[:4]))
	respond := func(member string) *subnetReplayRpc {
		return newSubnetReplayRpc(&subnetReplayTestRelay{response: []byte(`[{"jsonrpc":"2.0","id":0,` + member + `}]`)}, DefaultSubnetReplaySettings())
	}
	for _, c := range []struct {
		member string
		reason string
	}{
		{member: `"result":"` + hexutil.Encode(append(append([]byte{}, encoded...), 0x00)) + `"`, reason: "view-malformed"},
		{member: `"result":"` + hexutil.Encode(append(append([]byte{}, encoded...), make([]byte, 32)...)) + `"`, reason: "view-noncanonical"},
		{member: `"result":"0x"`, reason: "view-empty"},
		{member: `"error":{"code":3,"message":"execution reverted","data":` + string(revert) + `}`, reason: "view-failed"},
	} {
		view := newSubnetReplayView(contracts.vault, subnetReplayTestVault, "totalCaptured")
		if err := readSubnetReplayViews(context.Background(), respond(c.member), checkpoint, view); subnetReplayUnknownReason(err) != c.reason {
			t.Fatalf("%s gave %v", c.reason, err)
		}
	}
	view := newSubnetReplayView(contracts.coordinator, subnetReplayTestCoordinator, "operatorAt", big.NewInt(1), big.NewInt(0)).revertible()
	if err := readSubnetReplayViews(context.Background(), respond(`"error":{"code":3,"message":"execution reverted","data":`+string(revert)+`}`), checkpoint, view); err != nil || !view.revertedWith(contracts.coordinator, "UnknownOperator") || view.revertedWith(contracts.coordinator, "InvalidPolicy") {
		t.Fatalf("an allowed typed revert was not retained: %v", err)
	}
	view = newSubnetReplayView(contracts.vault, subnetReplayTestVault, "totalCaptured")
	if err := readSubnetReplayViews(context.Background(), respond(`"result":"`+hexutil.Encode(encoded)+`"`), checkpoint, view); err != nil {
		t.Fatal(err)
	}
	if _, err := subnetReplayValue[[32]byte](view, 0); subnetReplayUnknownReason(err) != "view-shape-invalid" {
		t.Fatalf("a wrong typed conversion did not become a gap: %v", err)
	}
	if value, err := subnetReplayValue[*big.Int](view, 0); err != nil || value.Int64() != 5 {
		t.Fatal("a canonical view did not decode")
	}
}

func TestSubnetReplayLogWindowsRefuseInvalidEntries(t *testing.T) {
	address := subnetReplayTestVault
	valid := types.Log{Address: address, Topics: []common.Hash{{0x01}}, Data: []byte{}, BlockNumber: 11, TxHash: common.Hash{0x02}, BlockHash: common.Hash{0x03}, Index: 1}
	for _, c := range []struct {
		logs   []types.Log
		reason string
	}{
		{logs: []types.Log{func() types.Log { log := valid; log.Address = common.Address{0x09}; return log }()}, reason: "log-entry-invalid"},
		{logs: []types.Log{func() types.Log { log := valid; log.Removed = true; return log }()}, reason: "log-entry-invalid"},
		{logs: []types.Log{func() types.Log { log := valid; log.BlockNumber = 30; return log }()}, reason: "log-entry-invalid"},
		{logs: []types.Log{valid, valid}, reason: "log-order-invalid"},
		{logs: []types.Log{valid, func() types.Log { log := valid; log.Index = 2; return log }(), func() types.Log { log := valid; log.Index = 3; return log }()}, reason: "log-window-truncated"},
	} {
		encoded, _ := json.Marshal(c.logs)
		rpc := newSubnetReplayRpc(&subnetReplayTestRelay{response: []byte(`[{"jsonrpc":"2.0","id":0,"result":` + string(encoded) + `}]`)}, DefaultSubnetReplaySettings())
		if err := readSubnetReplayLogs(context.Background(), rpc, address, 3, &subnetReplayLogWindow{from: 10, to: 20}); subnetReplayUnknownReason(err) != c.reason {
			t.Fatalf("logs gave %v, want %s", err, c.reason)
		}
	}
}

func TestSubnetReplayApiReaderBounds(t *testing.T) {
	settings := *DefaultSubnetReplaySettings()
	settings.ApiMaximumBodyBytes = 8
	respond := func(response *http.Response) grafanaHTTPClient {
		return subnetReplayTestHttpClient(func(request *http.Request) (*http.Response, error) {
			response.Request = request
			return response, nil
		})
	}
	body := func(value string) io.ReadCloser { return io.NopCloser(strings.NewReader(value)) }
	for _, c := range []struct {
		response *http.Response
		reason   string
	}{
		{response: &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: -1, Body: body("0123456789")}, reason: "api-body-bound-invalid"},
		{response: &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: 3, Body: body("01")}, reason: "api-body-bound-invalid"},
		{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, ContentLength: -1, Body: body("01")}, reason: "api-response-contract-invalid"},
		{response: &http.Response{StatusCode: 200, Header: http.Header{"Etag": {"a", "b"}}, ContentLength: -1, Body: body("01")}, reason: "api-response-contract-invalid"},
	} {
		_, err := readSubnetReplayApi(context.Background(), respond(c.response), "https://one.replay.example.test", "/sn/artifact", nil, &settings)
		if subnetReplayUnknownReason(err) != c.reason {
			t.Fatalf("api reader gave %v, want %s", err, c.reason)
		}
	}
	redirected := subnetReplayTestHttpClient(func(request *http.Request) (*http.Response, error) {
		moved, _ := http.NewRequest(http.MethodGet, "https://elsewhere.example.test/sn/artifact", nil)
		return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: 2, Body: body("ok"), Request: moved}, nil
	})
	if _, err := readSubnetReplayApi(context.Background(), redirected, "https://one.replay.example.test", "/sn/artifact", nil, &settings); subnetReplayUnknownReason(err) != "api-response-contract-invalid" {
		t.Fatalf("a redirected response was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readSubnetReplayApi(ctx, redirected, "https://one.replay.example.test", "/sn/artifact", nil, &settings); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled read=%v", err)
	}
}

type subnetReplayTestHttpClient func(*http.Request) (*http.Response, error)

func (self subnetReplayTestHttpClient) Do(request *http.Request) (*http.Response, error) {
	return self(request)
}

func TestSubnetReplayLatchRetainsOnlyProvedCounterexamples(t *testing.T) {
	latch := newSubnetReplayLatch()
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	proved := finding{probeId: "subnet/settlement", tier: tierPage, class: "subnet-vault-conservation", target: "subnet-settlement/vault", frame: "escrow-backing", observed: "state=violated"}
	unknown := finding{probeId: "subnet/settlement", tier: tierWarn, class: "subnet-vault-conservation", target: "subnet-settlement/vault", frame: "other", unobservable: true}
	progress := finding{probeId: "subnet/settlement", tier: tierWarn, class: "subnet-epoch-progress", target: "subnet-settlement/no-1", frame: "epoch-1/root"}
	latch.Retain(first, []finding{proved, unknown, progress})
	latch.Retain(first.Add(time.Minute), []finding{proved})
	output := latch.Retain(first.Add(2*time.Minute), nil)
	if len(output) != 1 || output[0].frame != "escrow-backing" || !strings.Contains(output[0].observed, "first_proved=2026-10-01T00:00:00Z last_proved=2026-10-01T00:01:00Z current_read=absent") || output[0].tier != tierPage {
		t.Fatalf("latch output=%+v", output)
	}
}
