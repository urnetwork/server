// Hotkey network delegation (sn protocol urnetwork-hotkey-network-delegation-v1):
// the network consent with the hotkey in place of the coldkey. The hotkey
// signs that one network of this operator earns to the global hotkey consent
// head the statement pins, after the operator co-signs the issuance boundary.
// One append-only chain per (domain, network), beside the network consents and
// keyed apart from them. The exact original, its nonce and its chain position
// commit together; no wallet projection changes with it.
package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
)

// Serializing one (domain, network) prevents two accepted successors from
// acquiring the same predecessor. The key space is separate from the network
// consent chains.
func lockHotkeyNetworkDelegation(ctx context.Context, tx server.PgTx, domain [32]byte, networkId server.Id) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "hotkey-network-delegation:"+hex.EncodeToString(domain[:])+":"+networkId.String()))
}

// Issuance requires the global chain through the pinned head to be retained
// for the hotkey in the subnet of the operator's domain, then fixes the
// authenticated network owner, the predecessor, the nonce, the operator's
// issuance boundary and the five minute acceptance window. It neither signs
// for the hotkey nor changes any wallet.
func CreateHotkeyNetworkDelegationChallenge(ctx context.Context, owner NetworkWalletMappingOwner, hotkey [32]byte, consentHeadHash [32]byte, consentGeneration uint64, fromEpoch, throughEpoch uint64) (message string, returnErr error) {
	if ctx == nil || owner.Prospective == nil {
		return "", protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	domain, err := owner.Domain.Digest()
	if err != nil {
		return "", err
	}
	// the global chain is append-only, so a head retained now stays retained
	// for the transaction below
	subnet := owner.Domain.HotkeySubnet()
	consents, err := ReadHotkeyWalletMappingHistory(ctx, subnet, hotkey, consentGeneration, consentHeadHash)
	if err != nil {
		return "", err
	}
	head, headHash, err := protocol.VerifyHotkeyWalletMappingLineage(ctx, consents)
	if err != nil {
		return "", err
	}
	if headHash != consentHeadHash || head.Hotkey != hotkey || head.Subnet != subnet {
		return "", protocol.ErrWalletMappingIntegrity
	}
	now := server.NowUtc().Unix()
	statement := protocol.HotkeyNetworkDelegationStatement{Domain: owner.Domain, UserId: [16]byte(owner.UserId), NetworkId: [16]byte(owner.NetworkId), Hotkey: hotkey, ConsentHeadHash: consentHeadHash, ConsentGeneration: consentGeneration, IssuedAt: now, ExpiresAt: now + 300, FromEpoch: fromEpoch, ThroughEpoch: throughEpoch}
	if _, err := rand.Read(statement.Nonce[:]); err != nil {
		return "", err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(walletMappingAbort); ok {
				message, returnErr = "", refusal.cause
			} else {
				panic(recovered)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		message = ""
		lockHotkeyNetworkDelegation(ctx, tx, domain, owner.NetworkId)
		if err := checkNetworkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		statement.Generation, statement.PreviousHash = 1, [32]byte{}
		var generation uint64
		var previous, original []byte
		err := tx.QueryRow(ctx, `SELECT generation,original_hash,original FROM hotkey_network_delegation WHERE domain_hash=$1 AND network_id=$2 ORDER BY generation DESC LIMIT 1`, domain[:], owner.NetworkId).Scan(&generation, &previous, &original)
		if err == nil {
			if len(previous) != 32 || generation >= protocol.MaxWalletMappingHistory {
				panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
			}
			var prior protocol.WalletMappingConsent
			server.Raise(json.Unmarshal(original, &prior))
			value, hash, err := protocol.VerifyHotkeyNetworkDelegation(ctx, prior)
			if err != nil || !bytes.Equal(previous, hash[:]) || fromEpoch <= value.FromEpoch {
				panic(walletMappingAbort{cause: errors.Join(protocol.ErrWalletMappingIntegrity, err)})
			}
			statement.Generation, statement.PreviousHash = generation+1, hash
		} else if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		if err := protocol.SignProspectiveHotkeyNetworkDelegation(&statement, owner.Prospective.Boundary, owner.Prospective.RootKey); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		var pending int
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM hotkey_network_delegation_challenge c WHERE domain_hash=$1 AND network_id=$2 AND expires_at>=$3 AND NOT EXISTS (SELECT 1 FROM hotkey_network_delegation s WHERE s.nonce=c.nonce)`, domain[:], owner.NetworkId, now).Scan(&pending))
		if pending >= 16 {
			panic(walletMappingAbort{cause: errors.New("hotkey network delegation challenge capacity is temporarily full")})
		}
		message, err = statement.Message()
		if err != nil {
			panic(walletMappingAbort{cause: err})
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO hotkey_network_delegation_challenge(nonce,domain_hash,network_id,generation,expires_at,message) VALUES($1,$2,$3,$4,$5,$6)`, statement.Nonce[:], domain[:], owner.NetworkId, statement.Generation, statement.ExpiresAt, message))
	})
	return message, nil
}

// The hotkey's signature, the issued nonce and the operator's boundary are
// verified before the delegation is retained. hotkeySs58 names the signer and
// must be the statement's hotkey. An exact replay returns the same hash and
// generation without appending history.
func AcceptHotkeyNetworkDelegation(ctx context.Context, owner NetworkWalletMappingOwner, original protocol.WalletMappingConsent, hotkeySs58 string) (accepted *WalletMappingAccepted, returnErr error) {
	if ctx == nil || owner.Prospective == nil || owner.Prospective.RootKey == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	statement, hash, err := protocol.VerifyHotkeyNetworkDelegation(ctx, original)
	if err != nil {
		return nil, err
	}
	if statement.Domain != owner.Domain || statement.UserId != [16]byte(owner.UserId) || statement.NetworkId != [16]byte(owner.NetworkId) {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	actualKey, err := DecodeBittensorAddress(hotkeySs58)
	if err != nil || actualKey != statement.Hotkey {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	scope := owner.Prospective
	if statement.Prospective.Signer != crypto.PubkeyToAddress(scope.RootKey.PublicKey) || scope.Boundary.Validate() != nil || scope.Boundary.Epoch >= statement.FromEpoch || scope.Boundary.Epoch < statement.Prospective.Boundary.Epoch || scope.Boundary.Block < statement.Prospective.Boundary.Block || scope.Boundary.Block == statement.Prospective.Boundary.Block && scope.Boundary != statement.Prospective.Boundary {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	raw, err := json.Marshal(original)
	if err != nil || len(raw) > protocol.MaxWalletMappingConsentBytes {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	domain, _ := owner.Domain.Digest()
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(walletMappingAbort); ok {
				accepted, returnErr = nil, refusal.cause
			} else {
				panic(recovered)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		accepted = nil
		lockHotkeyNetworkDelegation(ctx, tx, domain, owner.NetworkId)
		if err := checkNetworkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		var issued string
		err := tx.QueryRow(ctx, `SELECT message FROM hotkey_network_delegation_challenge WHERE nonce=$1`, statement.Nonce[:]).Scan(&issued)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && issued != original.Message {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
		}
		server.Raise(err)
		var retained []byte
		err = tx.QueryRow(ctx, `SELECT original FROM hotkey_network_delegation WHERE nonce=$1`, statement.Nonce[:]).Scan(&retained)
		if err == nil {
			if !bytes.Equal(retained, raw) {
				panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
			}
			accepted = &WalletMappingAccepted{OriginalHash: hash, Generation: statement.Generation}
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		now := server.NowUtc()
		if now.Unix() < statement.IssuedAt || now.Unix() > statement.ExpiresAt {
			panic(walletMappingAbort{cause: errors.New("hotkey network delegation original challenge is expired or not yet effective")})
		}
		var previous []byte
		var generation uint64
		err = tx.QueryRow(ctx, `SELECT generation,original_hash FROM hotkey_network_delegation WHERE domain_hash=$1 AND network_id=$2 ORDER BY generation DESC LIMIT 1`, domain[:], owner.NetworkId).Scan(&generation, &previous)
		if errors.Is(err, pgx.ErrNoRows) {
			generation, previous = 0, make([]byte, 32)
		} else {
			server.Raise(err)
		}
		if statement.Generation != generation+1 || !bytes.Equal(previous, statement.PreviousHash[:]) {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO hotkey_network_delegation(domain_hash,network_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, domain[:], owner.NetworkId, statement.Generation, hash[:], statement.Nonce[:], raw, now))
		accepted = &WalletMappingAccepted{OriginalHash: hash, Generation: statement.Generation, Applied: true}
	})
	return accepted, nil
}

// Exact-window callers supply a separately approved head. This returns the
// original bytes only; it does not declare its newest SQL row authoritative.
func ReadHotkeyNetworkDelegationHistory(ctx context.Context, domain protocol.ClientKeyHistoryDomain, networkId server.Id, generation uint64, head [32]byte) ([]protocol.WalletMappingConsent, error) {
	if ctx == nil || networkId == (server.Id{}) || generation == 0 || generation > protocol.MaxWalletMappingHistory || head == ([32]byte{}) {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	digest, err := domain.Digest()
	if err != nil {
		return nil, err
	}
	owner, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	var result []protocol.WalletMappingConsent
	var last []byte
	server.Db(owner, func(conn server.PgConn) {
		rows, err := conn.Query(owner, `SELECT generation,original_hash,original FROM hotkey_network_delegation WHERE domain_hash=$1 AND network_id=$2 AND generation<=$3 ORDER BY generation`, digest[:], networkId, generation)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var index uint64
				var raw []byte
				server.Raise(rows.Scan(&index, &last, &raw))
				if index != uint64(len(result))+1 || len(raw) > protocol.MaxWalletMappingConsentBytes {
					server.Raise(fmt.Errorf("hotkey network delegation retained sequence is invalid"))
				}
				var original protocol.WalletMappingConsent
				server.Raise(json.Unmarshal(raw, &original))
				result = append(result, original)
			}
		})
	})
	if err := owner.Err(); err != nil {
		return nil, err
	}
	if uint64(len(result)) != generation || !bytes.Equal(last, head[:]) {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	return result, nil
}

// The newest accepted delegation and the global consent it shows, for
// display. It carries no settlement authority: settlement reads the
// delegation head an independent roster pins.
type HotkeyNetworkDelegationView struct {
	NetworkId  server.Id
	Hotkey     [32]byte
	HotkeySs58 string
	// the newest delegation, which is the head of the network's chain
	OriginalHash [32]byte
	Generation   uint64
	FromEpoch    uint64
	ThroughEpoch uint64
	// the global consent head the newest delegation pins
	ConsentHeadHash   [32]byte
	ConsentGeneration uint64
	// the coldkey of the global consent effective at the requested epoch, else
	// of the pinned consent head
	Coldkey     [32]byte
	ColdkeySs58 string
	AcceptedAt  time.Time
}

// Nil when the network has no accepted delegation in the domain. epochKnown
// false selects the consent head's coldkey. A retained delegation or consent
// chain that no longer verifies is an error, never a displayed wallet.
func GetHotkeyNetworkDelegation(ctx context.Context, domain protocol.ClientKeyHistoryDomain, networkId server.Id, epoch uint64, epochKnown bool) (*HotkeyNetworkDelegationView, error) {
	digest, err := domain.Digest()
	if err != nil {
		return nil, err
	}
	var found bool
	var generation uint64
	var hash, raw []byte
	var acceptedAt time.Time
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(ctx, `SELECT generation,original_hash,original,accepted_at FROM hotkey_network_delegation WHERE domain_hash=$1 AND network_id=$2 ORDER BY generation DESC LIMIT 1`, digest[:], networkId).Scan(&generation, &hash, &raw, &acceptedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		found = true
	})
	if !found {
		return nil, nil
	}
	var original protocol.WalletMappingConsent
	if err := json.Unmarshal(raw, &original); err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	statement, verified, err := protocol.VerifyHotkeyNetworkDelegation(ctx, original)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(hash, verified[:]) || statement.Generation != generation || statement.NetworkId != [16]byte(networkId) || statement.Domain != domain {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	subnet := domain.HotkeySubnet()
	consents, err := ReadHotkeyWalletMappingHistory(ctx, subnet, statement.Hotkey, statement.ConsentGeneration, statement.ConsentHeadHash)
	if err != nil {
		return nil, err
	}
	// settlement's selection rule, so the display names the coldkey that would
	// earn at the epoch
	var coldkey [32]byte
	if epochKnown {
		mapping, err := protocol.VerifyHotkeyWalletMappingHistory(ctx, consents, protocol.HotkeyWalletMappingHistoryExpectation{Subnet: subnet, Hotkey: statement.Hotkey, HeadHash: statement.ConsentHeadHash, Generation: statement.ConsentGeneration, Epoch: epoch})
		if err == nil {
			coldkey = mapping.Statement.Coldkey
		} else if !errors.Is(err, protocol.ErrWalletMappingNotEffective) {
			return nil, err
		}
	}
	if coldkey == ([32]byte{}) {
		head, headHash, err := protocol.VerifyHotkeyWalletMappingLineage(ctx, consents)
		if err != nil {
			return nil, err
		}
		if headHash != statement.ConsentHeadHash || head.Hotkey != statement.Hotkey || head.Subnet != subnet {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		coldkey = head.Coldkey
	}
	coldkeySs58, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	hotkeySs58, err := ss58.Encode(statement.Hotkey, ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	return &HotkeyNetworkDelegationView{
		NetworkId:         networkId,
		Hotkey:            statement.Hotkey,
		HotkeySs58:        hotkeySs58,
		OriginalHash:      verified,
		Generation:        statement.Generation,
		FromEpoch:         statement.FromEpoch,
		ThroughEpoch:      statement.ThroughEpoch,
		ConsentHeadHash:   statement.ConsentHeadHash,
		ConsentGeneration: statement.ConsentGeneration,
		Coldkey:           coldkey,
		ColdkeySs58:       coldkeySs58,
		AcceptedAt:        acceptedAt,
	}, nil
}
