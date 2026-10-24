// A deterministic synthetic subnet deployment for the §22.1--§22.5 readers.
// It answers the real Json-Rpc and Http shapes through the injected Do seam:
// native methods from storage keyed with the real runtime 482 metadata, Evm
// views dispatched through the real stabi ABIs, logs, transactions and Api
// routes. Nothing here reaches a socket, host or production value.
package monitor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The decoded real runtime 482 metadata is shared read-only by every test.
var subnetWorldMetadataOnce sync.Once
var subnetWorldMetadata *types.Metadata
var subnetWorldMetadataHex string
var subnetWorldMetadataHash string
var subnetWorldMetadataErr error

// The sibling sn checkout is the module replacement this package compiles
// against, so its reviewed metadata fixture is always present.
func subnetWorldRuntimeMetadata(t testing.TB) (*types.Metadata, string, string) {
	t.Helper()
	subnetWorldMetadataOnce.Do(func() {
		raw, err := os.ReadFile("../../sn/crv4/testdata/runtime482-metadata.scale.gz.base64")
		if err != nil {
			subnetWorldMetadataErr = err
			return
		}
		compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			subnetWorldMetadataErr = err
			return
		}
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			subnetWorldMetadataErr = err
			return
		}
		scale, err := io.ReadAll(io.LimitReader(reader, 8<<20))
		if err != nil {
			subnetWorldMetadataErr = err
			return
		}
		subnetWorldMetadataHex = codec.HexEncodeToString(scale)
		subnetWorldMetadata, subnetWorldMetadataHash, subnetWorldMetadataErr = crv4.DecodeRuntimeMetadata(subnetWorldMetadataHex)
		subnetWorldMetadataHash = strings.ToLower(subnetWorldMetadataHash)
	})
	if subnetWorldMetadataErr != nil {
		t.Fatalf("runtime metadata fixture: %v", subnetWorldMetadataErr)
	}
	return subnetWorldMetadata, subnetWorldMetadataHex, subnetWorldMetadataHash
}

// A synthetic Evm view handler: decoded inputs to output values.
type subnetWorldView func(inputs []any) ([]any, error)

type subnetWorldContract struct {
	abi   *abi.ABI
	code  []byte
	slots map[common.Hash]common.Hash
	views map[string]subnetWorldView
}

// A synthetic included transaction.
type subnetWorldTransaction struct {
	to        common.Address
	input     []byte
	blockHash common.Hash
}

// The world. Every field is test-owned; the handler reads under one lock.
type subnetWorld struct {
	t         *testing.T
	stateLock sync.Mutex

	deployment SubnetDeploymentIdentity
	settings   *SubnetCorrectnessSettings
	now        time.Time

	metadata     *types.Metadata
	metadataHex  string
	metadataHash string
	codeHash     string
	version      crv4.RuntimeVersionIdentity
	genesis      types.Hash

	finalized    uint64
	evmFinalized uint64
	nativeHashes map[uint64]types.Hash
	evmHashes    map[uint64]common.Hash
	storage      map[string][]byte

	policyDocument []byte
	policy         *protocol.Policy
	policyVersions []stabi.STCoordinatorPolicySnapshot
	currentEpoch   uint64

	coordinator common.Address
	vault       common.Address
	sink        common.Address
	contracts   map[common.Address]*subnetWorldContract
	logs        []ethtypes.Log
	txs         map[common.Hash]subnetWorldTransaction
	routes      map[string]func(*http.Request) *http.Response

	activeBindings    map[[16]byte]stabi.STCoordinatorBindingRecord
	artifactProviders []payoutartifact.ProviderInput

	verifyKeys []*controller.VerifyKey
	proofs     *controller.GetVerifyProofsResult
	stats      *controller.GetVerifyStatsResult

	// Per-endpoint fault injection: return a result, an error, or not handled.
	override func(endpoint, method string, params []json.RawMessage) (any, error, bool)
	calls    map[string]int
	requests []string
}

// Endpoint URLs are synthetic and never rendered by the readers.
const (
	subnetWorldNative1 = "https://native-one.private.example.test/rpc"
	subnetWorldNative2 = "https://native-two.private.example.test/rpc"
	subnetWorldEvm1    = "https://evm-one.private.example.test/rpc"
	subnetWorldEvm2    = "https://evm-two.private.example.test/rpc"
	subnetWorldApi1    = "https://api-one.private.example.test"
	subnetWorldApi2    = "https://api-two.private.example.test"
)

func subnetWorldHash(seed string) common.Hash {
	return crypto.Keccak256Hash([]byte("synthetic-subnet-world/" + seed))
}

func subnetWorldAddress(seed string) common.Address {
	return common.BytesToAddress(subnetWorldHash(seed).Bytes()[12:])
}

func subnetWorldKey(seed string) [32]byte {
	return [32]byte(subnetWorldHash("key/" + seed))
}

func subnetWorldHex(value [32]byte) string {
	return "0x" + hex.EncodeToString(value[:])
}

func subnetWorldAbi(t testing.TB, parse func() (*abi.ABI, error)) *abi.ABI {
	t.Helper()
	metadata, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

// A healthy deployment at finalized height 1000, epoch 3, with two operators
// and two validators. Tests mutate it before running a signal.
func newSubnetWorld(t *testing.T) *subnetWorld {
	t.Helper()
	metadata, metadataHex, metadataHash := subnetWorldRuntimeMetadata(t)
	world := &subnetWorld{
		t: t, now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		metadata: metadata, metadataHex: metadataHex, metadataHash: metadataHash,
		codeHash:     subnetWorldHex(subnetWorldKey("runtime-code")),
		version:      crv4.RuntimeVersionIdentity{SpecName: "node-subtensor", SpecVersion: 482, TransactionVersion: 1, StateVersion: 1},
		genesis:      types.Hash(subnetWorldBlockHash("native", 0)),
		currentEpoch: 3,
		finalized:    120000,
		evmFinalized: 120000,
		nativeHashes: map[uint64]types.Hash{},
		evmHashes:    map[uint64]common.Hash{},
		storage:      map[string][]byte{},
		contracts:    map[common.Address]*subnetWorldContract{},
		txs:          map[common.Hash]subnetWorldTransaction{},
		routes:       map[string]func(*http.Request) *http.Response{},
		calls:        map[string]int{},
	}
	world.coordinator = subnetWorldAddress("coordinator")
	world.vault = subnetWorldAddress("vault")
	world.sink = subnetWorldAddress("sink")
	world.policyDocument, world.policy = subnetWorldPolicyDocument(t)
	policyHash, err := world.policy.Hash()
	if err != nil {
		t.Fatal(err)
	}
	world.deployment = SubnetDeploymentIdentity{
		Status: subnetDeploymentAvailable, ChainId: mainSTProtocolChainID, GenesisHash: subnetHashHex(world.genesis), Netuid: 521,
		DeploymentId: "synthetic-private-deployment", Coordinator: strings.ToLower(world.coordinator.Hex()),
		SettlementVault: strings.ToLower(world.vault.Hex()), PolicyHash: subnetHex32(policyHash),
	}
	world.policyVersions = world.reviewedPolicyVersions()
	world.installContracts()
	world.installNativeState()
	world.installIdentityState()
	world.installDepositState()
	world.settings = world.enrollment()
	world.installValidationState()
	world.installHeadState()
	return world
}

// One bound head fleet from an earlier epoch, active now, with the rows and
// incentive a healthy subnet applies to it.
func (self *subnetWorld) installHeadState() {
	self.settings.Limits.LogChunkBlocks = 10000
	netuid := subnetNetuidArg(self.deployment.Netuid)
	hotkey := subnetWorldKey("fleet-2-hotkey")
	self.setStorage("SubtensorModule", "Keys", types.AccountID(hotkey), netuid, subnetNetuidArg(30))
	self.setStorage("SubtensorModule", "Uids", types.NewU16(30), netuid, hotkey[:])
	self.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("fleet-2-coldkey")), hotkey[:])
	self.setStorage("SubtensorModule", "SubnetOwnerHotkey", types.AccountID(subnetWorldKey("subnet-owner-hotkey")), netuid)
	self.bindHead([16]byte(subnetWorldHash("fleet-2-client").Bytes()[:16]), hotkey, 30, 60000, 3, 8)
	for uid := uint16(1); uid <= 2; uid++ {
		self.setWeights(uid, []subnetWeightPair{{Uid: 11, Weight: 20000}, {Uid: 12, Weight: 20000}, {Uid: 30, Weight: 25535}}, self.finalized-100)
	}
	incentives := make([]types.U16, 32)
	incentives[11], incentives[12], incentives[30] = 20000, 20000, 25535
	self.setStorage("SubtensorModule", "Incentive", incentives, netuid)
	self.activeBindings = map[[16]byte]stabi.STCoordinatorBindingRecord{}
	self.contracts[self.coordinator].views["bindingAt"] = func(inputs []any) ([]any, error) {
		record, active := self.activeBindings[inputs[0].([16]byte)]
		return []any{active, record}, nil
	}
}

// Emits one FleetBound for a head fleet member; its calldata is not needed
// outside the identity reader's frontier.
func (self *subnetWorld) bindHead(client [16]byte, hotkey [32]byte, uid uint16, block, validFrom, validTo uint64) {
	coordinatorAbi := subnetWorldAbi(self.t, stabi.STCoordinatorMetaData.ParseABI)
	self.emit(self.coordinator, coordinatorAbi, "FleetBound", block, uint(len(self.logs)), map[string]any{
		"clientId": client, "fleetId": subnetWorldKey(fmt.Sprintf("fleet/%x", hotkey[:4])), "hotkey": hotkey, "uid": uid,
		"generation": uint64(1), "validFromEpoch": validFrom, "validToEpoch": validTo,
	})
}

// Commits a source-epoch artifact with explicit provider rows.
func (self *subnetWorld) commitArtifactWith(noId, epoch uint64, providers []payoutartifact.ProviderInput) *payoutartifact.Artifact {
	self.t.Helper()
	usage := []uint64{}
	for _, provider := range providers {
		usage = append(usage, provider.UsageBytes)
	}
	self.artifactProviders = providers
	artifact, _ := self.commitArtifact(noId, epoch, usage)
	self.artifactProviders = nil
	return artifact
}

// A validator's measurement key; a public test constant.
func subnetWorldValidatorKey(index int) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(subnetWorldHash(fmt.Sprintf("validator-%d-vpk-seed", index)).Bytes())
}

// The verify server's signing key for key id 1; a public test constant.
func subnetWorldServerKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(subnetWorldHash("verify-server-key-1").Bytes())
}

// One signed public proof row.
func subnetWorldProofRow(validator ed25519.PrivateKey, trail uint64, depth int, complete bool, created time.Time) *controller.PublicVerifyProofRow {
	var trailId connect.Id
	binary.BigEndian.PutUint64(trailId[8:], trail)
	trailId[0] = 0x7a
	nonce := subnetWorldHash(fmt.Sprintf("nonce/%d", trail)).Bytes()
	vpk := []byte(validator.Public().(ed25519.PublicKey))
	row := &controller.PublicVerifyProofRow{TrailId: server.Id(trailId), Vpk: vpk, ServerKeyId: 1, ServerNonce: nonce, Depth: depth, CreateTime: created}
	hops := []connect.VerifyProofHop{}
	ids := []connect.Id{}
	for i := 0; i < depth; i++ {
		var client connect.Id
		binary.BigEndian.PutUint64(client[:8], trail)
		client[15] = byte(i + 1)
		ids = append(ids, client)
		hops = append(hops, connect.VerifyProofHop{ClientId: client, TimeMs: uint64(created.UnixMilli()) + uint64(i*100), EgressIpHash: [32]byte(subnetWorldHash(fmt.Sprintf("egress/%d/%d", trail, i)))})
	}
	if !complete {
		row.Status = model.VerifyTrailRowStatusExpired
		row.Depth = 2
		expired, _ := json.Marshal([]map[string]any{{"client_id": ids[0], "time_ms": hops[0].TimeMs}, {"client_id": ids[1], "time_ms": hops[1].TimeMs}})
		row.Hops = expired
		return row
	}
	row.Status = model.VerifyTrailRowStatusComplete
	encoded, _ := json.Marshal(hops)
	row.Hops = encoded
	final, _ := connect.BuildVerifyFinalMessage(1, trailId, nonce, vpk, byte(depth), hops)
	row.FinalSig = ed25519.Sign(subnetWorldServerKey(), final)
	extend, _ := connect.BuildVerifyExtendMessage(trailId, nonce, vpk, byte(depth), ids)
	row.VerifierSig = ed25519.Sign(validator, extend)
	completed := created.Add(time.Duration(depth) * 100 * time.Millisecond)
	row.CompleteTime = &completed
	return row
}

// One valid signed expired transition at depth one.
func (self *subnetWorld) transition(validator ed25519.PrivateKey, trail uint64) *model.VerifyOriginalTransition {
	var trailId connect.Id
	binary.BigEndian.PutUint64(trailId[8:], trail)
	trailId[0] = 0x7b
	nonce := subnetWorldHash(fmt.Sprintf("transition-nonce/%d", trail)).Bytes()
	vpk := []byte(validator.Public().(ed25519.PublicKey))
	seed := connect.Id{0x51, byte(trail)}
	next := connect.Id{0x52, byte(trail)}
	at := uint64(self.now.Add(-10 * time.Minute).UnixMilli())
	request, _ := connect.BuildVerifyExtendMessage(trailId, nonce, vpk, 8, []connect.Id{seed, next})
	genesis, _ := subnetParseHash(self.deployment.GenesisHash)
	policy, _ := subnetParseHash(self.deployment.PolicyHash)
	body := protocol.ProviderAttemptReceiptBody{
		Schema: protocol.ProviderAttemptReceiptDomain,
		Scope: &protocol.ProviderAttemptReceiptScope{Profile: "mainnet", GenesisHash: [32]byte(genesis), DeploymentId: self.deployment.DeploymentId,
			DeploymentKey: self.deployment.deploymentKey(), PolicyHash: [32]byte(policy), Netuid: uint64(self.deployment.Netuid), NoId: 1},
		PreviousDepth: 1, RecoveryMs: at + 1000,
		Trail: &protocol.ProviderAttemptReceiptTrail{TrailId: trailId, ClientId: connect.Id{0x53, byte(trail)}, Vpk: vpk, ServerNonce: nonce, M: 8, ServerKeyId: 1,
			Status: "expired", CreateMs: at - 1000, ActivityMs: at,
			Hops: []*protocol.ProviderAttemptReceiptHop{{ClientId: seed, ConfirmedMs: at - 900, Seed: true}, {ClientId: next, AssignedMs: at - 800, ConfirmedMs: at - 700, AssignN: 1}}},
		RequestMessage: request, RequestSignature: ed25519.Sign(validator, request),
	}
	encoded, _ := json.Marshal(body)
	return &model.VerifyOriginalTransition{Body: encoded, Signature: ed25519.Sign(subnetWorldServerKey(), append([]byte(protocol.ProviderAttemptReceiptDomain), encoded...))}
}

// The public verify routes of a healthy operator: two validators' trails,
// signed transitions and fresh exact statistics.
func (self *subnetWorld) installValidationState() {
	for i := range self.settings.Validators {
		self.settings.Validators[i].Vpk = "0x" + hex.EncodeToString(subnetWorldValidatorKey(i+1).Public().(ed25519.PublicKey))
	}
	self.verifyKeys = []*controller.VerifyKey{{ServerKeyId: 1, PublicKey: subnetWorldServerKey().Public().(ed25519.PublicKey)}}
	self.proofs = &controller.GetVerifyProofsResult{Schema: "urnetwork-verify-proof-index-v1", Profile: "mainnet", PolicyHash: self.deployment.PolicyHash, OriginalCoverage: "operator_index_only"}
	for validator := 1; validator <= 2; validator++ {
		key := subnetWorldValidatorKey(validator)
		for trail := 0; trail < 12; trail++ {
			id := uint64(validator*1000 + trail)
			self.proofs.Rows = append(self.proofs.Rows, subnetWorldProofRow(key, id, 8, trail%6 != 5, self.now.Add(-time.Duration(30-trail)*time.Minute)))
		}
		self.proofs.OriginalTransitions = append(self.proofs.OriginalTransitions, self.transition(key, uint64(validator)))
	}
	self.stats = &controller.GetVerifyStatsResult{Schema: "urnetwork-verify-stats-index-v1", Profile: "mainnet", PolicyHash: self.deployment.PolicyHash, EgressHashKeyId: self.policy.Verify.EgressHashKeyID}
	for provider := 0; provider < 4; provider++ {
		id := server.Id(connect.Id{0x61, byte(provider + 1)})
		self.stats.Rows = append(self.stats.Rows, &controller.PublicVerifyStatsRow{PeriodStart: self.now.Add(-15 * time.Minute), PeriodEnd: self.now.Add(-time.Minute), ClientId: id, Assignments: 10, Confirmations: 9, ReliabilityPPM: protocol.ReliabilityPPM(9, 10, self.policy.Verify.ReliabilityAMin)})
	}
	self.routes["/verify/keys"] = func(request *http.Request) *http.Response {
		encoded, _ := json.Marshal(controller.GetVerifyKeysResult{Keys: self.verifyKeys})
		return subnetWorldJsonResponse(request, encoded)
	}
	self.routes["/verify/proofs"] = func(request *http.Request) *http.Response {
		self.proofs.PolicyHash = self.deployment.PolicyHash
		encoded, _ := json.Marshal(self.proofs)
		return subnetWorldJsonResponse(request, encoded)
	}
	self.routes["/verify/stats"] = func(request *http.Request) *http.Response {
		self.stats.PolicyHash = self.deployment.PolicyHash
		encoded, _ := json.Marshal(self.stats)
		return subnetWorldJsonResponse(request, encoded)
	}
}

// The reserve read path, zero reserve takes and the zero-price deposit
// counters of the reviewed launch policy.
func (self *subnetWorld) installDepositState() {
	netuid := subnetNetuidArg(self.deployment.Netuid)
	staking := self.contract(subnetStakingPrecompile, subnetWorldAbi(self.t, func() (*abi.ABI, error) {
		parsed, err := abi.JSON(strings.NewReader(subnetStakingAbiJson))
		return &parsed, err
	}))
	sink := self.contracts[self.sink]
	staking.views["getStake"] = func(inputs []any) ([]any, error) {
		if inputs[0].([32]byte) != subnetWorldKey("reserve") || inputs[1].([32]byte) != subnetMirroredColdkey(self.sink) || inputs[2].(*big.Int).Uint64() != uint64(self.deployment.Netuid) {
			return []any{new(big.Int)}, nil
		}
		return sink.views["liveStake"](nil)
	}
	reserve := subnetWorldKey("reserve")
	self.setStorage("SubtensorModule", "Delegates", types.NewU16(0), reserve[:])
	self.setStorage("SubtensorModule", "ChildkeyTake", types.NewU16(0), reserve[:], netuid)
	coordinator := self.contracts[self.coordinator]
	zero := subnetWorldConstant(new(big.Int))
	coordinator.views["epochDeposits"] = zero
	coordinator.views["epochConvictionAdded"] = zero
	coordinator.views["nextDepositNonce"] = zero
	coordinator.views["rootCommitments"] = subnetWorldConstant([32]byte{}, [32]byte{}, common.Address{}, uint64(0))
	self.setStorage("SubtensorModule", "RevealPeriodEpochs", types.NewU64(1), netuid)
}

// Moves the agreed checkpoint and installs its Evm mapping.
func (self *subnetWorld) moveFinalized(number uint64) {
	self.finalized, self.evmFinalized = number, number
	for height := number - 10; height <= number+10; height++ {
		self.setStorage("Ethereum", "BlockHash", types.Hash(self.evmHash(height)), subnetWorldU256(height))
	}
}

// The artifact signing key of an operator; a public test constant.
func subnetWorldArtifactKey(t testing.TB, noId uint64) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.ToECDSA(subnetWorldHash(fmt.Sprintf("operator-%d-artifact-key", noId)).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A priced variant of the reviewed policy: one rao per byte in every tier.
func (self *subnetWorld) usePricedPolicy() {
	self.t.Helper()
	document := strings.ReplaceAll(string(self.policyDocument), "rate_numerator_rao_per_gib: 0", "rate_numerator_rao_per_gib: 1073741824")
	policy, err := protocol.ParsePolicy([]byte(document))
	if err != nil {
		self.t.Fatal(err)
	}
	if policy.IsZeroPrice() {
		self.t.Fatal("priced fixture is still zero-price")
	}
	self.policyDocument, self.policy = []byte(document), policy
	digest, _ := policy.Hash()
	self.deployment.PolicyHash = subnetHex32(digest)
	self.policyVersions = self.reviewedPolicyVersions()
	self.settings.PolicyDocument = base64.StdEncoding.EncodeToString(self.policyDocument)
	for i := range self.settings.Operators {
		self.settings.Operators[i].ArtifactSigner = strings.ToLower(crypto.PubkeyToAddress(subnetWorldArtifactKey(self.t, self.settings.Operators[i].NoId).PublicKey).Hex())
	}
}

// Publishes one operator's signed source-epoch artifact at both origins and
// commits it on chain; returns the artifact and its required deposit.
func (self *subnetWorld) commitArtifact(noId, epoch uint64, usage []uint64) (*payoutartifact.Artifact, *big.Int) {
	self.t.Helper()
	start, end := self.epochStartBlock(epoch), self.epochStartBlock(epoch+1)
	providers := append([]payoutartifact.ProviderInput(nil), self.artifactProviders...)
	if len(providers) == 0 {
		for i, bytes := range usage {
			providers = append(providers, payoutartifact.ProviderInput{ClientID: [16]byte{byte(noId), byte(i + 1)}, NetworkID: [16]byte{byte(noId), byte(i + 100)}, Coldkey: [32]byte{byte(noId), byte(i + 1)}, UsageBytes: bytes, Assignments: 10, Confirmations: 10, Eligible: true})
		}
	}
	artifact, err := payoutartifact.Build(payoutartifact.BuildInput{
		DeploymentID: self.deployment.DeploymentId, ChainID: self.deployment.ChainId, Netuid: self.deployment.Netuid,
		GenesisHash: self.deployment.GenesisHash, PolicyHash: self.deployment.PolicyHash,
		Coordinator: self.coordinator, SettlementVault: self.vault, Epoch: epoch, NoID: noId,
		Start:                payoutartifact.Boundary{Number: start, Hash: strings.ToLower(self.evmHash(start).Hex())},
		End:                  payoutartifact.Boundary{Number: end, Hash: strings.ToLower(self.evmHash(end).Hex())},
		OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32),
		Providers: providers, ReliabilityAMin: 8, CreatedAt: self.now.Add(-24 * time.Hour),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	if err := payoutartifact.Sign(artifact, subnetWorldArtifactKey(self.t, noId)); err != nil {
		self.t.Fatal(err)
	}
	raw, err := payoutartifact.Bytes(artifact)
	if err != nil {
		self.t.Fatal(err)
	}
	previous := self.routes["/sn/artifact"]
	self.routes["/sn/artifact"] = func(request *http.Request) *http.Response {
		if request.URL.Query().Get("hash") == artifact.ContentHash {
			response := subnetWorldJsonResponse(request, raw)
			response.Header.Set("ETag", `"`+artifact.ContentHash+`"`)
			return response
		}
		if previous != nil {
			return previous(request)
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(artifact.ContentHash, "sha256:"))
	if err != nil {
		self.t.Fatal(err)
	}
	commitments := self.contracts[self.coordinator].views["rootCommitments"]
	self.contracts[self.coordinator].views["rootCommitments"] = func(inputs []any) ([]any, error) {
		if inputs[0].(*big.Int).Uint64() == epoch && inputs[1].(*big.Int).Uint64() == noId {
			return []any{artifact.PayoutRoot, [32]byte(digest), subnetWorldAddress(fmt.Sprintf("operator-%d-root-signer", noId)), end + 100}, nil
		}
		return commitments(inputs)
	}
	total := uint64(0)
	for _, bytes := range usage {
		total += bytes
	}
	required, _, err := protocol.RequiredDepositRao(total, 0, big.NewInt(500*int64(noId)), self.policy.Deposit)
	if err != nil {
		self.t.Fatal(err)
	}
	return artifact, required
}

// Sets an operator's current-epoch deposit and keeps every reserve view
// consistent with it.
func (self *subnetWorld) setDeposit(noId uint64, amount *big.Int) {
	coordinator := self.contracts[self.coordinator]
	sink := self.contracts[self.sink]
	deposits := map[uint64]*big.Int{}
	if previous := coordinator.views["epochDeposits"]; previous != nil {
		for id := uint64(1); id <= 2; id++ {
			values, _ := previous([]any{new(big.Int).SetUint64(self.currentEpoch), new(big.Int).SetUint64(id)})
			deposits[id] = values[0].(*big.Int)
		}
	}
	deposits[noId] = amount
	coordinator.views["epochDeposits"] = func(inputs []any) ([]any, error) {
		if inputs[0].(*big.Int).Uint64() != self.currentEpoch {
			return []any{new(big.Int)}, nil
		}
		return []any{new(big.Int).Set(deposits[inputs[1].(*big.Int).Uint64()])}, nil
	}
	principal := func(id uint64) *big.Int { return new(big.Int).Add(big.NewInt(500*int64(id)), deposits[id]) }
	coordinator.views["cumulativeConviction"] = func(inputs []any) ([]any, error) {
		return []any{principal(inputs[0].(*big.Int).Uint64())}, nil
	}
	sink.views["operatorPrincipal"] = func(inputs []any) ([]any, error) {
		return []any{principal(inputs[0].(*big.Int).Uint64())}, nil
	}
	total := new(big.Int).Add(principal(1), principal(2))
	sink.views["principal"] = subnetWorldConstant(total)
	sink.views["liveStake"] = subnetWorldConstant(new(big.Int).Add(total, big.NewInt(4)))
}

// Writes one validator's applied weight row and its update block.
func (self *subnetWorld) setWeights(uid uint16, pairs []subnetWeightPair, updated uint64) {
	netuid := subnetNetuidArg(self.deployment.Netuid)
	self.setStorage("SubtensorModule", "Weights", pairs, netuid, subnetNetuidArg(uid))
	updates := make([]types.U64, 32)
	key, _ := types.CreateStorageKey(self.metadata, "SubtensorModule", "LastUpdate", netuid)
	if raw, ok := self.storage[codec.HexEncodeToString(key)]; ok {
		_ = codec.Decode(raw, &updates)
	}
	updates[uid] = types.NewU64(updated)
	self.setStorage("SubtensorModule", "LastUpdate", updates, netuid)
}

// The synthetic fleet: one hotkey whose seeds are public test constants.
func subnetWorldFleetKeys(t testing.TB) (*crv4.Keypair, ed25519.PrivateKey) {
	t.Helper()
	hotkey, err := crv4.KeypairFromSeed(subnetWorldKey("fleet-1-hotkey-seed"))
	if err != nil {
		t.Fatal(err)
	}
	client := ed25519.NewKeyFromSeed(subnetWorldHash("fleet-1-client-seed").Bytes())
	return hotkey, client
}

// One tagged fleet registration exactly as the commitments pallet stores it:
// a u64 deposit, the u32 commitment block and the Raw40 tagged info.
func subnetWorldCommitmentRegistration(t testing.TB, hash [32]byte, block uint64) []byte {
	t.Helper()
	info, err := crv4.EncodeFleetCommitmentInfo(hash)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 12)
	binary.LittleEndian.PutUint64(raw[:8], 1000)
	binary.LittleEndian.PutUint32(raw[8:12], uint32(block))
	return append(raw, info...)
}

// Writes raw stored bytes under a key derived exactly as the runtime does.
func (self *subnetWorld) setRawStorage(pallet, item string, raw []byte, args ...[]byte) {
	self.t.Helper()
	key, err := types.CreateStorageKey(self.metadata, pallet, item, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	self.storage[codec.HexEncodeToString(key)] = raw
}

// Pools, escrow, validators and one mirrored fleet with an accepted binding.
func (self *subnetWorld) installIdentityState() {
	netuid := subnetNetuidArg(self.deployment.Netuid)
	vaultColdkey := types.AccountID(subnetMirroredColdkey(self.vault))
	register := func(hotkey [32]byte, uid uint16, owner [32]byte) {
		self.setStorage("SubtensorModule", "Keys", types.AccountID(hotkey), netuid, subnetNetuidArg(uid))
		self.setStorage("SubtensorModule", "Uids", types.NewU16(uid), netuid, hotkey[:])
		self.setStorage("SubtensorModule", "Owner", types.AccountID(owner), hotkey[:])
	}
	for noId := uint64(1); noId <= 2; noId++ {
		register(subnetWorldKey(fmt.Sprintf("operator-%d-pool", noId)), uint16(10+noId), [32]byte(vaultColdkey))
	}
	register(subnetWorldKey("escrow"), 5, [32]byte(vaultColdkey))
	permits := make([]types.Bool, 32)
	for index := 1; index <= 2; index++ {
		hotkey := subnetWorldKey(fmt.Sprintf("validator-%d-hotkey", index))
		register(hotkey, uint16(index), subnetWorldKey(fmt.Sprintf("validator-%d-coldkey", index)))
		self.setStorage("SubtensorModule", "TotalHotkeyAlpha", types.NewU64(5000), hotkey[:], netuid)
		permits[index] = true
	}
	self.setStorage("SubtensorModule", "ValidatorPermit", permits, netuid)
	self.setStorage("SubtensorModule", "StakeThreshold", types.NewU64(1000))

	hotkey, client := subnetWorldFleetKeys(self.t)
	fleetHotkey := hotkey.PublicKey()
	register(fleetHotkey, 20, subnetWorldKey("fleet-1-coldkey"))
	commitmentHash := subnetWorldKey("fleet-1-manifest")
	commitmentBlock := self.finalized - 100
	self.setRawStorage(crv4.CommitmentsPalletName, "CommitmentOf", subnetWorldCommitmentRegistration(self.t, commitmentHash, commitmentBlock), netuid, fleetHotkey[:])
	coordinator := self.contracts[self.coordinator]
	coordinator.views["mirroredCommitments"] = func(inputs []any) ([]any, error) {
		if inputs[0].([32]byte) != fleetHotkey {
			return []any{[32]byte{}, [32]byte{}, uint64(0)}, nil
		}
		return []any{commitmentHash, [32]byte(self.nativeHash(commitmentBlock)), commitmentBlock}, nil
	}
	binding := stabi.STCoordinatorFleetBinding{
		ChainId: self.deployment.ChainId, Netuid: self.deployment.Netuid, Coordinator: self.coordinator,
		FleetId: subnetWorldKey("fleet-1"), Hotkey: fleetHotkey, ClientId: [16]byte(subnetWorldHash("fleet-1-client").Bytes()[:16]),
		ClientKey: [32]byte(client.Public().(ed25519.PublicKey)), Generation: 1, ValidFromEpoch: 4, ValidToEpoch: 5, CommitmentHash: commitmentHash,
	}
	self.addFleetBinding(binding, client, hotkey, self.finalized-50)
}

// Accepts one binding: the FleetBound event and its original calldata.
func (self *subnetWorld) addFleetBinding(binding stabi.STCoordinatorFleetBinding, client ed25519.PrivateKey, hotkey *crv4.Keypair, block uint64) {
	self.t.Helper()
	digest, err := subnetWorldProtocolBinding(binding).Digest()
	if err != nil {
		self.t.Fatal(err)
	}
	clientSignature := ed25519.Sign(client, digest[:])
	hotkeySignature, err := hotkey.Sign(digest[:])
	if err != nil {
		self.t.Fatal(err)
	}
	coordinatorAbi := subnetWorldAbi(self.t, stabi.STCoordinatorMetaData.ParseABI)
	txHash := self.emit(self.coordinator, coordinatorAbi, "FleetBound", block, uint(len(self.logs)), map[string]any{
		"clientId": binding.ClientId, "fleetId": binding.FleetId, "hotkey": binding.Hotkey, "uid": uint16(20),
		"generation": binding.Generation, "validFromEpoch": binding.ValidFromEpoch, "validToEpoch": binding.ValidToEpoch,
	})
	self.txs[txHash] = subnetWorldTransaction{to: self.coordinator, input: stabi.NewSTCoordinator().PackBindFleetMember(binding, clientSignature, hotkeySignature), blockHash: self.evmHash(block)}
}

func subnetWorldProtocolBinding(binding stabi.STCoordinatorFleetBinding) protocol.FleetBinding {
	return protocol.FleetBinding{
		ChainID: binding.ChainId, Netuid: binding.Netuid, Coordinator: [20]byte(binding.Coordinator), FleetID: binding.FleetId,
		Hotkey: binding.Hotkey, ClientID: binding.ClientId, ClientKey: binding.ClientKey, Generation: binding.Generation,
		ValidFromEpoch: binding.ValidFromEpoch, ValidToEpoch: binding.ValidToEpoch, CommitmentHash: binding.CommitmentHash,
	}
}

// Block hashes carry their height in the first eight bytes so a header
// lookup is direct; the tail keeps distinct kinds and forks distinct.
func subnetWorldBlockHash(kind string, number uint64) [32]byte {
	tail := crypto.Keccak256([]byte(fmt.Sprintf("synthetic-subnet-world/%s/%d", kind, number)))
	var hash [32]byte
	binary.BigEndian.PutUint64(hash[:8], number)
	copy(hash[8:], tail[:24])
	return hash
}

func (self *subnetWorld) nativeHash(number uint64) types.Hash {
	if hash, ok := self.nativeHashes[number]; ok {
		return hash
	}
	return types.Hash(subnetWorldBlockHash("native", number))
}

func (self *subnetWorld) evmHash(number uint64) common.Hash {
	if hash, ok := self.evmHashes[number]; ok {
		return hash
	}
	return common.Hash(subnetWorldBlockHash("evm", number))
}

// Writes one storage value keyed exactly as the runtime metadata keys it.
func (self *subnetWorld) setStorage(pallet, item string, value any, args ...[]byte) {
	self.t.Helper()
	key, err := types.CreateStorageKey(self.metadata, pallet, item, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	encoded, err := codec.Encode(value)
	if err != nil {
		self.t.Fatal(err)
	}
	self.storage[codec.HexEncodeToString(key)] = encoded
}

// The hex storage key the runtime metadata derives.
func (self *subnetWorld) storageKeyHex(pallet, item string, args ...[]byte) string {
	self.t.Helper()
	key, err := types.CreateStorageKey(self.metadata, pallet, item, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	return codec.HexEncodeToString(key)
}

func (self *subnetWorld) deleteStorage(pallet, item string, args ...[]byte) {
	self.t.Helper()
	key, err := types.CreateStorageKey(self.metadata, pallet, item, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	delete(self.storage, codec.HexEncodeToString(key))
}

func subnetWorldU256(number uint64) []byte {
	argument := make([]byte, 32)
	binary.LittleEndian.PutUint64(argument, number)
	return argument
}

// The healthy native state: the Evm mapping around the checkpoint and the
// reviewed native profile.
func (self *subnetWorld) installNativeState() {
	netuid := subnetNetuidArg(self.deployment.Netuid)
	for number := self.finalized - 10; number <= self.finalized+10; number++ {
		self.setStorage("Ethereum", "BlockHash", types.Hash(self.evmHash(number)), subnetWorldU256(number))
	}
	self.setStorage("SubtensorModule", "NetworksAdded", types.NewBool(true), netuid)
	self.setStorage("SubtensorModule", "MaxAllowedUids", types.NewU16(256), netuid)
	self.setStorage("SubtensorModule", "MaxAllowedValidators", types.NewU16(64), netuid)
	self.setStorage("SubtensorModule", "ImmunityPeriod", types.NewU16(5000), netuid)
	self.setStorage("SubtensorModule", "Tempo", types.NewU16(360), netuid)
	self.setStorage("SubtensorModule", "CommitRevealWeightsEnabled", types.NewBool(true), netuid)
	self.setStorage("SubtensorModule", "LiquidAlphaOn", types.NewBool(false), netuid)
	self.setStorage("SubtensorModule", "WeightsVersionKey", types.NewU64(7), netuid)
	self.setStorage("SubtensorModule", "MechanismCountCurrent", types.NewU8(1), netuid)
}

// The real reviewed mainnet policy document from the sibling sn checkout.
func subnetWorldPolicyDocument(t testing.TB) ([]byte, *protocol.Policy) {
	t.Helper()
	document, err := os.ReadFile("../../sn/deploy/mainnet/policy-v1.yml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := protocol.ParsePolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	return document, policy
}

// The coordinator versions the reviewed launch installs: epoch 0 on the
// settlement windows, then the production cadence from epoch 1, both under
// the one reviewed policy hash.
func (self *subnetWorld) reviewedPolicyVersions() []stabi.STCoordinatorPolicySnapshot {
	digest, _ := self.policy.Hash()
	settlement := self.policy.Settlement
	cadence := self.policy.ProductionCadence
	base := stabi.STCoordinatorPolicySnapshot{
		PolicyHash: digest, ClaimTTLEpochs: settlement.ClaimTTLEpochs, ClaimGraceEpochs: settlement.ClaimGraceEpochs,
		MaximumBindingValidityEpochs: self.policy.Binding.MaximumValidityEpochs,
		EpochDepositCapRao:           new(big.Int).SetUint64(self.policy.Deposit.EpochCapRaoPerOperator),
		CampaignDepositCapRao:        new(big.Int).SetUint64(self.policy.Deposit.TotalTestCampaignCapRao),
	}
	first := base
	first.EffectiveEpoch, first.EffectiveBlock = 0, 100
	first.EpochBlocks, first.RootCommitWindowBlocks, first.FinalizeOffsetBlocks, first.CloseGraceBlocks = settlement.EpochBlocks, settlement.RootCommitWindowBlocks, settlement.FinalizeOffsetBlocks, settlement.CloseGraceBlocks
	first.CommitmentMaxAgeBlocks = 2 * settlement.EpochBlocks
	production := base
	production.EffectiveEpoch, production.EffectiveBlock = cadence.AfterAcceleratedEpochs, 100+cadence.AfterAcceleratedEpochs*settlement.EpochBlocks
	production.EpochBlocks, production.RootCommitWindowBlocks, production.FinalizeOffsetBlocks, production.CloseGraceBlocks = cadence.EpochBlocks, cadence.RootCommitWindowBlocks, cadence.FinalizeOffsetBlocks, cadence.CloseGraceBlocks
	production.CommitmentMaxAgeBlocks = 2 * cadence.EpochBlocks
	return []stabi.STCoordinatorPolicySnapshot{first, production}
}

func (self *subnetWorld) policies() []stabi.STCoordinatorPolicySnapshot {
	return self.policyVersions
}

// The first block of an epoch under the coordinator's own boundary rule.
func (self *subnetWorld) epochStartBlock(epoch uint64) uint64 {
	active := self.policyVersions[0]
	for _, policy := range self.policyVersions {
		if policy.EffectiveEpoch <= epoch {
			active = policy
		}
	}
	return active.EffectiveBlock + (epoch-active.EffectiveEpoch)*active.EpochBlocks
}

func (self *subnetWorld) contract(address common.Address, metadata *abi.ABI) *subnetWorldContract {
	contract, ok := self.contracts[address]
	if !ok {
		contract = &subnetWorldContract{abi: metadata, code: []byte("synthetic-code/" + address.Hex()), slots: map[common.Hash]common.Hash{}, views: map[string]subnetWorldView{}}
		self.contracts[address] = contract
	}
	return contract
}

// Constant view outputs.
func subnetWorldConstant(values ...any) subnetWorldView {
	return func([]any) ([]any, error) { return values, nil }
}

func (self *subnetWorld) installContracts() {
	coordinatorAbi := subnetWorldAbi(self.t, stabi.STCoordinatorMetaData.ParseABI)
	vaultAbi := subnetWorldAbi(self.t, stabi.STSettlementVaultMetaData.ParseABI)
	sinkAbi := subnetWorldAbi(self.t, stabi.STReserveSinkMetaData.ParseABI)
	implementation := subnetWorldAddress("coordinator-implementation")
	coordinator := self.contract(self.coordinator, coordinatorAbi)
	coordinator.slots[subnetErc1967ImplementationSlot] = common.BytesToHash(implementation.Bytes())
	self.contract(implementation, coordinatorAbi)
	vault := self.contract(self.vault, vaultAbi)
	sink := self.contract(self.sink, sinkAbi)
	self.contract(subnetWorldAddress("validator-evidence"), nil)
	owner := self.contract(subnetWorldAddress("owner-safe"), subnetWorldAbi(self.t, subnetSafeAbi))
	owner.views["getThreshold"] = subnetWorldConstant(big.NewInt(2))
	owner.views["getOwners"] = subnetWorldConstant([]common.Address{subnetWorldAddress("signer-1"), subnetWorldAddress("signer-2"), subnetWorldAddress("signer-3")})

	coordinator.views["netuid"] = subnetWorldConstant(self.deployment.Netuid)
	coordinator.views["selfColdkey"] = subnetWorldConstant(subnetMirroredColdkey(self.coordinator))
	coordinator.views["settlementVault"] = subnetWorldConstant(self.vault)
	coordinator.views["reserveSink"] = subnetWorldConstant(self.sink)
	coordinator.views["owner"] = subnetWorldConstant(subnetWorldAddress("owner-safe"))
	coordinator.views["guardian"] = subnetWorldConstant(subnetWorldAddress("guardian"))
	coordinator.views["activeGuardian"] = subnetWorldConstant(subnetWorldAddress("guardian"))
	coordinator.views["pendingGuardian"] = subnetWorldConstant(common.Address{})
	coordinator.views["pendingGuardianEpoch"] = subnetWorldConstant(uint64(0))
	coordinator.views["commitmentOracle"] = subnetWorldConstant(subnetWorldAddress("oracle"))
	coordinator.views["activeCommitmentOracle"] = subnetWorldConstant(subnetWorldAddress("oracle"))
	coordinator.views["pendingCommitmentOracle"] = subnetWorldConstant(common.Address{})
	coordinator.views["pendingCommitmentOracleEpoch"] = subnetWorldConstant(uint64(0))
	coordinator.views["paused"] = subnetWorldConstant(false)
	coordinator.views["validatorEvidence"] = subnetWorldConstant(subnetWorldAddress("validator-evidence"))
	coordinator.views["currentEpoch"] = func([]any) ([]any, error) { return []any{new(big.Int).SetUint64(self.currentEpoch)}, nil }
	coordinator.views["policyCount"] = func([]any) ([]any, error) { return []any{big.NewInt(int64(len(self.policies())))}, nil }
	coordinator.views["epochStartBlock"] = func(inputs []any) ([]any, error) {
		return []any{new(big.Int).SetUint64(self.epochStartBlock(inputs[0].(*big.Int).Uint64()))}, nil
	}
	coordinator.views["epochEndBlock"] = func(inputs []any) ([]any, error) {
		epoch := inputs[0].(*big.Int).Uint64()
		return []any{new(big.Int).SetUint64(self.epochStartBlock(epoch + 1))}, nil
	}
	coordinator.views["RESERVE_ROUNDING_ALLOWANCE_RAO"] = subnetWorldConstant(big.NewInt(2))
	coordinator.views["RUNTIME_SHARE_ROUNDING_ALLOWANCE_RAO"] = subnetWorldConstant(big.NewInt(1))
	coordinator.views["policyByIndex"] = func(inputs []any) ([]any, error) {
		index := inputs[0].(*big.Int)
		if !index.IsUint64() || index.Uint64() >= uint64(len(self.policies())) {
			return nil, fmt.Errorf("revert")
		}
		return []any{self.policies()[index.Uint64()]}, nil
	}
	coordinator.views["policyAt"] = func(inputs []any) ([]any, error) {
		epoch := inputs[0].(*big.Int).Uint64()
		var active stabi.STCoordinatorPolicySnapshot
		for _, policy := range self.policies() {
			if policy.EffectiveEpoch <= epoch {
				active = policy
			}
		}
		return []any{active}, nil
	}
	coordinator.views["operatorCount"] = subnetWorldConstant(big.NewInt(2))
	coordinator.views["operatorIdAt"] = func(inputs []any) ([]any, error) {
		return []any{new(big.Int).Add(inputs[0].(*big.Int), big.NewInt(1))}, nil
	}
	coordinator.views["operatorAt"] = func(inputs []any) ([]any, error) {
		noId := inputs[0].(*big.Int).Uint64()
		return []any{stabi.STCoordinatorOperatorVersion{
			Coldkey: subnetWorldKey(fmt.Sprintf("operator-%d-coldkey", noId)), PoolHotkey: subnetWorldKey(fmt.Sprintf("operator-%d-pool", noId)),
			DepositHotkey: subnetWorldKey(fmt.Sprintf("operator-%d-deposit", noId)), DepositSigner: subnetWorldAddress(fmt.Sprintf("operator-%d-deposit-signer", noId)),
			RootSigner: subnetWorldAddress(fmt.Sprintf("operator-%d-root-signer", noId)), EffectiveEpoch: 1, Active: true,
		}}, nil
	}
	coordinator.views["cumulativeConviction"] = func(inputs []any) ([]any, error) {
		return []any{big.NewInt(500 * inputs[0].(*big.Int).Int64())}, nil
	}
	vault.views["coordinator"] = subnetWorldConstant(self.coordinator)
	vault.views["netuid"] = subnetWorldConstant(self.deployment.Netuid)
	vault.views["selfColdkey"] = subnetWorldConstant(subnetMirroredColdkey(self.vault))
	vault.views["escrowHotkey"] = subnetWorldConstant(subnetWorldKey("escrow"))
	vault.views["escrowRegistered"] = subnetWorldConstant(true)
	vault.views["pools"] = func(inputs []any) ([]any, error) {
		noId := inputs[0].(*big.Int).Uint64()
		return []any{subnetWorldKey(fmt.Sprintf("operator-%d-pool", noId)), uint16(10 + noId), true}, nil
	}
	sink.views["recorder"] = subnetWorldConstant(self.coordinator)
	sink.views["netuid"] = subnetWorldConstant(self.deployment.Netuid)
	sink.views["selfColdkey"] = subnetWorldConstant(subnetMirroredColdkey(self.sink))
	sink.views["reserveHotkey"] = subnetWorldConstant(subnetWorldKey("reserve"))
	sink.views["liveStake"] = subnetWorldConstant(big.NewInt(1504))
	sink.views["principal"] = subnetWorldConstant(big.NewInt(1500))
	sink.views["operatorPrincipal"] = func(inputs []any) ([]any, error) {
		return []any{big.NewInt(500 * inputs[0].(*big.Int).Int64())}, nil
	}
}

func subnetSafeAbi() (*abi.ABI, error) {
	parsed, err := abi.JSON(strings.NewReader(subnetSafeAbiJson))
	return &parsed, err
}

// The reviewed enrollment matching the healthy world.
func (self *subnetWorld) enrollment() *SubnetCorrectnessSettings {
	codeHash := func(address common.Address) string {
		return strings.ToLower(crypto.Keccak256Hash(self.contracts[address].code).Hex())
	}
	implementation := subnetWorldAddress("coordinator-implementation")
	return &SubnetCorrectnessSettings{
		Schema:      subnetCorrectnessEnrollmentSchema,
		ReviewFrom:  self.now.Add(-time.Hour).Format(time.RFC3339Nano),
		ReviewUntil: self.now.Add(time.Hour).Format(time.RFC3339Nano),
		NativeRpc:   []string{subnetWorldNative1, subnetWorldNative2},
		EvmRpc:      []string{subnetWorldEvm1, subnetWorldEvm2},
		ApiOrigins:  []string{subnetWorldApi1, subnetWorldApi2},
		DeployBlock: 100,
		ReviewedRuntimes: []SubnetReviewedRuntime{{
			SpecName: self.version.SpecName, SpecVersion: self.version.SpecVersion, TransactionVersion: self.version.TransactionVersion,
			StateVersion: self.version.StateVersion, CodeHash: self.codeHash, MetadataHash: self.metadataHash,
		}},
		Governance: SubnetReviewedGovernance{
			CoordinatorImplementation:         strings.ToLower(implementation.Hex()),
			CoordinatorProxyCodeHash:          codeHash(self.coordinator),
			CoordinatorImplementationCodeHash: codeHash(implementation),
			SettlementVaultCodeHash:           codeHash(self.vault),
			ReserveSink:                       strings.ToLower(self.sink.Hex()),
			ReserveSinkCodeHash:               codeHash(self.sink),
			Owner:                             strings.ToLower(subnetWorldAddress("owner-safe").Hex()),
			OwnerThreshold:                    2,
			OwnerSigners:                      3,
			Guardian:                          strings.ToLower(subnetWorldAddress("guardian").Hex()),
			CommitmentOracle:                  strings.ToLower(subnetWorldAddress("oracle").Hex()),
		},
		PolicyDocument: base64.StdEncoding.EncodeToString(self.policyDocument),
		NativeProfile:  SubnetReviewedNativeProfile{MaxAllowedUids: 256, MaxAllowedValidators: 64, ImmunityPeriod: 5000, CommitRevealWeightsEnabled: true, LiquidAlphaEnabled: false, WeightsVersionKey: 7, MechanismCount: 1, Tempo: 360},
		Operators: []SubnetReviewedOperator{
			{NoId: 1, ArtifactSigner: strings.ToLower(subnetWorldAddress("operator-1-artifact-signer").Hex())},
			{NoId: 2, ArtifactSigner: strings.ToLower(subnetWorldAddress("operator-2-artifact-signer").Hex())},
		},
		Validators: []SubnetReviewedValidator{
			{Hotkey: subnetWorldHex(subnetWorldKey("validator-1-hotkey")), Coldkey: subnetWorldHex(subnetWorldKey("validator-1-coldkey")), Vpk: subnetWorldHex(subnetWorldKey("validator-1-vpk"))},
			{Hotkey: subnetWorldHex(subnetWorldKey("validator-2-hotkey")), Coldkey: subnetWorldHex(subnetWorldKey("validator-2-coldkey")), Vpk: subnetWorldHex(subnetWorldKey("validator-2-vpk"))},
		},
		Deployment: self.deployment,
	}
}

// Signal settings with the world as the only source.
func (self *subnetWorld) signalSettings() SignalSettings {
	settings := syntheticSettings(self)
	now := self.now
	settings.Now = func() time.Time { return now }
	settings.STConfigStatus = STConfigurationEnabled
	settings.VerificationEnabled = true
	self.settings.Deployment = self.deployment
	settings.STDeploymentKey = self.deployment.deploymentKey()
	settings.SubnetCorrectness = cloneSubnetCorrectnessSettings(self.settings)
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
	return settings
}

// SignalSource methods are inert; only Do answers.
func (self *subnetWorld) PostgreSQL(context.Context, string) ([]Row, error) {
	return nil, fmt.Errorf("unexpected PostgreSQL read")
}
func (self *subnetWorld) Redis(context.Context, HostSettings, int, ...string) (string, error) {
	return "", fmt.Errorf("unexpected Redis read")
}
func (self *subnetWorld) Host(context.Context, HostSettings, string) (string, error) {
	return "", fmt.Errorf("unexpected host command")
}

func subnetWorldJsonResponse(request *http.Request, body []byte) *http.Response {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: http.StatusOK, Header: header, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body)), Request: request}
}

// The one Do seam: Json-Rpc for the four enrolled endpoints, routes for Api.
func (self *subnetWorld) Do(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	endpoint := request.URL.String()
	self.requests = append(self.requests, request.Method+" "+request.URL.Path)
	switch {
	case request.Method == http.MethodPost && (endpoint == subnetWorldNative1 || endpoint == subnetWorldNative2 || endpoint == subnetWorldEvm1 || endpoint == subnetWorldEvm2):
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		return subnetWorldJsonResponse(request, self.answerRpc(endpoint, body)), nil
	case request.Method == http.MethodGet:
		origin := request.URL.Scheme + "://" + request.URL.Host
		if origin != subnetWorldApi1 && origin != subnetWorldApi2 {
			return nil, fmt.Errorf("unexpected origin")
		}
		route, ok := self.routes[request.URL.Path]
		if !ok {
			return nil, fmt.Errorf("unexpected route")
		}
		return route(request), nil
	}
	return nil, fmt.Errorf("unexpected request")
}

type subnetWorldRequest struct {
	JsonRpc string            `json:"jsonrpc"`
	Id      uint64            `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

func (self *subnetWorld) answerRpc(endpoint string, body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) != 0 && trimmed[0] == '[' {
		var requests []subnetWorldRequest
		if err := json.Unmarshal(body, &requests); err != nil {
			self.t.Fatalf("malformed batch: %v", err)
		}
		replies := []json.RawMessage{}
		for _, request := range requests {
			replies = append(replies, self.answerOne(endpoint, request))
		}
		encoded, _ := json.Marshal(replies)
		return encoded
	}
	var request subnetWorldRequest
	if err := json.Unmarshal(body, &request); err != nil {
		self.t.Fatalf("malformed request: %v", err)
	}
	return self.answerOne(endpoint, request)
}

func (self *subnetWorld) answerOne(endpoint string, request subnetWorldRequest) json.RawMessage {
	self.calls[endpoint+" "+request.Method]++
	var result any
	var err error
	handled := false
	if self.override != nil {
		result, err, handled = self.override(endpoint, request.Method, request.Params)
	}
	if !handled {
		result, err = self.dispatch(endpoint, request.Method, request.Params)
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": request.Id}
	if err != nil {
		reply["error"] = map[string]any{"code": -32000, "message": "synthetic failure"}
	} else {
		reply["result"] = result
	}
	encoded, _ := json.Marshal(reply)
	return encoded
}

func subnetWorldString(t testing.TB, raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("parameter is not a string: %s", raw)
	}
	return value
}

func (self *subnetWorld) dispatch(endpoint, method string, params []json.RawMessage) (any, error) {
	switch method {
	case "chain_getFinalizedHead":
		return subnetHashHex(self.nativeHash(self.finalized)), nil
	case "chain_getHeader":
		hash, ok := subnetParseHash(subnetWorldString(self.t, params[0]))
		number := binary.BigEndian.Uint64(hash[:8])
		if !ok || number > self.finalized+20 || self.nativeHash(number) != hash {
			return nil, nil
		}
		parent := types.Hash{}
		if number != 0 {
			parent = self.nativeHash(number - 1)
		}
		return map[string]any{"parentHash": subnetHashHex(parent), "number": hexutil.EncodeUint64(number)}, nil
	case "chain_getBlockHash":
		var number uint64
		if err := json.Unmarshal(params[0], &number); err != nil {
			return nil, err
		}
		if number == 0 {
			return subnetHashHex(self.genesis), nil
		}
		if number > self.finalized+20 {
			return nil, nil
		}
		return subnetHashHex(self.nativeHash(number)), nil
	case "state_getRuntimeVersion":
		return map[string]any{"specName": self.version.SpecName, "implName": "synthetic", "authoringVersion": 1, "specVersion": self.version.SpecVersion, "implVersion": 1, "apis": []any{}, "transactionVersion": self.version.TransactionVersion, "stateVersion": self.version.StateVersion}, nil
	case "state_getStorageHash":
		if subnetWorldString(self.t, params[0]) == subnetRuntimeCodeKey {
			return self.codeHash, nil
		}
		return nil, nil
	case "state_getMetadata":
		return self.metadataHex, nil
	case "state_getStorage":
		value, ok := self.storage[subnetWorldString(self.t, params[0])]
		if !ok {
			return nil, nil
		}
		return codec.HexEncodeToString(value), nil
	case "state_getKeysPaged":
		prefix := subnetWorldString(self.t, params[0])
		var count int
		_ = json.Unmarshal(params[1], &count)
		start := ""
		_ = json.Unmarshal(params[2], &start)
		keys := []string{}
		for key := range self.storage {
			if strings.HasPrefix(key, prefix) && key > start {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		if len(keys) > count {
			keys = keys[:count]
		}
		return keys, nil
	case "eth_chainId":
		return hexutil.EncodeUint64(self.deployment.ChainId), nil
	case "eth_getBlockByNumber":
		tag := subnetWorldString(self.t, params[0])
		number := self.evmFinalized
		if tag != "finalized" {
			parsed, err := hexutil.DecodeUint64(tag)
			if err != nil {
				return nil, err
			}
			number = parsed
		}
		if number > self.evmFinalized+20 {
			return nil, nil
		}
		return map[string]any{"number": hexutil.EncodeUint64(number), "hash": self.evmHash(number).Hex()}, nil
	case "eth_call":
		var call struct {
			To   common.Address `json:"to"`
			Data hexutil.Bytes  `json:"data"`
		}
		if err := json.Unmarshal(params[0], &call); err != nil {
			return nil, err
		}
		return self.call(call.To, call.Data)
	case "eth_getCode":
		var address common.Address
		_ = json.Unmarshal(params[0], &address)
		contract, ok := self.contracts[address]
		if !ok {
			return "0x", nil
		}
		return hexutil.Encode(contract.code), nil
	case "eth_getStorageAt":
		var address common.Address
		var slot common.Hash
		_ = json.Unmarshal(params[0], &address)
		_ = json.Unmarshal(params[1], &slot)
		word := common.Hash{}
		if contract, ok := self.contracts[address]; ok {
			word = contract.slots[slot]
		}
		return word.Hex(), nil
	case "eth_getLogs":
		var filter struct {
			Address   common.Address  `json:"address"`
			FromBlock hexutil.Uint64  `json:"fromBlock"`
			ToBlock   hexutil.Uint64  `json:"toBlock"`
			Topics    [][]common.Hash `json:"topics"`
		}
		if err := json.Unmarshal(params[0], &filter); err != nil {
			return nil, err
		}
		logs := []ethtypes.Log{}
		for _, log := range self.logs {
			if log.Address != filter.Address || log.BlockNumber < uint64(filter.FromBlock) || uint64(filter.ToBlock) < log.BlockNumber {
				continue
			}
			if len(filter.Topics) != 0 && len(filter.Topics[0]) != 0 {
				matched := false
				for _, topic := range filter.Topics[0] {
					matched = matched || (len(log.Topics) != 0 && log.Topics[0] == topic)
				}
				if !matched {
					continue
				}
			}
			logs = append(logs, log)
		}
		return logs, nil
	case "eth_getTransactionByHash":
		var hash common.Hash
		_ = json.Unmarshal(params[0], &hash)
		transaction, ok := self.txs[hash]
		if !ok {
			return nil, nil
		}
		to := transaction.to.Hex()
		return map[string]any{"hash": hash.Hex(), "blockHash": transaction.blockHash.Hex(), "to": to, "input": hexutil.Encode(transaction.input)}, nil
	}
	self.t.Fatalf("unexpected method %s", method)
	return nil, nil
}

// Dispatches one view through the contract's real Abi.
func (self *subnetWorld) call(to common.Address, data []byte) (any, error) {
	contract, ok := self.contracts[to]
	if !ok || contract.abi == nil || len(data) < 4 {
		return "0x", nil
	}
	method, err := contract.abi.MethodById(data[:4])
	if err != nil {
		return nil, err
	}
	view, ok := contract.views[method.Name]
	if !ok {
		return nil, fmt.Errorf("revert: no view %s", method.Name)
	}
	inputs, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		return nil, err
	}
	outputs, err := view(inputs)
	if err != nil {
		return nil, err
	}
	packed, err := method.Outputs.Pack(outputs...)
	if err != nil {
		self.t.Fatalf("pack %s: %v", method.Name, err)
	}
	return hexutil.Encode(packed), nil
}

// Appends one event log emitted by a contract through its real Abi.
func (self *subnetWorld) emit(address common.Address, metadata *abi.ABI, name string, block uint64, index uint, values map[string]any) common.Hash {
	self.t.Helper()
	event, ok := metadata.Events[name]
	if !ok {
		self.t.Fatalf("no event %s", name)
	}
	topics := []common.Hash{event.ID}
	nonIndexed := []any{}
	for _, input := range event.Inputs {
		value, ok := values[input.Name]
		if !ok {
			self.t.Fatalf("event %s is missing %s", name, input.Name)
		}
		if input.Indexed {
			topic, err := abi.MakeTopics([]any{value})
			if err != nil {
				self.t.Fatal(err)
			}
			topics = append(topics, topic[0][0])
			continue
		}
		nonIndexed = append(nonIndexed, value)
	}
	data, err := event.Inputs.NonIndexed().Pack(nonIndexed...)
	if err != nil {
		self.t.Fatal(err)
	}
	txHash := subnetWorldHash(fmt.Sprintf("tx/%s/%d/%d", name, block, index))
	self.logs = append(self.logs, ethtypes.Log{Address: address, Topics: topics, Data: data, BlockNumber: block, TxHash: txHash, BlockHash: self.evmHash(block), Index: index})
	return txHash
}

// Runs one signal and fails the test on an execution error.
func runSubnetWorld(t *testing.T, signal Signal, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("signal run failed: %v", err)
	}
	requireSubnetEvidenceFrame(t, alerts)
	return alerts
}

// Every §22.1--§22.5 alert carries the SIGNALS.md §22.10 frame, and every
// decided one names its invariant with expected and observed values.
func requireSubnetEvidenceFrame(t *testing.T, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		for _, key := range []string{"environment=", "scope=", "checkpoint=", "completeness=", "comparison_source="} {
			if !strings.Contains(alert.Context, key) {
				t.Errorf("%s %s %s lacks %s in its context: %q", alert.Class, alert.Target, alert.Frame, key, alert.Context)
			}
		}
		if strings.HasPrefix(alert.Observed, "state=unknown ") {
			continue
		}
		for _, key := range []string{"invariant=", "expected=", "observed="} {
			if !strings.Contains(alert.Observed, key) {
				t.Errorf("%s %s %s lacks %s: %q", alert.Class, alert.Target, alert.Frame, key, alert.Observed)
			}
		}
	}
}

// Alerts of one class, optionally narrowed to a target.
func subnetAlerts(alerts Alerts, class, target string) []Alert {
	selected := []Alert{}
	for _, alert := range alerts {
		if alert.Class == class && (target == "" || alert.Target == target) {
			selected = append(selected, alert)
		}
	}
	return selected
}

// Every alert's rendered form must omit every synthetic private identity.
func (self *subnetWorld) requirePrivate(t *testing.T, alerts Alerts) {
	t.Helper()
	forbidden := []string{subnetWorldNative1, subnetWorldNative2, subnetWorldEvm1, subnetWorldEvm2, "private.example.test", self.deployment.DeploymentId, self.deployment.Coordinator, self.deployment.SettlementVault, self.deployment.GenesisHash, self.deployment.PolicyHash, self.codeHash, self.metadataHash}
	for _, validator := range self.settings.Validators {
		forbidden = append(forbidden, validator.Hotkey, validator.Coldkey, validator.Vpk, validator.Hotkey[2:])
	}
	for address := range self.contracts {
		forbidden = append(forbidden, strings.ToLower(address.Hex()), address.Hex(), strings.ToLower(address.Hex())[2:])
	}
	for _, alert := range alerts {
		markdown := alert.Markdown()
		for _, value := range forbidden {
			if value != "" && strings.Contains(strings.ToLower(markdown), strings.ToLower(value)) {
				t.Fatalf("alert %s/%s rendered a private identity", alert.Class, alert.Target)
			}
		}
	}
}

func requireNoSubnetAlerts(t *testing.T, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		t.Errorf("unexpected alert %s %s %s %s: %s", alert.Severity, alert.Class, alert.Target, alert.Frame, alert.Observed)
	}
}

func requireSubnetAlert(t *testing.T, alerts Alerts, class, target, frame string, severity Severity) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target && alert.Frame == frame {
			if alert.Severity != severity {
				t.Fatalf("alert %s %s %s severity %s, want %s", class, target, frame, alert.Severity, severity)
			}
			return alert
		}
	}
	for _, alert := range alerts {
		t.Logf("present: %s %s %s %s %s", alert.Severity, alert.Class, alert.Target, alert.Frame, alert.Observed)
	}
	t.Fatalf("missing alert %s %s %s", class, target, frame)
	return Alert{}
}

// SIGNALS.md §22.10 acceptance helpers: a world whose run is canceled when
// it first calls the method, and a world whose method source fails.
func subnetWorldAcceptanceCanceled(t *testing.T, signal Signal, trigger string) (Signal, SignalSettings, context.Context) {
	world := newSubnetWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == trigger {
			cancel()
		}
		return nil, nil, false
	}
	return signal, world.signalSettings(), ctx
}

func subnetWorldAcceptanceUnknown(t *testing.T, signal Signal, failing string) (Signal, SignalSettings) {
	world := newSubnetWorld(t)
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == failing {
			return nil, errSubnetWorldSourceLoss, true
		}
		return nil, nil, false
	}
	return signal, world.signalSettings()
}

var errSubnetWorldSourceLoss = errors.New("synthetic source loss")
