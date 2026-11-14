// A synthetic coordinator and v1 settlement vault that apply the contracts'
// state machine and emit their abi-encoded events. Healthy fixtures are
// therefore internally consistent; each failing fixture is one mutation.
package monitor

import (
	"bytes"
	"encoding/hex"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/stabi"
)

// Synthetic policy: 100-block epochs from block 1000.
const subnetSettlementTestDeployBlock = 1000

type subnetSettlementTestChain struct {
	t            *testing.T
	contracts    subnetSettlementContracts
	policy       stabi.STCoordinatorPolicySnapshot
	currentEpoch uint64
	operators    []uint64
	versions     map[uint64]stabi.STCoordinatorOperatorVersion
	entitlements map[subnetSettlementKey]*stabi.STSettlementVaultEntitlement
	roots        map[subnetSettlementKey]*stabi.RootCommitmentsOutput
	carry        map[uint64]*big.Int
	credit       map[[32]byte]*big.Int
	captured     *big.Int
	paid         *big.Int
	pending      *big.Int
	outstanding  *big.Int
	accounted    *big.Int
	escrowStake  *big.Int
	transferFlr  uint64
	claimTtl     uint64
	logs         []types.Log
	tx           uint64
	logIndex     uint
	paused       bool
	holds        *bool
	coordinator  common.Address
	artifacts    map[[32]byte][]byte
	signerKey    string
}

func newSubnetSettlementTestChain(t *testing.T, operators ...uint64) *subnetSettlementTestChain {
	t.Helper()
	contracts, err := subnetSettlementAbis()
	if err != nil {
		t.Fatal(err)
	}
	chain := &subnetSettlementTestChain{
		t: t, contracts: contracts,
		policy: stabi.STCoordinatorPolicySnapshot{
			PolicyHash: subnetReplayTestPolicyHash, EffectiveEpoch: 0, EffectiveBlock: subnetSettlementTestDeployBlock,
			EpochBlocks: 100, RootCommitWindowBlocks: 30, FinalizeOffsetBlocks: 50, CloseGraceBlocks: 10,
			ClaimTTLEpochs: 2, ClaimGraceEpochs: 1, MaximumBindingValidityEpochs: 4, CommitmentMaxAgeBlocks: 100,
			EpochDepositCapRao: big.NewInt(1_000_000), CampaignDepositCapRao: big.NewInt(10_000_000),
		},
		operators: operators, versions: map[uint64]stabi.STCoordinatorOperatorVersion{},
		entitlements: map[subnetSettlementKey]*stabi.STSettlementVaultEntitlement{},
		roots:        map[subnetSettlementKey]*stabi.RootCommitmentsOutput{},
		carry:        map[uint64]*big.Int{}, credit: map[[32]byte]*big.Int{},
		captured: new(big.Int), paid: new(big.Int), pending: new(big.Int), outstanding: new(big.Int),
		accounted: new(big.Int), escrowStake: new(big.Int), transferFlr: 1000, claimTtl: 20,
		coordinator: subnetReplayTestCoordinator, artifacts: map[[32]byte][]byte{},
		signerKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	for _, noId := range operators {
		chain.versions[noId] = stabi.STCoordinatorOperatorVersion{
			Coldkey: [32]byte{byte(noId)}, PoolHotkey: [32]byte{0x50, byte(noId)}, DepositHotkey: [32]byte{0x60, byte(noId)},
			DepositSigner: common.BigToAddress(big.NewInt(int64(0xd00 + noId))), RootSigner: common.BigToAddress(big.NewInt(int64(0xa00 + noId))),
			EffectiveEpoch: 0, Active: true,
		}
		chain.carry[noId] = new(big.Int)
	}
	return chain
}

func (self *subnetSettlementTestChain) start(epoch uint64) uint64 {
	return subnetSettlementTestDeployBlock + epoch*self.policy.EpochBlocks
}

func (self *subnetSettlementTestChain) end(epoch uint64) uint64 {
	return self.start(epoch) + self.policy.EpochBlocks
}

func (self *subnetSettlementTestChain) entitlement(epoch, noId uint64) *stabi.STSettlementVaultEntitlement {
	key := subnetSettlementKey{epoch: epoch, noId: noId}
	value, ok := self.entitlements[key]
	if !ok {
		value = &stabi.STSettlementVaultEntitlement{Funded: new(big.Int), Total: new(big.Int), Claimed: new(big.Int)}
		self.entitlements[key] = value
	}
	return value
}

func (self *subnetSettlementTestChain) emit(name string, block uint64, values ...any) {
	self.logIndex += 1
	self.logs = append(self.logs, subnetReplayTestLog(self.t, self.contracts.vault, subnetReplayTestVault, name, block, self.logIndex, subnetReplayTestTx(self.tx), values...))
}

func (self *subnetSettlementTestChain) nextTx() {
	self.tx += 1
}

// Captures pool stake at the epoch boundary.
func (self *subnetSettlementTestChain) capture(epoch, noId uint64, amount int64, block uint64) {
	self.nextTx()
	record := self.entitlement(epoch, noId)
	record.Status = subnetSettlementFunded
	record.Funded = big.NewInt(amount)
	self.captured.Add(self.captured, record.Funded)
	self.pending.Add(self.pending, record.Funded)
	self.accounted.Add(self.accounted, record.Funded)
	self.escrowStake.Add(self.escrowStake, record.Funded)
	self.emit("EmissionCaptured", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), self.versions[noId].PoolHotkey, record.Funded)
}

// Records a zero-funded dust epoch exactly as the vault's dust path does.
func (self *subnetSettlementTestChain) dust(epoch, noId uint64, block uint64) {
	self.nextTx()
	record := self.entitlement(epoch, noId)
	record.Status = subnetSettlementFunded
	self.emit("EmissionDeferred", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId))
	self.emit("EmissionDustDeferred", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), self.versions[noId].PoolHotkey, big.NewInt(5), big.NewInt(int64(self.transferFlr-1)), self.transferFlr)
}

// Builds and signs a real payout artifact and commits its root.
func (self *subnetSettlementTestChain) commit(epoch, noId uint64, block uint64, coldkeys ...[32]byte) *payoutartifact.Artifact {
	self.t.Helper()
	providers := []payoutartifact.ProviderInput{}
	for i, coldkey := range coldkeys {
		providers = append(providers, payoutartifact.ProviderInput{
			ClientID: [16]byte{byte(epoch), byte(noId), byte(i + 1)}, NetworkID: [16]byte{0x11, byte(i + 1)}, Coldkey: coldkey,
			UsageBytes: uint64(100 * (i + 1)), Assignments: 10, Confirmations: 10, Eligible: true,
		})
	}
	artifact, err := payoutartifact.Build(payoutartifact.BuildInput{
		DeploymentID: "synthetic-replay-deployment", ChainID: 964, Netuid: 25,
		GenesisHash: subnetReplayTestGenesis.Hex(), PolicyHash: subnetReplayTestPolicyHash.Hex(),
		Coordinator: subnetReplayTestCoordinator, SettlementVault: subnetReplayTestVault,
		Epoch: epoch, NoID: noId,
		Start:                payoutartifact.Boundary{Number: self.start(epoch), Hash: "0x" + strings.Repeat("01", 32)},
		End:                  payoutartifact.Boundary{Number: self.end(epoch) - 1, Hash: "0x" + strings.Repeat("02", 32)},
		OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32),
		Providers: providers, ReliabilityAMin: 8, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	key, err := crypto.HexToECDSA(self.signerKey)
	if err != nil {
		self.t.Fatal(err)
	}
	if err := payoutartifact.Sign(artifact, key); err != nil {
		self.t.Fatal(err)
	}
	raw, err := payoutartifact.Bytes(artifact)
	if err != nil {
		self.t.Fatal(err)
	}
	var artifactHash [32]byte
	decoded, err := hex.DecodeString(strings.TrimPrefix(artifact.ContentHash, "sha256:"))
	if err != nil || len(decoded) != 32 {
		self.t.Fatal("synthetic artifact content hash is invalid")
	}
	copy(artifactHash[:], decoded)
	self.artifacts[artifactHash] = raw
	self.roots[subnetSettlementKey{epoch: epoch, noId: noId}] = &stabi.RootCommitmentsOutput{
		PayoutRoot: artifact.PayoutRoot, ArtifactHash: artifactHash, Committer: self.versions[noId].RootSigner, CommitBlock: block,
	}
	return artifact
}

// Finalizes with the committed root, or marks the root missed.
func (self *subnetSettlementTestChain) finalize(epoch, noId uint64, block uint64) {
	self.nextTx()
	key := subnetSettlementKey{epoch: epoch, noId: noId}
	record := self.entitlement(epoch, noId)
	self.pending.Sub(self.pending, record.Funded)
	self.outstanding.Add(self.outstanding, record.Funded)
	root, present := self.roots[key]
	if !present {
		self.carry[noId].Add(self.carry[noId], record.Funded)
		record.Total = new(big.Int).Set(record.Funded)
		record.Status = subnetSettlementRootMissed
		self.emit("RootMissed", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), record.Funded)
		return
	}
	record.Total = new(big.Int).Add(record.Funded, self.carry[noId])
	self.carry[noId] = new(big.Int)
	record.PayoutRoot = root.PayoutRoot
	record.ArtifactHash = root.ArtifactHash
	expiry := self.start(epoch+self.policy.ClaimTTLEpochs+self.policy.ClaimGraceEpochs+1) - 1
	if expiry < block+self.claimTtl {
		expiry = block + self.claimTtl
	}
	record.ExpiryBlock = expiry
	record.Status = subnetSettlementFinalized
	self.emit("EntitlementFinalized", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), record.PayoutRoot, record.ArtifactHash, record.Total, record.ExpiryBlock)
}

// Claims one leaf and settles its credit immediately or defers payment.
func (self *subnetSettlementTestChain) claim(epoch, noId uint64, coldkey [32]byte, shareBps int64, block uint64, pay bool) {
	self.nextTx()
	record := self.entitlement(epoch, noId)
	amount := new(big.Int).Div(new(big.Int).Mul(record.Total, big.NewInt(shareBps)), big.NewInt(subnetSettlementBps))
	record.Claimed = new(big.Int).Add(record.Claimed, amount)
	credit, ok := self.credit[coldkey]
	if !ok {
		credit = new(big.Int)
		self.credit[coldkey] = credit
	}
	credit.Add(credit, amount)
	relayer := common.BigToAddress(big.NewInt(0xbee))
	self.emit("Claimed", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), coldkey, big.NewInt(shareBps), amount, relayer)
	if !pay {
		self.emit("ClaimPaymentDeferred", block, coldkey, new(big.Int).Set(credit), big.NewInt(0), self.transferFlr, uint8(1))
		return
	}
	paid := new(big.Int).Set(credit)
	self.outstanding.Sub(self.outstanding, paid)
	self.accounted.Sub(self.accounted, paid)
	self.paid.Add(self.paid, paid)
	self.escrowStake.Sub(self.escrowStake, paid)
	credit.SetInt64(0)
	self.emit("ClaimPaid", block, coldkey, paid, relayer)
}

// Expires an entitlement into its own operator's carry.
func (self *subnetSettlementTestChain) expire(epoch, noId uint64, block uint64) {
	self.nextTx()
	record := self.entitlement(epoch, noId)
	remainder := new(big.Int).Sub(record.Total, record.Claimed)
	self.carry[noId].Add(self.carry[noId], remainder)
	record.Status = subnetSettlementCarried
	self.emit("EntitlementExpired", block, new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId), remainder, new(big.Int).Set(self.carry[noId]))
}

// A healthy history across two operators: a finalized epoch with paid and
// deferred claims, a missed root whose carry the next root consumes, a dust
// epoch, and one epoch awaiting finalization within its window.
func newHealthySubnetSettlementChain(t *testing.T) *subnetSettlementTestChain {
	t.Helper()
	chain := newSubnetSettlementTestChain(t, 1, 2)
	alice, bob := [32]byte{0xa1}, [32]byte{0xb2}
	chain.coordinatorFixed()
	// Epoch 0: both operators capture and commit; operator 1 claims are paid.
	chain.capture(0, 1, 10_000, chain.end(0))
	chain.capture(0, 2, 6_000, chain.end(0))
	chain.commit(0, 1, chain.end(0)+5, alice, bob)
	chain.commit(0, 2, chain.end(0)+6, alice)
	chain.finalize(0, 1, chain.end(0)+50)
	chain.finalize(0, 2, chain.end(0)+51)
	chain.claim(0, 1, alice, 3333, chain.end(0)+60, true)
	chain.claim(0, 1, bob, 6667, chain.end(0)+61, false)
	// Epoch 1: operator 1 commits; operator 2 misses its root and carries.
	chain.capture(1, 1, 9_000, chain.end(1))
	chain.capture(1, 2, 4_000, chain.end(1))
	chain.commit(1, 1, chain.end(1)+7, alice)
	chain.finalize(1, 1, chain.end(1)+50)
	chain.finalize(1, 2, chain.end(1)+51)
	// Epoch 2: operator 1 is dust deferred; operator 2's root consumes carry.
	chain.dust(2, 1, chain.end(2))
	chain.capture(2, 2, 5_000, chain.end(2))
	chain.commit(2, 1, chain.end(2)+8, bob)
	chain.commit(2, 2, chain.end(2)+9, bob)
	chain.finalize(2, 1, chain.end(2)+50)
	chain.finalize(2, 2, chain.end(2)+52)
	chain.claim(2, 2, bob, 10000, chain.end(2)+55, true)
	// Epoch 0 of operator 2 expires into its own carry after ttl and grace.
	chain.expire(0, 2, chain.start(4))
	// Epoch 3 is captured, committed and within its finalize offset.
	chain.capture(3, 1, 7_000, chain.end(3))
	chain.capture(3, 2, 3_000, chain.end(3))
	chain.commit(3, 1, chain.end(3)+4, alice)
	chain.commit(3, 2, chain.end(3)+4, bob)
	chain.currentEpoch = 4
	return chain
}

func (self *subnetSettlementTestChain) coordinatorFixed() {
	self.nextTx()
	self.emit("CoordinatorFixed", subnetSettlementTestDeployBlock, self.coordinator)
}

// The finalized evm block of the healthy history: inside epoch 4, after
// epoch 3's root window and before its finalize offset.
func (self *subnetSettlementTestChain) finalizedBlock() uint64 {
	return self.end(3) + 40
}

// Installs the chain's contracts and logs on a test node.
func (self *subnetSettlementTestChain) install(node *subnetReplayTestNode) {
	node.contracts[subnetReplayTestCoordinator] = &subnetReplayTestContract{contract: self.contracts.coordinator, handle: self.coordinatorHandler, deployed: subnetSettlementTestDeployBlock}
	node.contracts[subnetReplayTestVault] = &subnetReplayTestContract{contract: self.contracts.vault, handle: self.vaultHandler, deployed: subnetSettlementTestDeployBlock}
	node.logs[subnetReplayTestVault] = append([]types.Log(nil), self.logs...)
}

func (self *subnetSettlementTestChain) coordinatorHandler(method string, args []any) ([]any, string) {
	uintArgument := func(index int) uint64 { return args[index].(*big.Int).Uint64() }
	switch method {
	case "settlementVault":
		return []any{subnetReplayTestVault}, ""
	case "netuid":
		return []any{uint16(25)}, ""
	case "currentEpoch":
		return []any{new(big.Int).SetUint64(self.currentEpoch)}, ""
	case "operatorCount":
		return []any{big.NewInt(int64(len(self.operators)))}, ""
	case "operatorIdAt":
		return []any{new(big.Int).SetUint64(self.operators[uintArgument(0)])}, ""
	case "paused":
		return []any{self.paused}, ""
	case "policyAt":
		return []any{self.policy}, ""
	case "epochEndBlock":
		return []any{new(big.Int).SetUint64(self.end(uintArgument(0)))}, ""
	case "epochStartBlock":
		return []any{new(big.Int).SetUint64(self.start(uintArgument(0)))}, ""
	case "rootCommitments":
		root, ok := self.roots[subnetSettlementKey{epoch: uintArgument(0), noId: uintArgument(1)}]
		if !ok {
			return []any{[32]byte{}, [32]byte{}, common.Address{}, uint64(0)}, ""
		}
		return []any{root.PayoutRoot, root.ArtifactHash, root.Committer, root.CommitBlock}, ""
	case "operatorAt":
		version, ok := self.versions[uintArgument(0)]
		if !ok || uintArgument(1) < version.EffectiveEpoch {
			return nil, "UnknownOperator"
		}
		return []any{version}, ""
	}
	self.t.Fatalf("unexpected coordinator view %s", method)
	return nil, ""
}

func (self *subnetSettlementTestChain) vaultHandler(method string, args []any) ([]any, string) {
	switch method {
	case "coordinator":
		return []any{self.coordinator}, ""
	case "netuid":
		return []any{uint16(25)}, ""
	case "escrowHotkey":
		return []any{[32]byte{0xe5}}, ""
	case "totalCaptured":
		return []any{new(big.Int).Set(self.captured)}, ""
	case "totalPaid":
		return []any{new(big.Int).Set(self.paid)}, ""
	case "pendingFunding":
		return []any{new(big.Int).Set(self.pending)}, ""
	case "outstandingLiability":
		return []any{new(big.Int).Set(self.outstanding)}, ""
	case "escrowAccounted":
		return []any{new(big.Int).Set(self.accounted)}, ""
	case "liveEscrowStake":
		return []any{new(big.Int).Set(self.escrowStake)}, ""
	case "conservationHolds":
		if self.holds != nil {
			return []any{*self.holds}, ""
		}
		holds := self.captured.Cmp(new(big.Int).Add(self.paid, self.accounted)) == 0 && self.accounted.Cmp(new(big.Int).Add(self.pending, self.outstanding)) == 0 && self.escrowStake.Cmp(self.accounted) >= 0
		return []any{holds}, ""
	case "minimumClaimTTLBlocks":
		return []any{self.claimTtl}, ""
	case "minimumTransferTaoRao":
		return []any{self.transferFlr}, ""
	case "entitlement":
		key := subnetSettlementKey{epoch: args[0].(*big.Int).Uint64(), noId: args[1].(*big.Int).Uint64()}
		record, ok := self.entitlements[key]
		if !ok {
			return []any{stabi.STSettlementVaultEntitlement{Funded: new(big.Int), Total: new(big.Int), Claimed: new(big.Int)}}, ""
		}
		return []any{*record}, ""
	case "carry":
		carry, ok := self.carry[args[0].(*big.Int).Uint64()]
		if !ok {
			carry = new(big.Int)
		}
		return []any{new(big.Int).Set(carry)}, ""
	case "claimCredit":
		credit, ok := self.credit[args[0].([32]byte)]
		if !ok {
			credit = new(big.Int)
		}
		return []any{new(big.Int).Set(credit)}, ""
	}
	self.t.Fatalf("unexpected vault view %s", method)
	return nil, ""
}

// Serves the chain's artifacts through the exact public artifact route.
func (self *subnetSettlementTestChain) apiHandler(t *testing.T) func(*http.Request) (*http.Response, error) {
	return func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/sn/artifact" || request.Header.Get("Authorization") != "" {
			t.Fatal("request departed from the public artifact contract")
		}
		hash := strings.TrimPrefix(request.URL.Query().Get("hash"), "sha256:")
		decoded, err := hex.DecodeString(hash)
		var key [32]byte
		if err != nil || len(decoded) != 32 {
			return subnetReplayTestHttpResponse(request, http.StatusBadRequest, nil, ""), nil
		}
		copy(key[:], decoded)
		raw, ok := self.artifacts[key]
		if !ok {
			return subnetReplayTestHttpResponse(request, http.StatusNotFound, []byte("not found"), ""), nil
		}
		return subnetReplayTestHttpResponse(request, http.StatusOK, raw, `"sha256:`+hash+`"`), nil
	}
}

func subnetReplayTestHttpResponse(request *http.Request, status int, body []byte, etag string) *http.Response {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if etag != "" {
		header.Set("ETag", etag)
	}
	return &http.Response{StatusCode: status, Header: header, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body)), Request: request}
}

// A ready signal environment over the healthy history.
func newSubnetSettlementFixture(t *testing.T) (*subnetSettlementTestChain, *subnetReplayTestNode, SignalSettings, *subnetReplayTestSource) {
	t.Helper()
	chain := newHealthySubnetSettlementChain(t)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, chain.finalizedBlock())
	chain.install(node)
	settings, source := subnetReplayTestSettings(t, node)
	settings.SubnetReplay.ReplayFromBlock = subnetSettlementTestDeployBlock
	source.api = chain.apiHandler(t)
	return chain, node, settings, source
}
