// Settlement v2 verifies each retained consent chain once per head
// (VERSION2-FINAL.md §8.2). A chain that verifies completely through a head
// stores every generation's selection fields; a later epoch selects from them
// instead of re-reading and re-verifying the originals. Refusals are not
// stored: like v1, a refused chain is refused again at every epoch.
package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// The retained chain kinds epoch wallet selection reads.
const (
	stWalletChainProvider   = "provider"
	stWalletChainNetwork    = "network"
	stWalletChainDelegation = "delegation"
	stWalletChainHotkey     = "hotkey"
)

// Owners per stored-consent query and rows per stored-consent insert.
const stVerifiedWalletConsentBatch = 4096

// One pinned head of an owner's chain. A client or network fills the first 16
// bytes of owner; a hotkey fills all 32.
type stWalletChainHead struct {
	owner      [32]byte
	generation uint64
	hash       [32]byte
}

// The stored owner bytes of a head of the kind.
func (self stWalletChainHead) ownerBytes(kind string) []byte {
	if kind == stWalletChainHotkey {
		return self.owner[:]
	}
	return self.owner[:16]
}

// The fields of one verified generation that selection reads. A field the
// chain kind does not carry is zero.
type stVerifiedWalletConsent struct {
	generation   uint64
	originalHash [32]byte
	fromEpoch    uint64
	throughEpoch uint64
	// the coldkey; for a delegation, the delegated hotkey
	wallet [32]byte
	// the network a provider consent names
	networkId [16]byte
	// the global consent head a delegation pins
	consentHeadHash   [32]byte
	consentGeneration uint64
	// the operator's prospective approval; a zero signer has none
	prospectiveSigner common.Address
	prospectiveBlock  uint64
	prospectiveEpoch  uint64
	expiresAt         int64
}

// One head's outcome: the retained read's refusal, the refusal of the
// complete chain, or its verified generations 1 through the head.
type stWalletChain struct {
	readErr    error
	verdictErr error
	consents   []stVerifiedWalletConsent
}

// The selection of protocol.Verify*History at the epoch: the chain's own
// refusal, else the last generation from at or before the epoch, which is
// ErrWalletMappingNotEffective when none is or its interval ended earlier.
func (self *stWalletChain) at(epoch uint64) (*stVerifiedWalletConsent, error) {
	if self.readErr != nil {
		return nil, self.readErr
	}
	if self.verdictErr != nil {
		return nil, self.verdictErr
	}
	var selected *stVerifiedWalletConsent
	for index := range self.consents {
		if self.consents[index].fromEpoch <= epoch {
			selected = &self.consents[index]
		}
	}
	if selected == nil || selected.throughEpoch < epoch {
		return nil, errors.Join(protocol.ErrWalletMappingUnavailable, protocol.ErrWalletMappingNotEffective)
	}
	return selected, nil
}

// The chains through heads of the kind. Stored generations answer a head they
// cover through its hash; any other head is read with the v1 reader, verified
// as protocol.Verify*History verifies it, and stored when it verifies. A
// failed database read raises.
func getStWalletChains(ctx context.Context, domain protocol.ClientKeyHistoryDomain, scopeHash [32]byte, kind string, heads []stWalletChainHead) map[stWalletChainHead]*stWalletChain {
	chains := make(map[stWalletChainHead]*stWalletChain, len(heads))
	owners := [][]byte{}
	seen := map[[32]byte]bool{}
	for _, head := range heads {
		if !seen[head.owner] {
			seen[head.owner] = true
			owners = append(owners, head.ownerBytes(kind))
		}
	}
	stored := readStVerifiedWalletConsents(ctx, scopeHash, kind, owners)
	pending := []stWalletChainHead{}
	for _, head := range heads {
		if _, ok := chains[head]; ok {
			continue
		}
		if consents, ok := stVerifiedWalletHead(stored[string(head.ownerBytes(kind))], head); ok {
			chains[head] = &stWalletChain{consents: consents}
			continue
		}
		chains[head] = nil
		pending = append(pending, head)
	}
	verified := verifyStWalletChains(ctx, domain, kind, pending)
	verifiedHeads := []stWalletChainHead{}
	for index, head := range pending {
		chains[head] = verified[index]
		if verified[index].readErr == nil && verified[index].verdictErr == nil {
			verifiedHeads = append(verifiedHeads, head)
		}
	}
	addStVerifiedWalletConsents(ctx, scopeHash, kind, verifiedHeads, chains)
	return chains
}

// Concurrent verifications of new heads. Each reads one chain and checks its
// signatures independently of every other head; the bound keeps the reads
// within a share of the database pool.
const stWalletChainVerifiers = 8

// Verifies each head with up to stWalletChainVerifiers workers. A worker's
// failed read raises again here, where a sequential read would have raised.
func verifyStWalletChains(ctx context.Context, domain protocol.ClientKeyHistoryDomain, kind string, heads []stWalletChainHead) []*stWalletChain {
	chains := make([]*stWalletChain, len(heads))
	var next atomic.Int64
	var failureLock sync.Mutex
	var failure any
	var workers sync.WaitGroup
	for range min(stWalletChainVerifiers, len(heads)) {
		workers.Go(func() {
			defer func() {
				if cause := recover(); cause != nil {
					failureLock.Lock()
					if failure == nil {
						failure = cause
					}
					failureLock.Unlock()
				}
			}()
			for index := int(next.Add(1) - 1); index < len(heads); index = int(next.Add(1) - 1) {
				chains[index] = verifyStWalletChain(ctx, domain, kind, heads[index])
			}
		})
	}
	workers.Wait()
	if failure != nil {
		panic(failure)
	}
	return chains
}

// The stored generations 1 through the head, when they end in its hash. A
// chain's generations are stored in one transaction, and every stored head of
// an owner is a prefix of the same retained originals, so an owner's stored
// generations run from 1 without a gap.
func stVerifiedWalletHead(consents []stVerifiedWalletConsent, head stWalletChainHead) ([]stVerifiedWalletConsent, bool) {
	if head.generation == 0 || uint64(len(consents)) < head.generation || consents[head.generation-1].originalHash != head.hash {
		return nil, false
	}
	return consents[:head.generation], true
}

// Every stored generation of the owners, by owner bytes, in generation order.
func readStVerifiedWalletConsents(ctx context.Context, scopeHash [32]byte, kind string, owners [][]byte) map[string][]stVerifiedWalletConsent {
	consents := map[string][]stVerifiedWalletConsent{}
	for start := 0; start < len(owners); start += stVerifiedWalletConsentBatch {
		batch := owners[start:min(start+stVerifiedWalletConsentBatch, len(owners))]
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT owner,generation,original_hash,from_epoch,through_epoch,wallet,network_id,consent_head_hash,consent_generation,prospective_signer,prospective_block,prospective_epoch,expires_at FROM st_verified_wallet_consent WHERE scope_hash=$1 AND kind=$2 AND owner=ANY($3) ORDER BY owner,generation`, scopeHash[:], kind, batch)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var owner, originalHash, wallet, networkId, consentHeadHash, signer []byte
					var generation, fromEpoch, throughEpoch, consentGeneration, block, epoch int64
					consent := stVerifiedWalletConsent{}
					server.Raise(rows.Scan(&owner, &generation, &originalHash, &fromEpoch, &throughEpoch, &wallet, &networkId, &consentHeadHash, &consentGeneration, &signer, &block, &epoch, &consent.expiresAt))
					// uint64 values are stored by bit pattern
					consent.generation, consent.fromEpoch, consent.throughEpoch = uint64(generation), uint64(fromEpoch), uint64(throughEpoch)
					consent.consentGeneration, consent.prospectiveBlock, consent.prospectiveEpoch = uint64(consentGeneration), uint64(block), uint64(epoch)
					copy(consent.originalHash[:], originalHash)
					copy(consent.wallet[:], wallet)
					copy(consent.networkId[:], networkId)
					copy(consent.consentHeadHash[:], consentHeadHash)
					copy(consent.prospectiveSigner[:], signer)
					consents[string(owner)] = append(consents[string(owner)], consent)
				}
			})
		})
	}
	return consents
}

// Stores every generation of the verified chains through heads. A generation
// already stored is the same retained original, so it is left as it is.
func addStVerifiedWalletConsents(ctx context.Context, scopeHash [32]byte, kind string, heads []stWalletChainHead, chains map[stWalletChainHead]*stWalletChain) {
	type row struct {
		owner   []byte
		consent stVerifiedWalletConsent
	}
	rows := []row{}
	for _, head := range heads {
		for _, consent := range chains[head].consents {
			rows = append(rows, row{owner: head.ownerBytes(kind), consent: consent})
		}
	}
	if len(rows) == 0 {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for start := 0; start < len(rows); start += stVerifiedWalletConsentBatch {
			batch := rows[start:min(start+stVerifiedWalletConsentBatch, len(rows))]
			owners, originalHashes, wallets, networkIds, consentHeadHashes, signers := make([][]byte, len(batch)), make([][]byte, len(batch)), make([][]byte, len(batch)), make([][]byte, len(batch)), make([][]byte, len(batch)), make([][]byte, len(batch))
			generations, fromEpochs, throughEpochs, consentGenerations, blocks, epochs, expiries := make([]int64, len(batch)), make([]int64, len(batch)), make([]int64, len(batch)), make([]int64, len(batch)), make([]int64, len(batch)), make([]int64, len(batch)), make([]int64, len(batch))
			for index, row := range batch {
				consent := row.consent
				owners[index] = row.owner
				originalHashes[index], wallets[index], networkIds[index] = consent.originalHash[:], consent.wallet[:], consent.networkId[:]
				consentHeadHashes[index], signers[index] = consent.consentHeadHash[:], consent.prospectiveSigner[:]
				generations[index], fromEpochs[index], throughEpochs[index] = int64(consent.generation), int64(consent.fromEpoch), int64(consent.throughEpoch)
				consentGenerations[index], blocks[index], epochs[index], expiries[index] = int64(consent.consentGeneration), int64(consent.prospectiveBlock), int64(consent.prospectiveEpoch), consent.expiresAt
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_verified_wallet_consent(scope_hash,kind,owner,generation,original_hash,from_epoch,through_epoch,wallet,network_id,consent_head_hash,consent_generation,prospective_signer,prospective_block,prospective_epoch,expires_at) SELECT $1::bytea,$2::varchar,* FROM unnest($3::bytea[],$4::bigint[],$5::bytea[],$6::bigint[],$7::bigint[],$8::bytea[],$9::bytea[],$10::bytea[],$11::bigint[],$12::bytea[],$13::bigint[],$14::bigint[],$15::bigint[]) ON CONFLICT DO NOTHING`, scopeHash[:], kind, owners, generations, originalHashes, fromEpochs, throughEpochs, wallets, networkIds, consentHeadHashes, consentGenerations, signers, blocks, epochs, expiries))
		}
	})
}

// Reads the chain through head with the reader v1 uses, then verifies every
// original and the lineage exactly as the kind's protocol.Verify*History
// does, without its epoch selection. The reader already refused each input
// that function refuses before its lineage walk.
func verifyStWalletChain(ctx context.Context, domain protocol.ClientKeyHistoryDomain, kind string, head stWalletChainHead) *stWalletChain {
	switch kind {
	case stWalletChainProvider:
		return verifyStProviderWalletChain(ctx, domain, head)
	case stWalletChainNetwork:
		return verifyStNetworkWalletChain(ctx, domain, head)
	case stWalletChainDelegation:
		return verifyStDelegationWalletChain(ctx, domain, head)
	default:
		return verifyStHotkeyWalletChain(ctx, domain.HotkeySubnet(), head)
	}
}

// protocol.VerifyWalletMappingHistory's lineage walk.
func verifyStProviderWalletChain(ctx context.Context, domain protocol.ClientKeyHistoryDomain, head stWalletChainHead) *stWalletChain {
	clientId := [16]byte(head.owner[:16])
	originals, err := ReadWalletMappingHistory(ctx, domain, server.Id(clientId), head.generation, head.hash)
	if err != nil {
		return &stWalletChain{readErr: err}
	}
	consents := make([]stVerifiedWalletConsent, 0, len(originals))
	var previous [32]byte
	var previousEpoch uint64
	for index, original := range originals {
		statement, hash, err := protocol.VerifyWalletMappingConsent(ctx, original)
		if err != nil {
			return &stWalletChain{verdictErr: err}
		}
		if statement.Domain != domain || statement.ClientId != clientId || statement.Generation != uint64(index)+1 || statement.PreviousHash != previous || index > 0 && statement.FromEpoch <= previousEpoch {
			return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
		}
		previous, previousEpoch = hash, statement.FromEpoch
		consent := stVerifiedWalletConsent{generation: statement.Generation, originalHash: hash, fromEpoch: statement.FromEpoch, throughEpoch: statement.ThroughEpoch, wallet: statement.Coldkey, networkId: statement.NetworkId, expiresAt: statement.ExpiresAt}
		// a consent of the first schema proves key possession only
		if statement.Schema == protocol.WalletMappingProspectiveSchema {
			consent.prospectiveSigner, consent.prospectiveBlock, consent.prospectiveEpoch = statement.Prospective.Signer, statement.Prospective.Boundary.Block, statement.Prospective.Boundary.Epoch
		}
		consents = append(consents, consent)
	}
	if previous != head.hash {
		return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
	}
	return &stWalletChain{consents: consents}
}

// protocol.VerifyNetworkWalletMappingHistory's lineage walk.
func verifyStNetworkWalletChain(ctx context.Context, domain protocol.ClientKeyHistoryDomain, head stWalletChainHead) *stWalletChain {
	networkId := [16]byte(head.owner[:16])
	originals, err := ReadNetworkWalletMappingHistory(ctx, domain, server.Id(networkId), head.generation, head.hash)
	if err != nil {
		return &stWalletChain{readErr: err}
	}
	consents := make([]stVerifiedWalletConsent, 0, len(originals))
	var previous [32]byte
	var previousEpoch uint64
	for index, original := range originals {
		statement, hash, err := protocol.VerifyNetworkWalletMappingConsent(ctx, original)
		if err != nil {
			return &stWalletChain{verdictErr: err}
		}
		if statement.Domain != domain || statement.NetworkId != networkId || statement.Generation != uint64(index)+1 || statement.PreviousHash != previous || index > 0 && statement.FromEpoch <= previousEpoch {
			return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
		}
		previous, previousEpoch = hash, statement.FromEpoch
		consents = append(consents, stVerifiedWalletConsent{generation: statement.Generation, originalHash: hash, fromEpoch: statement.FromEpoch, throughEpoch: statement.ThroughEpoch, wallet: statement.Coldkey,
			prospectiveSigner: statement.Prospective.Signer, prospectiveBlock: statement.Prospective.Boundary.Block, prospectiveEpoch: statement.Prospective.Boundary.Epoch, expiresAt: statement.ExpiresAt})
	}
	if previous != head.hash {
		return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
	}
	return &stWalletChain{consents: consents}
}

// protocol.VerifyHotkeyNetworkDelegationHistory's lineage walk.
func verifyStDelegationWalletChain(ctx context.Context, domain protocol.ClientKeyHistoryDomain, head stWalletChainHead) *stWalletChain {
	networkId := [16]byte(head.owner[:16])
	originals, err := ReadHotkeyNetworkDelegationHistory(ctx, domain, server.Id(networkId), head.generation, head.hash)
	if err != nil {
		return &stWalletChain{readErr: err}
	}
	consents := make([]stVerifiedWalletConsent, 0, len(originals))
	var previous [32]byte
	var previousEpoch uint64
	for index, original := range originals {
		statement, hash, err := protocol.VerifyHotkeyNetworkDelegation(ctx, original)
		if err != nil {
			return &stWalletChain{verdictErr: err}
		}
		if statement.Domain != domain || statement.NetworkId != networkId || statement.Generation != uint64(index)+1 || statement.PreviousHash != previous || index > 0 && statement.FromEpoch <= previousEpoch {
			return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
		}
		previous, previousEpoch = hash, statement.FromEpoch
		consents = append(consents, stVerifiedWalletConsent{generation: statement.Generation, originalHash: hash, fromEpoch: statement.FromEpoch, throughEpoch: statement.ThroughEpoch, wallet: statement.Hotkey,
			consentHeadHash: statement.ConsentHeadHash, consentGeneration: statement.ConsentGeneration,
			prospectiveSigner: statement.Prospective.Signer, prospectiveBlock: statement.Prospective.Boundary.Block, prospectiveEpoch: statement.Prospective.Boundary.Epoch, expiresAt: statement.ExpiresAt})
	}
	if previous != head.hash {
		return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
	}
	return &stWalletChain{consents: consents}
}

// protocol.VerifyHotkeyWalletMappingHistory's lineage walk: every generation
// shares the head's subnet and hotkey, which must be the expected ones.
func verifyStHotkeyWalletChain(ctx context.Context, subnet protocol.HotkeyWalletMappingSubnet, head stWalletChainHead) *stWalletChain {
	originals, err := ReadHotkeyWalletMappingHistory(ctx, subnet, head.owner, head.generation, head.hash)
	if err != nil {
		return &stWalletChain{readErr: err}
	}
	consents := make([]stVerifiedWalletConsent, 0, len(originals))
	var last *protocol.HotkeyWalletMappingStatement
	var previous [32]byte
	for index, original := range originals {
		statement, hash, err := protocol.VerifyHotkeyWalletMappingConsent(ctx, original)
		if err != nil {
			return &stWalletChain{verdictErr: err}
		}
		if statement.Generation != uint64(index)+1 || statement.PreviousHash != previous || last != nil && (statement.Subnet != last.Subnet || statement.Hotkey != last.Hotkey || statement.FromEpoch <= last.FromEpoch) {
			return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
		}
		last, previous = statement, hash
		consents = append(consents, stVerifiedWalletConsent{generation: statement.Generation, originalHash: hash, fromEpoch: statement.FromEpoch, throughEpoch: statement.ThroughEpoch, wallet: statement.Coldkey})
	}
	if last.Subnet != subnet || last.Hotkey != head.owner || previous != head.hash {
		return &stWalletChain{verdictErr: protocol.ErrWalletMappingIntegrity}
	}
	return &stWalletChain{consents: consents}
}
