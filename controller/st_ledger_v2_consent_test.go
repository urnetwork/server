package controller

// st_ledger_v2_consent_test.go — retained consent chains for the settlement v2
// builder tests: each original is signed with a synthetic key and retained
// directly in SQL, so a test can retain chains for any population.

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
)

// The retained chain kinds.
const (
	ledgerV2ConsentProvider   = "provider"
	ledgerV2ConsentNetwork    = "network"
	ledgerV2ConsentDelegation = "delegation"
	ledgerV2ConsentHotkey     = "hotkey"
)

// ledgerV2Consents signs chains under one operator domain, each approved
// prospectively by the root key at a boundary before the epochs selected, and
// retains them on flush.
type ledgerV2Consents struct {
	domain     protocol.ClientKeyHistoryDomain
	domainHash [32]byte
	subnetHash [32]byte
	rootKey    *ecdsa.PrivateKey
	boundary   protocol.ClientKeyEffectiveBoundary
	// every acceptance expired at this unix time, before the epochs selected
	expiresAt int64

	nonces  atomic.Uint64
	keyLock sync.Mutex
	keys    map[uint32]*schnorrkel.SecretKey

	pendingLock sync.Mutex
	pending     map[string][]ledgerV2RetainedConsent
}

// One retained original; nonce is zero for a global hotkey consent.
type ledgerV2RetainedConsent struct {
	owner      []byte
	generation uint64
	hash       [32]byte
	nonce      [32]byte
	raw        []byte
	message    string
}

// newLedgerV2Consents signs under cfg's operator domain at the policy hash, with
// the prospective approval at boundary and acceptances expired by expiresAt.
func newLedgerV2Consents(t testing.TB, cfg *StConfig, policyHash [32]byte, boundary protocol.ClientKeyEffectiveBoundary, expiresAt time.Time) *ledgerV2Consents {
	t.Helper()
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress,
		SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: policyHash, NoID: cfg.NoId}
	domainHash, err := domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	subnet := domain.HotkeySubnet()
	payload := []byte("urnetwork-hotkey-wallet-mapping-subnet-v1\x00")
	payload = binary.BigEndian.AppendUint64(payload, subnet.ChainID)
	payload = append(payload, subnet.GenesisHash[:]...)
	payload = binary.BigEndian.AppendUint16(payload, subnet.Netuid)
	return &ledgerV2Consents{domain: domain, domainHash: domainHash, subnetHash: sha256.Sum256(payload), rootKey: cfg.RootKey, boundary: boundary,
		expiresAt: expiresAt.Unix(), keys: map[uint32]*schnorrkel.SecretKey{}, pending: map[string][]ledgerV2RetainedConsent{}}
}

// A deterministic sr25519 key per seed, expanded once.
func (self *ledgerV2Consents) key(t testing.TB, seed uint32) *schnorrkel.SecretKey {
	self.keyLock.Lock()
	defer self.keyLock.Unlock()
	if key := self.keys[seed]; key != nil {
		return key
	}
	var raw [32]byte
	binary.BigEndian.PutUint32(raw[:4], seed)
	raw[31] = 0x6b
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	key := mini.ExpandEd25519()
	self.keys[seed] = key
	return key
}

// public is the coldkey (or hotkey) of a seed.
func (self *ledgerV2Consents) public(t testing.TB, seed uint32) [32]byte {
	public, err := self.key(t, seed).Public()
	if err != nil {
		t.Fatal(err)
	}
	return public.Encode()
}

func (self *ledgerV2Consents) sign(t testing.TB, seed uint32, message string) [64]byte {
	signature, err := self.key(t, seed).Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	return signature.Encode()
}

func (self *ledgerV2Consents) nonce() [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "st-ledger-v2-consent-nonce/%d", self.nonces.Add(1)))
}

// Records one generation for the next flush and returns its hash.
func (self *ledgerV2Consents) retain(t testing.TB, kind string, owner []byte, generation uint64, message string, signed any, nonce [32]byte) [32]byte {
	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	self.pendingLock.Lock()
	self.pending[kind] = append(self.pending[kind], ledgerV2RetainedConsent{owner: owner, generation: generation, hash: hash, nonce: nonce, raw: raw, message: message})
	self.pendingLock.Unlock()
	return hash
}

// A provider's chain, one generation per (coldkey seed, from epoch) pair.
func (self *ledgerV2Consents) providerChain(t testing.TB, clientId server.Id, networkId server.Id, generations ...[2]uint64) {
	var previous [32]byte
	for index, generation := range generations {
		statement := protocol.WalletMappingStatement{Schema: protocol.WalletMappingConsentSchema, Domain: self.domain, UserId: [16]byte{1}, ClientId: [16]byte(clientId),
			NetworkId: [16]byte(networkId), Coldkey: self.public(t, uint32(generation[0])), Generation: uint64(index) + 1, PreviousHash: previous, Nonce: self.nonce(),
			IssuedAt: self.expiresAt - 120, ExpiresAt: self.expiresAt, FromEpoch: generation[1], ThroughEpoch: generation[1] + 1000}
		if err := protocol.SignProspectiveWalletMapping(&statement, self.boundary, self.rootKey); err != nil {
			t.Fatal(err)
		}
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.WalletMappingConsent{Message: message, Signature: self.sign(t, uint32(generation[0]), message)}
		previous = self.retain(t, ledgerV2ConsentProvider, clientId.Bytes(), uint64(index)+1, message, original, statement.Nonce)
	}
}

// A network's one-generation consent chain to the coldkey of a seed.
func (self *ledgerV2Consents) networkChain(t testing.TB, networkId server.Id, coldkey uint32, from uint64) {
	statement := protocol.NetworkWalletMappingStatement{Domain: self.domain, UserId: [16]byte{1}, NetworkId: [16]byte(networkId), Coldkey: self.public(t, coldkey),
		Generation: 1, Nonce: self.nonce(), IssuedAt: self.expiresAt - 120, ExpiresAt: self.expiresAt, FromEpoch: from, ThroughEpoch: from + 1000}
	if err := protocol.SignProspectiveNetworkWalletMapping(&statement, self.boundary, self.rootKey); err != nil {
		t.Fatal(err)
	}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	original := protocol.WalletMappingConsent{Message: message, Signature: self.sign(t, coldkey, message)}
	self.retain(t, ledgerV2ConsentNetwork, networkId.Bytes(), 1, message, original, statement.Nonce)
}

// A hotkey's one-generation global consent to the coldkey of a seed; returns
// its head hash.
func (self *ledgerV2Consents) hotkeyChain(t testing.TB, hotkey uint32, coldkey uint32, from uint64) [32]byte {
	hotkeyPublic := self.public(t, hotkey)
	statement := protocol.HotkeyWalletMappingStatement{Schema: protocol.HotkeyWalletMappingConsentSchema, Scope: protocol.HotkeyWalletMappingScope, Subnet: self.domain.HotkeySubnet(),
		Hotkey: hotkeyPublic, Coldkey: self.public(t, coldkey), Generation: 1, Nonce: self.nonce(), IssuedAt: self.expiresAt - 120, FromEpoch: from, ThroughEpoch: from + 1000}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	original := protocol.HotkeyWalletMappingConsent{Message: message, ColdkeySignature: self.sign(t, coldkey, message), HotkeySignature: self.sign(t, hotkey, message)}
	return self.retain(t, ledgerV2ConsentHotkey, hotkeyPublic[:], 1, message, original, [32]byte{})
}

// A network's one-generation delegation to a hotkey whose global consent head
// is consentHead.
func (self *ledgerV2Consents) delegationChain(t testing.TB, networkId server.Id, hotkey uint32, consentHead [32]byte, from uint64) {
	statement := protocol.HotkeyNetworkDelegationStatement{Domain: self.domain, UserId: [16]byte{1}, NetworkId: [16]byte(networkId), Hotkey: self.public(t, hotkey),
		ConsentHeadHash: consentHead, ConsentGeneration: 1, Generation: 1, Nonce: self.nonce(), IssuedAt: self.expiresAt - 120, ExpiresAt: self.expiresAt,
		FromEpoch: from, ThroughEpoch: from + 1000}
	if err := protocol.SignProspectiveHotkeyNetworkDelegation(&statement, self.boundary, self.rootKey); err != nil {
		t.Fatal(err)
	}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	original := protocol.WalletMappingConsent{Message: message, Signature: self.sign(t, hotkey, message)}
	self.retain(t, ledgerV2ConsentDelegation, networkId.Bytes(), 1, message, original, statement.Nonce)
}

// Retains every pending original, challenges before the originals that
// reference them, in batches.
func (self *ledgerV2Consents) flush(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	self.pendingLock.Lock()
	pending := self.pending
	self.pending = map[string][]ledgerV2RetainedConsent{}
	self.pendingLock.Unlock()
	tables := map[string][3]string{
		ledgerV2ConsentProvider:   {"wallet_mapping_challenge", "wallet_mapping_consent", "client_id"},
		ledgerV2ConsentNetwork:    {"network_wallet_mapping_challenge", "network_wallet_mapping_consent", "network_id"},
		ledgerV2ConsentDelegation: {"hotkey_network_delegation_challenge", "hotkey_network_delegation", "network_id"},
	}
	accepted := time.Unix(self.expiresAt, 0).UTC().Add(-time.Minute)
	for _, kind := range []string{ledgerV2ConsentProvider, ledgerV2ConsentNetwork, ledgerV2ConsentDelegation, ledgerV2ConsentHotkey} {
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
				generations[index], expiries[index], messages[index] = int64(row.generation), self.expiresAt, row.message
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if kind == ledgerV2ConsentHotkey {
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
