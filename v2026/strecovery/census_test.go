// Synthetic signatures reproduce the lost terminal-status custody path and
// force adjacent refusal/restart boundaries without any chain or signing port.
package strecovery

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// A pure reader exposes the exact source call count and incomplete-source fault.
type censusFixtureReader struct {
	images     map[string]*DatabaseSnapshot
	calls      []string
	failSource string
}

// Returned images are copied to prove collection never mutates its input port.
func (self *censusFixtureReader) Snapshot(_ context.Context, source DatabaseSource, _ Limits) (*DatabaseSnapshot, error) {
	self.calls = append(self.calls, source.Id)
	if source.Id == self.failSource {
		return nil, &Refusal{Source: source.Id, Cause: "synthetic unavailable source"}
	}
	if self.images[source.Id] == nil {
		return nil, nil
	}
	raw, _ := json.Marshal(self.images[source.Id])
	var result DatabaseSnapshot
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// A private fixture directory is explicit under both permissive and strict umasks.
func censusTestDir(t testing.TB) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Keys are synthetic fixed scalars, unrelated to any deployed identity.
func censusTestKey(t testing.TB, scalar byte) *ecdsa.PrivateKey {
	t.Helper()
	raw := make([]byte, 32)
	raw[31] = scalar
	key, err := crypto.ToECDSA(raw)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Both supported fee formats and cancellation envelopes are really signed.
func censusTestTransaction(t testing.TB, key *ecdsa.PrivateKey, nonce uint64, kind string, dynamic bool, price int64) (*types.Transaction, []byte) {
	t.Helper()
	return censusTestTransactionOnChain(t, key, nonce, kind, dynamic, price, 31337)
}

func censusTestTransactionOnChain(t testing.TB, key *ecdsa.PrivateKey, nonce uint64, kind string, dynamic bool, price int64, chainId uint64) (*types.Transaction, []byte) {
	t.Helper()
	to, gas, data := common.HexToAddress("0x"+strings.Repeat("c", 40)), uint64(30000), []byte{1, 2, 3}
	if kind == "cancellation" {
		to, gas, data = crypto.PubkeyToAddress(key.PublicKey), 21000, nil
	}
	var tx *types.Transaction
	if dynamic {
		tx = types.NewTx(&types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(chainId), Nonce: nonce, To: &to, Gas: gas, GasTipCap: big.NewInt(10), GasFeeCap: big.NewInt(price), Value: new(big.Int), Data: data})
	} else {
		tx = types.NewTx(&types.LegacyTx{Nonce: nonce, To: &to, Gas: gas, GasPrice: big.NewInt(price), Value: new(big.Int), Data: data})
	}
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(new(big.Int).SetUint64(chainId)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return signed, raw
}

// Every retained column is synthetic, including UUID-shaped intent identities.
func censusTestIntent(key *ecdsa.PrivateKey, number int, nonce int64, status string) Intent {
	return censusTestIntentOnChain(key, number, nonce, status, 31337)
}

func censusTestIntentOnChain(key *ecdsa.PrivateKey, number int, nonce int64, status string, chainId uint64) Intent {
	data := []byte{1, 2, 3}
	return Intent{Id: fmt.Sprintf("00000000-0000-0000-0000-%012d", number), IntentKey: fmt.Sprintf("synthetic-%d:0", number), LogicalKey: fmt.Sprintf("synthetic-%d", number),
		Profile: "synthetic", DeploymentId: "synthetic-deployment", DeploymentKey: fmt.Sprint(chainId) + ":0x" + strings.Repeat("c", 40), ChainId: int64(chainId), Genesis: "0x" + strings.Repeat("a", 64),
		From: strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex()), To: "0x" + strings.Repeat("c", 40), CalldataHash: crypto.Keccak256Hash(data).Hex(), Calldata: data,
		Nonce: nonce, Status: status, CreateTime: time.Unix(1700000000, 0).UTC(), UpdateTime: time.Unix(1700000001, 0).UTC()}
}

// Adding an attempt mirrors the durable original/replacement numbering only.
func censusTestAddAttempt(t testing.TB, image *DatabaseSnapshot, intentIndex int, key *ecdsa.PrivateKey, kind string, dynamic bool, price int64) {
	t.Helper()
	intent := &image.Intents[intentIndex]
	tx, raw := censusTestTransactionOnChain(t, key, uint64(intent.Nonce), kind, dynamic, price, uint64(intent.ChainId))
	intent.AttemptCount++
	hash := tx.Hash().Hex()
	intent.CurrentHash = &hash
	attempt := Attempt{IntentId: intent.Id, Number: intent.AttemptCount, Kind: kind, Hash: hash, Raw: raw, GasLimit: int64(tx.Gas()), Status: "replaced", CreateTime: intent.CreateTime, UpdateTime: intent.UpdateTime}
	if dynamic {
		tip, fee := tx.GasTipCap().String(), tx.GasFeeCap().String()
		attempt.GasTipCap, attempt.GasFeeCap = &tip, &fee
	} else {
		price := tx.GasPrice().String()
		attempt.GasPrice = &price
	}
	image.Attempts = append(image.Attempts, attempt)
}

// Two databases retain five attempts, one store supplies the sixth signature,
// and four signatures are deliberately missing from that original store.
func censusTestFixture(t testing.TB) (Config, *censusFixtureReader) {
	t.Helper()
	return censusTestFixtureOnChain(t, 31337)
}

func censusTestFixtureOnChain(t testing.TB, chainId uint64) (Config, *censusFixtureReader) {
	t.Helper()
	keyA, keyB := censusTestKey(t, 1), censusTestKey(t, 2)
	imageA := &DatabaseSnapshot{Intents: []Intent{censusTestIntentOnChain(keyA, 1, 7, "reverted", chainId), censusTestIntentOnChain(keyA, 2, 8, "finalized", chainId)}, Attempts: []Attempt{}}
	imageA.Intents[1].Generation, imageA.Intents[1].LogicalKey, imageA.Intents[1].IntentKey = 1, imageA.Intents[0].LogicalKey, imageA.Intents[0].LogicalKey+":1"
	censusTestAddAttempt(t, imageA, 0, keyA, "execution", false, 100)
	censusTestAddAttempt(t, imageA, 0, keyA, "execution", true, 200)
	censusTestAddAttempt(t, imageA, 0, keyA, "cancellation", false, 150)
	censusTestAddAttempt(t, imageA, 1, keyA, "execution", false, 110)
	imageB := &DatabaseSnapshot{Intents: []Intent{censusTestIntentOnChain(keyB, 3, 11, "superseded", chainId), censusTestIntentOnChain(keyB, 4, 13, "prepared", chainId)}, Attempts: []Attempt{}}
	censusTestAddAttempt(t, imageB, 0, keyB, "execution", false, 100)
	root := censusTestDir(t)
	store := filepath.Join(root, "store")
	if err := os.Mkdir(store, 0700); err != nil {
		t.Fatal(err)
	}
	files := []StoreFile{{Name: strings.TrimPrefix(imageA.Attempts[0].Hash, "0x") + ".rlp", Raw: imageA.Attempts[0].Raw}, {Name: strings.Repeat("b", 64) + ".scale", Raw: []byte("synthetic opaque native bytes")}}
	storeTx, storeRaw := censusTestTransactionOnChain(t, keyB, 12, "execution", false, 100, chainId)
	files = append(files, StoreFile{Name: strings.TrimPrefix(storeTx.Hash().Hex(), "0x") + ".rlp", Raw: storeRaw})
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(store, file.Name), file.Raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{Schema: ConfigSchema, ChainId: chainId, Genesis: "0x" + strings.Repeat("a", 64),
		Roles: []Role{{Id: "operator-a", Address: imageA.Intents[0].From, FirstNonce: 7, NextNonce: 9}, {Id: "operator-b", Address: imageB.Intents[0].From, FirstNonce: 11, NextNonce: 13}},
		Databases: []DatabaseSource{{Id: "database-a", Connection: FileReference{Path: filepath.Join(root, "database-a.url"), Sha256: digest([]byte("synthetic-a"))}, Roles: []string{"operator-a"}},
			{Id: "database-b", Connection: FileReference{Path: filepath.Join(root, "database-b.url"), Sha256: digest([]byte("synthetic-b"))}, Roles: []string{"operator-b"}}},
		Stores: []StoreSource{{Id: "evidence", Directory: store, Roles: []string{"operator-a", "operator-b"}}},
		Limits: Limits{MaximumIntents: 32, MaximumAttempts: 64, MaximumTransactionBytes: 4096, MaximumTotalBytes: 64 * 1024}}
	return config, &censusFixtureReader{images: map[string]*DatabaseSnapshot{"database-a": imageA, "database-b": imageB}, calls: []string{}}
}

// The old active-only discovery loses every terminal signed intent; the full
// two-source census finds original, replacement, cancellation and store-only bytes.
func TestCensusDiscoversTerminalGenerationsAcrossDatabasesAndStore(t *testing.T) {
	config, reader := censusTestFixture(t)
	before := objectDigest(reader.images)
	activeSigned := 0
	for _, source := range reader.images {
		for _, intent := range source.Intents {
			if !contains([]string{"finalized", "reverted", "canceled", "superseded"}, intent.Status) {
				activeSigned += intent.AttemptCount
			}
		}
	}
	if activeSigned != 0 {
		t.Fatal("causal control unexpectedly discovers terminal signatures")
	}
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Transactions) != 6 || len(archive.Unsigned) != 1 || len(archive.Inspect().Missing) != 4 || archive.OpaqueNativeFiles != 1 || len(reader.calls) != 2 || objectDigest(reader.images) != before {
		t.Fatalf("incomplete or mutated source census: %+v", archive.Inspect())
	}
	if archive.Fees[0].AllSignaturesMaximumFee != "15450000" || archive.Fees[0].DistinctNoncesMaximumFee != "9300000" || archive.Fees[1].AllSignaturesMaximumFee != "6000000" {
		t.Fatalf("wrong maximum fee envelopes: %+v", archive.Fees)
	}
	if archive.Inspect().SpendingAuthorized || archive.Inspect().CanonicalReceiptsReconciled || archive.Inspect().ActualFeesReconciled {
		t.Fatal("custody was promoted to chain evidence or spend authority")
	}
	for _, tx := range archive.Transactions {
		decoded, _, err := decodeTransaction(tx.Raw, config.ChainId)
		if err != nil || decoded.Hash().Hex() != tx.Hash {
			t.Fatalf("signature changed: %v", err)
		}
	}
	if err := archive.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Every historical terminal label receives the same discovery treatment.
func TestCensusIgnoresIntentAndAttemptStatusForDiscovery(t *testing.T) {
	for _, status := range []string{"finalized", "reverted", "canceled", "superseded", "failed", "uncertain", "mined", "signed", "broadcast"} {
		config, reader := censusTestFixture(t)
		for _, image := range reader.images {
			for i := range image.Intents {
				if image.Intents[i].AttemptCount > 0 {
					image.Intents[i].Status = status
				}
			}
			for i := range image.Attempts {
				image.Attempts[i].Status = status
			}
		}
		archive, err := Collect(context.Background(), config, reader)
		if err != nil || len(archive.Transactions) != 6 {
			t.Fatalf("status %s lost signatures: %v", status, err)
		}
	}
}

// Equal bytes from another database add provenance without duplicate fee exposure.
func TestCensusDeduplicatesExactBytesAndKeepsEverySource(t *testing.T) {
	config, reader := censusTestFixture(t)
	copy := *reader.images["database-a"]
	reader.images["database-b"].Intents = append(reader.images["database-b"].Intents, copy.Intents...)
	reader.images["database-b"].Attempts = append(reader.images["database-b"].Attempts, copy.Attempts...)
	config.Databases[1].Roles = append(config.Databases[1].Roles, "operator-a")
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Transactions) != 6 || archive.Fees[0].AllSignaturesMaximumFee != "15450000" {
		t.Fatal("duplicate custody inflated fees")
	}
	for _, tx := range archive.Transactions {
		if tx.Hash == copy.Attempts[0].Hash && len(tx.Origins) != 3 {
			t.Fatalf("lost provenance: %+v", tx.Origins)
		}
	}
}

// Partial sources, row bounds and nonce holes never return a usable archive.
func TestCensusRefusesIncompleteSourcesAndGlobalBounds(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config, *censusFixtureReader)
	}{
		{name: "missing second source", change: func(_ *Config, r *censusFixtureReader) { r.failSource = "database-b" }},
		{name: "nil source", change: func(_ *Config, r *censusFixtureReader) { delete(r.images, "database-b") }},
		{name: "global rows", change: func(c *Config, _ *censusFixtureReader) { c.Limits.MaximumIntents = 3 }},
		{name: "global attempts", change: func(c *Config, _ *censusFixtureReader) { c.Limits.MaximumAttempts = 4 }},
		{name: "global raw bytes", change: func(c *Config, _ *censusFixtureReader) {
			c.Limits.MaximumTransactionBytes = 256
			c.Limits.MaximumTotalBytes = 300
		}},
		{name: "expected nonce", change: func(c *Config, _ *censusFixtureReader) { c.Roles[0].NextNonce++ }},
		{name: "unsigned nonce is not coverage", change: func(c *Config, _ *censusFixtureReader) { c.Roles[1].NextNonce++ }},
		{name: "missing original", change: func(_ *Config, r *censusFixtureReader) {
			r.images["database-a"].Attempts = r.images["database-a"].Attempts[1:]
		}},
		{name: "orphan attempt", change: func(_ *Config, r *censusFixtureReader) {
			r.images["database-a"].Attempts[0].IntentId = "missing-intent"
		}},
	}
	for _, item := range cases {
		config, reader := censusTestFixture(t)
		item.change(&config, reader)
		archive, err := Collect(context.Background(), config, reader)
		if err == nil || archive != nil {
			t.Fatalf("%s returned usable partial census", item.name)
		}
	}
}

// A valid sibling signature cannot be transplanted under another intent, fee,
// hash or cancellation label even when its raw bytes decode successfully.
func TestCensusRefusesDurableSignatureConflicts(t *testing.T) {
	cases := []struct {
		name   string
		change func(*DatabaseSnapshot)
	}{
		{name: "hash", change: func(s *DatabaseSnapshot) { s.Attempts[0].Hash = s.Attempts[1].Hash }},
		{name: "gas", change: func(s *DatabaseSnapshot) { s.Attempts[0].GasLimit++ }},
		{name: "price", change: func(s *DatabaseSnapshot) { v := "99"; s.Attempts[0].GasPrice = &v }},
		{name: "data", change: func(s *DatabaseSnapshot) {
			s.Intents[0].Calldata = []byte{9}
			s.Intents[0].CalldataHash = crypto.Keccak256Hash([]byte{9}).Hex()
		}},
		{name: "nonce", change: func(s *DatabaseSnapshot) { s.Intents[0].Nonce++ }},
		{name: "cancellation", change: func(s *DatabaseSnapshot) { s.Attempts[0].Kind = "cancellation" }},
		{name: "current", change: func(s *DatabaseSnapshot) { v := "0x" + strings.Repeat("d", 64); s.Intents[0].CurrentHash = &v }},
		{name: "duplicate number", change: func(s *DatabaseSnapshot) { s.Attempts[1].Number = 1 }},
		{name: "raw", change: func(s *DatabaseSnapshot) { s.Attempts[0].Raw = []byte{1, 2, 3} }},
		{name: "metadata hiding sender", change: func(s *DatabaseSnapshot) { s.Intents[0].From = "0x" + strings.Repeat("d", 40) }},
	}
	for _, item := range cases {
		config, reader := censusTestFixture(t)
		item.change(reader.images["database-a"])
		archive, err := Collect(context.Background(), config, reader)
		if err == nil || archive != nil {
			t.Fatalf("%s admitted conflicting custody", item.name)
		}
		var refusal *Refusal
		if !errors.As(err, &refusal) || refusal.Source != "database-a" || refusal.Record == "" {
			t.Fatalf("%s lost refusal provenance: %v", item.name, err)
		}
	}
}

// Shared stores may retain other operators; exclusion is explicit and raw
// provenance survives, but those signatures never enter selected restoration.
func TestCensusRetainsUnselectedHistoryWithoutRestoringIt(t *testing.T) {
	config, reader := censusTestFixture(t)
	key := censusTestKey(t, 3)
	intent := censusTestIntent(key, 9, 1, "finalized")
	image := &DatabaseSnapshot{Intents: []Intent{intent}, Attempts: []Attempt{}}
	censusTestAddAttempt(t, image, 0, key, "execution", false, 100)
	reader.images["database-a"].Intents = append(reader.images["database-a"].Intents, image.Intents...)
	reader.images["database-a"].Attempts = append(reader.images["database-a"].Attempts, image.Attempts...)
	filename := strings.TrimPrefix(image.Attempts[0].Hash, "0x") + ".rlp"
	if err := os.WriteFile(filepath.Join(config.Stores[0].Directory, filename), image.Attempts[0].Raw, 0600); err != nil {
		t.Fatal(err)
	}
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Transactions) != 6 || len(archive.Excluded) != 2 {
		t.Fatalf("unselected history lost or promoted: %+v", archive.Inspect())
	}
	if !bytes.Equal(archive.Databases[0].Snapshot.Attempts[4].Raw, image.Attempts[0].Raw) {
		t.Fatal("excluded original bytes were lost")
	}
}

// A refreshed digest cannot conceal tampered derived summaries: replay uses
// the archived original source records and reconstructs the expected projection.
func TestCensusArchiveReplaysSourcesAndRejectsResealedSummary(t *testing.T) {
	config, reader := censusTestFixture(t)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(censusTestDir(t), "census.json")
	if err := WriteArchive(context.Background(), path, archive); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadArchive(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(archive, loaded) {
		t.Fatal("archive replay changed provenance")
	}
	if err := WriteArchive(context.Background(), path, loaded); err != nil {
		t.Fatalf("identical restart: %v", err)
	}
	loaded.Fees[0].AllSignaturesMaximumFee = "1"
	loaded.CensusHash = loaded.hash()
	if err := loaded.Validate(context.Background()); err == nil {
		t.Fatal("resealed fee summary bypassed source replay")
	}
	loaded.Fees = archive.Fees
	loaded.Transactions = loaded.Transactions[:len(loaded.Transactions)-1]
	loaded.CensusHash = loaded.hash()
	if err := loaded.Validate(context.Background()); err == nil {
		t.Fatal("resealed omission bypassed source replay")
	}
}

// Missing files are restored from archived raw bytes; all originals and native
// evidence remain untouched and a second invocation performs zero additions.
func TestCensusRestoreOriginalBytesIsIdempotent(t *testing.T) {
	config, reader := censusTestFixture(t)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(config.Stores[0].Directory, strings.Repeat("b", 64)+".scale")
	nativeBefore, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Restore(context.Background(), archive, config.Stores[0].Directory, archive.CensusHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 4 || len(result.AlreadyPresent) != 2 {
		t.Fatalf("wrong restoration: %+v", result)
	}
	for _, tx := range archive.Transactions {
		raw, err := os.ReadFile(filepath.Join(config.Stores[0].Directory, strings.TrimPrefix(tx.Hash, "0x")+".rlp"))
		if err != nil || !bytes.Equal(raw, tx.Raw) {
			t.Fatalf("signature changed: %v", err)
		}
	}
	nativeAfter, err := os.ReadFile(native)
	if err != nil || !bytes.Equal(nativeBefore, nativeAfter) {
		t.Fatal("native evidence changed")
	}
	result, err = Restore(context.Background(), archive, config.Stores[0].Directory, archive.CensusHash)
	if err != nil || len(result.Created) != 0 || len(result.AlreadyPresent) != 6 {
		t.Fatalf("restart duplicated local actions: %+v %v", result, err)
	}
}

// An explicit barrier stops immediately after one durable atomic publication.
// A fresh owner resumes the same seal and never rewrites that first signature.
func TestCensusRestoreResumesInterruptedDurablePrefix(t *testing.T) {
	config, reader := censusTestFixture(t)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	destination := censusTestDir(t)
	interrupted := errors.New("synthetic interruption after durable file")
	result, err := restore(context.Background(), archive, destination, archive.CensusHash, func(string) error { return interrupted })
	if !errors.Is(err, interrupted) || len(result.Created) != 1 {
		t.Fatalf("barrier did not stop one-file prefix: %+v %v", result, err)
	}
	first := filepath.Join(destination, strings.TrimPrefix(result.Created[0], "0x")+".rlp")
	before, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Restore(context.Background(), archive, destination, archive.CensusHash)
	if err != nil || len(result.Created) != 5 || len(result.AlreadyPresent) != 1 {
		t.Fatalf("resume failed: %+v %v", result, err)
	}
	after, err := os.Stat(first)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("restart replaced original inode")
	}
}

// A conflict sorted after missing files is detected in preflight, before any
// signature can be published. Wrong review pins likewise have no local effect.
func TestCensusRestoreConflictRefusesBeforeAnyAddition(t *testing.T) {
	config, reader := censusTestFixture(t)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	destination := censusTestDir(t)
	last := archive.Transactions[len(archive.Transactions)-1]
	path := filepath.Join(destination, strings.TrimPrefix(last.Hash, "0x")+".rlp")
	conflict := []byte("synthetic conflicting original")
	if err := os.WriteFile(path, conflict, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Restore(context.Background(), archive, destination, archive.CensusHash)
	if err == nil || result != nil {
		t.Fatal("conflicting archive published a prefix")
	}
	files, err := os.ReadDir(destination)
	if err != nil || len(files) != 1 {
		t.Fatal("refused destination changed")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, conflict) {
		t.Fatal("conflicting evidence was overwritten")
	}
	if _, err := Restore(context.Background(), archive, destination, digest([]byte("wrong review"))); err == nil {
		t.Fatal("wrong review pin admitted")
	}
}

// Caller cancellation cannot return a complete collection or start restoration.
func TestCensusCanceledOwnerCannotPublish(t *testing.T) {
	config, reader := censusTestFixture(t)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader.calls = nil
	if result, err := Collect(ctx, config, reader); !errors.Is(err, context.Canceled) || result != nil || len(reader.calls) != 0 {
		t.Fatal("canceled collector reached a source")
	}
	destination := censusTestDir(t)
	if _, err := Restore(ctx, archive, destination, archive.CensusHash); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled restoration: %v", err)
	}
	files, err := os.ReadDir(destination)
	if err != nil || len(files) != 0 {
		t.Fatal("canceled restoration published bytes")
	}
}
