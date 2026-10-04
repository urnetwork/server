// Immutable signed transitions retain assignments and confirmations before
// Redis publishes them. The receipts attest an operator's observations, not a
// complete global verifier census or independently finalized chain state.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The domain is separate from the unchanged wire assign/final signatures.
const VerifyOriginalDomain = "urnetwork-verify-original-transition-v1\x00"

// Exact signed bytes remain authoritative; decoded fields are only indexes.
type VerifyOriginalTransition struct {
	Body      []byte `json:"body"`
	Signature []byte `json:"signature"`
}

// Full next state retains pending exposure and each original request/response.
// Network snapshots are operator observations at assignment, not current joins.
type VerifyOriginalBody struct {
	Schema           string               `json:"schema"`
	Scope            *VerifyOriginalScope `json:"scope,omitempty"`
	PreviousDepth    int                  `json:"previous_depth"`
	RecoveryMs       uint64               `json:"recovery_ms"`
	Trail            *VerifyTrail         `json:"trail"`
	RequestMessage   []byte               `json:"request_message"`
	RequestSignature []byte               `json:"request_signature"`
	ResponseJson     string               `json:"response_json"`
}

// Bind new metadata to the selected immutable deployment/policy. A missing
// scope is explicitly unusable as subnet earning authority.
type VerifyOriginalScope struct {
	Profile       string          `json:"profile"`
	GenesisHash   [32]byte        `json:"genesis_hash"`
	DeploymentId  string          `json:"deployment_id"`
	DeploymentKey StDeploymentKey `json:"deployment_key"`
	PolicyHash    [32]byte        `json:"policy_hash"`
	Netuid        uint64          `json:"netuid"`
	NoId          uint64          `json:"no_id"`
}

// Decode bounded retained bytes without promoting their source to authority.
func DecodeVerifyOriginal(original *VerifyOriginalTransition) (*VerifyOriginalBody, error) {
	if original == nil || len(original.Body) == 0 || len(original.Body) > 65536 || len(original.Signature) != ed25519.SignatureSize {
		return nil, errors.New("invalid verification original envelope")
	}
	var body VerifyOriginalBody
	decoder := json.NewDecoder(bytes.NewReader(original.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing verification original bytes")
	}
	if body.Schema != VerifyOriginalDomain || body.Trail == nil || body.Trail.Poison || body.PreviousDepth < 0 || body.PreviousDepth > 16 || len(body.Trail.Hops) < 1 || len(body.Trail.Hops) > 16 || len(body.RequestSignature) != ed25519.SignatureSize || len(body.RequestMessage) == 0 || body.RecoveryMs < body.Trail.ActivityMs {
		return nil, errors.New("invalid verification original body")
	}
	canonical, err := json.Marshal(&body)
	if err != nil || !bytes.Equal(canonical, original.Body) {
		return nil, errors.New("noncanonical verification original body")
	}
	return &body, nil
}

// A caller supplies its independently pinned historical key, never a key from
// the receipt itself. The receipt signature covers timing and sampling metadata.
func VerifyOriginalSignature(original *VerifyOriginalTransition, publicKey ed25519.PublicKey) bool {
	return original != nil && len(publicKey) == ed25519.PublicKeySize && ed25519.Verify(publicKey, append([]byte(VerifyOriginalDomain), original.Body...), original.Signature)
}

// Commit once per previous depth; an uncertain commit returns the exact first
// receipt for the same authenticated request instead of signing another choice.
func RetainVerifyOriginal(ctx context.Context, original *VerifyOriginalTransition) (retained *VerifyOriginalTransition) {
	server.Tx(ctx, func(tx server.PgTx) { retained = retainVerifyOriginalInTx(ctx, tx, original) }, server.TxReadCommitted)
	return
}

// The original request lock, first receipt and pending projection commit as
// one transaction. Caller owns the transaction and never retries a partial send.
func retainVerifyOriginalInTx(ctx context.Context, tx server.PgTx, original *VerifyOriginalTransition) *VerifyOriginalTransition {
	body, err := DecodeVerifyOriginal(original)
	server.Raise(err)
	retained := &VerifyOriginalTransition{}

	if prior := retainVerifyOriginalRequestInTx(ctx, tx, body); prior != nil {
		retained = prior
		return retained
	}
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_original_transition
   (trail_id,previous_depth,observed_time,original_body,original_signature) VALUES ($1,$2,$3,$4,$5)
   ON CONFLICT (trail_id,previous_depth) DO NOTHING`, body.Trail.TrailId, body.PreviousDepth,
		time.UnixMilli(int64(body.Trail.ActivityMs)).UTC(), original.Body, original.Signature))
	rows, err := tx.Query(ctx, `SELECT original_body,original_signature FROM verify_original_transition WHERE trail_id=$1 AND previous_depth=$2`, body.Trail.TrailId, body.PreviousDepth)
	server.WithPgResult(rows, err, func() {
		if !rows.Next() {
			panic(errors.New("verification original disappeared"))
		}
		server.Raise(rows.Scan(&retained.Body, &retained.Signature))
	})
	prior, err := DecodeVerifyOriginal(retained)
	server.Raise(err)
	if prior.Trail.TrailId != body.Trail.TrailId || prior.PreviousDepth != body.PreviousDepth || !bytes.Equal(prior.RequestMessage, body.RequestMessage) || !bytes.Equal(prior.RequestSignature, body.RequestSignature) {
		panic(errors.New("verification original request conflict"))
	}
	indexVerifyOriginalRequestInTx(ctx, tx, prior)
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_original_pending (trail_id,previous_depth,recovery_time) VALUES ($1,$2,$3)
 ON CONFLICT (trail_id) DO UPDATE SET previous_depth=EXCLUDED.previous_depth,recovery_time=EXCLUDED.recovery_time
 WHERE verify_original_pending.previous_depth <= EXCLUDED.previous_depth`, prior.Trail.TrailId, prior.PreviousDepth, time.UnixMilli(int64(prior.RecoveryMs)).UTC()))
	return retained
}

// The outbox also finds a committed seed whose Redis/reap publication was lost.
// Its primary key and due index bound each scheduled continuation batch.
func dueVerifyOriginalTrails(ctx context.Context, now time.Time, limit int) (trailIds []string) {
	if limit < 1 {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT trail_id FROM verify_original_pending WHERE recovery_time <= $1 ORDER BY recovery_time,trail_id LIMIT $2`, now.UTC(), limit)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var trailId server.Id
				server.Raise(rows.Scan(&trailId))
				trailIds = append(trailIds, trailId.String())
			}
		})
	})
	return
}

// Return the last committed transition for crash recovery, including one whose
// Redis publication never happened. A missing row is historical unknown.
func GetLatestVerifyOriginal(ctx context.Context, trailId server.Id) (original *VerifyOriginalTransition) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original_body,original_signature FROM verify_original_transition WHERE trail_id=$1 ORDER BY previous_depth DESC LIMIT 1`, trailId)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				original = &VerifyOriginalTransition{}
				server.Raise(rows.Scan(&original.Body, &original.Signature))
			}
		})
	})
	return
}

// The bounded public index includes active assignments; terminal-only indexes
// cannot expose lost or still pending assignments. It makes no completeness claim.
func ListVerifyOriginals(ctx context.Context, from, to time.Time, limit int) (originals []*VerifyOriginalTransition) {
	if !from.Before(to) || limit < 1 || limit > 10000 {
		panic(errors.New("invalid verification original range"))
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original_body,original_signature FROM verify_original_transition WHERE observed_time >= $1 AND observed_time < $2 ORDER BY observed_time,trail_id,previous_depth LIMIT $3`, from.UTC(), to.UTC(), limit)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				original := &VerifyOriginalTransition{}
				server.Raise(rows.Scan(&original.Body, &original.Signature))
				originals = append(originals, original)
			}
		})
	})
	return
}

// Replace the complete projection so replay after an uncertain Redis write
// cannot append a confirmation twice. Call under the existing per-trail fence.
func PublishVerifyOriginal(ctx context.Context, original *VerifyOriginalTransition, settings *VerifySettings) *VerifyOriginalBody {
	body, err := DecodeVerifyOriginal(original)
	server.Raise(err)
	trail := body.Trail
	if trail.Status == VerifyTrailStatusComplete {
		var response struct {
			Final *connect.VerifyFinalResult `json:"final,omitempty"`
		}
		server.Raise(json.Unmarshal([]byte(body.ResponseJson), &response))
		if response.Final == nil || response.Final.Proof == nil {
			panic(errors.New("verification final original missing proof"))
		}
		proof := response.Final.Proof
		hopsJson, err := json.Marshal(proof.Hops)
		server.Raise(err)
		state, err := json.Marshal(trail)
		server.Raise(err)
		completeTime := time.UnixMilli(int64(trail.ActivityMs)).UTC()
		InsertVerifyTrail(ctx, &VerifyTrailRow{TrailId: trail.TrailId, Vpk: trail.Vpk, ServerKeyId: trail.ServerKeyId, ServerNonce: trail.ServerNonce, Depth: trail.M, Status: VerifyTrailRowStatusComplete, HopsJson: string(hopsJson), FinalSig: proof.FinalSig, VerifierSig: proof.VerifierSig, CreateTime: time.UnixMilli(int64(trail.CreateMs)).UTC(), CompleteTime: &completeTime, OriginalState: state})
		row := GetVerifyTrailRow(ctx, trail.TrailId)
		if row == nil || row.Status != VerifyTrailRowStatusComplete || row.Depth != trail.M || row.ServerKeyId != trail.ServerKeyId || !bytes.Equal(row.Vpk, trail.Vpk) || !bytes.Equal(row.ServerNonce, trail.ServerNonce) || !bytes.Equal(row.FinalSig, proof.FinalSig) || !bytes.Equal(row.VerifierSig, proof.VerifierSig) || row.HopsJson != string(hopsJson) {
			panic(errors.New("verification terminal original conflict"))
		}
	} else if trail.Status == VerifyTrailStatusExpired {
		InsertVerifyTrail(ctx, NewExpiredVerifyTrailRow(trail))
		if row := GetVerifyTrailRow(ctx, trail.TrailId); row == nil || row.Status != VerifyTrailRowStatusExpired {
			panic(errors.New("verification expiry original conflict"))
		}
	}
	recordVerifyOriginalStats(ctx, body, settings)
	ttl := settings.TrailTtl(trail.M)
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			headerKey := verifyTrailHeaderKey(trail.TrailId)
			pipe.HSet(ctx, headerKey, "client_id", trail.ClientId.String(), "vpk", string(trail.Vpk), "server_nonce", string(trail.ServerNonce), "m", trail.M, "server_key_id", int(trail.ServerKeyId), "status", trail.Status, "poison", "0", "create_ms", trail.CreateMs, "activity_ms", trail.ActivityMs)
			pipe.Expire(ctx, headerKey, ttl)
			pipe.Del(ctx, verifyTrailHopsKey(trail.TrailId))
			for _, hop := range trail.Hops {
				pipe.RPush(ctx, verifyTrailHopsKey(trail.TrailId), verifyMarshalHop(hop))
			}
			pipe.Expire(ctx, verifyTrailHopsKey(trail.TrailId), ttl)
			if trail.Pending == nil {
				pipe.Del(ctx, verifyTrailPendingKey(trail.TrailId))
			} else {
				pipe.Set(ctx, verifyTrailPendingKey(trail.TrailId), verifyMarshalHop(trail.Pending), ttl)
			}
			pipe.Set(ctx, verifyTrailResponseKey(trail.TrailId), body.ResponseJson, ttl)
			return nil
		})
		server.Raise(err)
		if trail.Status == VerifyTrailStatusActive && trail.Pending != nil {
			deadline := float64(trail.Pending.AssignedMs) + float64((settings.StepTimeout+settings.StepTimeoutGrace)/time.Millisecond)
			server.Raise(r.ZAdd(ctx, verifyReapKey, redis.Z{Score: deadline, Member: trail.TrailId.String()}).Err())
		} else {
			server.Raise(r.ZRem(ctx, verifyReapKey, trail.TrailId.String()).Err())
		}
	})
	return body
}

// Counters are a recoverable convenience projection. A receipt marker shares
// the provider stats hash so a lost Redis reply cannot double the increment.
func recordVerifyOriginalStats(ctx context.Context, body *VerifyOriginalBody, settings *VerifySettings) {
	record := func(hop *VerifyTrailHop, depth int, confirmation bool) {
		if hop == nil || hop.Seed {
			return
		}
		atMs, field, markerKind, latencyField := hop.AssignedMs, "assignments", "a", ""
		if confirmation {
			atMs, field, markerKind = hop.ConfirmedMs, "confirmations", "c"
			latencyField = fmt.Sprintf("lb_%d", VerifyLatencyBucketIndex(int64(hop.ConfirmedMs-hop.AssignedMs)))
		}
		statsKey := verifyStatsKey(hop.ClientId, VerifyStatsPeriodStart(time.UnixMilli(int64(atMs)), settings))
		marker := fmt.Sprintf("original_%s_%s_%d", markerKind, body.Trail.TrailId, depth)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Eval(ctx, `
if redis.call('HSETNX', KEYS[1], ARGV[1], 1) == 1 then
 redis.call('HINCRBY', KEYS[1], ARGV[2], 1)
 if ARGV[3] ~= '' then redis.call('HINCRBY', KEYS[1], ARGV[3], 1) end
end
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1`, []string{statsKey}, marker, field, latencyField, (3 * settings.StatsPeriod).Milliseconds()).Err())
			server.Raise(r.SAdd(ctx, verifyStatClientsKey, hop.ClientId.String()).Err())
		})
	}
	if body.Trail.Pending != nil {
		record(body.Trail.Pending, len(body.Trail.Hops), false)
	}
	if body.PreviousDepth > 0 {
		record(body.Trail.Hops[len(body.Trail.Hops)-1], len(body.Trail.Hops)-1, true)
	}
}

// The historical projection is captured only when the provider is selected.
// Missing identities remain explicit and cannot acquire a later network owner.
func SnapshotVerifyProviderNetwork(ctx context.Context, hop *VerifyTrailHop) {
	if hop == nil {
		return
	}
	networkId, err := FindClientNetwork(ctx, hop.ClientId)
	if err == nil {
		hop.NetworkId = &networkId
	} else {
		hop.NetworkIssue = "identity_not_found"
	}
}

// Verify stored index consistency before a retained state is allowed to replace
// a live state. Its signature is checked at the controller's key boundary.
func VerifyOriginalMatchesTrail(body *VerifyOriginalBody, trail *VerifyTrail) error {
	if body == nil || body.Trail == nil || trail == nil || body.Trail.TrailId != trail.TrailId || body.Trail.ClientId != trail.ClientId || !bytes.Equal(body.Trail.Vpk, trail.Vpk) || !bytes.Equal(body.Trail.ServerNonce, trail.ServerNonce) || body.Trail.M != trail.M || body.Trail.ServerKeyId != trail.ServerKeyId {
		return fmt.Errorf("verification original identity mismatch")
	}
	return nil
}
