package controller

// The demand deposit's decisions that need no database: the price in force, the
// funding wiring, the usage statement's bytes and the text a publish row may hold.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/demand"
	"github.com/urfoundation/sn/v2026/demand/bridge"
	"github.com/urfoundation/sn/v2026/demand/pricesheet"
	"github.com/urfoundation/sn/v2026/demand/usage"
	"github.com/urfoundation/sn/v2026/ledger"
	"github.com/urfoundation/sn/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

const (
	stDemandSheetChain  uint64 = 964
	stDemandSheetNetuid uint16 = 25
)

// One signed price at one tier, in rao.
type stDemandSheetPrice struct {
	epoch, gib, user, capRao uint64
}

func stDemandRaoText(rao uint64) string {
	return fmt.Sprintf("%d.%09d", rao/1_000_000_000, rao%1_000_000_000)
}

// stDemandSheetFile is a published sheet whose entries the key signed.
func stDemandSheetFile(t testing.TB, key *ecdsa.PrivateKey, prices ...stDemandSheetPrice) []byte {
	t.Helper()
	var file strings.Builder
	file.WriteString("sn: 25\n")
	if len(prices) != 0 {
		file.WriteString("signed_schedule:\n")
	}
	for _, price := range prices {
		entry := pricesheet.Entry{EffectiveEpoch: price.epoch, EpochCapRao: price.capRao, Tiers: []protocol.DepositTier{{RateNumeratorRaoPerGiB: price.gib, RateNumeratorRaoPerUser: price.user, RateDenominator: 1}}}
		message, err := entry.Message(stDemandSheetChain, stDemandSheetNetuid)
		if err != nil {
			t.Fatal(err)
		}
		digest := crypto.Keccak256(fmt.Appendf(nil, "\x19Ethereum Signed Message:\n%d%s", len(message), message))
		signature, err := crypto.Sign(digest, key)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&file, "  %d:\n    epoch_cap_alpha: %s\n    signature: \"0x%s\"\n    tiers:\n      0:\n        alpha_per_gib: %s\n        alpha_per_user: %s\n", price.epoch, stDemandRaoText(price.capRao), hex.EncodeToString(signature), stDemandRaoText(price.gib), stDemandRaoText(price.user))
	}
	return []byte(file.String())
}

// stDemandMemorySheetStore stands in for the database's row.
type stDemandMemorySheetStore struct {
	raw     []byte
	loadErr error
}

func (self *stDemandMemorySheetStore) Load(context.Context) ([]byte, error) {
	return self.raw, self.loadErr
}

func (self *stDemandMemorySheetStore) Save(_ context.Context, raw []byte) error {
	self.raw = bytes.Clone(raw)
	return nil
}

// stDemandSheetSite is the published address: what it serves, and how often it was
// asked.
type stDemandSheetSite struct {
	body    atomic.Pointer[[]byte]
	fetches atomic.Int64
}

func (self *stDemandSheetSite) serve(body []byte) { self.body.Store(&body) }
func (self *stDemandSheetSite) down()             { self.body.Store(nil) }

// useStDemandSheet pins key as the deployment's signer, at a site of the test's own.
func useStDemandSheet(t testing.TB, key *ecdsa.PrivateKey) (*stDemandSheetSite, *stDemandMemorySheetStore) {
	t.Helper()
	site := &stDemandSheetSite{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		site.fetches.Add(1)
		body := site.body.Load()
		if body == nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(*body)
	}))
	store := &stDemandMemorySheetStore{}
	previousSigner, previousUrl, previousStorer := stDemandSheetSigner, stDemandSheetUrl, stDemandSheetStorer
	address := crypto.PubkeyToAddress(key.PublicKey)
	stDemandSheetSigner = func(chain uint64, netuid uint16) (common.Address, bool) {
		return address, chain == stDemandSheetChain && netuid == stDemandSheetNetuid
	}
	stDemandSheetUrl = server.URL
	stDemandSheetStorer = func(*StConfig) pricesheet.Store { return store }
	t.Cleanup(func() {
		stDemandSheetSigner, stDemandSheetUrl, stDemandSheetStorer = previousSigner, previousUrl, previousStorer
		server.Close()
	})
	return site, store
}

func stDemandSheetKey(t testing.TB, seed byte) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.ToECDSA(bytes.Repeat([]byte{seed}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A zero-price baseline on the pinned deployment.
func stDemandSheetConfig() *StConfig {
	return &StConfig{ChainId: stDemandSheetChain, Netuid: uint64(stDemandSheetNetuid), DepositEpochCapRao: 7, DepositZeroRateAction: protocol.DepositZeroRateEqualDemand, DepositTiers: []StDepositTier{{RateDenominator: 1}}, ContractAddress: common.Address{2}}
}

// The entry in force is the latest to take effect by the decision epoch, on either
// side of each boundary, and no entry yet leaves the policy baseline.
func TestStDepositEffectivePolicyAtEachBoundary(t *testing.T) {
	key := stDemandSheetKey(t, 0x31)
	site, _ := useStDemandSheet(t, key)
	site.serve(stDemandSheetFile(t, key, stDemandSheetPrice{10, 100, 5, 1000}, stDemandSheetPrice{20, 0, 0, 1000}, stDemandSheetPrice{30, 200, 6, 2000}))
	cfg := stDemandSheetConfig()
	for _, test := range []struct {
		epoch     uint64
		zeroPrice bool
		gib, user uint64
		capRao    uint64
	}{
		{9, true, 0, 0, 7}, {10, false, 100, 5, 1000}, {11, false, 100, 5, 1000}, {19, false, 100, 5, 1000},
		{20, true, 0, 0, 1000}, {29, true, 0, 0, 1000}, {30, false, 200, 6, 2000}, {31, false, 200, 6, 2000},
	} {
		policy, unknown := stDepositEffectivePolicy(context.Background(), cfg, test.epoch)
		if unknown != nil {
			t.Fatalf("epoch %d: %+v", test.epoch, unknown)
		}
		tier := policy.Tiers[0]
		if policy.IsZeroPrice() != test.zeroPrice || tier.RateNumeratorRaoPerGiB != test.gib || tier.RateNumeratorRaoPerUser != test.user || policy.EpochCapRaoPerOperator != test.capRao {
			t.Fatalf("epoch %d: %+v", test.epoch, policy)
		}
	}
	if fetches := site.fetches.Load(); fetches != 8 {
		t.Fatalf("%d fetches for 8 attempts", fetches)
	}
}

// No pinned signer means no fetch at all, whatever a site would serve.
func TestStDepositPolicyWithNoPinnedSignerFetchesNothing(t *testing.T) {
	key := stDemandSheetKey(t, 0x32)
	site, _ := useStDemandSheet(t, key)
	site.serve(stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000}))
	cfg := stDemandSheetConfig()
	cfg.Netuid = 521
	policy, unknown := stDepositEffectivePolicy(context.Background(), cfg, 5)
	if unknown != nil || !policy.IsZeroPrice() || site.fetches.Load() != 0 {
		t.Fatalf("%+v, %+v, %d fetches", policy, unknown, site.fetches.Load())
	}
	if _, unknown := stDepositStoredPolicy(context.Background(), cfg, 5); unknown != nil {
		t.Fatalf("%+v", unknown)
	}
}

// A file that does not verify never prices anything. With nothing verified the price
// is unknown, and the attempt stops with a delay. With a verified copy, that copy prices.
func TestStDepositForgedOrUnavailableSheetNeverPays(t *testing.T) {
	key, forger := stDemandSheetKey(t, 0x33), stDemandSheetKey(t, 0x34)
	cfg := stDemandSheetConfig()
	ctx := context.Background()
	forged := stDemandSheetFile(t, forger, stDemandSheetPrice{1, 999_999, 999_999, 999_999})
	for name, serve := range map[string]func(*stDemandSheetSite){
		"forged":      func(site *stDemandSheetSite) { site.serve(forged) },
		"unavailable": func(site *stDemandSheetSite) { site.down() },
		"garbage":     func(site *stDemandSheetSite) { site.serve([]byte("not: [a sheet")) },
	} {
		t.Run(name+" with nothing verified", func(t *testing.T) {
			site, store := useStDemandSheet(t, key)
			serve(site)
			_, unknown := stDepositEffectivePolicy(ctx, cfg, 5)
			if unknown == nil || !unknown.Retry || unknown.Status != model.StPublishStatusFailed || !strings.Contains(unknown.Reason, "the price is unknown") || unknown.RetryAt.IsZero() || len(store.raw) != 0 {
				t.Fatalf("%+v, stored %d bytes", unknown, len(store.raw))
			}
		})
		t.Run(name+" with a verified copy", func(t *testing.T) {
			site, store := useStDemandSheet(t, key)
			store.raw = stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000})
			good := bytes.Clone(store.raw)
			serve(site)
			policy, unknown := stDepositEffectivePolicy(ctx, cfg, 5)
			if unknown != nil || policy.IsZeroPrice() || policy.Tiers[0].RateNumeratorRaoPerGiB != 100 || !bytes.Equal(store.raw, good) {
				t.Fatalf("%+v, %+v", policy, unknown)
			}
		})
	}
	// A stored copy that no longer verifies is no sheet at all (pricesheet.Source.Refresh):
	// the published file, once verified, replaces it. With the site down it is unknown.
	t.Run("a stored copy that does not verify is replaced by a verified file", func(t *testing.T) {
		site, store := useStDemandSheet(t, key)
		store.raw = forged
		published := stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000})
		site.serve(published)
		policy, unknown := stDepositEffectivePolicy(ctx, cfg, 5)
		if unknown != nil || policy.IsZeroPrice() || policy.Tiers[0].RateNumeratorRaoPerGiB != 100 || !bytes.Equal(store.raw, published) {
			t.Fatalf("%+v, %+v", policy, unknown)
		}
		store.raw = forged
		site.down()
		if _, unknown := stDepositEffectivePolicy(ctx, cfg, 5); unknown == nil || !unknown.Retry || !bytes.Equal(store.raw, forged) {
			t.Fatalf("%+v", unknown)
		}
	})
	// A store that cannot be read may still hold a good copy, so nothing overwrites it,
	// but a published file that verifies is priced from. With the site down it is unknown.
	t.Run("a database that cannot be read prices from a verified file and keeps its copy", func(t *testing.T) {
		site, store := useStDemandSheet(t, key)
		store.loadErr = errors.New("relation does not exist")
		site.serve(stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000}))
		policy, unknown := stDepositEffectivePolicy(ctx, cfg, 5)
		if unknown != nil || policy.IsZeroPrice() || len(store.raw) != 0 {
			t.Fatalf("%+v, %+v, stored %d bytes", policy, unknown, len(store.raw))
		}
		site.down()
		if _, unknown := stDepositEffectivePolicy(ctx, cfg, 5); unknown == nil || !unknown.Retry {
			t.Fatalf("%+v", unknown)
		}
	})
	t.Run("a file that drops a signed entry is refused", func(t *testing.T) {
		site, store := useStDemandSheet(t, key)
		store.raw = stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000})
		site.serve(stDemandSheetFile(t, key))
		policy, unknown := stDepositEffectivePolicy(ctx, cfg, 5)
		if unknown != nil || policy.IsZeroPrice() {
			t.Fatalf("%+v, %+v", policy, unknown)
		}
	})
	t.Run("a sheet that verifies replaces the copy", func(t *testing.T) {
		site, store := useStDemandSheet(t, key)
		next := stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000})
		site.serve(next)
		if _, unknown := stDepositEffectivePolicy(ctx, cfg, 5); unknown != nil || !bytes.Equal(store.raw, next) {
			t.Fatalf("%+v", unknown)
		}
	})
}

// A sheet with no entries, as at launch, is a known zero price, never an unknown one.
func TestStDepositLaunchSheetIsAKnownZeroPrice(t *testing.T) {
	key := stDemandSheetKey(t, 0x35)
	site, _ := useStDemandSheet(t, key)
	site.serve([]byte("sn: 25\ntiered_per_block_demand_deposits:\n  0:\n    alpha_per_gib: 0\n    alpha_per_user: 0\n"))
	policy, unknown := stDepositEffectivePolicy(context.Background(), stDemandSheetConfig(), 5)
	if unknown != nil || !policy.IsZeroPrice() {
		t.Fatalf("%+v, %+v", policy, unknown)
	}
}

// Readers that must not fetch read the last verified copy.
func TestStDepositStoredPolicyFetchesNothing(t *testing.T) {
	key := stDemandSheetKey(t, 0x36)
	site, store := useStDemandSheet(t, key)
	cfg := stDemandSheetConfig()
	if _, unknown := stDepositStoredPolicy(context.Background(), cfg, 5); unknown == nil {
		t.Fatal("no copy read as a known price")
	}
	store.raw = stDemandSheetFile(t, key, stDemandSheetPrice{1, 100, 5, 1000})
	policy, unknown := stDepositStoredPolicy(context.Background(), cfg, 5)
	if unknown != nil || policy.IsZeroPrice() || site.fetches.Load() != 0 {
		t.Fatalf("%+v, %+v, %d fetches", policy, unknown, site.fetches.Load())
	}
}

// The status command's price: its source, what is in force and the next entry.
func TestStDemandPriceStatus(t *testing.T) {
	key := stDemandSheetKey(t, 0x37)
	raw := stDemandSheetFile(t, key, stDemandSheetPrice{10, 100, 5, 1000}, stDemandSheetPrice{20, 200, 6, 2000})
	sheet, err := pricesheet.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := stDemandSheetConfig()
	early := stDemandPriceStatusAt(cfg, 9, "0xsigner", sheet, nil)
	if early.Source != "policy baseline" || !early.ZeroPrice || early.InForce != nil || early.NextEntry == nil || early.NextEntry.EffectiveEpoch != 10 || early.Signer != "0xsigner" {
		t.Fatalf("%+v", early)
	}
	during := stDemandPriceStatusAt(cfg, 10, "0xsigner", sheet, nil)
	if during.Source != "signed entry" || during.ZeroPrice || during.InForce.EffectiveEpoch != 10 || during.NextEntry.EffectiveEpoch != 20 || during.EpochCapRao != 1000 {
		t.Fatalf("%+v", during)
	}
	last := stDemandPriceStatusAt(cfg, 25, "0xsigner", sheet, nil)
	if last.InForce.EffectiveEpoch != 20 || last.NextEntry != nil {
		t.Fatalf("%+v", last)
	}
	unknown := stDemandPriceStatusAt(cfg, 25, "0xsigner", nil, errors.New("Post \"https://rpc.example/?api-key=SECRET\": refused"))
	if unknown.Source != "unknown" || strings.Contains(unknown.Problem, "SECRET") {
		t.Fatalf("%+v", unknown)
	}
	if none := stDemandPriceStatusAt(cfg, 25, "", nil, nil); none.Source != "policy baseline" || !none.ZeroPrice {
		t.Fatalf("%+v", none)
	}
}

func stDemandStatementInput(t testing.TB) (*StConfig, *stLedgerV2StatementInput) {
	t.Helper()
	cfg := &StConfig{ChainId: stDemandSheetChain, GenesisHash: [32]byte{0xAB, 1}, Netuid: 25, NoId: 1, DeploymentId: "statement-fixture", ContractAddress: common.HexToAddress("0x00000000000000000000000000000000000000C1"), VaultV2: common.HexToAddress("0x00000000000000000000000000000000000000C2"), ArtifactKey: stDemandSheetKey(t, 0x41)}
	chunks := []string{ledger.RefOf([]byte("second")).String(), ledger.RefOf([]byte("first")).String()}
	authority := &StPayoutEpochAuthority{Epoch: 12, PolicyHash: [32]byte{0xCD, 2}, Start: protocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte{0xEE, 3}}, End: protocol.ClientKeyEffectiveBoundary{Block: 200, Hash: [32]byte{0xFF, 4}}, EndTime: time.Date(2026, 11, 7, 1, 2, 3, 4, time.FixedZone("elsewhere", 3600))}
	return cfg, &stLedgerV2StatementInput{Epoch: 12, Authority: authority, Chunks: chunks, TotalBytes: 12345, Rows: 2, Users: 7}
}

// Every retry of one epoch signs and publishes identical bytes, so a retry can never
// publish a second statement. The result is what validators decode and verify.
func TestStLedgerV2StatementIsByteIdenticalAcrossRetries(t *testing.T) {
	cfg, input := stDemandStatementInput(t)
	statement, payload, envelope, err := stLedgerV2SealStatement(cfg, input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := startifact.EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, again, envelopeAgain, err := stLedgerV2SealStatement(cfg, input)
		if err != nil {
			t.Fatal(err)
		}
		encodedAgain, err := startifact.EvidenceBytes(envelopeAgain)
		if err != nil || !bytes.Equal(again, payload) || !bytes.Equal(encodedAgain, encoded) || envelopeAgain.ContentHash != envelope.ContentHash {
			t.Fatalf("attempt %d differs: %v", attempt, err)
		}
	}
	if err := startifact.VerifyEvidence(envelope); err != nil {
		t.Fatal(err)
	}
	decoded, err := usage.Decode(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.Verify(crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != usage.EvidenceKind || envelope.RunID != usage.RunID(1, 12) || envelope.CreatedAt != "2026-11-07T00:02:03.000000004Z" || envelope.GenesisHash != strings.ToLower(envelope.GenesisHash) {
		t.Fatalf("%+v", envelope)
	}
	if decoded.ProviderChunks[0] != input.Chunks[0] || decoded.ProviderChunks[1] != input.Chunks[1] {
		t.Fatalf("the header's order was lost: %v", decoded.ProviderChunks)
	}
	for _, hash := range []string{statement.GenesisHash, statement.PolicyHash, statement.Start.Hash, statement.End.Hash} {
		if !strings.HasPrefix(hash, "0x") || hash != strings.ToLower(hash) || len(hash) != 66 {
			t.Fatalf("not lowercase 0x-hex: %s", hash)
		}
	}
	input.Chunks = nil
	empty, _, _, err := stLedgerV2SealStatement(cfg, input)
	if err != nil || empty.ProviderChunks == nil {
		t.Fatalf("no rows must read as an empty list: %v", err)
	}
	cfg.ArtifactKey = nil
	if _, _, _, err := stLedgerV2SealStatement(cfg, input); err == nil {
		t.Fatal("sealed without a key")
	}
}

// A deposit consumes a statement's totals only for this operator, epoch and boundaries.
func TestStDepositStatementUsagePinsIdentityAndBoundaries(t *testing.T) {
	cfg, input := stDemandStatementInput(t)
	statement, _, _, err := stLedgerV2SealStatement(cfg, input)
	if err != nil {
		t.Fatal(err)
	}
	usageBytes, users, err := stDepositStatementUsage(statement, cfg, 12, input.Authority)
	if err != nil || usageBytes != 12345 || users != 7 {
		t.Fatal(usageBytes, users, err)
	}
	if _, _, err := stDepositStatementUsage(statement, cfg, 13, input.Authority); err == nil {
		t.Fatal("another epoch admitted")
	}
	otherOperator := *cfg
	otherOperator.NoId = 2
	if _, _, err := stDepositStatementUsage(statement, &otherOperator, 12, input.Authority); err == nil {
		t.Fatal("another operator admitted")
	}
	otherSigner := *cfg
	otherSigner.ArtifactKey = stDemandSheetKey(t, 0x42)
	if _, _, err := stDepositStatementUsage(statement, &otherSigner, 12, input.Authority); err == nil {
		t.Fatal("another signer admitted")
	}
	moved := *input.Authority
	moved.End.Block++
	if _, _, err := stDepositStatementUsage(statement, cfg, 12, &moved); err == nil {
		t.Fatal("other boundaries admitted")
	}
	otherPolicy := *input.Authority
	otherPolicy.PolicyHash[0]++
	if _, _, err := stDepositStatementUsage(statement, cfg, 12, &otherPolicy); err == nil {
		t.Fatal("another policy admitted")
	}
	statement.TotalUsers++
	if _, _, err := stDepositStatementUsage(statement, cfg, 12, input.Authority); err == nil {
		t.Fatal("a changed total admitted")
	}
}

// Users first, then the statement once the root committed for the epoch is mirrored.
// Recorded rows alone never plan it. An epoch the committed header declares
// unrecoverable has no step, so nothing restarts one every sync turn.
func TestStLedgerV2UsageSteps(t *testing.T) {
	committed := &ledger.HeaderEpoch{Epoch: 5, Providers: []ledger.Ref{}}
	captured := &model.StLedgerV2EpochUsage{Epoch: 5, TotalUsers: 3}
	for name, test := range map[string]struct {
		record    *model.StLedgerV2EpochUsage
		committed *ledger.HeaderEpoch
		want      string
	}{
		"nothing yet":            {nil, nil, StLedgerV2UsageStepUsers},
		"a commit before users":  {nil, committed, StLedgerV2UsageStepUsers},
		"waiting on the commit":  {captured, nil, ""},
		"committed":              {captured, committed, StLedgerV2UsageStepStatement},
		"declared unrecoverable": {captured, &ledger.HeaderEpoch{Epoch: 5, Null: true}, ""},
		"done":                   {&model.StLedgerV2EpochUsage{Epoch: 5, Statement: []byte("{}")}, committed, ""},
	} {
		if got := stLedgerV2UsageDue(test.record, test.committed); got != test.want {
			t.Fatalf("%s: %q, want %q", name, got, test.want)
		}
	}
}

// A late price is paid only while the epoch's concluding deposit row is the zero-price
// skip: a confirmed, refused or otherwise skipped deposit row ends it, and a failed one
// does not. The zero-price skip is told from every other skip by its reason.
func TestStDepositLateEntryOpen(t *testing.T) {
	zero := (&StDepositSizing{Epoch: 7, SourceEpoch: 6, ZeroPrice: true}).String()
	if !strings.HasPrefix(zero, stDepositZeroPriceSkip) {
		t.Fatalf("the zero-price skip reads %q", zero)
	}
	row := func(kind, status, reason string) *model.StPublish {
		return &model.StPublish{Epoch: 7, Kind: kind, Status: status, Error: &reason}
	}
	skip := row(model.StPublishKindDeposit, model.StPublishStatusSkipped, zero)
	for name, test := range map[string]struct {
		rows []*model.StPublish
		open bool
	}{
		"no rows":                {nil, false},
		"a zero-price skip":      {[]*model.StPublish{skip}, true},
		"then a failed attempt":  {[]*model.StPublish{skip, row(model.StPublishKindDepositPush, model.StPublishStatusConfirmed, ""), row(model.StPublishKindDeposit, model.StPublishStatusFailed, "deposit credit failed")}, true},
		"then a skipped push":    {[]*model.StPublish{skip, row(model.StPublishKindDepositPush, model.StPublishStatusSkipped, "already credited")}, true},
		"then confirmed":         {[]*model.StPublish{skip, row(model.StPublishKindDeposit, model.StPublishStatusConfirmed, "")}, false},
		"then refused":           {[]*model.StPublish{skip, row(model.StPublishKindDeposit, model.StPublishStatusRefused, "below the minimum")}, false},
		"then nothing owed":      {[]*model.StPublish{skip, row(model.StPublishKindDeposit, model.StPublishStatusSkipped, "deposit size is zero (no usage, or deposit rate/cap not configured)")}, false},
		"then already deposited": {[]*model.StPublish{skip, row(model.StPublishKindDeposit, model.StPublishStatusSkipped, "epoch 7 already has 5 rao deposited")}, false},
		"only another skip":      {[]*model.StPublish{row(model.StPublishKindDeposit, model.StPublishStatusSkipped, "epoch 7 is not the current epoch (rolled 8, pending 8)")}, false},
		"a skip with no reason":  {[]*model.StPublish{{Epoch: 7, Kind: model.StPublishKindDeposit, Status: model.StPublishStatusSkipped}}, false},
	} {
		if got := stDepositLateEntryOpen(test.rows); got != test.open {
			t.Fatalf("%s: %t", name, got)
		}
	}
}

// A publish row holds what its column can, without a keyed endpoint.
func TestStPublishReasonFitsItsColumn(t *testing.T) {
	long := stPublishReason(strings.Repeat("é", 2_000))
	if utf8.RuneCountInString(long) != stPublishReasonMaxChars || !utf8.ValidString(long) {
		t.Fatal(utf8.RuneCountInString(long))
	}
	if got := stPublishReason("a\x00b\xffc"); got != "ab\uFFFDc" {
		t.Fatalf("%q", got)
	}
	if got := stPublishReason("short"); got != "short" {
		t.Fatalf("%q", got)
	}
	if got := stPublishReason(`Post "https://rpc.example/v2/SECRET?api-key=SECRET": timeout`); strings.Contains(got, "SECRET") || !strings.Contains(got, "https://rpc.example") {
		t.Fatalf("%q", got)
	}
}

// Nothing an RPC or Solana endpoint's error names can carry its key onward.
func TestStScrubEndpoints(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{`st: no complete RPC observation: https://mainnet.helius-rpc.com/?api-key=abc123: dial failed`, `st: no complete RPC observation: https://mainnet.helius-rpc.com dial failed`},
		{`Post "https://eth.example/v2/abc123": context deadline exceeded`, `Post "https://eth.example": context deadline exceeded`},
		{`wss://user:abc123@node.example/ws closed`, `wss://node.example closed`},
		{`request failed api-key=abc123&x=1`, `request failed api-key=(redacted)&x=1`},
		{`token=abc123 refused`, `token=(redacted) refused`},
		{`demand: the USDC wallet cannot cover the shortfall`, `demand: the USDC wallet cannot cover the shortfall`},
	} {
		if got := stScrubEndpoints(test.in); got != test.want {
			t.Fatalf("%q: %q, want %q", test.in, got, test.want)
		}
	}
	data := stScrubEventData(map[string]any{"reason": `https://a.example/?api-key=abc123 failed`, "amount": 5})
	if data["reason"] != "https://a.example failed" || data["amount"] != 5 {
		t.Fatalf("%v", data)
	}
}

// One answer per process, and only a durable one: the parsed file or its proved
// absence. A lookup that proved nothing and a file that does not parse are read again.
func TestStDemandConfigKeepsOnlyADurableAnswer(t *testing.T) {
	previousBytes := stDemandVaultBytes
	reset := func() {
		stDemandConfigLock.Lock()
		stDemandConfigKnown, stDemandConfigValue = false, nil
		stDemandConfigLock.Unlock()
	}
	t.Cleanup(func() { stDemandVaultBytes = previousBytes; reset() })
	reset()
	reads := 0
	answer := func(raw []byte, err error) {
		stDemandVaultBytes = func() ([]byte, error) { reads++; return raw, err }
	}
	answer(nil, fmt.Errorf("%w: vault mount", server.ErrResourceUnavailable))
	if wallets, err := stDemandConfigOnce(); err == nil || wallets != nil {
		t.Fatal("an unavailable vault read as an answer")
	}
	answer(nil, errors.Join(server.ErrResourceNotFound, server.ErrResourceUnavailable))
	if _, err := stDemandConfigOnce(); err == nil {
		t.Fatal("a lookup that could not prove absence read as absent")
	}
	answer([]byte("not: [yaml"), nil)
	if _, err := stDemandConfigOnce(); err == nil {
		t.Fatal("a broken file read as absent")
	}
	answer(nil, fmt.Errorf("%w in vault", server.ErrResourceNotFound))
	if wallets, err := stDemandConfigOnce(); err != nil || wallets != nil {
		t.Fatal(wallets, err)
	}
	seen := reads
	if wallets, err := stDemandConfigOnce(); err != nil || wallets != nil || reads != seen {
		t.Fatal("proved absence was read again", wallets, err, reads)
	}
	if seen != 4 {
		t.Fatalf("%d reads, want each failure read again", seen)
	}
}

// A retained intent holds a nonce of the deposit key, so its form alone admits it,
// whatever has become of demand.yml. demand.yml still gates a new one.
func TestStDemandRetainedIntentNeedsNoDemandYml(t *testing.T) {
	client, rpcClient, _ := newStDepositCustodyFixture(t, 1)
	unwrap := stDemandPackWithdraw(5)
	stake := stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid)
	retained := func(to common.Address, data []byte) *model.StTransactionIntent {
		intent, err := stDemandIntent(client, "demand:retained", to, data)
		if err != nil {
			t.Fatal(err)
		}
		intent.IntentId = server.NewId()
		return intent
	}
	broken := func() (*demand.Config, error) { return nil, errors.New("demand: demand.yml does not parse") }
	for name, load := range map[string]func() (*demand.Config, error){"removed": func() (*demand.Config, error) { return nil, nil }, "broken": broken} {
		withoutStDemandWallets(t)
		stDemandConfig = load
		for what, intent := range map[string]*model.StTransactionIntent{"unwrap": retained(bridge.WTAO, unwrap), "stake": retained(stStakingPrecompileAddress, stake)} {
			if err := client.validateDepositAttempt(context.Background(), rpcClient, intent, model.StTxAttemptExecution, true); err != nil {
				t.Fatalf("demand.yml %s, retained %s: %v", name, what, err)
			}
		}
		for what, refused := range map[string]*model.StTransactionIntent{
			"another hotkey": retained(stStakingPrecompileAddress, stDemandPackStake([32]byte{1}, 5, 7, client.cfg.Netuid)),
			"another subnet": retained(stStakingPrecompileAddress, stDemandPackStake(client.cfg.DepositHotkey, 5, 7, client.cfg.Netuid+1)),
			"elsewhere":      retained(common.HexToAddress("0x1"), unwrap),
		} {
			if err := client.validateDepositAttempt(context.Background(), rpcClient, refused, model.StTxAttemptExecution, true); err == nil {
				t.Fatalf("demand.yml %s, retained %s signed", name, what)
			}
		}
		fresh, err := stDemandIntent(client, "demand:new", bridge.WTAO, unwrap)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.validateDepositAttempt(context.Background(), rpcClient, fresh, model.StTxAttemptExecution, false); err == nil {
			t.Fatalf("demand.yml %s, a new intent signed", name)
		}
	}
}

// The funding decisions in order: settle first, then one custody snapshot decides.
func TestStDemandFundingDecisions(t *testing.T) {
	ctx := context.Background()
	const epoch = 7
	amount := big.NewInt(100)

	// The funder reads alpha at the latest block and custody at the finalized one, so
	// a send that has just landed looks like a shortfall until it is settled.
	t.Run("what a settled send moved is not bought again", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		settles := 0
		client.demandSettle = func(context.Context) error {
			settles++
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			fixture.source.SetInt64(103)
			return nil
		}
		if outcome := client.fundDemandDeposit(ctx, epoch, amount); outcome != nil || settles != 1 || len(payer.requests) != 0 {
			t.Fatalf("%+v, %d settles, %d requests", outcome, settles, len(payer.requests))
		}
	})
	t.Run("one rao short converts", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		fixture.source.SetInt64(102)
		if outcome := client.fundDemandDeposit(ctx, epoch, amount); outcome != nil || len(payer.requests) != 1 {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	t.Run("transactions that have not settled stop it", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		client.demandSettle = func(context.Context) error { return errors.New("earlier intent still waiting") }
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || len(payer.requests) != 0 || outcome.RetryAt.IsZero() {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	t.Run("a dust deficit spends nothing and ends the epoch", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		fixture.minimumTao = 10
		fixture.staged.SetInt64(101)
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || outcome.Retry || outcome.Status != model.StPublishStatusRefused || len(payer.requests) != 0 {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	t.Run("a deposit below the minimum spends nothing and ends the epoch", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		fixture.minimumTao = 1_000
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || outcome.Retry || outcome.Status != model.StPublishStatusRefused || len(payer.requests) != 0 || !strings.Contains(outcome.Reason, errStDepositBelowRuntimeMinimum.Error()) {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	t.Run("a retained staging intent spends nothing", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		client.demandRetained = func(_ context.Context, retainedEpoch uint64, nonce *big.Int) (bool, error) {
			return retainedEpoch == epoch && nonce.Int64() == 3, nil
		}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || len(payer.requests) != 0 || !strings.Contains(outcome.Reason, "retained deposit funding intent") {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	t.Run("too close to the deadline to stage spends nothing", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		fixture.boundary.Block, fixture.boundary.Hash = 1_849, [32]byte(stTransactionReconcileBlockHash(1_849))
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || len(payer.requests) != 0 || !strings.Contains(outcome.Reason, "staging stops 150 blocks") {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	// A passing market condition: the pool has no weight while the deposit is unpaid.
	t.Run("the price guard retries in minutes", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{err: fmt.Errorf("%w: 1 USDC buys too little", demand.ErrPriceGuard)}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		delay := time.Until(outcome.RetryAt)
		if !outcome.Retry || delay < 4*time.Minute || 6*time.Minute < delay {
			t.Fatalf("%+v, %s", outcome, delay)
		}
	})
	// What the funder refuses before any USDC moves takes the long delay, whatever it
	// is called: a wallet without gas, a hotkey that cannot take stake, an empty wallet.
	t.Run("any other refusal retries after the long delay", func(t *testing.T) {
		for _, refusal := range []string{"demand: the alpha wallet needs TAO for gas: it holds 5 rao", "demand: the deposit hotkey cannot take stake: execution reverted", "demand: the USDC wallet needs SOL for fees"} {
			client, _ := stDemandShortClient(t)
			client.demandPayer = &stDemandFakePayer{err: errors.New(refusal)}
			outcome := client.fundDemandDeposit(ctx, epoch, amount)
			if outcome == nil || !outcome.Retry || !outcome.RetryAt.IsZero() || outcome.Status != model.StPublishStatusFailed || !strings.Contains(outcome.Reason, refusal) {
				t.Fatalf("%+v", outcome)
			}
		}
	})
	t.Run("a lost state write retries soon", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{err: fmt.Errorf("%w: version 3", demand.ErrStale)}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if !outcome.Retry || outcome.RetryAt.IsZero() || 2*time.Minute < time.Until(outcome.RetryAt) {
			t.Fatalf("%+v", outcome)
		}
	})
	// None of these refuses the funding: the next attempt goes on from there, and may
	// find the wallet funded.
	t.Run("what is still under way is not a refusal", func(t *testing.T) {
		for name, err := range map[string]error{
			"earlier transactions": fmt.Errorf("demand: settle the wallet's earlier transactions: %w: %w", errStDemandUnsettled, errors.New("st: no rpc endpoint answered")),
			"a send's finality":    fmt.Errorf("demand: stake TAO: st: transaction 0xabc finality uncertain: %w", context.DeadlineExceeded),
			"a stopped run":        fmt.Errorf("demand: read the alpha position: %w", context.Canceled),
		} {
			client, _ := stDemandShortClient(t)
			client.demandPayer = &stDemandFakePayer{err: err}
			outcome := client.fundDemandDeposit(ctx, epoch, amount)
			if outcome == nil || !outcome.Retry || outcome.RetryAt.IsZero() || 2*time.Minute < time.Until(outcome.RetryAt) {
				t.Fatalf("%s: %+v", name, outcome)
			}
		}
	})
	t.Run("a custody read that fails is not a refusal", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		client.demandSettle = func(context.Context) error {
			stDepositCustodyExpire(fixture)
			return nil
		}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || outcome.RetryAt.IsZero() || len(payer.requests) != 0 {
			t.Fatalf("%+v, %d requests", outcome, len(payer.requests))
		}
	})
	// A budget moved to a future epoch would stop conversions until the chain reached it,
	// and nothing resets it. This is every path's only way to Ensure, the manual one too.
	t.Run("only the chain's current epoch reaches the funder", func(t *testing.T) {
		for _, other := range []uint64{epoch - 1, epoch + 1, epoch + 1_000} {
			client, _ := stDemandShortClient(t)
			payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
			client.demandPayer = payer
			if outcome := client.fundDemandDeposit(ctx, other, amount); outcome == nil || len(payer.requests) != 0 {
				t.Fatalf("epoch %d: %+v, %d requests", other, outcome, len(payer.requests))
			}
		}
		client, _ := stDemandShortClient(t)
		payer := &stDemandFakePayer{result: &demand.Result{Funded: true}}
		client.demandPayer = payer
		if outcome := client.fundDemandDeposit(ctx, epoch, amount); outcome != nil || len(payer.requests) != 1 || payer.requests[0].Epoch != epoch {
			t.Fatalf("%+v, %+v", outcome, payer.requests)
		}
	})
	t.Run("an older epoch's budget ends a stale run", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{
			err:    fmt.Errorf("%w: epoch 7 is older than epoch 8, and spends nothing", demand.ErrEpochBudget),
			during: func() { stDepositCustodyExpire(fixture) },
		}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || outcome.Retry || outcome.Status != model.StPublishStatusFailed || !strings.Contains(outcome.Reason, "older than") {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("a budget used up in the current epoch is a refusal", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{err: fmt.Errorf("%w: 5 USDC would pass the 4 USDC left", demand.ErrEpochBudget)}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || !outcome.RetryAt.IsZero() {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("a hotkey that refuses a stake alerts and takes the long delay", func(t *testing.T) {
		alerts := stDemandCaptureAlerts(t)
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{err: fmt.Errorf("%w: %w", demand.ErrHotkeyNotReady, errors.New(`Post "https://rpc.example/SECRET": execution reverted`))}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || !outcome.RetryAt.IsZero() || outcome.Status != model.StPublishStatusFailed {
			t.Fatalf("%+v", outcome)
		}
		if len(alerts.lines) != 1 || !strings.HasPrefix(alerts.lines[0], "demand_hotkey_not_ready|epoch 7|") || strings.Contains(alerts.lines[0], "SECRET") {
			t.Fatalf("%q", alerts.lines)
		}
		if len(alerts.events) != 1 || alerts.events[0].kind != stDemandEventHotkeyNotReady || alerts.events[0].epoch != epoch {
			t.Fatalf("%+v", alerts.events)
		}
	})
	t.Run("a keyed endpoint in the funder's error reaches nothing", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = &stDemandFakePayer{err: errors.New(`Post "https://mainnet.helius-rpc.com/?api-key=SECRET": timeout`)}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if strings.Contains(outcome.Reason, "SECRET") || strings.Contains(outcome.String(), "SECRET") {
			t.Fatalf("%+v", outcome)
		}
	})
}

// stDemandAlerts is what a test's alerts raised: the log lines and the events the
// monitor pages on.
type stDemandAlerts struct {
	lines  []string
	events []stDemandAlertEvent
}

type stDemandAlertEvent struct {
	epoch uint64
	kind  string
	data  map[string]any
}

// stDemandCaptureAlerts replaces where alerts go, and forgets which were raised.
func stDemandCaptureAlerts(t testing.TB) *stDemandAlerts {
	t.Helper()
	alerts := &stDemandAlerts{}
	previousSink, previousRecord := stLedgerV2AlertSink, stDemandRecordAlert
	stLedgerV2AlertSink = func(name, subject, message string) {
		alerts.lines = append(alerts.lines, name+"|"+subject+"|"+message)
	}
	stDemandRecordAlert = func(_ model.StDeploymentKey, _ time.Time, epoch uint64, kind string, data map[string]any) {
		alerts.events = append(alerts.events, stDemandAlertEvent{epoch: epoch, kind: kind, data: data})
	}
	stDemandAlerted.Clear()
	t.Cleanup(func() {
		stLedgerV2AlertSink, stDemandRecordAlert = previousSink, previousRecord
		stDemandAlerted.Clear()
	})
	return alerts
}

// A line the funder marks ALERT is an alert, the first time and again after the
// repeat delay, as it recurs on every poll of its wait. No other line is.
func TestStDemandFunderAlertLines(t *testing.T) {
	alerts := stDemandCaptureAlerts(t)
	client, _, _ := newStDepositCustodyFixture(t, 1)
	logf := stDemandFunderLog(client.cfg)
	logf("[demand]epoch %d swap %s paid %d rao of TAO, below the floor %d", 7, "sig", 1, 2)
	logf("[demand]epoch %d jupiter execute: %v; the signature decides", 7, errors.New("ALERT is only in this error"))
	if len(alerts.lines) != 0 {
		t.Fatalf("%q", alerts.lines)
	}
	const alert = "[demand]epoch %d ALERT swap %s: %s USDC has left the wallet and no TAO has come. %s"
	for range 3 {
		logf(alert, 7, "sig", "5", `Post "https://mainnet.helius-rpc.com/?api-key=SECRET": timeout`)
	}
	if len(alerts.lines) != 1 || len(alerts.events) != 1 {
		t.Fatalf("%q, %+v", alerts.lines, alerts.events)
	}
	event := alerts.events[0]
	if event.kind != stDemandEventFunderAlert || event.epoch != 7 || !strings.Contains(fmt.Sprint(event.data["message"]), "ALERT swap sig") || strings.Contains(fmt.Sprint(event.data), "SECRET") || strings.Contains(alerts.lines[0], "SECRET") {
		t.Fatalf("%+v, %q", event, alerts.lines)
	}
	logf(alert, 8, "other", "5", "")
	if len(alerts.events) != 2 || alerts.events[1].epoch != 8 {
		t.Fatalf("%+v", alerts.events)
	}
	stDemandAlerted.Range(func(key, _ any) bool {
		stDemandAlerted.Store(key, server.NowUtc().Add(-stDemandAlertRepeat))
		return true
	})
	logf(alert, 8, "other", "5", "")
	if len(alerts.events) != 3 {
		t.Fatalf("no repeat after its delay: %+v", alerts.events)
	}
}

// The kinds the monitor names (signal_subnet_demand_events.go) fit st_demand_event.kind.
func TestStDemandAlertKinds(t *testing.T) {
	for _, kind := range []string{stDemandEventSwapNeedsPerson, stDemandEventFunderAlert, stDemandEventHotkeyNotReady} {
		if 32 < len(kind) {
			t.Fatal(kind)
		}
	}
	if stDemandEventSwapNeedsPerson != "swap_needs_person" || stDemandEventFunderAlert != "funder_alert" || stDemandEventHotkeyNotReady != "hotkey_not_ready" {
		t.Fatal("the monitor pages on these names")
	}
}

// The status command prints the report as JSON, the funder's gas reserve with it.
func TestStDemandStatusReportsTheGasReserve(t *testing.T) {
	encoded, err := json.Marshal(&StDemandStatusReport{WalletStatus: &demand.WalletStatus{GasReserveRao: 200_000_000}})
	if err != nil || !strings.Contains(string(encoded), `"gas_reserve_rao":200000000`) {
		t.Fatalf("%s, %v", encoded, err)
	}
}

// The funder as it is, in front of the server's sender: what its settle returns for a
// wallet whose earlier transactions cannot finish, and what it returns for an epoch its
// budget has passed, each read the way the deposit must read it.
func TestStDemandFunderContract(t *testing.T) {
	ctx := context.Background()
	const epoch = 7
	amount := big.NewInt(100)
	funderWith := func(t *testing.T, client *CoreStClient, sender demand.Sender, state *demand.State) *demand.Funder {
		t.Helper()
		wallets := useStDemandWallets(t, client.cfg)
		funder, err := demand.NewFunder(wallets, uint16(client.cfg.Netuid), demand.Deps{Store: &stDemandFakeStore{state: state}, EVM: stDemandEmptyEvm{}, Sender: sender, Solana: stDemandNoSolana{}})
		if err != nil {
			t.Fatal(err)
		}
		return funder
	}
	t.Run("a settle that fails is not a refusal, and buys nothing", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		// A sender with no key stops before its prepare, as a failed reconciliation does.
		client.demandPayer = funderWith(t, client, &stDemandSender{core: client}, &demand.State{})
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || outcome.RetryAt.IsZero() || 2*time.Minute < time.Until(outcome.RetryAt) || !strings.Contains(outcome.Reason, "settle the wallet's earlier transactions") {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("an epoch its budget has passed, once the chain has moved on, stops", func(t *testing.T) {
		client, fixture := stDemandShortClient(t)
		funder := funderWith(t, client, stDemandSettledSender{}, &demand.State{Epoch: epoch + 1})
		client.demandPayer = stDemandPayerThen{payer: funder, before: func() { stDepositCustodyExpire(fixture) }}
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || outcome.Retry || !strings.Contains(outcome.Reason, "older than") {
			t.Fatalf("%+v", outcome)
		}
	})
	t.Run("a budget ahead of the chain is a refusal that retries slowly", func(t *testing.T) {
		client, _ := stDemandShortClient(t)
		client.demandPayer = funderWith(t, client, stDemandSettledSender{}, &demand.State{Epoch: epoch + 1})
		outcome := client.fundDemandDeposit(ctx, epoch, amount)
		if outcome == nil || !outcome.Retry || !outcome.RetryAt.IsZero() || !strings.Contains(outcome.Reason, "older than") {
			t.Fatalf("%+v", outcome)
		}
	})
}

type stDemandFakeStore struct {
	state  *demand.State
	events []demand.Event
}

func (self *stDemandFakeStore) Load(context.Context) (*demand.State, uint64, error) {
	state := *self.state
	return &state, 1, nil
}

func (self *stDemandFakeStore) Save(_ context.Context, state *demand.State, version uint64) (uint64, error) {
	self.state = state
	return version + 1, nil
}

func (self *stDemandFakeStore) Record(_ context.Context, event demand.Event) error {
	self.events = append(self.events, event)
	return nil
}

// A wallet holding nothing at all.
type stDemandEmptyEvm struct{}

func (stDemandEmptyEvm) Call(context.Context, common.Address, []byte) ([]byte, error) {
	return make([]byte, 32), nil
}

func (stDemandEmptyEvm) Balance(context.Context, common.Address) (*big.Int, error) {
	return new(big.Int), nil
}

func (stDemandEmptyEvm) PendingNonce(context.Context, common.Address) (uint64, error) {
	return 0, nil
}

// Nothing here may reach Solana.
type stDemandNoSolana struct {
	demand.SolanaAPI
}

// A sender with nothing earlier to settle.
type stDemandSettledSender struct{}

func (stDemandSettledSender) SendPrepared(context.Context, demand.Prepare) (string, error) {
	return "", nil
}

type stDemandPayerThen struct {
	payer  stDemandPayer
	before func()
}

func (self stDemandPayerThen) Ensure(ctx context.Context, request demand.Request) (*demand.Result, error) {
	self.before()
	return self.payer.Ensure(ctx, request)
}

// The guard alerts once, and only after hours. A new epoch or a clear run starts over.
func TestStDemandPriceGuardAlertsOnlyAfterHours(t *testing.T) {
	watch := &stDemandPriceGuardWatch{}
	start := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	if _, alert := watch.refused(7, start); alert {
		t.Fatal("the first refusal alerted")
	}
	if _, alert := watch.refused(7, start.Add(stDepositPriceGuardAlertAfter-time.Minute)); alert {
		t.Fatal("alerted early")
	}
	if lasted, alert := watch.refused(7, start.Add(stDepositPriceGuardAlertAfter)); !alert || lasted != stDepositPriceGuardAlertAfter {
		t.Fatal("no alert after hours", lasted, alert)
	}
	if _, alert := watch.refused(7, start.Add(stDepositPriceGuardAlertAfter+time.Minute)); alert {
		t.Fatal("alerted twice")
	}
	if lasted, alert := watch.refused(8, start.Add(10*time.Hour)); alert || lasted != 0 {
		t.Fatal("a new epoch inherited the refusal", lasted, alert)
	}
	watch.passed()
	if lasted, alert := watch.refused(8, start.Add(20*time.Hour)); alert || lasted != 0 {
		t.Fatal("a clear run did not start over", lasted, alert)
	}
}

// Funding waits from shortly before the sweep epoch's end until its close is confirmed
// or its grace has passed. Exactly: the lead's first block and the grace's last.
func TestStDemandSweepHoldWindow(t *testing.T) {
	const end, grace = 1_000, 120
	for _, test := range []struct {
		confirmed bool
		latest    uint64
		held      bool
	}{
		{false, end - stDemandSweepHoldLeadBlocks - 1, false},
		{false, end - stDemandSweepHoldLeadBlocks, true},
		{false, end, true},
		{false, end + grace, true},
		{false, end + grace + 1, false},
		{true, end, false},
	} {
		if got := stDemandSweepHeld(test.confirmed, test.latest, end, grace); got != test.held {
			t.Fatalf("confirmed %t latest %d: %t", test.confirmed, test.latest, got)
		}
	}
}

// The hold reads the configured sweep's own window and publish rows. An answer it
// cannot read holds a conversion, and no sweep configured reads nothing. What it holds
// is TestStDemandSweepHoldGatesOnlyTheConversion's.
func TestStDemandSweepHoldWindowReads(t *testing.T) {
	previous := stV1SweepPublishes
	var publishes []*model.StPublish
	stV1SweepPublishes = func(context.Context, *StConfig, uint64) []*model.StPublish { return publishes }
	t.Cleanup(func() { stV1SweepPublishes = previous })
	fixture := newLedgerV2RPC(t)
	client, cfg := newLedgerV1RPCClient(t, fixture)
	cfg.V1SweepEpoch = 7
	// The fixture ends epoch 7 at block 50 with a grace of 120 blocks, and its latest
	// block is inside that.
	hold, err := client.demandSweepHold(context.Background())
	if err != nil || !strings.Contains(hold, "held for the sweep") {
		t.Fatalf("hold %q, %v", hold, err)
	}
	publishes = []*model.StPublish{{Kind: StPublishKindV1SweepClose, Status: model.StPublishStatusFailed}}
	if hold, err := client.demandSweepHold(context.Background()); err != nil || hold == "" {
		t.Fatalf("a failed close released the hold: %q, %v", hold, err)
	}
	publishes = []*model.StPublish{{Kind: StPublishKindV1SweepClose, Status: model.StPublishStatusConfirmed}}
	if hold, err := client.demandSweepHold(context.Background()); err != nil || hold != "" {
		t.Fatalf("a confirmed close kept the hold: %q, %v", hold, err)
	}
	publishes = []*model.StPublish{{Kind: StPublishKindV1SweepRefused, Status: model.StPublishStatusSkipped}}
	if hold, err := client.demandSweepHold(context.Background()); err != nil || hold != "" {
		t.Fatalf("a concluded sweep kept the hold: %q, %v", hold, err)
	}
	cfg.V1SweepEpoch = 0
	publishes = nil
	if hold, err := client.demandSweepHold(context.Background()); err != nil || hold != "" {
		t.Fatalf("no sweep held: %q, %v", hold, err)
	}
}

// A retained staging's epoch and deposit nonce are found through its digest. Only one
// that is provably older is rolled: the current operation, and anything this view of
// the chain has not reached, are not.
func TestStDepositStagingRolled(t *testing.T) {
	cfg := &StConfig{ChainId: 945, ContractAddress: common.Address{2}, NoId: 1, GenesisHash: [32]byte{1}}
	key := func(epoch uint64, nonce int64) string {
		logical, err := stTransactionLogicalKey(cfg, fmt.Sprintf("deposit-fund:%d:1:%d", epoch, nonce))
		if err != nil {
			t.Fatal(err)
		}
		return logical
	}
	for _, test := range []struct {
		name         string
		logical      string
		epoch, nonce uint64
		rolled       bool
	}{
		{"the current operation", key(8, 3), 8, 3, false},
		{"the epoch rolled", key(7, 3), 8, 3, true},
		{"the deposit nonce moved in its epoch", key(8, 3), 8, 4, true},
		{"both moved", key(7, 3), 9, 5, true},
		{"a later epoch than this view", key(9, 3), 8, 3, false},
		{"a later nonce than this view", key(8, 4), 8, 3, false},
		{"beyond the search", key(1, 3), 1 + stDepositStagingSearchEpochs, 3, false},
		{"the last epoch of the search", key(2, 3), 1 + stDepositStagingSearchEpochs, 3, true},
		{"another operation", "st:v2:unrelated", 8, 3, false},
		{"epoch zero", key(0, 0), 0, 0, false},
	} {
		if got := stDepositStagingRolled(cfg, test.logical, test.epoch, new(big.Int).SetUint64(test.nonce)); got != test.rolled {
			t.Fatalf("%s: %t", test.name, got)
		}
	}
}
