package controller

// sn_ledger_v2_handler_test.go — the public ledger endpoints (VERSION2-FINAL.md
// §8.3) over ledgers built by the ledger package, published to a blob store
// and mirrored into the real database: SV-3.

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
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// ledgerV2StoredAccounts reads a ledger's accounts from its published header
// and account chunks, as the operator's builder does, and counts the reads,
// each of which takes at least delay.
type ledgerV2StoredAccounts struct {
	reads atomic.Int64
	delay time.Duration
}

func (self *ledgerV2StoredAccounts) Build(context.Context, *StLedgerV2BuildInput) (*StLedgerV2Build, error) {
	return nil, errors.New("the endpoint tests build ledgers with the ledger package")
}

func (self *ledgerV2StoredAccounts) Accounts(ctx context.Context, ledgerHash [32]byte) ([]merkle.LedgerAccount, error) {
	self.reads.Add(1)
	time.Sleep(self.delay)
	header, err := GetStLedgerV2Object(ctx, ledger.Ref(ledgerHash).String())
	if err != nil {
		return nil, err
	}
	decoded, err := ledger.DecodeHeader(header)
	if err != nil {
		return nil, err
	}
	chunks := [][]byte{}
	for _, ref := range decoded.Accounts {
		chunk, err := GetStLedgerV2Object(ctx, ref.String())
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return ledger.DecodeAccountChunks(chunks)
}

// The endpoint fixture's coldkeys. A, B and C earn; H only as a head-excluded
// provider; Z's share rounds to zero.
var (
	ledgerV2A = ledgerV2Coldkey(0x0a)
	ledgerV2B = ledgerV2Coldkey(0x0b)
	ledgerV2C = ledgerV2Coldkey(0x0c)
	ledgerV2H = ledgerV2Coldkey(0x0e)
	ledgerV2Z = ledgerV2Coldkey(0x0f)
)

func ledgerV2ClientId(marker byte) [16]byte {
	var clientId [16]byte
	clientId[0] = marker
	return clientId
}

// ledgerV2EndpointFixture holds, for pool 1, finalized root 0 of epoch 5 (A
// 600 and B 400, recorded) and pending root 1 of epoch 6 (A 1,600, B 400, and
// C 1,000), and for pool 2 finalized root 0 of epoch 9 (A 777, recorded). The
// objects of all three are published; vault2 has paid A 250 and B 400 in
// pool 1.
type ledgerV2EndpointFixture struct {
	t       testing.TB
	ctx     context.Context
	cfg     *StConfig
	chain   *fakeLedgerV2Chain
	store   server.BlobStore
	builder *ledgerV2StoredAccounts
	root0   *ledger.Ledger
	root1   *ledger.Ledger
	pool2   *ledger.Ledger
	objects map[string][]byte
}

func newLedgerV2EndpointFixture(t testing.TB) *ledgerV2EndpointFixture {
	f := &ledgerV2EndpointFixture{t: t, ctx: context.Background(), cfg: ledgerV2TestConfig(), builder: &ledgerV2StoredAccounts{}, objects: map[string][]byte{}}
	f.chain = newFakeLedgerV2Chain(&[]string{})
	f.store = server.NewLocalBlobStore(t.TempDir(), "test")
	SetStConfig(f.cfg)
	SetStLedgerV2Client(f.chain)
	SetStLedgerBuilder(f.builder)
	SetStLedgerV2ObjectStore(f.store)
	resetLedgerV2Caches()
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStLedgerV2Client(nil)
		SetStLedgerBuilder(nil)
		SetStLedgerV2ObjectStore(nil)
		resetLedgerV2Caches()
	})

	// Epoch 5 allocates 1,000 rao: H is head-excluded, the zero coldkey has
	// no wallet, and Z's share rounds to zero, so only A and B get accounts.
	f.root0 = f.build(1, 5, 5, nil, 1_000, []protocol.LedgerUsageRow{
		{ClientID: ledgerV2ClientId(1), Coldkey: ledgerV2A, UsageBytes: 600_000},
		{ClientID: ledgerV2ClientId(2), Coldkey: ledgerV2B, UsageBytes: 400_000},
		{ClientID: ledgerV2ClientId(3), Coldkey: ledgerV2H, UsageBytes: 5_000, HeadExcluded: true},
		{ClientID: ledgerV2ClientId(4), Coldkey: ledgerV2Z, UsageBytes: 1},
		{ClientID: ledgerV2ClientId(5), UsageBytes: 1_000},
	})
	f.root1 = f.build(1, 6, 5, f.root0, 3_000, []protocol.LedgerUsageRow{
		{ClientID: ledgerV2ClientId(1), Coldkey: ledgerV2A, UsageBytes: 1},
		{ClientID: ledgerV2ClientId(6), Coldkey: ledgerV2C, UsageBytes: 1},
	})
	f.pool2 = f.build(2, 9, 9, nil, 777, []protocol.LedgerUsageRow{
		{ClientID: ledgerV2ClientId(1), Coldkey: ledgerV2A, UsageBytes: 5},
	})
	if !reflect.DeepEqual(f.root0.Accounts, ledgerV2Accounts(ledgerV2A, 600, ledgerV2B, 400)) ||
		!reflect.DeepEqual(f.root1.Accounts, ledgerV2Accounts(ledgerV2A, 1600, ledgerV2B, 400, ledgerV2C, 1000)) ||
		!reflect.DeepEqual(f.pool2.Accounts, ledgerV2Accounts(ledgerV2A, 777)) {
		t.Fatalf("built accounts %v, %v, %v", f.root0.Accounts, f.root1.Accounts, f.pool2.Accounts)
	}

	zero := uint64(0)
	f.mirror(1, 1, f.root0, &zero)
	f.record(1, 1, 0, ledgerV2A, ledgerV2B)
	f.mirror(1, 2, f.root1, nil)
	f.mirror(2, 1, f.pool2, &zero)
	f.record(2, 1, 0, ledgerV2A)
	f.chain.claimed[[2]uint64{1, 0}] = *uint256.NewInt(250)
	f.chain.claimed[[2]uint64{1, 1}] = *uint256.NewInt(400)
	return f
}

// build builds a single-epoch root and publishes its objects.
func (self *ledgerV2EndpointFixture) build(noId uint64, epoch uint64, firstEpoch uint64, previous *ledger.Ledger, fundedThrough uint64, rows []protocol.LedgerUsageRow) *ledger.Ledger {
	self.t.Helper()
	input := ledger.BuildInput{
		NoID: noId, Epoch: epoch, FirstEpoch: firstEpoch,
		Epochs:        []ledger.EpochInput{{Epoch: epoch, Rows: rows}},
		FundedThrough: map[uint64]uint256.Int{epoch: *uint256.NewInt(fundedThrough)},
	}
	if previous != nil {
		input.Previous = &ledger.Previous{Root: previous.Commit, Accounts: previous.Accounts}
	}
	built, err := ledger.Build(self.ctx, input)
	if err != nil {
		self.t.Fatal(err)
	}
	for ref, object := range built.Objects {
		published, err := PutStLedgerV2Object(self.ctx, object)
		if err != nil || published != ref.String() {
			self.t.Fatalf("publish %s: %s, %v", ref, published, err)
		}
		self.objects[published] = object
	}
	return built
}

// mirror mirrors a commit of a built root, and its finalization at
// rootIndex unless that is nil.
func (self *ledgerV2EndpointFixture) mirror(noId uint64, seq uint64, built *ledger.Ledger, rootIndex *uint64) {
	key := self.cfg.DeploymentKey()
	root := &model.StLedgerV2Root{NoId: noId, Seq: seq, Epoch: built.Commit.Epoch, Root: built.Commit.Root, Total: built.Commit.Total, LeafCount: built.Commit.LeafCount, LedgerHash: built.Commit.LedgerHash}
	model.AddStLedgerV2Commit(self.ctx, key, root, 1_000*seq, 0)
	if rootIndex != nil {
		root.RootIndex = rootIndex
		model.SetStLedgerV2RootFinalized(self.ctx, key, root, 1_000*seq+500)
	}
}

func (self *ledgerV2EndpointFixture) record(noId uint64, seq uint64, firstIndex uint64, coldkeys ...[32]byte) {
	self.t.Helper()
	if err := model.RecordStLedgerV2Root(self.ctx, self.cfg.DeploymentKey(), noId, seq, firstIndex, coldkeys, nil); err != nil {
		self.t.Fatal(err)
	}
}

func serveLedgerV2(handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodGet, target, nil))
	return response
}

func getLedgerV2Account(coldkey string) *httptest.ResponseRecorder {
	return serveLedgerV2(SnLedgerAccount, "/sn/ledger/account?coldkey="+url.QueryEscape(coldkey))
}

func requireLedgerV2Answer(t testing.TB, response *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if response.Code != status || strings.TrimSpace(response.Body.String()) != body {
		t.Fatalf("answer %d %q\nwant   %d %q", response.Code, response.Body.String(), status, body)
	}
}

func ledgerV2Hex(coldkey [32]byte) string {
	return "0x" + hex.EncodeToString(coldkey[:])
}

func ledgerV2Ss58(t testing.TB, coldkey [32]byte) string {
	t.Helper()
	address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

// requireLedgerV2ProofsVerify checks every finalized entry's proof against
// the mirrored root it names.
func requireLedgerV2ProofsVerify(t testing.TB, f *ledgerV2EndpointFixture, body []byte) int {
	t.Helper()
	var answer StLedgerV2AccountsResult
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	coldkey, err := ParseStLedgerV2Coldkey(answer.Coldkey)
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, entry := range answer.Accounts {
		if entry.RootIndex == nil {
			continue
		}
		root := model.GetStLedgerV2FinalizedRoot(f.ctx, f.cfg.DeploymentKey(), entry.NoId, *entry.RootIndex)
		cumulative, err := uint256.FromDecimal(entry.Cumulative)
		if err != nil || root == nil {
			t.Fatalf("entry %+v: root %+v, %v", entry, root, err)
		}
		proof := make([]merkle.LedgerNode, len(entry.Proof))
		for k, node := range entry.Proof {
			hash, err := hex.DecodeString(strings.TrimPrefix(node.Hash, "0x"))
			sum, sumErr := uint256.FromDecimal(node.Sum)
			if err != nil || sumErr != nil || len(hash) != 32 {
				t.Fatalf("proof node %+v", node)
			}
			proof[k] = merkle.LedgerNode{Hash: [32]byte(hash), Sum: *sum}
		}
		if !merkle.VerifyLedgerProof(root.Root, &root.Total, root.LeafCount, entry.Index, coldkey, cumulative, proof) {
			t.Fatalf("pool %d index %d proof does not verify against root %d", entry.NoId, entry.Index, *entry.RootIndex)
		}
		verified++
	}
	return verified
}

// SV-3 (object immutability): every published object is served as its exact
// bytes, as an immutable octet stream, as often as asked and after it is
// published again; bytes whose sha256 differs from the hash are never served.
func TestSnLedgerObjectServesImmutableBytes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		if len(f.objects) != 9 {
			t.Fatalf("published %d objects", len(f.objects))
		}
		for ref, object := range f.objects {
			for range 2 {
				response := serveLedgerV2(SnLedgerObject, "/sn/ledger/object?hash="+ref)
				if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), object) {
					t.Fatalf("%s: %d %q", ref, response.Code, response.Body.Bytes())
				}
				header := response.Header()
				if header.Get("Content-Type") != "application/octet-stream" || header.Get("Cache-Control") != "public, immutable, max-age=31536000" || header.Get("ETag") != `"`+ref+`"` {
					t.Fatalf("%s headers %v", ref, header)
				}
				if again, err := PutStLedgerV2Object(f.ctx, object); err != nil || again != ref {
					t.Fatalf("republish %s: %s, %v", ref, again, err)
				}
			}
		}

		// A store holding other bytes under a reference serves nothing for it.
		header := ledger.Ref(f.root0.Commit.LedgerHash).String()
		tampered := server.NewLocalBlobStore(t.TempDir(), "test")
		other := filepath.Join(t.TempDir(), "other")
		if err := os.WriteFile(other, []byte("other bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := tampered.PutIfAbsent(f.ctx, "test/st/v2/ledger/sha256/"+strings.TrimPrefix(header, "sha256:"), other, "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		SetStLedgerV2ObjectStore(tampered)
		requireLedgerV2Answer(t, serveLedgerV2(SnLedgerObject, "/sn/ledger/object?hash="+header), http.StatusBadGateway, "Ledger object unavailable or failed integrity.")
	})
}

// SV-3 (malformed input): a reference that is not sha256: and 64 lowercase
// hex digits is a 400; a well-formed reference to no object is a 404.
func TestSnLedgerObjectRefusesMalformedAndAbsentReferences(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		newLedgerV2EndpointFixture(t)
		digits := strings.Repeat("ab", 32)
		for _, ref := range []string{"", digits, "sha256:", "sha256:" + digits[:63], "sha256:" + digits + "a", "sha256:" + strings.ToUpper(digits), "sha256:" + strings.Repeat("zz", 32), "sha1:" + digits, " sha256:" + digits} {
			response := serveLedgerV2(SnLedgerObject, "/sn/ledger/object?hash="+url.QueryEscape(ref))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "is not sha256: and 64 lowercase hex digits") {
				t.Fatalf("%q: %d %q", ref, response.Code, response.Body.String())
			}
		}
		requireLedgerV2Answer(t, serveLedgerV2(SnLedgerObject, "/sn/ledger/object?hash=sha256:"+digits), http.StatusNotFound, "Ledger object not found.")
	})
}

// SV-3 (ss58 and hex coldkeys, several pools, account proofs verify): a
// coldkey's answer lists its account in each pool by no_id, the same for its
// ss58 address and its hex in either case. A finalized account has its root
// index, cumulative and bottom-up proof, which verifies against the stored
// root ([] at depth 0), the claimed read from vault2, and the pending
// cumulative when a pending root holds it (null otherwise). An account only
// in the pending root has a null root index and proof and zero cumulative
// and claimed.
func TestSnLedgerAccountServesEveryPool(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		vault := testLedgerV2Vault
		sibling := merkle.LedgerLeafHash(ledgerV2B, uint256.NewInt(400))
		want := fmt.Sprintf(`{"coldkey":"%s","accounts":[`+
			`{"vault":"%s","no_id":1,"index":0,"root_index":0,"cumulative":"600","proof":[{"hash":"0x%x","sum":"400"}],"claimed":"250","pending_cumulative":"1600"},`+
			`{"vault":"%s","no_id":2,"index":0,"root_index":0,"cumulative":"777","proof":[],"claimed":"0","pending_cumulative":null}]}`,
			ledgerV2Hex(ledgerV2A), vault, sibling, vault)
		for _, coldkey := range []string{ledgerV2Ss58(t, ledgerV2A), ledgerV2Hex(ledgerV2A), "0x" + strings.ToUpper(ledgerV2Hex(ledgerV2A)[2:])} {
			response := getLedgerV2Account(coldkey)
			requireLedgerV2Answer(t, response, http.StatusOK, want)
			if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("headers %v", response.Header())
			}
			if verified := requireLedgerV2ProofsVerify(t, f, response.Body.Bytes()); verified != 2 {
				t.Fatalf("%d proofs verified", verified)
			}
		}

		// B is paid in full: claimed equals its cumulative.
		siblingA := merkle.LedgerLeafHash(ledgerV2A, uint256.NewInt(600))
		response := getLedgerV2Account(ledgerV2Hex(ledgerV2B))
		requireLedgerV2Answer(t, response, http.StatusOK, fmt.Sprintf(`{"coldkey":"%s","accounts":[`+
			`{"vault":"%s","no_id":1,"index":1,"root_index":0,"cumulative":"400","proof":[{"hash":"0x%x","sum":"600"}],"claimed":"400","pending_cumulative":"400"}]}`,
			ledgerV2Hex(ledgerV2B), vault, siblingA))
		requireLedgerV2ProofsVerify(t, f, response.Body.Bytes())

		// C is only in the pending root.
		requireLedgerV2Answer(t, getLedgerV2Account(ledgerV2Ss58(t, ledgerV2C)), http.StatusOK, fmt.Sprintf(`{"coldkey":"%s","accounts":[`+
			`{"vault":"%s","no_id":1,"index":2,"root_index":null,"cumulative":"0","proof":null,"claimed":"0","pending_cumulative":"1000"}]}`,
			ledgerV2Hex(ledgerV2C), vault))
	})
}

// SV-3 (zero-delta and head-excluded accounts, unknown coldkey; §6.1): a
// provider that is head-excluded, has no wallet, or whose share rounds to
// zero has no account, and its coldkey, like one the ledger never saw, is
// answered with an empty list, never null. Without settlement v2 every
// coldkey is unknown.
func TestSnLedgerAccountUnknownColdkeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		newLedgerV2EndpointFixture(t)
		for _, coldkey := range [][32]byte{ledgerV2H, ledgerV2Z, {}, ledgerV2Coldkey(0x77)} {
			requireLedgerV2Answer(t, getLedgerV2Account(ledgerV2Ss58(t, coldkey)), http.StatusOK, fmt.Sprintf(`{"coldkey":"%s","accounts":[]}`, ledgerV2Hex(coldkey)))
		}

		cfg := ledgerV2TestConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(cfg)
		requireLedgerV2Answer(t, getLedgerV2Account(ledgerV2Hex(ledgerV2A)), http.StatusOK, fmt.Sprintf(`{"coldkey":"%s","accounts":[]}`, ledgerV2Hex(ledgerV2A)))
		SetStConfig(nil)
		requireLedgerV2Answer(t, getLedgerV2Account(ledgerV2Hex(ledgerV2A)), http.StatusOK, fmt.Sprintf(`{"coldkey":"%s","accounts":[]}`, ledgerV2Hex(ledgerV2A)))
	})
}

// SV-3: once the pending root finalizes, before its accounts are recorded,
// its new accounts are found after the recorded ones and every account is
// answered from it: root index 1, a proof against it, and no pending
// cumulative.
func TestSnLedgerAccountReadsAFinalizedRootBeforeItIsRecorded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		one := uint64(1)
		f.mirror(1, 2, f.root1, &one)
		f.chain.claimed[[2]uint64{1, 2}] = *uint256.NewInt(100)
		tree := f.root1.Tree
		proof := func(index uint64) string {
			nodes, err := tree.Proof(index)
			if err != nil {
				t.Fatal(err)
			}
			parts := []string{}
			for _, node := range nodes {
				parts = append(parts, fmt.Sprintf(`{"hash":"0x%x","sum":"%s"}`, node.Hash, node.Sum.Dec()))
			}
			return "[" + strings.Join(parts, ",") + "]"
		}
		for _, test := range []struct {
			coldkey [32]byte
			want    string
		}{
			{ledgerV2C, fmt.Sprintf(`{"vault":"%s","no_id":1,"index":2,"root_index":1,"cumulative":"1000","proof":%s,"claimed":"100","pending_cumulative":null}`, testLedgerV2Vault, proof(2))},
			{ledgerV2B, fmt.Sprintf(`{"vault":"%s","no_id":1,"index":1,"root_index":1,"cumulative":"400","proof":%s,"claimed":"400","pending_cumulative":null}`, testLedgerV2Vault, proof(1))},
		} {
			response := getLedgerV2Account(ledgerV2Hex(test.coldkey))
			requireLedgerV2Answer(t, response, http.StatusOK, fmt.Sprintf(`{"coldkey":"%s","accounts":[%s]}`, ledgerV2Hex(test.coldkey), test.want))
			if verified := requireLedgerV2ProofsVerify(t, f, response.Body.Bytes()); verified != 1 {
				t.Fatalf("%d proofs verified", verified)
			}
		}
	})
}

// SV-3 (malformed input): a coldkey that is neither an ss58 address with the
// Bittensor prefix nor 0x and 64 hex digits is a 400.
func TestSnLedgerAccountRefusesMalformedColdkeys(t *testing.T) {
	polkadot, err := ss58.Encode(ledgerV2A, 0)
	if err != nil {
		t.Fatal(err)
	}
	valid := ledgerV2Ss58(t, ledgerV2A)
	digits := ledgerV2Hex(ledgerV2A)[2:]
	for _, coldkey := range []string{"", "0x", "0x" + digits[:63], "0x" + digits + "0", "0x" + strings.Repeat("zz", 32), "0X" + digits, digits, polkadot, valid[:len(valid)-1] + "x", " " + valid, "not an address"} {
		requireLedgerV2Answer(t, getLedgerV2Account(coldkey), http.StatusBadRequest, ErrStLedgerV2Coldkey.Error())
		if _, err := ParseStLedgerV2Coldkey(coldkey); !errors.Is(err, ErrStLedgerV2Coldkey) {
			t.Fatalf("%q: %v", coldkey, err)
		}
	}
}

// SV-3: an answer that cannot be read whole and consistent is a 502, never a
// partial or zero answer: vault2's claimed above the finalized cumulative, a
// failed claimed read, no chain client, a root whose objects cannot be read,
// and an account index that disagrees with the finalized root. An account
// only in the pending root needs no chain read.
func TestSnLedgerAccountRefusesInconsistentAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		unavailable := "Ledger account unavailable."
		a := ledgerV2Hex(ledgerV2A)

		f.chain.claimed[[2]uint64{1, 0}] = *uint256.NewInt(601)
		requireLedgerV2Answer(t, getLedgerV2Account(a), http.StatusBadGateway, unavailable)
		f.chain.claimed[[2]uint64{1, 0}] = *uint256.NewInt(600)
		if response := getLedgerV2Account(a); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cumulative":"600","proof":[{"hash"`) || !strings.Contains(response.Body.String(), `"claimed":"600"`) {
			t.Fatalf("claimed equal to the cumulative: %d %s", response.Code, response.Body.String())
		}

		f.chain.failNext("claimed", errors.New("vault2 claimed(1, 0): rpc down"))
		requireLedgerV2Answer(t, getLedgerV2Account(a), http.StatusBadGateway, unavailable)
		if _, err := StLedgerV2Accounts(f.ctx, ledgerV2A); err != nil {
			t.Fatalf("a read after the failure: %v", err)
		}

		// Neither a v2 client nor the core client is configured.
		SetStLedgerV2Client(nil)
		SetStClient(newStubStClient(&StEpochState{}))
		t.Cleanup(func() { SetStClient(nil) })
		requireLedgerV2Answer(t, getLedgerV2Account(a), http.StatusBadGateway, unavailable)
		if _, err := StLedgerV2Accounts(f.ctx, ledgerV2A); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("no chain client: %v", err)
		}
		if response := getLedgerV2Account(ledgerV2Hex(ledgerV2C)); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"pending_cumulative":"1000"`) {
			t.Fatalf("a pending-only account without a chain client: %d %s", response.Code, response.Body.String())
		}
		SetStClient(nil)
		SetStLedgerV2Client(f.chain)

		// A pending root whose objects are not published.
		missing := *f.root1
		missing.Commit.LedgerHash = [32]byte{0xee}
		model.SetStLedgerV2RootDeleted(f.ctx, f.cfg.DeploymentKey(), 1, 2, 3_000)
		f.mirror(1, 3, &missing, nil)
		resetLedgerV2Caches()
		requireLedgerV2Answer(t, getLedgerV2Account(a), http.StatusBadGateway, unavailable)
		if _, err := StLedgerV2Accounts(f.ctx, ledgerV2A); !errors.Is(err, startifact.ErrLedgerObjectNotFound) {
			t.Fatalf("missing objects: %v", err)
		}
		model.SetStLedgerV2RootDeleted(f.ctx, f.cfg.DeploymentKey(), 1, 3, 4_000)

		// A finalized root whose objects are not published.
		lost := *f.pool2
		lost.Commit.LedgerHash = [32]byte{0xef}
		zero := uint64(0)
		f.mirror(3, 1, &lost, &zero)
		f.record(3, 1, 0, ledgerV2B)
		requireLedgerV2Answer(t, getLedgerV2Account(ledgerV2Hex(ledgerV2B)), http.StatusBadGateway, unavailable)

		// The account index places A at position 1 of pool 4, where the
		// finalized root holds B.
		f.mirror(4, 1, f.root0, &zero)
		f.record(4, 1, 0, ledgerV2B, ledgerV2A)
		if _, err := StLedgerV2Accounts(f.ctx, ledgerV2A); err == nil || err.Error() != fmt.Sprintf("st ledger v2: the finalized root of pool 4 does not hold coldkey 0x%x at its recorded index 1", ledgerV2A) {
			t.Fatalf("a recorded index that disagrees: %v", err)
		}
		requireLedgerV2Answer(t, getLedgerV2Account(a), http.StatusBadGateway, unavailable)
	})
}

// SV-3: requests that arrive together after new roots read each root's
// accounts once, and later requests answer from the cached trees.
func TestSnLedgerAccountBuildsEachTreeOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		want := getLedgerV2Account(ledgerV2Hex(ledgerV2A)).Body.String()
		resetLedgerV2Caches()
		f.builder.reads.Store(0)
		f.builder.delay = 200 * time.Millisecond
		var group sync.WaitGroup
		bodies := make([]string, 8)
		for i := range bodies {
			group.Add(1)
			go func() {
				defer group.Done()
				bodies[i] = getLedgerV2Account(ledgerV2Hex(ledgerV2A)).Body.String()
			}()
		}
		group.Wait()
		for _, body := range bodies {
			if body != want {
				t.Fatalf("answer %q\nwant   %q", body, want)
			}
		}
		// pool 1's finalized and pending roots, and pool 2's finalized root
		if reads := f.builder.reads.Load(); reads != 3 {
			t.Fatalf("%d account reads for 3 roots", reads)
		}
	})
}

// SV-3, CL-2: the SDK's strict parser (sdk/sn_ledger.go) accepts the
// endpoint's real answers over HTTP and shows every state from them:
// finalized, paid, claimable and pending across several pools, an account
// only in the pending root, and an unknown coldkey as an empty list rather
// than an error. The 502 of a ledger that cannot be read reaches the SDK as
// ledger_unavailable with no balances, never as zero.
func TestSnLedgerAccountAnswersParseInTheSdk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		mux := http.NewServeMux()
		mux.HandleFunc("/sn/ledger/account", SnLedgerAccount)
		host := httptest.NewServer(mux)
		t.Cleanup(host.Close)
		fetch := func(coldkey [32]byte) *sdk.SnLedgerAccountsResult {
			settings := sdk.NewSnChainSettings()
			settings.ArtifactBaseUrl = host.URL
			results := make(chan *sdk.SnLedgerAccountsResult, 1)
			sdk.SnLedgerAccountsFor(settings, ledgerV2Ss58(t, coldkey), connect.NewApiCallback(func(result *sdk.SnLedgerAccountsResult, err error) {
				if err != nil {
					t.Errorf("sdk read: %v", err)
				}
				results <- result
			}))
			select {
			case result := <-results:
				return result
			case <-time.After(30 * time.Second):
				t.Fatal("the sdk read did not call back")
				return nil
			}
		}
		read := func(coldkey [32]byte) *sdk.SnLedgerAccountsResult {
			result := fetch(coldkey)
			if result.Error != nil {
				t.Fatalf("the sdk refused the answer: %+v", result.Error)
			}
			return result
		}
		accounts := func(result *sdk.SnLedgerAccountsResult) []sdk.SnLedgerAccount {
			out := []sdk.SnLedgerAccount{}
			for i := range result.Accounts.Len() {
				out = append(out, *result.Accounts.Get(i))
			}
			return out
		}
		vault := testLedgerV2Vault
		for _, test := range []struct {
			coldkey [32]byte
			want    []sdk.SnLedgerAccount
		}{
			{ledgerV2A, []sdk.SnLedgerAccount{
				{Vault: vault, NoId: 1, Index: 0, FinalizedRao: 600, ClaimableRao: 350, PaidRao: 250, Pending: true, PendingRao: 1600},
				{Vault: vault, NoId: 2, Index: 0, FinalizedRao: 777, ClaimableRao: 777},
			}},
			{ledgerV2B, []sdk.SnLedgerAccount{{Vault: vault, NoId: 1, Index: 1, FinalizedRao: 400, PaidRao: 400, Pending: true, PendingRao: 400}}},
			{ledgerV2C, []sdk.SnLedgerAccount{{Vault: vault, NoId: 1, Index: 2, Pending: true, PendingRao: 1000}}},
			{ledgerV2H, []sdk.SnLedgerAccount{}},
		} {
			if got := accounts(read(test.coldkey)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("0x%x: %+v\nwant %+v", test.coldkey, got, test.want)
			}
		}

		f.chain.failNext("claimed", errors.New("vault2 claimed(1, 0): rpc down"))
		result := fetch(ledgerV2A)
		if result.Error == nil || result.Error.Code != sdk.SnErrorCodeLedgerUnavailable || result.Error.Message != "http 502 fetching the ledger account" || result.Accounts.Len() != 0 {
			t.Fatalf("an unavailable ledger in the sdk: %+v, %d accounts", result.Error, result.Accounts.Len())
		}
	})
}

// ledgerV2CallBlocks records the block of each eth_call it forwards.
type ledgerV2CallBlocks struct {
	rpc    *ledgerV2RPC
	lock   sync.Mutex
	blocks []string
}

func (self *ledgerV2CallBlocks) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.rpc.t.Errorf("read RPC request: %v", err)
		return
	}
	var call stEpochRPCRequest
	if json.Unmarshal(body, &call) == nil && call.Method == "eth_call" && len(call.Params) == 2 {
		var block string
		json.Unmarshal(call.Params[1], &block)
		self.lock.Lock()
		self.blocks = append(self.blocks, block)
		self.lock.Unlock()
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	self.rpc.ServeHTTP(writer, request)
}

// SV-3 (the chain read of claimed): the core client reads vault2's
// claimed(noId, index) with eth_call at the finalized head, and the endpoint
// serves what it read; a refused read is an error naming the call, and a 502.
func TestSnLedgerAccountReadsClaimedAtTheFinalizedHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2EndpointFixture(t)
		fixture := newLedgerV2RPC(t)
		fixture.finalizedBlock = 4_242
		calls := &ledgerV2CallBlocks{rpc: fixture}
		listener := httptest.NewServer(calls)
		t.Cleanup(listener.Close)
		rpcClient, err := ethclient.Dial(listener.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(rpcClient.Close)
		cfg := ledgerV2TestConfig()
		cfg.RpcUrls = []string{listener.URL}
		client := &CoreStClient{cfg: cfg, clients: map[string]*ethclient.Client{listener.URL: rpcClient}}
		parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
		if err != nil {
			t.Fatal(err)
		}
		claimedCall := stLedgerV2VaultAbi.PackClaimed(big.NewInt(1), big.NewInt(0))
		answer := func(value int64) []byte {
			out, err := parsed.Methods["claimed"].Outputs.Pack(big.NewInt(value))
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		fixture.results[string(claimedCall)] = answer(250)
		fixture.results[string(stLedgerV2VaultAbi.PackClaimed(big.NewInt(2), big.NewInt(0)))] = answer(77)

		claimed, err := client.LedgerV2Claimed(f.ctx, 1, 0)
		if err != nil || claimed.Uint64() != 250 {
			t.Fatalf("claimed %s, %v", claimed.Dec(), err)
		}
		calls.lock.Lock()
		blocks := calls.blocks
		calls.blocks = nil
		calls.lock.Unlock()
		if !reflect.DeepEqual(blocks, []string{"0x1092"}) {
			t.Fatalf("claimed read at blocks %q, want the finalized block 0x1092", blocks)
		}
		if events := fixture.takeEvents(); !reflect.DeepEqual(events, []string{"call " + hex.EncodeToString(claimedCall[:4])}) {
			t.Fatalf("rpc calls %q", events)
		}

		SetStLedgerV2Client(client)
		response := getLedgerV2Account(ledgerV2Hex(ledgerV2A))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cumulative":"600"`) || !strings.Contains(response.Body.String(), `"claimed":"250","pending_cumulative":"1600"`) || !strings.Contains(response.Body.String(), `"cumulative":"777","proof":[],"claimed":"77"`) {
			t.Fatalf("answer %d %s", response.Code, response.Body.String())
		}

		fixture.reverts[string(claimedCall)] = "UnknownPool"
		if _, err := client.LedgerV2Claimed(f.ctx, 1, 0); err == nil || !strings.Contains(err.Error(), "vault2 claimed(1, 0):") {
			t.Fatalf("a refused read: %v", err)
		}
		requireLedgerV2Answer(t, getLedgerV2Account(ledgerV2Hex(ledgerV2A)), http.StatusBadGateway, "Ledger account unavailable.")
	})
}
