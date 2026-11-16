// Owner-signed request closure is a permanent execution fence, not a census.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// Permanent refusal and contradictory retained consent are distinct from
// unavailable storage or a canceled owner, whose original causes propagate.
var (
	ErrVerifyRequestClosed          = errors.New("409 verification request permanently closed")
	ErrVerifyRequestClosureConflict = errors.New("409 verification request closure contradicts retained originals")
)

// Exactly one branch is present. A committed original always wins over absence.
type VerifyRequestClosureResult struct {
	Original         *VerifyOriginalTransition                 `json:"original,omitempty"`
	ClosedUnreceived *protocol.ProviderAttemptClosedUnreceived `json:"closed_unreceived,omitempty"`
}

// Preserve the original scope grammar used by migration 773 and assignment.
func VerifyRequestClosureLocator(value protocol.ProviderAttemptRequestClosure) VerifyOriginalRequest {
	scope := value.Scope
	return VerifyOriginalRequest{Scope: &VerifyOriginalScope{Profile: scope.Profile, GenesisHash: scope.GenesisHash, DeploymentId: scope.DeploymentId, DeploymentKey: StDeploymentKey(scope.DeploymentKey), PolicyHash: scope.PolicyHash, Netuid: scope.Netuid, NoId: scope.NoId}, ClientId: server.Id(value.ClientId), Message: value.Message, Signature: value.RequestSignature}
}

// Original request locking precedes the rolling-writer wire fence consistently.
func lockVerifyOriginalRequest(ctx context.Context, tx server.PgTx, request VerifyOriginalRequest) [32]byte {
	hash, err := request.Hash()
	server.Raise(err)
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(hash[:8]))))
	server.RaisePgResult(tx.Exec(ctx, `SELECT verify_original_request_wire_lock($1,$2)`, request.Message, request.Signature))
	return hash
}

// The exact locator, closure and receipt survive key rotation and process loss.
func readVerifyRequestClosure(ctx context.Context, conn server.PgCanQuery, request VerifyOriginalRequest) ([]byte, *protocol.ProviderAttemptClosedUnreceived) {
	hash, err := request.Hash()
	server.Raise(err)
	var raw []byte
	var receipt *protocol.ProviderAttemptClosedUnreceived
	rows, err := conn.Query(ctx, `SELECT closure_original,receipt_body,receipt_signature FROM verify_original_request_closed WHERE request_hash=$1`, hash[:])
	server.WithPgResult(rows, err, func() {
		if rows.Next() {
			receipt = &protocol.ProviderAttemptClosedUnreceived{}
			server.Raise(rows.Scan(&raw, &receipt.Body, &receipt.Signature))
		}
	})
	if receipt != nil {
		var closure protocol.ProviderAttemptRequestClosure
		server.Raise(json.Unmarshal(raw, &closure))
		canonical, err := json.Marshal(closure)
		server.Raise(err)
		other := VerifyRequestClosureLocator(closure)
		otherHash, err := other.Hash()
		server.Raise(err)
		if !bytes.Equal(raw, canonical) || otherHash != hash || other.ClientId != request.ClientId || request.Scope == nil || *other.Scope != *request.Scope || !bytes.Equal(other.Message, request.Message) || !bytes.Equal(other.Signature, request.Signature) {
			panic(ErrVerifyRequestClosureConflict)
		}
	}
	return raw, receipt
}

// A missing read alone never creates this error or authorizes a tombstone.
func refuseClosedVerifyRequest(ctx context.Context, conn server.PgCanQuery, request VerifyOriginalRequest) {
	if _, receipt := readVerifyRequestClosure(ctx, conn, request); receipt != nil {
		panic(ErrVerifyRequestClosed)
	}
}

// Commit the first exact signed closure only if assignment did not win the same
// request fence. Retried delivery returns the first server key and signature.
func CloseVerifyOriginalRequest(ctx context.Context, closure protocol.ProviderAttemptRequestClosure, proposed *protocol.ProviderAttemptClosedUnreceived, serverKeys map[byte]ed25519.PublicKey) (result *VerifyRequestClosureResult) {
	server.Tx(ctx, func(tx server.PgTx) { result = closeVerifyOriginalRequestInTx(ctx, tx, closure, proposed, serverKeys) }, server.TxReadCommitted)
	return
}

// The caller owns the transaction through commit. No signed result is delivered
// before both the permanent fence and exact receipt are durably retained.
func closeVerifyOriginalRequestInTx(ctx context.Context, tx server.PgTx, closure protocol.ProviderAttemptRequestClosure, proposed *protocol.ProviderAttemptClosedUnreceived, serverKeys map[byte]ed25519.PublicKey) *VerifyRequestClosureResult {
	server.Raise(protocol.VerifyProviderAttemptRequestClosure(ctx, closure, closure.Scope))
	if proposed == nil {
		panic(ErrVerifyRequestClosureConflict)
	}
	server.Raise(protocol.VerifyProviderAttemptClosedUnreceived(ctx, *proposed, closure, serverKeys))
	raw, err := json.Marshal(closure)
	server.Raise(err)
	if len(raw) > 8192 {
		panic(ErrVerifyRequestClosureConflict)
	}
	request := VerifyRequestClosureLocator(closure)
	hash := lockVerifyOriginalRequest(ctx, tx, request)
	if original := readVerifyOriginalRequest(ctx, tx, request); original != nil {
		return &VerifyRequestClosureResult{Original: original}
	}
	prior, receipt := readVerifyRequestClosure(ctx, tx, request)
	if receipt != nil {
		if !bytes.Equal(prior, raw) {
			panic(ErrVerifyRequestClosureConflict)
		}
		server.Raise(protocol.VerifyProviderAttemptClosedUnreceived(ctx, *receipt, closure, serverKeys))
		return &VerifyRequestClosureResult{ClosedUnreceived: receipt}
	}
	scope, err := json.Marshal(request.Scope)
	server.Raise(err)
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_original_request_closed(request_hash,client_id,scope_json,request_message,request_signature,closure_original,receipt_body,receipt_signature) VALUES($1,$2,$3::jsonb,$4,$5,$6,$7,$8)`, hash[:], request.ClientId, string(scope), request.Message, request.Signature, raw, proposed.Body, proposed.Signature))
	return &VerifyRequestClosureResult{ClosedUnreceived: proposed}
}
