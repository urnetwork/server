// Network wallet mapping consent: the coldkey owner signs once for a network,
// and the consent covers every provider client of the network. One append-only
// chain per (domain, network) beside the per-provider chains. The exact
// original, its nonce and its chain position commit together; no wallet
// projection changes with it (a network consent is not a provider consent).
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

// The controller supplies the authenticated network owner and the
// independently selected deployment. A network consent names no client.
// Network consents are prospective from their first schema, so issuance and
// acceptance require the operator owner.
type NetworkWalletMappingOwner struct {
	Domain      protocol.ClientKeyHistoryDomain
	UserId      server.Id
	NetworkId   server.Id
	Prospective *WalletMappingProspectiveOwner
}

// Serializing one (domain, network) prevents two accepted successors from
// acquiring the same predecessor. The key space is separate from the
// per-provider chains.
func lockNetworkWalletMapping(ctx context.Context, tx server.PgTx, domain [32]byte, networkId server.Id) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "network-wallet-mapping:"+hex.EncodeToString(domain[:])+":"+networkId.String()))
}

// The authenticated user must still administer the network, checked in the
// same transaction as the consent.
func checkNetworkWalletMappingOwner(ctx context.Context, tx server.PgTx, owner NetworkWalletMappingOwner) error {
	if owner.UserId == (server.Id{}) || owner.NetworkId == (server.Id{}) {
		return protocol.ErrWalletMappingIntegrity
	}
	var adminUserId server.Id
	err := tx.QueryRow(ctx, `SELECT admin_user_id FROM network WHERE network_id=$1 FOR SHARE`, owner.NetworkId).Scan(&adminUserId)
	if errors.Is(err, pgx.ErrNoRows) {
		return protocol.ErrWalletMappingUnavailable
	}
	if err != nil {
		server.Raise(err)
	}
	if adminUserId != owner.UserId {
		return protocol.ErrWalletMappingIntegrity
	}
	return nil
}

// Issuance fixes the authenticated network owner, predecessor, nonce, the
// operator's issuance boundary and the five minute acceptance window. It
// neither signs for the wallet nor changes any wallet.
func CreateNetworkWalletMappingChallenge(ctx context.Context, owner NetworkWalletMappingOwner, coldkey [32]byte, fromEpoch, throughEpoch uint64) (message string, returnErr error) {
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
	now := server.NowUtc().Unix()
	statement := protocol.NetworkWalletMappingStatement{Domain: owner.Domain, UserId: [16]byte(owner.UserId), NetworkId: [16]byte(owner.NetworkId), Coldkey: coldkey, IssuedAt: now, ExpiresAt: now + 300, FromEpoch: fromEpoch, ThroughEpoch: throughEpoch}
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
		lockNetworkWalletMapping(ctx, tx, domain, owner.NetworkId)
		if err := checkNetworkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		statement.Generation, statement.PreviousHash = 1, [32]byte{}
		var generation uint64
		var previous, original []byte
		err := tx.QueryRow(ctx, `SELECT generation,original_hash,original FROM network_wallet_mapping_consent WHERE domain_hash=$1 AND network_id=$2 ORDER BY generation DESC LIMIT 1`, domain[:], owner.NetworkId).Scan(&generation, &previous, &original)
		if err == nil {
			if len(previous) != 32 || generation >= protocol.MaxWalletMappingHistory {
				panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
			}
			var prior protocol.WalletMappingConsent
			server.Raise(json.Unmarshal(original, &prior))
			value, hash, err := protocol.VerifyNetworkWalletMappingConsent(ctx, prior)
			if err != nil || !bytes.Equal(previous, hash[:]) || fromEpoch <= value.FromEpoch {
				panic(walletMappingAbort{cause: errors.Join(protocol.ErrWalletMappingIntegrity, err)})
			}
			statement.Generation, statement.PreviousHash = generation+1, hash
		} else if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		if err := protocol.SignProspectiveNetworkWalletMapping(&statement, owner.Prospective.Boundary, owner.Prospective.RootKey); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		var pending int
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM network_wallet_mapping_challenge c WHERE domain_hash=$1 AND network_id=$2 AND expires_at>=$3 AND NOT EXISTS (SELECT 1 FROM network_wallet_mapping_consent s WHERE s.nonce=c.nonce)`, domain[:], owner.NetworkId, now).Scan(&pending))
		if pending >= 16 {
			panic(walletMappingAbort{cause: errors.New("network wallet mapping challenge capacity is temporarily full")})
		}
		message, err = statement.Message()
		if err != nil {
			panic(walletMappingAbort{cause: err})
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_wallet_mapping_challenge(nonce,domain_hash,network_id,generation,expires_at,message) VALUES($1,$2,$3,$4,$5,$6)`, statement.Nonce[:], domain[:], owner.NetworkId, statement.Generation, statement.ExpiresAt, message))
	})
	return message, nil
}

// The original signature, the issued nonce and the operator's boundary are
// verified before the consent is retained. An exact replay returns the same
// hash and generation without appending history.
func AcceptNetworkWalletMappingConsent(ctx context.Context, owner NetworkWalletMappingOwner, original protocol.WalletMappingConsent, coldkeySs58 string) (accepted *WalletMappingAccepted, returnErr error) {
	if ctx == nil || owner.Prospective == nil || owner.Prospective.RootKey == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	statement, hash, err := protocol.VerifyNetworkWalletMappingConsent(ctx, original)
	if err != nil {
		return nil, err
	}
	if statement.Domain != owner.Domain || statement.UserId != [16]byte(owner.UserId) || statement.NetworkId != [16]byte(owner.NetworkId) {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	actualKey, err := DecodeBittensorAddress(coldkeySs58)
	if err != nil || actualKey != statement.Coldkey {
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
		lockNetworkWalletMapping(ctx, tx, domain, owner.NetworkId)
		if err := checkNetworkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		var issued string
		err := tx.QueryRow(ctx, `SELECT message FROM network_wallet_mapping_challenge WHERE nonce=$1`, statement.Nonce[:]).Scan(&issued)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && issued != original.Message {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
		}
		server.Raise(err)
		var retained []byte
		err = tx.QueryRow(ctx, `SELECT original FROM network_wallet_mapping_consent WHERE nonce=$1`, statement.Nonce[:]).Scan(&retained)
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
			panic(walletMappingAbort{cause: errors.New("network wallet mapping original challenge is expired or not yet effective")})
		}
		var previous []byte
		var generation uint64
		err = tx.QueryRow(ctx, `SELECT generation,original_hash FROM network_wallet_mapping_consent WHERE domain_hash=$1 AND network_id=$2 ORDER BY generation DESC LIMIT 1`, domain[:], owner.NetworkId).Scan(&generation, &previous)
		if errors.Is(err, pgx.ErrNoRows) {
			generation, previous = 0, make([]byte, 32)
		} else {
			server.Raise(err)
		}
		if statement.Generation != generation+1 || !bytes.Equal(previous, statement.PreviousHash[:]) {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_wallet_mapping_consent(domain_hash,network_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, domain[:], owner.NetworkId, statement.Generation, hash[:], statement.Nonce[:], raw, now))
		accepted = &WalletMappingAccepted{OriginalHash: hash, Generation: statement.Generation, Applied: true}
	})
	return accepted, nil
}

// Exact-window callers supply a separately approved head. This returns the
// original bytes only; it does not declare its newest SQL row authoritative.
func ReadNetworkWalletMappingHistory(ctx context.Context, domain protocol.ClientKeyHistoryDomain, networkId server.Id, generation uint64, head [32]byte) ([]protocol.WalletMappingConsent, error) {
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
		rows, err := conn.Query(owner, `SELECT generation,original_hash,original FROM network_wallet_mapping_consent WHERE domain_hash=$1 AND network_id=$2 AND generation<=$3 ORDER BY generation`, digest[:], networkId, generation)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var index uint64
				var raw []byte
				server.Raise(rows.Scan(&index, &last, &raw))
				if index != uint64(len(result))+1 || len(raw) > protocol.MaxWalletMappingConsentBytes {
					server.Raise(fmt.Errorf("network wallet mapping retained sequence is invalid"))
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

// The newest accepted network consent, for display. It carries no settlement
// authority: settlement reads the chain head an independent roster pins.
type NetworkWalletMappingConsentView struct {
	NetworkId    server.Id
	ColdkeySs58  string
	Coldkey      [32]byte
	Generation   uint64
	OriginalHash [32]byte
	FromEpoch    uint64
	ThroughEpoch uint64
	AcceptedAt   time.Time
}

// Nil when the network has no accepted consent in the domain. A retained row
// that no longer verifies is an error, never a displayed wallet.
func GetNetworkWalletMappingConsent(ctx context.Context, domain protocol.ClientKeyHistoryDomain, networkId server.Id) (*NetworkWalletMappingConsentView, error) {
	digest, err := domain.Digest()
	if err != nil {
		return nil, err
	}
	var found bool
	var generation uint64
	var hash, raw []byte
	var acceptedAt time.Time
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(ctx, `SELECT generation,original_hash,original,accepted_at FROM network_wallet_mapping_consent WHERE domain_hash=$1 AND network_id=$2 ORDER BY generation DESC LIMIT 1`, digest[:], networkId).Scan(&generation, &hash, &raw, &acceptedAt)
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
	statement, verified, err := protocol.VerifyNetworkWalletMappingConsent(ctx, original)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(hash, verified[:]) || statement.Generation != generation || statement.NetworkId != [16]byte(networkId) || statement.Domain != domain {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	address, err := ss58.Encode(statement.Coldkey, ss58.BittensorPrefix)
	if err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	return &NetworkWalletMappingConsentView{
		NetworkId:    networkId,
		ColdkeySs58:  address,
		Coldkey:      statement.Coldkey,
		Generation:   statement.Generation,
		OriginalHash: verified,
		FromEpoch:    statement.FromEpoch,
		ThroughEpoch: statement.ThroughEpoch,
		AcceptedAt:   acceptedAt,
	}, nil
}
