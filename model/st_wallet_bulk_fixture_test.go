// Consent chains for the settlement v2 bulk wallet selection tests. Chains are
// signed with synthetic keys and retained directly in SQL, so a test builds
// every selection branch, including chains the acceptance API refuses.
package model

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// The fixture epoch; scopeAt moves the start block and time with the epoch.
const stWalletBulkEpoch = 100

type stWalletBulkFixture struct {
	domain     protocol.ClientKeyHistoryDomain
	domainHash [32]byte
	subnetHash [32]byte
	rootKey    *ecdsa.PrivateKey
	root       common.Address
	otherKey   *ecdsa.PrivateKey
	boundary   protocol.ClientKeyEffectiveBoundary
	startTime  time.Time

	nonces  atomic.Uint64
	keyLock sync.Mutex
	keys    map[uint32]*schnorrkel.SecretKey

	pendingLock sync.Mutex
	pending     map[string][]stWalletRetainedRow
}

// One retained original. owner is a client or network id, or a hotkey; nonce
// is zero for a global hotkey consent, which has no challenge.
type stWalletRetainedRow struct {
	owner      []byte
	generation uint64
	hash       [32]byte
	nonce      [32]byte
	raw        []byte
	message    string
	expiresAt  int64
}

func newStWalletBulkFixture(t testing.TB) *stWalletBulkFixture {
	t.Helper()
	rootKey, err := crypto.HexToECDSA(strings.Repeat("51", 32))
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := crypto.HexToECDSA(strings.Repeat("52", 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &stWalletBulkFixture{
		domain: protocol.ClientKeyHistoryDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 7, Coordinator: common.Address{2},
			SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte("st-wallet-bulk")), PolicyHash: [32]byte{5}, NoID: 1},
		rootKey: rootKey, root: crypto.PubkeyToAddress(rootKey.PublicKey), otherKey: otherKey,
		boundary:  protocol.ClientKeyEffectiveBoundary{Epoch: 90, Block: 900, Hash: [32]byte{9}},
		startTime: time.Unix(1_800_000_000, 0).UTC(),
		keys:      map[uint32]*schnorrkel.SecretKey{},
		pending:   map[string][]stWalletRetainedRow{},
	}
	f.domainHash, err = f.domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.subnetHash, err = hotkeyWalletMappingSubnetHash(f.domain.HotkeySubnet())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The fixture scope at epoch: 100 blocks and one hour per epoch after the
// fixture epoch.
func (self *stWalletBulkFixture) scopeAt(epoch uint64) StProviderWalletEpochScope {
	offset := epoch - stWalletBulkEpoch
	start := 1000 + 100*offset
	return StProviderWalletEpochScope{
		Domain: self.domain, Epoch: epoch,
		Start:     payoutartifact.Boundary{Number: start, Hash: common.BigToHash(new(big.Int).SetUint64(start)).Hex()},
		End:       payoutartifact.Boundary{Number: start + 100, Hash: common.BigToHash(new(big.Int).SetUint64(start + 100)).Hex()},
		StartTime: self.startTime.Add(time.Duration(offset) * time.Hour),
	}
}

// A deterministic sr25519 key per seed, expanded once.
func (self *stWalletBulkFixture) key(t testing.TB, seed uint32) *schnorrkel.SecretKey {
	self.keyLock.Lock()
	defer self.keyLock.Unlock()
	if key := self.keys[seed]; key != nil {
		return key
	}
	var raw [32]byte
	binary.BigEndian.PutUint32(raw[:4], seed)
	raw[31] = 0x5a
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	key := mini.ExpandEd25519()
	self.keys[seed] = key
	return key
}

func (self *stWalletBulkFixture) public(t testing.TB, seed uint32) [32]byte {
	public, err := self.key(t, seed).Public()
	if err != nil {
		t.Fatal(err)
	}
	return public.Encode()
}

func (self *stWalletBulkFixture) sign(t testing.TB, seed uint32, message string) [64]byte {
	signature, err := self.key(t, seed).Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	return signature.Encode()
}

func (self *stWalletBulkFixture) nonce() [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "st-wallet-bulk-nonce/%d", self.nonces.Add(1)))
}

// One generation of a chain. Zero fields take the fixture's defaults.
type stWalletConsentSpec struct {
	// the signing coldkey; for a delegation, the delegated hotkey
	key     uint32
	from    uint64
	through uint64
	// a provider consent of the first schema, without a prospective approval
	legacy    bool
	rootKey   *ecdsa.PrivateKey
	boundary  *protocol.ClientKeyEffectiveBoundary
	expiresAt int64
	// a provider consent naming another network
	networkId *server.Id
	// a delegation's pinned global head
	consentHead       [32]byte
	consentGeneration uint64
	// corruptions the acceptance API refuses
	badSignature bool
	badLink      bool
	hashColumn   *[32]byte
}

func (self *stWalletBulkFixture) defaults(spec stWalletConsentSpec) stWalletConsentSpec {
	if spec.rootKey == nil {
		spec.rootKey = self.rootKey
	}
	if spec.boundary == nil {
		spec.boundary = &self.boundary
	}
	if spec.expiresAt == 0 {
		spec.expiresAt = self.startTime.Unix() - 60
	}
	if spec.through == 0 {
		spec.through = spec.from + 1000
	}
	return spec
}

// Links one generation and records its row; returns the generation's hash.
func (self *stWalletBulkFixture) retain(t testing.TB, kind string, owner []byte, generation uint64, spec stWalletConsentSpec, message string, signed any, nonce [32]byte) [32]byte {
	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	column := hash
	if spec.hashColumn != nil {
		column = *spec.hashColumn
	}
	self.pendingLock.Lock()
	self.pending[kind] = append(self.pending[kind], stWalletRetainedRow{owner: owner, generation: generation, hash: column, nonce: nonce, raw: raw, message: message, expiresAt: spec.expiresAt})
	self.pendingLock.Unlock()
	return column
}

// The previous hash a generation links, or a wrong one.
func stWalletBulkPrevious(previous [32]byte, spec stWalletConsentSpec) [32]byte {
	if spec.badLink {
		previous[0] ^= 0xff
	}
	return previous
}

func (self *stWalletBulkFixture) signature(t testing.TB, seed uint32, message string, bad bool) [64]byte {
	signature := self.sign(t, seed, message)
	if bad {
		signature = self.sign(t, seed+1_000_000, message)
	}
	return signature
}

// A provider's chain; returns each generation's head hash.
func (self *stWalletBulkFixture) providerChain(t testing.TB, clientId server.Id, networkId server.Id, specs ...stWalletConsentSpec) [][32]byte {
	t.Helper()
	heads := [][32]byte{}
	var previous [32]byte
	for index, spec := range specs {
		spec = self.defaults(spec)
		named := networkId
		if spec.networkId != nil {
			named = *spec.networkId
		}
		statement := protocol.WalletMappingStatement{Schema: protocol.WalletMappingConsentSchema, Domain: self.domain, UserId: [16]byte{1}, ClientId: [16]byte(clientId), NetworkId: [16]byte(named),
			Coldkey: self.public(t, spec.key), Generation: uint64(index) + 1, PreviousHash: stWalletBulkPrevious(previous, spec), Nonce: self.nonce(),
			IssuedAt: spec.expiresAt - 120, ExpiresAt: spec.expiresAt, FromEpoch: spec.from, ThroughEpoch: spec.through}
		if !spec.legacy {
			if err := protocol.SignProspectiveWalletMapping(&statement, *spec.boundary, spec.rootKey); err != nil {
				t.Fatal(err)
			}
		}
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.WalletMappingConsent{Message: message, Signature: self.signature(t, spec.key, message, spec.badSignature)}
		previous = self.retain(t, stWalletChainProvider, clientId.Bytes(), uint64(index)+1, spec, message, original, statement.Nonce)
		heads = append(heads, previous)
	}
	return heads
}

// A network's consent chain.
func (self *stWalletBulkFixture) networkChain(t testing.TB, networkId server.Id, specs ...stWalletConsentSpec) [][32]byte {
	t.Helper()
	heads := [][32]byte{}
	var previous [32]byte
	for index, spec := range specs {
		spec = self.defaults(spec)
		statement := protocol.NetworkWalletMappingStatement{Domain: self.domain, UserId: [16]byte{1}, NetworkId: [16]byte(networkId), Coldkey: self.public(t, spec.key),
			Generation: uint64(index) + 1, PreviousHash: stWalletBulkPrevious(previous, spec), Nonce: self.nonce(),
			IssuedAt: spec.expiresAt - 120, ExpiresAt: spec.expiresAt, FromEpoch: spec.from, ThroughEpoch: spec.through}
		if err := protocol.SignProspectiveNetworkWalletMapping(&statement, *spec.boundary, spec.rootKey); err != nil {
			t.Fatal(err)
		}
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.WalletMappingConsent{Message: message, Signature: self.signature(t, spec.key, message, spec.badSignature)}
		previous = self.retain(t, stWalletChainNetwork, networkId.Bytes(), uint64(index)+1, spec, message, original, statement.Nonce)
		heads = append(heads, previous)
	}
	return heads
}

// A network's hotkey delegation chain; spec.key is the delegated hotkey.
func (self *stWalletBulkFixture) delegationChain(t testing.TB, networkId server.Id, specs ...stWalletConsentSpec) [][32]byte {
	t.Helper()
	heads := [][32]byte{}
	var previous [32]byte
	for index, spec := range specs {
		spec = self.defaults(spec)
		statement := protocol.HotkeyNetworkDelegationStatement{Domain: self.domain, UserId: [16]byte{1}, NetworkId: [16]byte(networkId), Hotkey: self.public(t, spec.key),
			ConsentHeadHash: spec.consentHead, ConsentGeneration: spec.consentGeneration,
			Generation: uint64(index) + 1, PreviousHash: stWalletBulkPrevious(previous, spec), Nonce: self.nonce(),
			IssuedAt: spec.expiresAt - 120, ExpiresAt: spec.expiresAt, FromEpoch: spec.from, ThroughEpoch: spec.through}
		if err := protocol.SignProspectiveHotkeyNetworkDelegation(&statement, *spec.boundary, spec.rootKey); err != nil {
			t.Fatal(err)
		}
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.WalletMappingConsent{Message: message, Signature: self.signature(t, spec.key, message, spec.badSignature)}
		previous = self.retain(t, stWalletChainDelegation, networkId.Bytes(), uint64(index)+1, spec, message, original, statement.Nonce)
		heads = append(heads, previous)
	}
	return heads
}

// A hotkey's global consent chain; spec.key is each generation's coldkey.
func (self *stWalletBulkFixture) hotkeyChain(t testing.TB, hotkey uint32, specs ...stWalletConsentSpec) [][32]byte {
	t.Helper()
	heads := [][32]byte{}
	var previous [32]byte
	hotkeyPublic := self.public(t, hotkey)
	for index, spec := range specs {
		spec = self.defaults(spec)
		statement := protocol.HotkeyWalletMappingStatement{Schema: protocol.HotkeyWalletMappingConsentSchema, Scope: protocol.HotkeyWalletMappingScope, Subnet: self.domain.HotkeySubnet(),
			Hotkey: hotkeyPublic, Coldkey: self.public(t, spec.key), Generation: uint64(index) + 1, PreviousHash: stWalletBulkPrevious(previous, spec), Nonce: self.nonce(),
			IssuedAt: spec.expiresAt - 120, FromEpoch: spec.from, ThroughEpoch: spec.through}
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.HotkeyWalletMappingConsent{Message: message, ColdkeySignature: self.signature(t, spec.key, message, spec.badSignature), HotkeySignature: self.sign(t, hotkey, message)}
		previous = self.retain(t, stWalletChainHotkey, hotkeyPublic[:], uint64(index)+1, spec, message, original, [32]byte{})
		heads = append(heads, previous)
	}
	return heads
}

// Retains every pending row: challenges before the originals that reference
// them, in batches.
func (self *stWalletBulkFixture) flush(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	self.pendingLock.Lock()
	pending := self.pending
	self.pending = map[string][]stWalletRetainedRow{}
	self.pendingLock.Unlock()
	tables := map[string][3]string{
		stWalletChainProvider:   {"wallet_mapping_challenge", "wallet_mapping_consent", "client_id"},
		stWalletChainNetwork:    {"network_wallet_mapping_challenge", "network_wallet_mapping_consent", "network_id"},
		stWalletChainDelegation: {"hotkey_network_delegation_challenge", "hotkey_network_delegation", "network_id"},
	}
	accepted := self.startTime.Add(-time.Hour)
	for _, kind := range []string{stWalletChainProvider, stWalletChainNetwork, stWalletChainDelegation, stWalletChainHotkey} {
		rows := pending[kind]
		for start := 0; start < len(rows); start += 4096 {
			batch := rows[start:min(start+4096, len(rows))]
			owners, hashes, nonces, raws := make([][]byte, len(batch)), make([][]byte, len(batch)), make([][]byte, len(batch)), make([][]byte, len(batch))
			ownerIds := make([]server.Id, len(batch))
			generations, expiries := make([]int64, len(batch)), make([]int64, len(batch))
			messages := make([]string, len(batch))
			for index, row := range batch {
				owners[index], hashes[index], nonces[index], raws[index] = row.owner, row.hash[:], row.nonce[:], row.raw
				if len(row.owner) == 16 {
					ownerIds[index] = server.Id(row.owner)
				}
				generations[index], expiries[index], messages[index] = int64(row.generation), row.expiresAt, row.message
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if kind == stWalletChainHotkey {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO hotkey_wallet_mapping_consent(subnet_hash,hotkey,generation,original_hash,original,accepted_at) SELECT $1::bytea,k,g,h,o,$6::timestamp FROM unnest($2::bytea[],$3::bigint[],$4::bytea[],$5::bytea[]) AS t(k,g,h,o)`, self.subnetHash[:], owners, generations, hashes, raws, accepted))
					return
				}
				table := tables[kind]
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO `+table[0]+`(nonce,domain_hash,`+table[2]+`,generation,expires_at,message) SELECT n,$1::bytea,o,g,e,m FROM unnest($2::bytea[],$3::uuid[],$4::bigint[],$5::bigint[],$6::text[]) AS t(n,o,g,e,m)`, self.domainHash[:], nonces, ownerIds, generations, expiries, messages))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO `+table[1]+`(domain_hash,`+table[2]+`,generation,original_hash,nonce,original,accepted_at) SELECT $1::bytea,o,g,h,n,r,$7::timestamp FROM unnest($2::uuid[],$3::bigint[],$4::bytea[],$5::bytea[],$6::bytea[]) AS t(o,g,h,n,r)`, self.domainHash[:], ownerIds, generations, hashes, nonces, raws, accepted))
			})
		}
	}
}

// A signed roster pinning exactly the given heads.
func (self *stWalletBulkFixture) roster(t testing.TB, scope StProviderWalletEpochScope, providers []payoutartifact.WholeWorkExpectedProvider, networks []payoutartifact.WholeWorkNetworkWallet, delegations []payoutartifact.WholeWorkHotkeyDelegation) ([]byte, payoutartifact.WholeWorkExpectation) {
	t.Helper()
	rosterKey, err := crypto.HexToECDSA(strings.Repeat("53", 32))
	if err != nil {
		t.Fatal(err)
	}
	ordered := append([]payoutartifact.WholeWorkExpectedProvider(nil), providers...)
	slices.SortFunc(ordered, func(a payoutartifact.WholeWorkExpectedProvider, b payoutartifact.WholeWorkExpectedProvider) int {
		return bytes.Compare(a.ClientId[:], b.ClientId[:])
	})
	networks = slices.SortedFunc(slices.Values(networks), func(a payoutartifact.WholeWorkNetworkWallet, b payoutartifact.WholeWorkNetworkWallet) int {
		return bytes.Compare(a.NetworkId[:], b.NetworkId[:])
	})
	delegations = slices.SortedFunc(slices.Values(delegations), func(a payoutartifact.WholeWorkHotkeyDelegation, b payoutartifact.WholeWorkHotkeyDelegation) int {
		return bytes.Compare(a.NetworkId[:], b.NetworkId[:])
	})
	authority := payoutartifact.WholeWorkAuthority{Domain: scope.Domain, Epoch: scope.Epoch, Start: scope.Start, End: scope.End, RequestPublicKey: [32]byte{150},
		PriorContracts: []payoutartifact.WholeWorkPriorContract{}, Owners: []payoutartifact.WholeWorkOwner{}, ExpectedProviders: ordered, NetworkWallets: networks, HotkeyDelegations: delegations}
	for index, provider := range ordered {
		authority.Owners = append(authority.Owners, payoutartifact.WholeWorkOwner{ClientId: provider.ClientId, NetworkId: provider.NetworkId, Generation: [16]byte{byte(index + 1)}, PublicKey: [32]byte{byte(index + 1)}})
	}
	signed, err := payoutartifact.SignWholeWorkAuthority(t.Context(), authority, rosterKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return raw, payoutartifact.WholeWorkExpectation{AuthoritySigner: crypto.PubkeyToAddress(rosterKey.PublicKey), ClientKeyRootSigner: self.root, AuthorityHash: "sha256:" + hex.EncodeToString(digest[:])}
}

// The v1 and bulk outcomes of one selection are the same for every provider:
// the same mapped set, each with v1's resolution and coldkey, or the same
// error.
func requireStWalletResolutionsEqualV1(t testing.TB, label string, v1 map[server.Id]*StProviderWallet, v1Err error, bulk map[server.Id]*StPayoutWalletResolution, bulkErr error) {
	t.Helper()
	if (v1Err == nil) != (bulkErr == nil) || v1Err != nil && v1Err.Error() != bulkErr.Error() {
		t.Fatalf("%s: v1 error %v, bulk error %v", label, v1Err, bulkErr)
	}
	if len(v1) != len(bulk) || (v1 == nil) != (bulk == nil) {
		t.Fatalf("%s: v1 mapped %d providers, bulk %d", label, len(v1), len(bulk))
	}
	for clientId, wallet := range v1 {
		got := bulk[clientId]
		if got == nil || wallet.Resolution == nil || *got != *wallet.Resolution || got.Coldkey != wallet.ColdkeyPubkey || got.ClientId != wallet.ClientId || got.NetworkId != wallet.NetworkId {
			t.Fatalf("%s: provider %s v1 %+v, bulk %+v", label, clientId, wallet.Resolution, got)
		}
	}
}
