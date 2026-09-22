// Signed client-key transitions are committed before Redis projection or
// public publication. The current head and every exact signed generation
// survive process crashes and client reaping. Functions are safe concurrently.
package model

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

const (
	MaxStClientKeyHistoryRegistrations = uint64(1024)
	MaxStClientKeyHistoryBytes         = uint64(8 * 1024 * 1024)
)

// Both canonical layers are retained before any public write. The outer hash
// names the existing evidence endpoint; the inner hash names key continuity.
type StClientKeyHistoryRecord struct {
	Registration      protocol.ClientKeyRegistration
	RegistrationBytes []byte
	EvidenceHash      string
	EvidenceBytes     []byte
}

// Typed mutation inputs come only from authenticated client control and the
// controller's real pinned operator observation. Signers are local owners.
type StClientKeyRegistrationInput struct {
	Domain       protocol.ClientKeyHistoryDomain
	DeploymentID string
	ClientID     server.Id
	PublicKey    []byte
	Boundary     protocol.ClientKeyEffectiveBoundary
	RootKey      *ecdsa.PrivateKey
	ArtifactKey  *ecdsa.PrivateKey
	CreatedAt    time.Time
}

// Private key ownership is copied before the first DB callback. A malformed
// or mismatched public half must fail rather than being silently reconstructed.
func ownStClientKeySigner(key *ecdsa.PrivateKey) (*ecdsa.PrivateKey, error) {
	if key == nil || key.D == nil || key.Curve != crypto.S256() || key.X == nil || key.Y == nil || key.D.Sign() <= 0 || key.D.Cmp(crypto.S256().Params().N) >= 0 || !crypto.S256().IsOnCurve(key.X, key.Y) {
		return nil, errors.New("client-key private signing owner is malformed")
	}
	owned, err := crypto.ToECDSA(crypto.FromECDSA(key))
	if err != nil || owned.X.Cmp(key.X) != 0 || owned.Y.Cmp(key.Y) != 0 {
		return nil, errors.Join(errors.New("client-key private and public signing identities differ"), err)
	}
	return owned, nil
}

// Refuses row tamper and mismatched wrappers before returning a usable record.
func decodeStClientKeyHistoryRecord(registrationBytes, registrationHash, evidenceBytes []byte, evidenceHash string) (StClientKeyHistoryRecord, error) {
	registration, err := protocol.DecodeClientKeyRegistration(registrationBytes)
	if err != nil {
		return StClientKeyHistoryRecord{}, err
	}
	contentHash := sha256.Sum256(registrationBytes)
	if !bytes.Equal(contentHash[:], registrationHash) || len(evidenceBytes) == 0 || len(evidenceBytes) > startifact.MaxClientKeyEvidenceBytes {
		return StClientKeyHistoryRecord{}, errors.New("stored client-key registration byte identity differs")
	}
	var envelope startifact.EvidenceEnvelope
	if err := json.Unmarshal(evidenceBytes, &envelope); err != nil {
		return StClientKeyHistoryRecord{}, err
	}
	canonical, err := startifact.EvidenceBytes(&envelope)
	if err != nil || !bytes.Equal(canonical, evidenceBytes) || !bytes.Equal(envelope.Payload, registrationBytes) || envelope.Kind != startifact.ClientKeyRegistrationEvidenceKind || envelope.ContentHash != evidenceHash || envelope.RunID != "" || envelope.ChainID != registration.Domain.ChainID || envelope.Netuid != registration.Domain.Netuid || envelope.GenesisHash != fmt.Sprintf("0x%x", registration.Domain.GenesisHash) || sha256.Sum256([]byte(envelope.DeploymentID)) != registration.Domain.DeploymentIDHash {
		return StClientKeyHistoryRecord{}, errors.Join(errors.New("stored client-key evidence wrapper differs from its signed registration"), err)
	}
	return StClientKeyHistoryRecord{Registration: registration, RegistrationBytes: bytes.Clone(registrationBytes), EvidenceHash: evidenceHash, EvidenceBytes: bytes.Clone(evidenceBytes)}, nil
}

// Exact retries reuse the original signed record, including creation metadata.
// Only pure encoding/signing happens inside the DB transaction; external blob
// callbacks and Redis projection cannot retain a row lock through their exit.
func StoreStClientKeyRegistration(ctx context.Context, input StClientKeyRegistrationInput) (result *StClientKeyHistoryRecord, resultErr error) {
	if ctx == nil || input.ClientID == (server.Id{}) || len(input.PublicKey) != 0 && len(input.PublicKey) != 32 || input.CreatedAt.IsZero() || input.RootKey == nil || input.ArtifactKey == nil {
		return nil, errors.New("client-key registration mutation owner is incomplete")
	}
	if err := errors.Join(ctx.Err(), input.Domain.Validate(), input.Boundary.Validate()); err != nil {
		return nil, err
	}
	if sha256.Sum256([]byte(input.DeploymentID)) != input.Domain.DeploymentIDHash {
		return nil, errors.New("client-key registration deployment differs from its namespace")
	}
	input.PublicKey = bytes.Clone(input.PublicKey)
	rootKey, err := ownStClientKeySigner(input.RootKey)
	if err != nil {
		return nil, err
	}
	artifactKey, err := ownStClientKeySigner(input.ArtifactKey)
	if err != nil {
		return nil, err
	}
	domainHash, err := input.Domain.Digest()
	if err != nil {
		return nil, err
	}
	var publicKey [32]byte
	copy(publicKey[:], input.PublicKey)
	if len(input.PublicKey) != 0 && publicKey == ([32]byte{}) {
		return nil, errors.New("client-key registration cannot publish the zero key")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = errors.Join(resultErr, err)
			} else {
				panic(recovered)
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = nil
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		result = nil
		// The authenticated client row serializes first insertion as well as
		// rotations, and the delete trigger retires this head in the same lock.
		var networkID server.Id
		server.Raise(tx.QueryRow(ctx, `SELECT network_id FROM network_client WHERE client_id = $1 AND active = true FOR UPDATE`, input.ClientID).Scan(&networkID))
		var priorRegistration *protocol.ClientKeyRegistration
		var storedDomain, registrationBytes, registrationHash, evidenceBytes []byte
		var storedNetworkID server.Id
		var generation int64
		var retired bool
		var evidenceHash string
		err := tx.QueryRow(ctx, `
			SELECT h.domain_hash, h.network_id, h.generation, h.retired,
				r.registration, r.registration_hash, r.evidence, r.evidence_hash
			FROM st_client_key_head h
			JOIN st_client_key_history r ON r.client_id = h.client_id AND r.generation = h.generation
			WHERE h.client_id = $1 FOR UPDATE OF h
		`, input.ClientID).Scan(&storedDomain, &storedNetworkID, &generation, &retired, &registrationBytes, &registrationHash, &evidenceBytes, &evidenceHash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			server.Raise(err)
		}
		if err == nil {
			if retired || storedNetworkID != networkID || !bytes.Equal(storedDomain, domainHash[:]) || generation <= 0 {
				server.Raise(errors.New("client-key registration cannot replace a retired or different-domain head"))
			}
			prior, err := decodeStClientKeyHistoryRecord(registrationBytes, registrationHash, evidenceBytes, evidenceHash)
			server.Raise(err)
			if prior.Registration.Domain != input.Domain || prior.Registration.ClientID != [16]byte(input.ClientID) || prior.Registration.NetworkID != [16]byte(networkID) || prior.Registration.Generation != uint64(generation) {
				server.Raise(errors.New("client-key registration head differs from its signed row"))
			}
			if prior.Registration.Present == (len(input.PublicKey) != 0) && prior.Registration.PublicKey == publicKey {
				result = &prior
				return
			}
			priorRegistration = &prior.Registration
		}
		if generation >= int64(MaxStClientKeyHistoryRegistrations) || generation == math.MaxInt64 {
			server.Raise(errors.New("client-key generation history has reached its finite lifetime bound"))
		}
		registration := protocol.ClientKeyRegistration{Domain: input.Domain, ClientID: [16]byte(input.ClientID), NetworkID: [16]byte(networkID), Generation: uint64(generation + 1), Present: len(input.PublicKey) != 0, PublicKey: publicKey, EffectiveBoundary: input.Boundary}
		if priorRegistration != nil {
			registration.PreviousHash, err = priorRegistration.ContentHash()
			server.Raise(err)
		}
		server.Raise(protocol.SignClientKeyRegistration(&registration, rootKey))
		server.Raise(registration.Follows(priorRegistration))
		registrationBytes, err = registration.Bytes()
		server.Raise(err)
		contentHash := sha256.Sum256(registrationBytes)
		evidenceBytes, evidenceHash, err = startifact.SealClientKeyRegistrationEvidence(input.DeploymentID, registration, artifactKey, input.CreatedAt)
		server.Raise(err)
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, registration, evidence_hash, evidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, input.ClientID, generation+1, domainHash[:], contentHash[:], registrationBytes, evidenceHash, evidenceBytes))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation, retired)
			VALUES ($1, $2, $3, $4, false)
			ON CONFLICT (client_id) DO UPDATE SET generation = EXCLUDED.generation
		`, input.ClientID, domainHash[:], networkID, generation+1))
		result = &StClientKeyHistoryRecord{Registration: registration, RegistrationBytes: registrationBytes, EvidenceHash: evidenceHash, EvidenceBytes: evidenceBytes}
	}, server.TxReadCommitted)
	if result == nil {
		return nil, errors.New("client-key registration transaction lost its completion result")
	}
	return result, ctx.Err()
}

// Reads count/byte admission and every row from one repeatable-read snapshot.
// A current API lookup never silently truncates an overlong or missing history.
func LoadStClientKeyHistory(ctx context.Context, domain protocol.ClientKeyHistoryDomain, clientID server.Id, maximumRegistrations, maximumBytes uint64) (result []StClientKeyHistoryRecord, resultErr error) {
	if ctx == nil || clientID == (server.Id{}) || maximumRegistrations == 0 || maximumRegistrations > MaxStClientKeyHistoryRegistrations || maximumBytes == 0 || maximumBytes > MaxStClientKeyHistoryBytes {
		return nil, errors.New("client-key history lookup or finite bounds are invalid")
	}
	domainHash, err := domain.Digest()
	if err != nil {
		return nil, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = errors.Join(resultErr, err)
			} else {
				panic(recovered)
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = nil
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		var generation int64
		var retired bool
		var networkID server.Id
		var storedDomain []byte
		server.Raise(tx.QueryRow(ctx, `
			SELECT h.generation, h.retired, h.domain_hash, h.network_id
			FROM st_client_key_head h JOIN network_client n ON n.client_id = h.client_id AND n.network_id = h.network_id AND n.active = true
			WHERE h.client_id = $1
		`, clientID).Scan(&generation, &retired, &storedDomain, &networkID))
		if retired || generation <= 0 || uint64(generation) > maximumRegistrations || !bytes.Equal(storedDomain, domainHash[:]) {
			server.Raise(errors.New("client-key history head is retired, wrong-domain or exceeds its finite census"))
		}
		var count, first, last, storedBytes int64
		// Cover row bytes, decoded/canonical copies, owned return buffers and
		// per-record fixed control storage before the proportional SELECT.
		controlBytes := int64(reflect.TypeFor[StClientKeyHistoryRecord]().Size() + reflect.TypeFor[startifact.EvidenceEnvelope]().Size() + reflect.TypeFor[protocol.ClientKeyRegistration]().Size() + 256)
		server.Raise(tx.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(MIN(generation), 0), COALESCE(MAX(generation), 0),
				COALESCE(SUM(6 * octet_length(registration) + 6 * octet_length(evidence) + 2 * octet_length(evidence_hash) + octet_length(registration_hash) + $2), 0)::bigint
			FROM st_client_key_history WHERE client_id = $1
		`, clientID, controlBytes).Scan(&count, &first, &last, &storedBytes))
		if count != generation || first != 1 || last != generation || storedBytes <= 0 || uint64(storedBytes) > maximumBytes {
			server.Raise(errors.New("client-key history census, continuity or bytes exceed the independent allowance"))
		}
		rows, err := tx.Query(ctx, `
			SELECT generation, domain_hash, registration_hash, registration, evidence_hash, evidence
			FROM st_client_key_history WHERE client_id = $1 ORDER BY generation ASC LIMIT $2
		`, clientID, generation+1)
		server.Raise(err)
		defer rows.Close()
		result = make([]StClientKeyHistoryRecord, 0, int(generation))
		var prior *protocol.ClientKeyRegistration
		for rows.Next() {
			if int64(len(result)) >= generation {
				server.Raise(errors.New("client-key history returned extra generations"))
			}
			var rowGeneration int64
			var registrationBytes, registrationHash, evidenceBytes, rowDomain []byte
			var evidenceHash string
			server.Raise(rows.Scan(&rowGeneration, &rowDomain, &registrationHash, &registrationBytes, &evidenceHash, &evidenceBytes))
			record, err := decodeStClientKeyHistoryRecord(registrationBytes, registrationHash, evidenceBytes, evidenceHash)
			server.Raise(err)
			if rowGeneration != int64(len(result))+1 || !bytes.Equal(rowDomain, domainHash[:]) || record.Registration.Generation != uint64(rowGeneration) || record.Registration.Domain != domain || record.Registration.ClientID != [16]byte(clientID) || record.Registration.NetworkID != [16]byte(networkID) {
				server.Raise(errors.New("client-key history row differs from its exact signed census"))
			}
			server.Raise(record.Registration.Follows(prior))
			result = append(result, record)
			prior = &result[len(result)-1].Registration
		}
		server.Raise(rows.Err())
		if int64(len(result)) != generation {
			server.Raise(errors.New("client-key history returned an incomplete generation census"))
		}
	}, pgx.ReadOnly)
	if len(result) == 0 {
		return nil, errors.New("client-key history lookup lost its complete result")
	}
	return result, ctx.Err()
}

// Current unsigned API callers may use Redis only for clients with no signed
// history. A retired/tombstoned history is authoritative absence, not fallback.
func stClientKeyCurrent(ctx context.Context, clientID server.Id) (publicKey []byte, found bool, resultErr error) {
	if ctx == nil || clientID == (server.Id{}) {
		return nil, false, errors.New("client-key current lookup identity is invalid")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = errors.Join(resultErr, err)
			} else {
				panic(recovered)
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			publicKey, found = nil, false
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		var retired, clientExists bool
		var networkID server.Id
		var generation int64
		var registrationBytes, registrationHash, evidenceBytes, domainHash []byte
		var evidenceHash string
		err := conn.QueryRow(ctx, `
			SELECT h.retired, EXISTS (SELECT 1 FROM network_client n WHERE n.client_id = h.client_id AND n.network_id = h.network_id AND n.active = true),
				h.network_id, h.generation, h.domain_hash, r.registration, r.registration_hash, r.evidence, r.evidence_hash
			FROM st_client_key_head h JOIN st_client_key_history r ON r.client_id = h.client_id AND r.generation = h.generation
			WHERE h.client_id = $1
		`, clientID).Scan(&retired, &clientExists, &networkID, &generation, &domainHash, &registrationBytes, &registrationHash, &evidenceBytes, &evidenceHash)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		found = true
		if retired || !clientExists {
			return
		}
		record, err := decodeStClientKeyHistoryRecord(registrationBytes, registrationHash, evidenceBytes, evidenceHash)
		server.Raise(err)
		wantedDomain, err := record.Registration.Domain.Digest()
		server.Raise(err)
		if record.Registration.ClientID != [16]byte(clientID) || record.Registration.NetworkID != [16]byte(networkID) || generation <= 0 || record.Registration.Generation != uint64(generation) || !bytes.Equal(domainHash, wantedDomain[:]) {
			server.Raise(errors.New("client-key current projection differs from the signed head"))
		}
		if record.Registration.Present {
			publicKey = bytes.Clone(record.Registration.PublicKey[:])
		}
	})
	return publicKey, found, ctx.Err()
}

// Non-authenticated deletion paths retire current access without inventing a
// signed historical tombstone. The immutable positive statements remain intact.
func retireStClientKeyHistory(ctx context.Context, clientID server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE st_client_key_head SET retired = true WHERE client_id = $1`, clientID))
	}, server.TxReadCommitted)
}

// Epoch-close's existing batched Redis reader must not keep using stale keys
// after a signed rotation or reaping. Bounded SQL batches override only known
// history clients; a tombstone deletes the legacy projection from the result.
func overlayStClientKeyCurrent(ctx context.Context, clientIDs []server.Id, publicKeyKVs map[server.Id][32]byte) {
	for chunk := range slices.Chunk(clientIDs, 100) {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `
				SELECT h.client_id, h.retired,
					EXISTS (SELECT 1 FROM network_client n WHERE n.client_id = h.client_id AND n.network_id = h.network_id AND n.active = true),
					h.network_id, h.generation, h.domain_hash, r.registration, r.registration_hash, r.evidence, r.evidence_hash
				FROM st_client_key_head h JOIN st_client_key_history r ON r.client_id = h.client_id AND r.generation = h.generation
				WHERE h.client_id = ANY($1::uuid[]) LIMIT 101
			`, idStrings(chunk))
			server.Raise(err)
			defer rows.Close()
			wanted := make(map[server.Id]bool, len(chunk))
			for _, clientID := range chunk {
				wanted[clientID] = true
			}
			seen := map[server.Id]bool{}
			for rows.Next() {
				var clientID, networkID server.Id
				var retired, clientExists bool
				var generation int64
				var domainHash, registrationBytes, registrationHash, evidenceBytes []byte
				var evidenceHash string
				server.Raise(rows.Scan(&clientID, &retired, &clientExists, &networkID, &generation, &domainHash, &registrationBytes, &registrationHash, &evidenceBytes, &evidenceHash))
				if !wanted[clientID] || seen[clientID] || len(seen) >= 100 {
					server.Raise(errors.New("client-key current batch returned a duplicate or outside client"))
				}
				seen[clientID] = true
				delete(publicKeyKVs, clientID)
				if retired || !clientExists {
					continue
				}
				record, err := decodeStClientKeyHistoryRecord(registrationBytes, registrationHash, evidenceBytes, evidenceHash)
				server.Raise(err)
				wantedDomain, err := record.Registration.Domain.Digest()
				server.Raise(err)
				if record.Registration.ClientID != [16]byte(clientID) || record.Registration.NetworkID != [16]byte(networkID) || generation <= 0 || record.Registration.Generation != uint64(generation) || !bytes.Equal(domainHash, wantedDomain[:]) {
					server.Raise(errors.New("client-key current batch differs from its signed head"))
				}
				if record.Registration.Present {
					publicKeyKVs[clientID] = record.Registration.PublicKey
				}
			}
			server.Raise(rows.Err())
		})
	}
}
