// Original SDK startup enrollment proves key possession for one generation.
// Its bounded index helps independent approvers discover tuples; neither the
// index nor admission claims a complete SDK population or current lifecycle.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

const MaximumProviderWorkOwnersPerClient = 1024
const ProviderWorkOwnerIndexSchema = "urnetwork-sdk-whole-work-owner-index-v1"

var ErrProviderWorkOwnerPending = errors.New("provider work owner awaits original client-key admission")

// An explicit index of retained statements has no completeness/current flag.
type ProviderWorkOwnerIndex struct {
	Schema string   `json:"schema"`
	Owners [][]byte `json:"owners"`
}

// A previously retained statement survives key rotation and client deletion.
// New statements serialize with actual client-key mutation and require its
// independently signed current head; no anonymous key can consume custody.
func RetainProviderWorkOwner(ctx context.Context, raw []byte, domain [32]byte, rootSigner common.Address) (receipt protocol.OriginalWorkOwnerReceipt, resultErr error) {
	defer func() {
		if ctx != nil {
			resultErr = errors.Join(resultErr, ctx.Err(), context.Cause(ctx))
		}
		if resultErr != nil {
			receipt = protocol.OriginalWorkOwnerReceipt{}
		}
	}()
	defer providerWorkRecover(&resultErr)
	owner, err := protocol.DecodeOriginalWorkOwnerEnrollment(ctx, raw)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return receipt, err
		}
		return receipt, errors.Join(ErrProviderWorkInvalid, err)
	}
	if domain == ([32]byte{}) || owner.DomainHash != domain || rootSigner == (common.Address{}) {
		return receipt, ErrProviderWorkInvalid
	}
	digest := sha256.Sum256(raw)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("provider-work-owner/%x", owner.ClientId)))
		var prior, priorHash []byte
		err := tx.QueryRow(ctx, `SELECT original,owner_hash FROM provider_work_owner WHERE domain_hash=$1 AND client_id=$2 AND generation=$3`, domain[:], owner.ClientId[:], owner.Generation[:]).Scan(&prior, &priorHash)
		if err == nil {
			if !bytes.Equal(prior, raw) || !bytes.Equal(priorHash, digest[:]) {
				panic(ErrProviderWorkConflict)
			}
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		var networkId server.Id
		err = tx.QueryRow(ctx, `SELECT network_id FROM network_client WHERE client_id=$1 AND active=true FOR UPDATE`, server.Id(owner.ClientId)).Scan(&networkId)
		if errors.Is(err, pgx.ErrNoRows) {
			panic(ErrProviderWorkOwnerPending)
		}
		server.Raise(err)
		var registrationRaw, registrationHash, evidence []byte
		var evidenceHash string
		var keyGeneration int64
		var retired bool
		var recordedNetworkId server.Id
		err = tx.QueryRow(ctx, `SELECT h.network_id,h.generation,h.retired,r.registration,r.registration_hash,r.evidence,r.evidence_hash FROM st_client_key_head h JOIN st_client_key_history r ON r.client_id=h.client_id AND r.domain_hash=h.domain_hash AND r.generation=h.generation WHERE h.client_id=$1 AND h.domain_hash=$2 AND h.is_current FOR UPDATE OF h`, server.Id(owner.ClientId), domain[:]).Scan(&recordedNetworkId, &keyGeneration, &retired, &registrationRaw, &registrationHash, &evidence, &evidenceHash)
		if errors.Is(err, pgx.ErrNoRows) {
			panic(ErrProviderWorkOwnerPending)
		}
		server.Raise(err)
		record, err := decodeStClientKeyHistoryRecord(registrationRaw, registrationHash, evidence, evidenceHash)
		if err != nil {
			panic(errors.Join(ErrProviderWorkConflict, err))
		}
		registeredDomain, err := record.Registration.Domain.Digest()
		if err != nil || registeredDomain != domain || record.Registration.Signer != rootSigner || record.Registration.ClientID != owner.ClientId || record.Registration.NetworkID != [16]byte(networkId) || recordedNetworkId != networkId || keyGeneration <= 0 || uint64(keyGeneration) != record.Registration.Generation {
			panic(errors.Join(ErrProviderWorkConflict, err))
		}
		if retired || !record.Registration.Present || record.Registration.PublicKey != owner.PublicKey {
			panic(ErrProviderWorkInvalid)
		}
		var count int
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM provider_work_owner WHERE client_id=$1`, owner.ClientId[:]).Scan(&count))
		if count >= MaximumProviderWorkOwnersPerClient {
			panic(ErrProviderWorkCapacity)
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_owner(domain_hash,client_id,generation,public_key,owner_hash,original,key_registration) VALUES($1,$2,$3,$4,$5,$6,$7)`, domain[:], owner.ClientId[:], owner.Generation[:], owner.PublicKey[:], digest[:], raw, registrationRaw))
	}, pgx.ReadCommitted)
	receipt = protocol.OriginalWorkOwnerReceipt{Schema: protocol.OriginalWorkOwnerReceiptSchema, OwnerHash: digest}
	return
}

// The full retained per-key index fits the fixed lifetime client bound. An
// absent statement is unknown, and no caller receives a truncated index.
func ListProviderWorkOwners(ctx context.Context, owner ProviderWorkOwner) (result ProviderWorkOwnerIndex, resultErr error) {
	defer func() {
		if ctx != nil {
			resultErr = errors.Join(resultErr, ctx.Err(), context.Cause(ctx))
		}
		if resultErr != nil {
			result = ProviderWorkOwnerIndex{}
		}
	}()
	defer providerWorkRecover(&resultErr)
	result = ProviderWorkOwnerIndex{Schema: ProviderWorkOwnerIndexSchema, Owners: make([][]byte, 0)}
	if ctx == nil || owner.DomainHash == ([32]byte{}) || owner.ClientId == ([16]byte{}) || owner.PublicKey == ([32]byte{}) {
		return result, ErrProviderWorkInvalid
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original,owner_hash FROM provider_work_owner WHERE domain_hash=$1 AND client_id=$2 AND public_key=$3 ORDER BY generation LIMIT $4`, owner.DomainHash[:], owner.ClientId[:], owner.PublicKey[:], MaximumProviderWorkOwnersPerClient+1)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var raw, digest []byte
				server.Raise(rows.Scan(&raw, &digest))
				enrollment, err := protocol.DecodeOriginalWorkOwnerEnrollment(ctx, raw)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						panic(err)
					}
					panic(errors.Join(ErrProviderWorkConflict, err))
				}
				actual := sha256.Sum256(raw)
				if !bytes.Equal(actual[:], digest) || enrollment.DomainHash != owner.DomainHash || enrollment.ClientId != owner.ClientId || enrollment.PublicKey != owner.PublicKey {
					panic(ErrProviderWorkConflict)
				}
				result.Owners = append(result.Owners, raw)
			}
		})
	})
	if len(result.Owners) > MaximumProviderWorkOwnersPerClient {
		return ProviderWorkOwnerIndex{}, ErrProviderWorkCapacity
	}
	return result, errors.Join(ctx.Err(), context.Cause(ctx))
}

// Exact public lookup never guesses which of several SDK lifecycles is live.
func GetProviderWorkOwner(ctx context.Context, owner ProviderWorkOwner) (raw []byte, resultErr error) {
	if owner.Generation == ([16]byte{}) {
		return nil, ErrProviderWorkInvalid
	}
	index, err := ListProviderWorkOwners(ctx, owner)
	if err != nil {
		return nil, err
	}
	for _, raw := range index.Owners {
		enrollment, err := protocol.DecodeOriginalWorkOwnerEnrollment(ctx, raw)
		if err != nil {
			return nil, err
		}
		if enrollment.Generation == owner.Generation {
			return raw, nil
		}
	}
	return nil, ErrProviderWorkMissing
}
