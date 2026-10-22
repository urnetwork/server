// Typed client-key statements reuse the existing immutable evidence content
// path. The inner operator root signature is independent authority; the outer
// artifact signature identifies the actual public storage publisher.
package startifact

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

const (
	ClientKeyRegistrationEvidenceKind = "client-key-registration-v1"
	ClientKeyObservationEvidenceKind  = "client-key-observation-v1"
	MaxClientKeyEvidenceBytes         = 16 * 1024
)

// Client-key evidence is stored only under its content key. Each registration
// and observation used to be written a second time under one run-less history
// prefix per kind. At one registration per hosted window identity those two
// prefixes grew by tens of millions of tiny objects (2026-10-22) while nothing
// read them: validators receive these bytes inline from the observation
// routes, and /sn/evidence serves them by content hash.
var evidenceHistoryUnindexedKinds = []string{ClientKeyRegistrationEvidenceKind, ClientKeyObservationEvidenceKind}

// EvidenceHistoryIndexed reports whether evidence of this kind is also written
// under its history key. Kinds that are not are refused by the history routes.
func EvidenceHistoryIndexed(kind string) bool {
	return !slices.Contains(evidenceHistoryUnindexedKinds, kind)
}

// EvidenceHistoryUnindexedKinds returns the kinds stored by content key only.
func EvidenceHistoryUnindexedKinds() []string {
	return slices.Clone(evidenceHistoryUnindexedKinds)
}

// ClientKeyEvidenceStoredObjects is the number of objects one client-key
// evidence publication writes to a store: its content key only. Local write
// batches reserve this many writes per envelope.
const ClientKeyEvidenceStoredObjects = 1

// Builds one exact signed wrapper for durable preparation. Callers retain
// these bytes before publication and reuse them verbatim after any crash.
func SealClientKeyRegistrationEvidence(deploymentID string, registration protocol.ClientKeyRegistration, key *ecdsa.PrivateKey, createdAt time.Time) ([]byte, string, error) {
	encoded, err := registration.Bytes()
	if err != nil {
		return nil, "", err
	}
	return sealClientKeyEvidence(deploymentID, registration.Domain, ClientKeyRegistrationEvidenceKind, encoded, key, createdAt)
}

// A response's signed request and selected durable generation are immutable.
func SealClientKeyObservationEvidence(deploymentID string, observation protocol.ClientKeyObservation, key *ecdsa.PrivateKey, createdAt time.Time) ([]byte, string, error) {
	encoded, err := observation.Bytes()
	if err != nil {
		return nil, "", err
	}
	return sealClientKeyEvidence(deploymentID, observation.Domain, ClientKeyObservationEvidenceKind, encoded, key, createdAt)
}

// CreatedAt remains descriptive envelope metadata, not a historical clock proof.
func sealClientKeyEvidence(deploymentID string, domain protocol.ClientKeyHistoryDomain, kind string, payload []byte, key *ecdsa.PrivateKey, createdAt time.Time) ([]byte, string, error) {
	if err := domain.Validate(); err != nil {
		return nil, "", err
	}
	if sha256.Sum256([]byte(deploymentID)) != domain.DeploymentIDHash || key == nil || createdAt.IsZero() || len(payload) > protocol.MaxClientKeyStatementBytes {
		return nil, "", errors.New("client-key evidence deployment, signer, metadata or payload is invalid")
	}
	envelope := EvidenceEnvelope{DeploymentID: deploymentID, ChainID: domain.ChainID, GenesisHash: fmt.Sprintf("0x%x", domain.GenesisHash), Netuid: domain.Netuid, Kind: kind, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano), Payload: bytes.Clone(payload)}
	if err := SignEvidence(&envelope, key); err != nil {
		return nil, "", err
	}
	encoded, err := EvidenceBytes(&envelope)
	if err != nil || len(encoded) > MaxClientKeyEvidenceBytes {
		return nil, "", errors.Join(errors.New("client-key evidence exceeds its fixed wrapper bound"), err)
	}
	return encoded, envelope.ContentHash, nil
}

// Rechecks the retained exact wrapper and typed payload before the real
// create-if-absent writes and exact readback. No metadata-only receipt suffices.
func PublishClientKeyEvidence(ctx context.Context, store server.BlobStore, encoded []byte, contentHash string) (*Published, error) {
	if ctx == nil || store == nil || len(encoded) == 0 || len(encoded) > MaxClientKeyEvidenceBytes {
		return nil, errors.New("client-key publication owner or finite payload is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded = bytes.Clone(encoded)
	var envelope EvidenceEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return nil, err
	}
	canonical, err := EvidenceBytes(&envelope)
	if err != nil || !bytes.Equal(encoded, canonical) || envelope.ContentHash != contentHash || envelope.RunID != "" {
		return nil, errors.Join(errors.New("client-key evidence wrapper is not its exact retained identity"), err)
	}
	var domain protocol.ClientKeyHistoryDomain
	switch envelope.Kind {
	case ClientKeyRegistrationEvidenceKind:
		registration, err := protocol.DecodeClientKeyRegistration(envelope.Payload)
		if err != nil {
			return nil, err
		}
		domain = registration.Domain
	case ClientKeyObservationEvidenceKind:
		observation, err := protocol.DecodeClientKeyObservation(envelope.Payload)
		if err != nil {
			return nil, err
		}
		domain = observation.Domain
	default:
		return nil, errors.New("client-key publication has an unrelated evidence kind")
	}
	if envelope.ChainID != domain.ChainID || envelope.Netuid != domain.Netuid || envelope.GenesisHash != fmt.Sprintf("0x%x", domain.GenesisHash) || sha256.Sum256([]byte(envelope.DeploymentID)) != domain.DeploymentIDHash {
		return nil, errors.New("client-key payload and public evidence domains differ")
	}
	return PublishEvidence(ctx, store, &envelope)
}
