// A durable request identity precedes any visible assignment projection. The
// original client/domain/message/signature binds the first retained response;
// mutable Redis state and directory membership cannot mint a second assignment.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/urnetwork/server/v2026"
)

// This public locator is not an authorization verdict. Readers independently
// verify the returned original signature, scope and exact request bytes.
type VerifyOriginalRequest struct {
	Scope     *VerifyOriginalScope `json:"scope,omitempty"`
	ClientId  server.Id            `json:"client_id"`
	Message   []byte               `json:"message"`
	Signature []byte               `json:"signature"`
}

// Canonical identity covers the whole original deployment, including legacy
// nil scope. A different client or deployment cannot borrow a valid signature.
func (self VerifyOriginalRequest) Hash() ([32]byte, error) {
	if self.ClientId == (server.Id{}) || len(self.Message) == 0 || len(self.Message) > 2048 || len(self.Signature) != 64 {
		return [32]byte{}, errors.New("invalid verification original request locator")
	}
	raw, err := json.Marshal(self)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("urnetwork-verify-original-request-v1\x00"), raw...)), nil
}

// Compare exact originals even after a hash index hit. Hashes never replace
// canonical request or original deployment equality at the admission boundary.
func (self VerifyOriginalRequest) matches(body *VerifyOriginalBody) bool {
	return body != nil && body.Trail != nil && body.Trail.ClientId == self.ClientId && ((body.Scope == nil && self.Scope == nil) || (body.Scope != nil && self.Scope != nil && *body.Scope == *self.Scope)) && bytes.Equal(body.RequestMessage, self.Message) && bytes.Equal(body.RequestSignature, self.Signature)
}

// The read seam works inside the existing transaction or an owned connection.
// A maximum of two historical matches is sufficient to prove ambiguity.
func readVerifyOriginalRequest(ctx context.Context, conn server.PgCanQuery, request VerifyOriginalRequest) (retained *VerifyOriginalTransition) {
	hash, err := request.Hash()
	server.Raise(err)
	rows, err := conn.Query(ctx, `SELECT t.original_body,t.original_signature FROM verify_original_request r JOIN verify_original_transition t USING(trail_id,previous_depth) WHERE r.request_hash=$1`, hash[:])
	server.WithPgResult(rows, err, func() {
		if rows.Next() {
			retained = &VerifyOriginalTransition{}
			server.Raise(rows.Scan(&retained.Body, &retained.Signature))
		}
	})
	if retained == nil {
		scope, err := json.Marshal(request.Scope)
		server.Raise(err)
		rows, err = conn.Query(ctx, `SELECT t.original_body,t.original_signature FROM verify_original_request_lookup r
   JOIN verify_original_transition t USING(trail_id,previous_depth)
   WHERE r.client_id=$1 AND sha256(r.request_message)=sha256($2::bytea) AND sha256(r.request_signature)=sha256($3::bytea)
   AND r.request_message=$2 AND r.request_signature=$3 AND r.scope_json=$4::jsonb
   ORDER BY r.trail_id,r.previous_depth LIMIT 2`, request.ClientId, request.Message, request.Signature, string(scope))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				if retained != nil {
					panic(errors.New("verification original signed request has multiple retained trails"))
				}
				retained = &VerifyOriginalTransition{}
				server.Raise(rows.Scan(&retained.Body, &retained.Signature))
			}
		})
	}
	if retained != nil {
		body, err := DecodeVerifyOriginal(retained)
		server.Raise(err)
		if !request.matches(body) {
			panic(errors.New("verification original request index conflicts with retained bytes"))
		}
	}
	return retained
}

// One scoped advisory lock fences initial request retention across independent
// server processes. Hash collisions merely serialize; full bytes still decide.
func retainVerifyOriginalRequestInTx(ctx context.Context, tx server.PgTx, body *VerifyOriginalBody) *VerifyOriginalTransition {
	request := VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature}
	lockVerifyOriginalRequest(ctx, tx, request)
	original := readVerifyOriginalRequest(ctx, tx, request)
	if original == nil {
		refuseClosedVerifyRequest(ctx, tx, request)
	}
	return original
}

// Called only after the matching original tuple exists in this same transaction.
func indexVerifyOriginalRequestInTx(ctx context.Context, tx server.PgTx, body *VerifyOriginalBody) {
	request := VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature}
	hash, err := request.Hash()
	server.Raise(err)
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_original_request(request_hash,trail_id,previous_depth) VALUES($1,$2,$3) ON CONFLICT(request_hash) DO NOTHING`, hash[:], body.Trail.TrailId, body.PreviousDepth))
	retained := readVerifyOriginalRequest(ctx, tx, request)
	original, err := DecodeVerifyOriginal(retained)
	server.Raise(err)
	if original.Trail.TrailId != body.Trail.TrailId || original.PreviousDepth != body.PreviousDepth {
		panic(errors.New("verification original request publication changed its first tuple"))
	}
}

// A missing request remains unavailable. This API does not turn absence from
// an operator index into a signed zero-exposure or complete-window assertion.
func GetVerifyOriginalRequest(ctx context.Context, request VerifyOriginalRequest) (retained *VerifyOriginalTransition) {
	server.Db(ctx, func(conn server.PgConn) {
		retained = readVerifyOriginalRequest(ctx, conn, request)
		if retained == nil {
			refuseClosedVerifyRequest(ctx, conn, request)
		}
	})
	return
}
