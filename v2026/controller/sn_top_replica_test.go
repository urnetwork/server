package controller

// Reader regressions use signed synthetic summaries and real immutable blob
// storage. No database, network RPC or production credentials are required.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/protocol/headsummary"
	"github.com/urfoundation/sn/v2026/validator"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

type snTopReplicaTestFixture struct {
	cfg      *StConfig
	store    server.BlobStore
	settings snTopReplicaSettings
	key      *snHeadSummaryTestKey
	vpk      ed25519.PrivateKey
	cut      validator.AttemptCutV2
	summary  *headsummary.Summary
	stored   *model.SnHeadSummary
}

// A genuine signed empty cut retains one live historical fleet identity.
func newSnTopReplicaTestFixture(t *testing.T) *snTopReplicaTestFixture {
	t.Helper()
	policy, err := protocol.LoadPolicy("../../sn/deploy/testnet/policy-v1.yml")
	if err != nil {
		t.Fatal(err)
	}
	policyHash, err := policy.Hash()
	if err != nil {
		t.Fatal(err)
	}
	key := newSnHeadSummaryTestKey(t, [32]byte{0x31, 0x41})
	settings := defaultSnTopReplicaSettings()
	settings.Replay.MaxScratchBytes = 16 * 1024 * 1024
	vpk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x51}, ed25519.SeedSize))
	cfg := &StConfig{ChainId: 945, GenesisHash: [32]byte{0x11}, DeploymentId: "head-replica-fixture", PolicyHash: policyHash,
		ContractAddress: common.Address{0x21}, SettlementVault: common.Address{0x22}, Netuid: 25, NoId: 9, ValidatorHotkeys: [][32]byte{key.hotkey}}
	zero := headsummary.Hex32([32]byte{})
	cut := validator.AttemptCutV2{Schema: validator.AttemptCutV2Schema, Context: validator.AttemptCutV2Context{
		Identity:      validator.AttemptLedgerIdentity{DeploymentID: cfg.DeploymentId, ChainID: cfg.ChainId, GenesisHash: headsummary.Hex32(cfg.GenesisHash), Netuid: uint16(cfg.Netuid), ValidatorID: 1, ValidatorUID: 1, NoID: cfg.NoId, ValidatorVPK: headsummary.Hex32([32]byte(vpk.Public().(ed25519.PublicKey)))},
		Activation:    validator.AttemptCutV2Activation{Domain: protocol.ValidatorEvidenceDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: [20]byte(cfg.ContractAddress), SettlementVault: [20]byte(cfg.SettlementVault), DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: policyHash, ActivationEpoch: 42, ActivationHash: [32]byte{0x23}}, Hotkey: key.hotkey, FirstSequence: 1, PriorRoot: zero},
		Boundary:      validator.AttemptBoundary{SettlementEpoch: 42, EVMBlock: 1000, EVMBlockHash: headsummary.Hex32([32]byte{0x24})},
		FirstSequence: 1, EgressFirstSequence: 1, EgressGeneration: 1, PriorRoot: zero,
	}, Root: zero, Records: validator.AttemptStreamV2Reference{ManifestHash: zero}, Proofs: validator.AttemptStreamV2Reference{ManifestHash: zero}}
	summary := &headsummary.Summary{
		Schema: headsummary.SchemaV2, Netuid: uint16(cfg.Netuid), NativeEpoch: 7, NativeSnapshotBlock: 200, NativeSnapshotHash: headsummary.Hex32([32]byte{0x25}), EVMSnapshotBlock: 1000, EVMSnapshotHash: cut.Context.Boundary.EVMBlockHash, SettlementEpoch: 42,
		PolicyID: policy.PolicyID, PolicyHash: headsummary.Hex32(policyHash), MeasurementArtifactHash: "sha256:" + headsummary.Hex32([32]byte{0x26})[2:],
		EvidenceEmptyPrior: true, EvidenceRecipient: cfg.NoId, EvidenceOperators: []uint64{cfg.NoId}, EvidencePolicy: policy,
		EvidenceCuts:    []headsummary.EvidenceCut{{NoId: cfg.NoId, NativeBlock: 198, NativeBlockHash: headsummary.Hex32([32]byte{0x27}), Bindings: []headsummary.EvidenceBinding{{ClientId: "00000000-0000-0000-0000-000000000031", FleetId: headsummary.Hex32([32]byte{0x32}), Hotkey: headsummary.Hex32([32]byte{0x33}), Generation: 2, Uid: 7}}}},
		ValidatorHotkey: key.hex(), ValidatorUID: 1, MaximumHeadFleets: policy.Steering.MaximumHeadFleets,
		Entries: []headsummary.Entry{}, SelectedUIDs: []uint16{}, RejectedUIDs: []uint16{},
	}
	fixture := &snTopReplicaTestFixture{cfg: cfg, store: server.NewLocalBlobStore(t.TempDir(), "head-fixture"), settings: settings, key: key, vpk: vpk, cut: cut, summary: summary}
	fixture.publish(t)
	return fixture
}

// Publish through the same immutable typed object boundary used by the API.
func (self *snTopReplicaTestFixture) publish(t *testing.T) {
	t.Helper()
	var err error
	self.cut.Signature, err = self.cut.Sign(self.vpk, self.settings.Cut)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := self.cut.CanonicalJSON(self.settings.Cut)
	if err != nil {
		t.Fatal(err)
	}
	hash := headsummary.Hex32(sha256.Sum256(raw))
	if err := startifact.PublishAttemptObject(t.Context(), self.store, startifact.AttemptObjectBounds{MetadataBytes: self.settings.Cut.MaxHeaderBytes, RecordBytes: self.settings.Cut.Records.MaxChunkBytes, ProofBytes: self.settings.Cut.Proofs.MaxChunkBytes}, "metadata", hash, raw); err != nil {
		t.Fatal(err)
	}
	self.summary.EvidenceCuts[0].ContentHash = hash
	self.sign(t)
}

// Keep the model's accepted identity and canonical signed body consistent.
func (self *snTopReplicaTestFixture) sign(t *testing.T) {
	t.Helper()
	body := self.key.sign(t, self.summary)
	var err error
	self.stored, err = snStoredHeadSummary(self.summary, body, snHeadSummaryTestSignedAt)
	if err != nil {
		t.Fatal(err)
	}
}

// Fresh private scratch is owned and retired by each invocation under test.
func (self *snTopReplicaTestFixture) replay(ctx context.Context) (map[model.SnTopFleetKey]map[[32]byte]bool, error) {
	return snTopReplayHeadWithSettings(ctx, self.stored, self.cfg, self.store, map[byte]ed25519.PublicKey{}, self.settings)
}

// The indexed bootstrap marker must come from the signed empty-prior claim,
// rather than merely an absent predecessor on a first migrated intent.
func TestSnTopReplicaStoresAuthenticatedGenesisOnly(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	if !fixture.stored.ReplicaGenesis {
		t.Fatal("signed empty prior lost its indexed genesis marker")
	}
	fixture.summary.EvidenceEmptyPrior = false
	fixture.sign(t)
	if fixture.stored.ReplicaGenesis {
		t.Fatal("first intent without empty-prior evidence became genesis")
	}
}

// Even when delivery moves across native windows, the signed cut and eligible
// generation remain the same. A live identity with no completed hop stays live.
func TestSnTopReplicaUsesSignedCutRegardlessOfDeliveryTime(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	want := map[model.SnTopFleetKey]map[[32]byte]bool{{FleetId: [32]byte{0x32}, Hotkey: [32]byte{0x33}, Generation: 2, Uid: 7}: {}}
	first, err := fixture.replay(t.Context())
	if err != nil || !reflect.DeepEqual(first, want) {
		t.Fatalf("initial signed cut: %v, %v", first, err)
	}
	fixture.stored.ReceivedAt = fixture.stored.ReceivedAt.Add(5 * 24 * time.Hour)
	second, err := fixture.replay(t.Context())
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("delayed delivery moved exact evidence: %v", err)
	}
}

func TestSnTopReplicaRejectsMissingOrChangedObjects(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	key, err := startifact.AttemptObjectKey(fixture.store, "metadata", fixture.summary.EvidenceCuts[0].ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(t.TempDir(), "changed.json")
	if err := os.WriteFile(changed, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Put(t.Context(), key, changed, "application/json"); err != nil {
		t.Fatal(err)
	}
	if prefixes, err := fixture.replay(t.Context()); err == nil || prefixes != nil {
		t.Fatal("corrupt content-addressed header exposed a partial replica")
	}
	fixture.summary.EvidenceCuts[0].ContentHash = headsummary.Hex32([32]byte{0x79})
	fixture.sign(t)
	if prefixes, err := fixture.replay(t.Context()); err == nil || prefixes != nil {
		t.Fatal("missing signed header became an empty tempo")
	}
}

func TestSnTopReplicaRequiresCompleteStreamsAndIndependentBounds(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	fixture.cut.LastSequence, fixture.cut.RecordCount, fixture.cut.FailedCount = 1, 1, 1
	fixture.cut.Root = headsummary.Hex32([32]byte{0x78})
	fixture.cut.Records = validator.AttemptStreamV2Reference{ManifestHash: headsummary.Hex32([32]byte{0x77}), ManifestBytes: 100, ItemCount: 1, DataBytes: 1, ChunkCount: 1, PageCount: 1}
	fixture.publish(t)
	if prefixes, err := fixture.replay(t.Context()); err == nil || prefixes != nil {
		t.Fatal("missing record stream became complete head evidence")
	}
	fixture.settings.Cut.MaxHeaderBytes = 1
	if prefixes, err := fixture.replay(t.Context()); err == nil || prefixes != nil {
		t.Fatal("signed header enlarged the independently configured limit")
	}
}

func TestSnTopReplicaRejectsPartialGlobalEvidenceAndWrongDeployment(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	fixture.summary.EvidenceOperators = []uint64{9, 10}
	fixture.sign(t)
	if prefixes, err := fixture.replay(t.Context()); err == nil || prefixes != nil {
		t.Fatal("one operator's cut was presented as a complete global replica")
	}
	fixture.summary.EvidenceOperators = []uint64{9}
	fixture.sign(t)
	fixture.cfg.DeploymentId = "another-synthetic-deployment"
	if prefixes, err := fixture.replay(t.Context()); err == nil || prefixes != nil {
		t.Fatal("candidate cut selected another deployment")
	}
}

func TestSnTopReplicaCancellationNeverExposesEvidence(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if prefixes, err := fixture.replay(ctx); !errors.Is(err, context.Canceled) || prefixes != nil {
		t.Fatalf("canceled replay retained evidence: %v", err)
	}
}

// Refusal occurs before rate-limit or persistence I/O, so another operator's
// private census cannot enter this operator's accepted summary history.
func TestSnTopReplicaIngestRejectsAnotherRecipient(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	fixture.cfg.NoId++
	if outcome, err := snUploadHeadSummary(t.Context(), fixture.cfg, nil, fixture.stored.Body, snHeadSummaryTestSignedAt); !errors.Is(err, ErrSnHeadSummaryNotAccepted) || outcome != "" {
		t.Fatalf("foreign private evidence entered ingest: %s %v", outcome, err)
	}
}

// The checked-in objects were produced by the validator's genuine M8 engine,
// disk ledger and sealer. Only immutable object storage is local to this test.
func TestSnTopReplicaReplaysCompleteTrailsAtTheHistoricalCut(t *testing.T) {
	type object struct {
		Kind string `json:"kind"`
		Hash string `json:"hash"`
		Data []byte `json:"data"`
	}
	var fixture struct {
		Summary    json.RawMessage            `json:"summary"`
		ServerKeys map[byte]ed25519.PublicKey `json:"server_keys"`
		Objects    []object                   `json:"objects"`
		Prefixes   []struct {
			Key    validator.FleetScoreKey `json:"key"`
			Hashes []string                `json:"hashes"`
		} `json:"prefixes"`
		PreCutPrefix  string `json:"pre_cut_prefix"`
		ReboundPrefix string `json:"rebound_prefix"`
	}
	file, err := os.Open("testdata/sn-top-replica-v2.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = compressed.Close() })
	decoder := json.NewDecoder(io.LimitReader(compressed, 1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	summary, err := headsummary.Decode(fixture.Summary)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := snStoredHeadSummary(summary, fixture.Summary, snHeadSummaryTestSignedAt)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultSnTopReplicaSettings()
	store := server.NewLocalBlobStore(t.TempDir(), "nonempty-head-fixture")
	bounds := startifact.AttemptObjectBounds{MetadataBytes: 2 * 1024 * 1024, RecordBytes: settings.Cut.Records.MaxChunkBytes, ProofBytes: settings.Cut.Proofs.MaxChunkBytes}
	var cut validator.AttemptCutV2
	var proofObject *object
	for index := range fixture.Objects {
		value := &fixture.Objects[index]
		if value.Kind == "metadata" && value.Hash == summary.EvidenceCuts[0].ContentHash {
			if err := json.Unmarshal(value.Data, &cut); err != nil {
				t.Fatal(err)
			}
		}
		if value.Kind == "proofs" {
			proofObject = value
			continue
		}
		if err := startifact.PublishAttemptObject(t.Context(), store, bounds, value.Kind, value.Hash, value.Data); err != nil {
			t.Fatal(err)
		}
	}
	if proofObject == nil || cut.CompleteCount != 2 || cut.RecordCount != 16 || cut.Context.EgressFirstSequence != 9 {
		t.Fatal("fixture no longer contains two complete trails with the first before the cut")
	}
	domain := cut.Context.Activation.Domain
	cfg := &StConfig{ChainId: domain.ChainID, GenesisHash: domain.GenesisHash, DeploymentId: cut.Context.Identity.DeploymentID, PolicyHash: domain.PolicyHash,
		ContractAddress: common.Address(domain.Coordinator), SettlementVault: common.Address(domain.SettlementVault), Netuid: uint64(domain.Netuid), NoId: summary.EvidenceRecipient,
		ValidatorHotkeys: [][32]byte{cut.Context.Activation.Hotkey}}
	replay := func() (map[model.SnTopFleetKey]map[[32]byte]bool, error) {
		return snTopReplayHeadWithSettings(t.Context(), stored, cfg, store, fixture.ServerKeys, settings)
	}
	// The complete signed header and all records are insufficient: a missing
	// terminal proof must not expose even the prefixes already visited.
	if prefixes, err := replay(); err == nil || prefixes != nil {
		t.Fatal("missing terminal proof published partial head evidence")
	}
	if err := startifact.PublishAttemptObject(t.Context(), store, bounds, proofObject.Kind, proofObject.Hash, proofObject.Data); err != nil {
		t.Fatal(err)
	}
	want := map[model.SnTopFleetKey]map[[32]byte]bool{}
	for _, value := range fixture.Prefixes {
		key := model.SnTopFleetKey{FleetId: value.Key.FleetID, Hotkey: value.Key.Hotkey, Generation: value.Key.Generation, Uid: value.Key.UID}
		want[key] = map[[32]byte]bool{}
		for _, hash := range value.Hashes {
			prefix, err := headsummary.ParseHex32(hash)
			if err != nil {
				t.Fatal(err)
			}
			want[key][prefix] = true
		}
	}
	first, err := replay()
	if err != nil || !reflect.DeepEqual(first, want) {
		t.Fatalf("actual record/proof replay changed historical attribution: %v", err)
	}
	oldPrefix, _ := headsummary.ParseHex32(fixture.PreCutPrefix)
	reboundPrefix, _ := headsummary.ParseHex32(fixture.ReboundPrefix)
	credited := 0
	for key, hashes := range first {
		credited += len(hashes)
		if hashes[oldPrefix] || hashes[reboundPrefix] || key.Generation == 2 && len(hashes) != 0 {
			t.Fatal("pre-cut work or a prior generation's prefix entered the replica")
		}
	}
	if credited != 6 || len(first) != 2 {
		t.Fatalf("fixture lost its six credited prefixes and live zero generation: %d/%d", credited, len(first))
	}
	stored.ReceivedAt = stored.ReceivedAt.Add(5 * 24 * time.Hour)
	second, err := replay()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("delayed delivery changed real replay evidence: %v", err)
	}
	// The same signed census under an unknown server key cannot authenticate
	// either the assignments or the terminal proof.
	keys := fixture.ServerKeys
	fixture.ServerKeys = map[byte]ed25519.PublicKey{}
	if prefixes, err := replay(); err == nil || prefixes != nil {
		t.Fatal("unknown server signing key authenticated real trail evidence")
	}
	fixture.ServerKeys = keys
	key, err := startifact.AttemptObjectKey(store, proofObject.Kind, proofObject.Hash)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), proofObject.Data...)
	corrupt[len(corrupt)/2] ^= 1
	path := filepath.Join(t.TempDir(), "corrupt-proof.jsonl")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), key, path, "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	if prefixes, err := replay(); err == nil || prefixes != nil {
		t.Fatal("corrupt terminal proof published partial head evidence")
	}
}
