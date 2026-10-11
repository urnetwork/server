// Two real operator owners retain exact Sql/blob signatures through a populated
// migration, policy activation and restart. Only raw chain responses are faked.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urfoundation/sn/validator"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/startifact"
)

type stPayoutPolicyRpcFixture struct {
	stateLock sync.Mutex
	base      stClientKeyHistoryRPCFixture
	head      uint64
	fault     string
	starts    int
}

func stPayoutPolicyBoundary(block uint64) protocol.ClientKeyEffectiveBoundary {
	epoch := uint64(0)
	if block >= 200 {
		epoch = 1 + (block-200)/50
	}
	return protocol.ClientKeyEffectiveBoundary{Block: block, Hash: [32]byte(stPayoutPolicyHeader(block).Hash()), Epoch: epoch}
}

func stPayoutPolicyTime(block uint64) time.Time {
	return time.Unix(1_700_000_000+int64(block)*12, 0).UTC()
}

// The original Frontier commitment uses milliseconds while its public JSON
// clock renders seconds. Every boundary uses this same recoverable RLP15 hash.
func stPayoutPolicyHeader(block uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(block), Time: uint64(stPayoutPolicyTime(block).UnixMilli()), Difficulty: big.NewInt(0), GasLimit: 1, Extra: []byte{7}}
}

func stPayoutPolicySnapshot(epoch uint64) stabi.STCoordinatorPolicySnapshot {
	policy := stabi.STCoordinatorPolicySnapshot{PolicyHash: [32]byte{4}, EffectiveBlock: 100, EpochBlocks: 100, EpochDepositCapRao: big.NewInt(1000), CampaignDepositCapRao: big.NewInt(10000)}
	if epoch > 0 {
		policy.PolicyHash, policy.EffectiveEpoch, policy.EffectiveBlock, policy.EpochBlocks = [32]byte{9}, 1, 200, 50
	}
	return policy
}

func (self *stPayoutPolicyRpcFixture) configure(head uint64, fault string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.head, self.fault, self.starts = head, fault, 0
}

func (self *stPayoutPolicyRpcFixture) snapshot() (stClientKeyHistoryRPCFixture, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	base := self.base
	base.boundary = stPayoutPolicyBoundary(self.head)
	base.domain.PolicyHash = stPayoutPolicySnapshot(base.boundary.Epoch).PolicyHash
	base.fault = self.fault
	return base, self.fault
}

func (self *stPayoutPolicyRpcFixture) ChainId(ctx context.Context) (hexutil.Uint64, error) {
	base, _ := self.snapshot()
	return base.ChainId(ctx)
}

func (self *stPayoutPolicyRpcFixture) GetBlockHash(ctx context.Context, block uint64) (*common.Hash, error) {
	base, _ := self.snapshot()
	return base.GetBlockHash(ctx, block)
}

func (self *stPayoutPolicyRpcFixture) GetBlockByNumber(ctx context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	base, fault := self.snapshot()
	if full || tag == 0 {
		return nil, nil
	}
	block := base.boundary.Block
	if tag != rpc.FinalizedBlockNumber {
		if tag < 100 || uint64(tag) > block {
			return nil, errors.New("unknown retained block")
		}
		block = uint64(tag)
	}
	boundary := stPayoutPolicyBoundary(block)
	if block == 100 {
		self.stateLock.Lock()
		self.starts++
		changed := fault == "changed-window" && self.starts >= 2
		self.stateLock.Unlock()
		if changed {
			boundary.Hash[1]++
		}
		if fault == "missing-window" {
			return nil, nil
		}
	}
	stamp := hexutil.EncodeUint64(uint64(stPayoutPolicyTime(block).Unix()))
	if fault == "missing-time" && block == 100 {
		return map[string]any{"number": hexutil.EncodeUint64(block), "hash": common.Hash(boundary.Hash)}, nil
	}
	raw, err := json.Marshal(stPayoutPolicyHeader(block))
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["hash"], fields["timestamp"] = common.Hash(boundary.Hash), stamp
	return fields, nil
}

func (self *stPayoutPolicyRpcFixture) Call(ctx context.Context, call map[string]hexutil.Bytes, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	base, fault := self.snapshot()
	data := call["data"]
	if len(data) == 0 {
		data = call["input"]
	}
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, err
	}
	method, err := parsed.MethodById(data)
	if err == nil && (method.Name == "cumulativeConviction" || method.Name == "epochDeposits" || method.Name == "epochConvictionAdded") {
		if common.BytesToAddress(call["to"]) != base.domain.Coordinator || selector.BlockNumber == nil || uint64(*selector.BlockNumber) != base.boundary.Block {
			return nil, errors.New("wrong conviction selector")
		}
		value := big.NewInt(0)
		if method.Name == "cumulativeConviction" {
			value.SetUint64(25)
		}
		return method.Outputs.Pack(value)
	}
	if selector.BlockHash == nil || selector.BlockNumber != nil || !selector.RequireCanonical {
		return nil, errors.New("policy fixture requires a canonical hash")
	}
	found := false
	for block := uint64(100); block <= base.boundary.Block; block++ {
		boundary := stPayoutPolicyBoundary(block)
		if common.Hash(boundary.Hash) == *selector.BlockHash {
			base.boundary = boundary
			base.domain.PolicyHash = stPayoutPolicySnapshot(boundary.Epoch).PolicyHash
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("policy fixture hash is unknown")
	}
	if err == nil && common.BytesToAddress(call["to"]) == base.domain.Coordinator && (method.Name == "policyAt" || method.Name == "epochStartBlock" || method.Name == "epochEndBlock") {
		args, err := method.Inputs.Unpack(data[4:])
		if err != nil || len(args) != 1 {
			return nil, errors.New("invalid policy epoch query")
		}
		epoch := args[0].(*big.Int).Uint64()
		if epoch > base.boundary.Epoch {
			return nil, errors.New("future policy query")
		}
		policy := stPayoutPolicySnapshot(epoch)
		if epoch == 0 && fault == "missing-history" {
			return nil, errors.New("retained policy unavailable")
		}
		if fault == "policy" || epoch == 0 && fault == "foreign-history" {
			policy.PolicyHash[1]++
		}
		if epoch == 0 && fault == "zero-history" {
			policy.PolicyHash = [32]byte{}
		}
		if epoch == 0 && fault == "future-history" {
			policy.EffectiveEpoch = 1
		}
		if method.Name == "policyAt" {
			encoded, err := method.Outputs.Pack(policy)
			if epoch == 0 && fault == "trailing-history" {
				encoded = append(encoded, make([]byte, 32)...)
			}
			return encoded, err
		}
		block := policy.EffectiveBlock + (epoch-policy.EffectiveEpoch)*policy.EpochBlocks
		if method.Name == "epochEndBlock" {
			block += policy.EpochBlocks
		}
		if epoch == 0 && fault == "wrong-window" {
			block++
		}
		return method.Outputs.Pack(new(big.Int).SetUint64(block))
	}
	return base.Call(ctx, call, selector)
}

// Each restart makes a fresh concrete connection owner and retains only the
// current configuration; no retained local policy is passed to production.
func newStPayoutPolicyFixture(t testing.TB, noId uint64) (*stPayoutPolicyRpcFixture, *session.ByJwt, *StConfig, func() *CoreStClient) {
	t.Helper()
	base, credential, original := newStClientKeyHistoryControllerFixture(t)
	cfg := *original
	cfg.NoId, cfg.PolicyHash = noId, [32]byte{9}
	var err error
	cfg.RootKey, err = crypto.HexToECDSA(strings.Repeat(fmt.Sprintf("%02x", noId+20), 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ArtifactKey, err = crypto.HexToECDSA(strings.Repeat(fmt.Sprintf("%02x", noId+30), 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DepositEpochCapRao = 1000
	cfg.ReliabilityAMin = 8
	cfg.DepositTiers = []StDepositTier{{RateNumerator: 10, UserRateNumerator: 3, RateDenominator: 1}}
	base.domain.NoID, base.domain.PolicyHash, base.root = noId, cfg.PolicyHash, crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
	fixture := &stPayoutPolicyRpcFixture{base: *base, head: 225}
	endpoint := rpc.NewServer()
	if err := errors.Join(endpoint.RegisterName("eth", fixture), endpoint.RegisterName("chain", fixture)); err != nil {
		t.Fatal(err)
	}
	httpEndpoint := httptest.NewServer(endpoint)
	t.Cleanup(func() { httpEndpoint.Close(); endpoint.Stop() })
	cfg.RpcUrls = []string{httpEndpoint.URL}
	restart := func() *CoreStClient {
		owned := cfg
		client := &CoreStClient{cfg: &owned, coordinator: stabi.NewSTCoordinator(), clients: map[string]*ethclient.Client{}, clientKeyRegistrations: newStClientKeyRegistrationCohorts(0)}
		SetStConfig(&owned)
		SetStClient(client)
		t.Cleanup(func() {
			for _, connection := range client.clients {
				connection.Close()
			}
		})
		return client
	}
	return fixture, credential, &cfg, restart
}

// No SDK owner has enrolled or produced work in this fixture. An independent
// signed empty roster proves that fact; an empty SQL query alone cannot. Epoch
// and clock still come through the actual policy RPC reader used by payout.
func stPayoutPolicyEmptyWorkAuthority(t testing.TB, cfg *StConfig, client *CoreStClient, epoch uint64) (payoutartifact.WholeWorkAuthority, [32]byte) {
	t.Helper()
	boundary, err := client.PayoutEpochAuthority(t.Context(), epoch)
	if err != nil || boundary == nil || len(boundary.StartHeader) == 0 || len(boundary.EndHeader) == 0 || boundary.ClockProfile != payoutartifact.FrontierWindowClockProfile {
		t.Fatal("fresh payout fixture lacks original authenticated Frontier clock", err)
	}
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: boundary.PolicyHash, NoID: cfg.NoId}
	domainHash, err := domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	approver := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{83}, ed25519.SeedSize))
	publicKey := [32]byte(approver.Public().(ed25519.PublicKey))
	root := crypto.PubkeyToAddress(cfg.RootKey.PublicKey)
	t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\n", ProviderWorkPolicySchema, domainHash, publicKey, root.Hex(), root.Hex()))))
	authority := payoutartifact.WholeWorkAuthority{Domain: domain, Epoch: epoch, Start: payoutartifact.Boundary{Number: boundary.Start.Block, Hash: common.Hash(boundary.Start.Hash).Hex()}, End: payoutartifact.Boundary{Number: boundary.End.Block, Hash: common.Hash(boundary.End.Hash).Hex()}, RequestPublicKey: publicKey, ClockProfile: boundary.ClockProfile, Owners: []payoutartifact.WholeWorkOwner{}, ExpectedProviders: []payoutartifact.WholeWorkExpectedProvider{}, PriorContracts: []payoutartifact.WholeWorkPriorContract{}}
	authority, err = payoutartifact.SignWholeWorkAuthority(t.Context(), authority, cfg.RootKey)
	if err != nil {
		t.Fatal(err)
	}
	return authority, domainHash
}

// Exact original payouts are created before rollover and never rewritten by
// consumption. Both operators can have usage without a paid transfer balance.
func stRetainPolicyPayout(t testing.TB, cfg *StConfig) ([]byte, *model.StPayoutArtifact) {
	t.Helper()
	artifact, err := startifact.Build(startifact.BuildInput{
		DeploymentID: cfg.DeploymentId, GenesisHash: fmt.Sprintf("0x%x", cfg.GenesisHash), PolicyHash: fmt.Sprintf("0x%x", stPayoutPolicySnapshot(0).PolicyHash),
		ChainID: cfg.ChainId, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, Epoch: 0, NoID: cfg.NoId,
		Start: startifact.Boundary{Number: 100, Hash: common.Hash(stPayoutPolicyBoundary(100).Hash).Hex()}, End: startifact.Boundary{Number: 200, Hash: common.Hash(stPayoutPolicyBoundary(200).Hash).Hex()},
		OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32),
		Providers: []startifact.ProviderInput{{ClientID: [16]byte{byte(cfg.NoId)}, Coldkey: [32]byte{byte(cfg.NoId)}, UsageBytes: 2 * 1024 * 1024 * 1024, Assignments: 8, Confirmations: 8, Eligible: true}}, TotalUsers: 3, ReliabilityAMin: 8, CreatedAt: stPayoutPolicyTime(200),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := startifact.Sign(artifact, cfg.ArtifactKey); err != nil {
		t.Fatal(err)
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("private payout store missing")
	}
	published, err := startifact.Publish(t.Context(), store, artifact)
	if err != nil {
		t.Fatal(err)
	}
	record := &model.StPayoutArtifact{Epoch: 0, NoId: cfg.NoId, ContentHash: published.ContentHash, ContentKey: published.ContentKey, HistoryKey: published.HistoryKey, PayoutRoot: artifact.PayoutRoot, CreateTime: stPayoutPolicyTime(200)}
	model.AddStPayoutArtifact(t.Context(), cfg.DeploymentKey(), record)
	_, encoded, err := startifact.Read(t.Context(), store, record.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, record
}

// This is the pre-fix causal failure: a valid prior-policy payout was rejected
// against the new config hash (and its old window guessed with the new length).
func TestStPayoutPolicyRolloverTwoOperatorsRetainSizingAfterMigrationAndRestart(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		server.ApplyDbMigrationsUpTo(ctx, 743)
		type operator struct {
			fixture      *stPayoutPolicyRpcFixture
			credential   *session.ByJwt
			cfg          *StConfig
			restart      func() *CoreStClient
			registration protocol.ClientKeyRegistration
			evidence     []byte
			payout       []byte
			record       *model.StPayoutArtifact
		}
		operators := make([]operator, 2)
		for index := range operators {
			value := &operators[index]
			value.fixture, value.credential, value.cfg, value.restart = newStPayoutPolicyFixture(tb, uint64(index+1))
			domain := value.fixture.base.domain
			domain.PolicyHash = [32]byte{4}
			seed := sha256.Sum256(value.credential.ClientId[:])
			key := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
			registration := protocol.ClientKeyRegistration{Domain: domain, ClientID: [16]byte(*value.credential.ClientId), NetworkID: [16]byte(value.credential.NetworkId), Generation: 1, Present: true, PublicKey: [32]byte(key), EffectiveBoundary: stPayoutPolicyBoundary(150)}
			if err := protocol.SignClientKeyRegistration(&registration, value.cfg.RootKey); err != nil {
				tb.Fatal(err)
			}
			encoded, err := registration.Bytes()
			if err != nil {
				tb.Fatal(err)
			}
			evidence, evidenceHash, err := startifact.SealClientKeyRegistrationEvidence(value.cfg.DeploymentId, registration, value.cfg.ArtifactKey, stPayoutPolicyTime(150))
			if err != nil {
				tb.Fatal(err)
			}
			domainHash, err := domain.Digest()
			if err != nil {
				tb.Fatal(err)
			}
			registrationHash := sha256.Sum256(encoded)
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, registration, evidence_hash, evidence) VALUES ($1, 1, $2, $3, $4, $5, $6)`, *value.credential.ClientId, domainHash[:], registrationHash[:], encoded, evidenceHash, evidence))
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, 1)`, *value.credential.ClientId, domainHash[:], value.credential.NetworkId))
			})
			value.registration, value.evidence = registration, bytes.Clone(evidence)
		}
		server.ApplyDbMigrations(ctx)
		for index := range operators {
			value := &operators[index]
			value.payout, value.record = stRetainPolicyPayout(tb, value.cfg)
			client := value.restart()
			authority, err := client.PayoutEpochAuthority(ctx, 0)
			if err != nil || authority.Start != stPayoutPolicyBoundary(100) || authority.End != stPayoutPolicyBoundary(200) || authority.PolicyHash != value.registration.Domain.PolicyHash {
				tb.Fatal("retained epoch authority changed its inclusive/exclusive policy boundaries", authority, err)
			}
			proofEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				router.WrapWithInputRequireClient(SnClientKeyObservation, w, r)
			}))
			tb.Cleanup(proofEndpoint.Close)
			reader, err := validator.NewHTTPClientKeyHistoryReader(proofEndpoint.URL, func() string { return value.credential.Testing_Sign() })
			if err != nil {
				tb.Fatal(err)
			}
			request := protocol.ClientKeyObservationRequest{ClientID: [16]byte(*value.credential.ClientId), ValidatorHotkey: [32]byte{10}, NativeBlock: 226, NativeHash: [32]byte{11}, NativeEpoch: 1, DecisionBoundary: stPayoutPolicyBoundary(225), Nonce: [32]byte{12}}
			if _, err := reader.Read(ctx, request, protocol.MaxClientKeyHistoryResponseBytes); err == nil {
				tb.Fatal("old-only policy history advertised current readiness")
			}
			// Paid sizing consumes all completed bytes; the model has no paid
			// transfer escrow for this fixture. Current rates price old usage.
			sizing, failed, err := stEpochDepositSizing(ctx, value.cfg, client, &StEpochState{TEpochBlocks: 50}, 1)
			if err != nil || failed != nil || sizing.RequiredRao.Cmp(big.NewInt(29)) != 0 || sizing.UsageBytes != 2*1024*1024*1024 || sizing.Users != 3 {
				tb.Fatalf("operator %d retained old-policy sizing: %+v, %v, %v", value.cfg.NoId, sizing, failed, err)
			}
			funding, err := stDepositFundingAmount(sizing.RequiredRao, big.NewInt(0))
			if err != nil || funding.Cmp(big.NewInt(32)) != 0 {
				tb.Fatal("reserve sizing lost its exact allowance", funding, err)
			}
			// Actual processed control appends generation one in the new
			// namespace. A fresh owner must then serve a fresh signed proof.
			endpoint := stClientKeyRegistrationConnectEndpoint(tb)
			seed := sha256.Sum256(value.credential.ClientId[:])
			key := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
			if err := stClientKeyRegistrationRequest(ctx, endpoint.URL, value.credential.Testing_Sign(), key); err != nil {
				tb.Fatal(err)
			}
			client = value.restart()
			// One registered member cannot make a partial operator cohort
			// ready. The other retained client belongs to a different operator.
			foreign := request
			foreign.ClientID = [16]byte(*operators[1-index].credential.ClientId)
			foreign.Nonce[1]++
			batch := &protocol.ClientKeyObservationBatchRequest{Requests: []protocol.ClientKeyObservationRequest{request, foreign}, MaximumResponseBytes: protocol.MaxClientKeyObservationBatchResponseBytes}
			if bytes.Compare(batch.Requests[0].ClientID[:], batch.Requests[1].ClientID[:]) > 0 {
				batch.Requests[0], batch.Requests[1] = batch.Requests[1], batch.Requests[0]
			}
			clientSession := session.Testing_CreateClientSession(ctx, value.credential)
			partial, batchErr := SnClientKeyObservations(batch, clientSession)
			clientSession.Cancel()
			if partial != nil || batchErr == nil || !strings.Contains(batchErr.Error(), "complete durable history") {
				tb.Fatal("partial/foreign policy cohort supplied a readiness prefix", partial, batchErr)
			}
			current, err := model.GetClientPublicKey(ctx, *value.credential.ClientId)
			if err != nil || !bytes.Equal(current, key) {
				tb.Fatal("restart lost processed current key", err)
			}
			proofBytes, err := reader.Read(ctx, request, protocol.MaxClientKeyHistoryResponseBytes)
			if err != nil {
				tb.Fatal("restart stopped fresh validator proof progress", err)
			}
			response, err := protocol.DecodeClientKeyHistoryResponse(proofBytes, protocol.MaxClientKeyHistoryResponseBytes)
			if err != nil || len(response.History) != 1 {
				tb.Fatal("new policy proof history differs", err)
			}
			domain := value.fixture.base.domain
			wrapper, err := protocol.DecodeClientKeyEvidence(response.History[0], domain, protocol.ClientKeyRegistrationEvidenceKind)
			if err != nil {
				tb.Fatal(err)
			}
			registration, err := protocol.DecodeClientKeyRegistration(wrapper.Payload)
			if err != nil || registration.Generation != 1 || registration.PreviousHash != ([32]byte{}) || registration.EffectiveBoundary != stPayoutPolicyBoundary(225) {
				tb.Fatal("new namespace was not authenticated generation one", err)
			}
			wrapper, err = protocol.DecodeClientKeyEvidence(response.Observation, domain, protocol.ClientKeyObservationEvidenceKind)
			if err != nil {
				tb.Fatal(err)
			}
			observation, err := protocol.DecodeClientKeyObservation(wrapper.Payload)
			if err != nil || observation.VerifyRegistration(registration, domain, request, value.fixture.base.root, value.fixture.base.root) != nil {
				tb.Fatal("fresh validator proof is not independently authentic", err)
			}
			stale := request
			stale.DecisionBoundary = stPayoutPolicyBoundary(150)
			if _, err := reader.Read(ctx, stale, protocol.MaxClientKeyHistoryResponseBytes); err == nil {
				tb.Fatal("current policy signed a stale-policy decision")
			}
			old, err := model.LoadStClientKeyHistory(ctx, value.registration.Domain, *value.credential.ClientId, 2, model.MaxStClientKeyHistoryBytes)
			if err != nil || len(old) != 1 || !bytes.Equal(old[0].EvidenceBytes, value.evidence) {
				tb.Fatal("populated migration/rollover rewrote signed history", err)
			}
			store, _ := server.LoadBlobStore()
			_, encoded, err := startifact.Read(ctx, store, value.record.ContentHash)
			if err != nil || !bytes.Equal(encoded, value.payout) {
				tb.Fatal("sizing rewrote retained payout bytes", err)
			}
			// A free schedule reports the same authentic totals, owes zero,
			// and remains zero if history is temporarily unavailable.
			free := *value.cfg
			free.DepositTiers = []StDepositTier{{RateDenominator: 1}}
			zero, failed, err := stEpochDepositSizing(ctx, &free, client, nil, 1)
			if err != nil || failed != nil || zero.RequiredRao.Sign() != 0 || zero.UsageBytes != sizing.UsageBytes || zero.Users != sizing.Users {
				tb.Fatal("free traffic totals differ", zero, failed, err)
			}
			value.fixture.configure(225, "missing-history")
			zero, failed, err = stEpochDepositSizing(ctx, &free, client, nil, 1)
			if err != nil || failed != nil || zero.RequiredRao.Sign() != 0 || zero.Note == "" {
				tb.Fatal("unavailable informational history made free traffic payable", zero, failed, err)
			}
			value.fixture.configure(225, "")
			if err := stClientKeyRegistrationRequest(ctx, endpoint.URL, value.credential.Testing_Sign(), nil); err != nil {
				tb.Fatal(err)
			}
			value.restart()
			current, err = model.GetClientPublicKey(ctx, *value.credential.ClientId)
			if err != nil || len(current) != 0 {
				tb.Fatal("restart resurrected a tombstoned key", err)
			}
			request.Nonce[1]++
			proofBytes, err = reader.Read(ctx, request, protocol.MaxClientKeyHistoryResponseBytes)
			if err != nil {
				tb.Fatal(err)
			}
			response, err = protocol.DecodeClientKeyHistoryResponse(proofBytes, protocol.MaxClientKeyHistoryResponseBytes)
			if err != nil || len(response.History) != 2 {
				tb.Fatal("tombstone history lost a generation", err)
			}
			wrapper, err = protocol.DecodeClientKeyEvidence(response.Observation, domain, protocol.ClientKeyObservationEvidenceKind)
			if err != nil {
				tb.Fatal(err)
			}
			observation, err = protocol.DecodeClientKeyObservation(wrapper.Payload)
			if err != nil {
				tb.Fatal(err)
			}
			wrapper, err = protocol.DecodeClientKeyEvidence(response.History[1], domain, protocol.ClientKeyRegistrationEvidenceKind)
			if err != nil {
				tb.Fatal(err)
			}
			registration, err = protocol.DecodeClientKeyRegistration(wrapper.Payload)
			if err != nil || registration.Present || observation.VerifyRegistration(registration, domain, request, value.fixture.base.root, value.fixture.base.root) != nil {
				tb.Fatal("tombstone proof differs from exact signed absence", err)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, "DELETE FROM network_client WHERE client_id=$1", *value.credential.ClientId))
			})
			value.restart()
			if _, err := reader.Read(ctx, request, protocol.MaxClientKeyHistoryResponseBytes); err == nil {
				tb.Fatal("deleted client regained proof readiness")
			}
		}
	})
}

// Integrity failures cannot downgrade to config, mirror rows, or artifact
// claims. Restoring the exact external authority repairs the same operation.
func TestStPayoutPolicyRolloverRejectsMissingCorruptAndForeignAuthority(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, _, cfg, restart := newStPayoutPolicyFixture(tb, 2)
		stRetainPolicyPayout(tb, cfg)
		client := restart()
		for _, fault := range []string{"missing-history", "foreign-history", "zero-history", "future-history", "trailing-history", "wrong-window", "missing-window", "changed-window", "missing-time", "native-wrong", "companion", "root", "policy"} {
			fixture.configure(225, fault)
			sizing, failed, err := stEpochDepositSizing(tb.Context(), cfg, client, nil, 1)
			if err != nil || sizing != nil || failed == nil || !failed.Retry {
				tb.Fatalf("%s admitted historical usage: %+v %v %v", fault, sizing, failed, err)
			}
		}
		fixture.configure(225, "")
		sizing, failed, err := stEpochDepositSizing(tb.Context(), cfg, client, nil, 1)
		if err != nil || failed != nil || sizing.RequiredRao.Cmp(big.NewInt(29)) != 0 {
			tb.Fatal("restored authority did not recover", sizing, failed, err)
		}
	})
}

// Fresh construction must prove its actual policy/window even with no paid
// usage or leaves; the successor policy cannot be backdated into epoch zero.
func TestStPayoutPolicyRolloverFreshIssuanceRejectsBackdating(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		controllerPayoutSchedule(tb, stPayoutPolicyTime(100))
		fixture, _, cfg, restart := newStPayoutPolicyFixture(tb, 1)
		client := restart()
		for _, fault := range []string{"", "missing-history", "foreign-history"} {
			fixture.configure(225, fault)
			if _, _, err := stComputeReleasePayout(tb.Context(), cfg, client, 0, stPayoutPolicyTime(100), stPayoutPolicyTime(200), 100, 200, nil); err == nil {
				tb.Fatalf("%s backdated current policy", fault)
			}
			if model.GetStPayoutArtifact(tb.Context(), cfg.DeploymentKey(), 0, cfg.NoId) != nil {
				tb.Fatal("failed authority left a published artifact")
			}
		}
		fixture.configure(275, "")
		workAuthority, domainHash := stPayoutPolicyEmptyWorkAuthority(tb, cfg, client, 1)
		if _, _, err := stComputeReleasePayout(tb.Context(), cfg, client, 1, stPayoutPolicyTime(200), stPayoutPolicyTime(250), 200, 250, nil); !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			tb.Fatal("fresh epoch without independently approved provider authority escaped pending", err)
		}
		if model.GetStPayoutArtifact(tb.Context(), cfg.DeploymentKey(), 1, cfg.NoId) != nil {
			tb.Fatal("missing provider authority published a fresh artifact")
		}
		authorityRaw, err := workAuthority.Bytes(tb.Context())
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := model.RetainProviderWorkAuthority(tb.Context(), authorityRaw, domainHash, workAuthority.RequestPublicKey, crypto.PubkeyToAddress(cfg.RootKey.PublicKey)); err != nil {
			tb.Fatal(err)
		}
		if _, _, err := stComputeReleasePayout(tb.Context(), cfg, client, 1, stPayoutPolicyTime(200), stPayoutPolicyTime(250), 200, 250, nil); err != nil {
			tb.Fatal("fresh successor-policy epoch failed", err)
		}
		prior := model.GetStPayoutArtifact(tb.Context(), cfg.DeploymentKey(), 1, cfg.NoId)
		if prior == nil {
			tb.Fatal("fresh epoch artifact missing")
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			tb.Fatal("fresh payout store is unavailable")
		}
		artifact, raw, err := startifact.Read(tb.Context(), store, prior.ContentHash)
		if err != nil || artifact == nil || artifact.ClosedWork == nil || artifact.ClosedWork.WholeInventory == nil || artifact.ClosedWork.WholeInventory.Clock == nil || artifact.ClosedWork.WholeInventory.Clock.HeaderProfile != payoutartifact.FrontierWindowClockProfile || len(artifact.Providers) != 0 {
			tb.Fatal("fresh epoch omitted its independently known-empty original work", err)
		}
		boundary, err := client.PayoutEpochAuthority(tb.Context(), 1)
		if err != nil {
			tb.Fatal(err)
		}
		approved, _, err := stLoadProviderWorkAuthority(tb.Context(), cfg, boundary)
		if err != nil || approved == nil {
			tb.Fatal("fresh provider authority is unavailable", err)
		}
		verified, err := payoutartifact.VerifyWholeWorkInventory(tb.Context(), artifact, approved.Expectation)
		if err != nil || verified == nil || !verified.Complete || verified.Contracts != 0 || len(verified.ExpectedProviders) != 0 {
			tb.Fatal("original empty window failed independent verification", verified, err)
		}
		fixture.configure(275, "missing-history")
		if _, _, err := stComputeReleasePayout(tb.Context(), cfg, client, 1, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			tb.Fatal("immutable retry demanded current authority", err)
		}
		_, again, err := startifact.Read(tb.Context(), store, prior.ContentHash)
		if err != nil || !bytes.Equal(raw, again) {
			tb.Fatal("fresh original authority was reinterpreted on immutable retry", err)
		}
	})
}

// Existing signed artifacts are consumed without a backfill or re-signature.
// This retained-only test stays portable; it requires no new SDK custody.
func TestStClosedWorkLegacyPublishedArtifactRemainsExactOnRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, _, cfg, restart := newStPayoutPolicyFixture(t, 1)
		original, record := stRetainPolicyPayout(t, cfg)
		fixture.configure(275, "missing-history")
		if _, _, err := stComputeReleasePayout(t.Context(), cfg, restart(), 0, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			t.Fatal("legacy original retry demanded component backfill", err)
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			t.Fatal("original store is absent")
		}
		artifact, again, err := startifact.Read(t.Context(), store, record.ContentHash)
		if err != nil || artifact.ClosedWork != nil || !bytes.Equal(original, again) {
			t.Fatal("legacy published bytes changed on rolling retry", err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(again, &wire); err != nil || wire["original_closed_work"] != nil {
			t.Fatal("legacy original acquired a serialized evidence claim", err)
		}
	})
}
