// Exact mapping consent, its nonce and both wallet projections commit together.
// Every accepted generation retains the original coldkey signature permanently.
package model

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// The controller supplies the authenticated caller and independently selected
// deployment. Public payload fields cannot change this ownership tuple.
type WalletMappingOwner struct {
	Domain      protocol.ClientKeyHistoryDomain
	UserId      server.Id
	ClientId    server.Id
	NetworkId   server.Id
	Prospective *WalletMappingProspectiveOwner
}

// Only the concrete controller constructs this original read/signing owner.
// It is never decoded from a request or persisted as a private signing key.
type WalletMappingProspectiveOwner struct {
	Boundary protocol.ClientKeyEffectiveBoundary
	RootKey  *ecdsa.PrivateKey
}

// Only this private refusal unwinds an uncommitted semantic transaction.
type walletMappingAbort struct{ cause error }

// Serializing one original client/domain prevents two accepted successors from
// acquiring the same predecessor. Different provider owners remain independent.
func lockWalletMapping(ctx context.Context, tx server.PgTx, domain [32]byte, clientId server.Id) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "wallet-mapping:"+hex.EncodeToString(domain[:])+":"+clientId.String()))
}

// A current directory check precedes mutation, inside the same transaction as
// consent. A removed or reassigned client cannot reuse a stale authenticated JWT.
func checkWalletMappingOwner(ctx context.Context, tx server.PgTx, owner WalletMappingOwner) error {
	if owner.UserId == (server.Id{}) || owner.ClientId == (server.Id{}) || owner.NetworkId == (server.Id{}) {
		return protocol.ErrWalletMappingIntegrity
	}
	var networkId server.Id
	err := tx.QueryRow(ctx, `SELECT network_id FROM network_client WHERE client_id=$1 AND active FOR SHARE`, owner.ClientId).Scan(&networkId)
	if errors.Is(err, pgx.ErrNoRows) {
		return protocol.ErrWalletMappingUnavailable
	}
	if err != nil {
		server.Raise(err)
	}
	if networkId != owner.NetworkId {
		return protocol.ErrWalletMappingIntegrity
	}
	return nil
}

// Issuance fixes exact authenticated identities, predecessor, nonce and five
// minute acceptance window. It neither signs for the wallet nor changes it.
func CreateWalletMappingChallenge(ctx context.Context, owner WalletMappingOwner, coldkey [32]byte, fromEpoch, throughEpoch uint64) (message string, returnErr error) {
	if ctx == nil {
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
	statement := protocol.WalletMappingStatement{Schema: protocol.WalletMappingConsentSchema, Domain: owner.Domain, UserId: [16]byte(owner.UserId), ClientId: [16]byte(owner.ClientId), NetworkId: [16]byte(owner.NetworkId), Coldkey: coldkey, IssuedAt: now, ExpiresAt: now + 300, FromEpoch: fromEpoch, ThroughEpoch: throughEpoch}
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
		lockWalletMapping(ctx, tx, domain, owner.ClientId)
		if err := checkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		statement.Generation, statement.PreviousHash = 1, [32]byte{}
		var generation uint64
		var previous, original []byte
		err := tx.QueryRow(ctx, `SELECT generation,original_hash,original FROM wallet_mapping_consent WHERE domain_hash=$1 AND client_id=$2 ORDER BY generation DESC LIMIT 1`, domain[:], owner.ClientId).Scan(&generation, &previous, &original)
		if err == nil {
			if len(previous) != 32 || generation >= protocol.MaxWalletMappingHistory {
				panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
			}
			var prior protocol.WalletMappingConsent
			server.Raise(json.Unmarshal(original, &prior))
			value, hash, err := protocol.VerifyWalletMappingConsent(ctx, prior)
			if err != nil || !bytes.Equal(previous, hash[:]) || fromEpoch <= value.FromEpoch {
				panic(walletMappingAbort{cause: errors.Join(protocol.ErrWalletMappingIntegrity, err)})
			}
			statement.Generation, statement.PreviousHash = generation+1, hash
		} else if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		if owner.Prospective != nil {
			if err := protocol.SignProspectiveWalletMapping(&statement, owner.Prospective.Boundary, owner.Prospective.RootKey); err != nil {
				panic(walletMappingAbort{cause: err})
			}
		}
		var pending int
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM wallet_mapping_challenge c WHERE domain_hash=$1 AND client_id=$2 AND expires_at>=$3 AND NOT EXISTS (SELECT 1 FROM wallet_mapping_consent s WHERE s.nonce=c.nonce)`, domain[:], owner.ClientId, now).Scan(&pending))
		if pending >= 16 {
			panic(walletMappingAbort{cause: errors.New("wallet mapping challenge capacity is temporarily full")})
		}
		message, err = statement.Message()
		if err != nil {
			panic(walletMappingAbort{cause: err})
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO wallet_mapping_challenge(nonce,domain_hash,client_id,generation,expires_at,message) VALUES($1,$2,$3,$4,$5,$6)`, statement.Nonce[:], domain[:], owner.ClientId, statement.Generation, statement.ExpiresAt, message))
	})
	return message, nil
}

// OriginalHash remains stable across lost acknowledgements, including retries
// after expiry or after a later consent has superseded the wallet projection.
type WalletMappingAccepted struct {
	OriginalHash [32]byte `json:"original_hash"`
	Generation   uint64   `json:"generation"`
	Applied      bool     `json:"applied"`
}

// The original signature and issued nonce are verified before any wallet row.
// Exact replay does not append history or restore an obsolete projection.
func AcceptWalletMappingConsent(ctx context.Context, owner WalletMappingOwner, original protocol.WalletMappingConsent, coldkeySs58 string) (accepted *WalletMappingAccepted, returnErr error) {
	statement, hash, err := protocol.VerifyWalletMappingConsent(ctx, original)
	if err != nil {
		return nil, err
	}
	if statement.Domain != owner.Domain || statement.UserId != [16]byte(owner.UserId) || statement.ClientId != [16]byte(owner.ClientId) || statement.NetworkId != [16]byte(owner.NetworkId) {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	actualKey, err := DecodeBittensorAddress(coldkeySs58)
	if err != nil || actualKey != statement.Coldkey {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
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
		lockWalletMapping(ctx, tx, domain, owner.ClientId)
		if err := checkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		var issued string
		err := tx.QueryRow(ctx, `SELECT message FROM wallet_mapping_challenge WHERE nonce=$1`, statement.Nonce[:]).Scan(&issued)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && issued != original.Message {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
		}
		server.Raise(err)
		var retained []byte
		err = tx.QueryRow(ctx, `SELECT original FROM wallet_mapping_consent WHERE nonce=$1`, statement.Nonce[:]).Scan(&retained)
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
		if owner.Prospective != nil {
			scope := owner.Prospective
			if statement.Schema != protocol.WalletMappingProspectiveSchema || scope.RootKey == nil || statement.Prospective.Signer != crypto.PubkeyToAddress(scope.RootKey.PublicKey) || scope.Boundary.Validate() != nil || scope.Boundary.Epoch >= statement.FromEpoch || scope.Boundary.Epoch < statement.Prospective.Boundary.Epoch || scope.Boundary.Block < statement.Prospective.Boundary.Block || scope.Boundary.Block == statement.Prospective.Boundary.Block && scope.Boundary != statement.Prospective.Boundary {
				panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
			}
		} else if statement.Schema == protocol.WalletMappingProspectiveSchema {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingUnavailable})
		}
		now := server.NowUtc()
		if now.Unix() < statement.IssuedAt || now.Unix() > statement.ExpiresAt {
			panic(walletMappingAbort{cause: errors.New("wallet mapping original challenge is expired or not yet effective")})
		}
		var previous []byte
		var generation uint64
		err = tx.QueryRow(ctx, `SELECT generation,original_hash FROM wallet_mapping_consent WHERE domain_hash=$1 AND client_id=$2 ORDER BY generation DESC LIMIT 1`, domain[:], owner.ClientId).Scan(&generation, &previous)
		if errors.Is(err, pgx.ErrNoRows) {
			generation, previous = 0, make([]byte, 32)
		} else {
			server.Raise(err)
		}
		if statement.Generation != generation+1 || !bytes.Equal(previous, statement.PreviousHash[:]) {
			panic(walletMappingAbort{cause: protocol.ErrWalletMappingIntegrity})
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO wallet_mapping_consent(domain_hash,client_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, domain[:], owner.ClientId, statement.Generation, hash[:], statement.Nonce[:], raw, now))
		signature := "0x" + hex.EncodeToString(original.Signature[:])
		setStProviderWalletOriginalInTx(ctx, tx, owner.ClientId, owner.NetworkId, coldkeySs58, statement.Coldkey, &original.Message, &signature)
		setStWalletInTx(ctx, tx, owner.NetworkId, coldkeySs58, statement.Coldkey)
		accepted = &WalletMappingAccepted{OriginalHash: hash, Generation: statement.Generation, Applied: true}
	})
	return accepted, nil
}

// Exact-window callers supply a separately approved head. This endpoint returns
// original bytes only; it does not declare its newest SQL row authoritative.
func ReadWalletMappingHistory(ctx context.Context, domain protocol.ClientKeyHistoryDomain, clientId server.Id, generation uint64, head [32]byte) ([]protocol.WalletMappingConsent, error) {
	if ctx == nil || generation == 0 || generation > protocol.MaxWalletMappingHistory || head == ([32]byte{}) {
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
		rows, err := conn.Query(owner, `SELECT generation,original_hash,original FROM wallet_mapping_consent WHERE domain_hash=$1 AND client_id=$2 AND generation<=$3 ORDER BY generation`, digest[:], clientId, generation)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var index uint64
				var raw []byte
				server.Raise(rows.Scan(&index, &last, &raw))
				if index != uint64(len(result))+1 || len(raw) > protocol.MaxWalletMappingConsentBytes {
					server.Raise(fmt.Errorf("wallet mapping retained sequence is invalid"))
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
