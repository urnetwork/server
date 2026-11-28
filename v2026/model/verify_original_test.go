// Retention tests use signed synthetic originals and explicit storage failures.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Construct a complete seed/assign pair with independently checkable signatures.
func testVerifySignedOriginal(t testing.TB) (*VerifyOriginalTransition, ed25519.PrivateKey) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, ed25519.SeedSize))
	verifier := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, ed25519.SeedSize))
	nowMs := uint64(server.NowUtc().UnixMilli())
	networkId := server.NewId()
	trail := &VerifyTrail{TrailId: server.NewId(), ClientId: server.NewId(), Vpk: verifier.Public().(ed25519.PublicKey), ServerNonce: bytes.Repeat([]byte{13}, connect.VerifyNonceSize), M: connect.VerifyMMin, ServerKeyId: 7, Status: VerifyTrailStatusActive, CreateMs: nowMs, ActivityMs: nowMs, Hops: []*VerifyTrailHop{{ClientId: server.NewId(), NetworkId: &networkId, ConfirmedMs: nowMs, Seed: true}}, Pending: &VerifyTrailHop{ClientId: server.NewId(), NetworkId: &networkId, AssignedMs: nowMs, AssignN: 8}}
	message, err := connect.BuildVerifySeedMessage(trail.Vpk, bytes.Repeat([]byte{14}, connect.VerifyNonceSize), byte(trail.M))
	if err != nil {
		t.Fatal(err)
	}
	assignMessage, err := connect.BuildVerifyAssignMessage(trail.ServerKeyId, connect.Id(trail.TrailId), trail.ServerNonce, trail.Vpk, byte(trail.M), []connect.Id{connect.Id(trail.Hops[0].ClientId), connect.Id(trail.Pending.ClientId)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(struct {
		Assign *connect.VerifyAssignResult `json:"assign,omitempty"`
	}{Assign: &connect.VerifyAssignResult{TrailId: connect.Id(trail.TrailId), ServerNonce: trail.ServerNonce, Trail: []connect.Id{connect.Id(trail.Hops[0].ClientId)}, NextHop: connect.Id(trail.Pending.ClientId), M: trail.M, ServerKeyId: trail.ServerKeyId, AssignSig: ed25519.Sign(key, assignMessage)}})
	if err != nil {
		t.Fatal(err)
	}
	body := &VerifyOriginalBody{Schema: VerifyOriginalDomain, Trail: trail, RecoveryMs: nowMs + uint64((DefaultVerifySettings().StepTimeout + DefaultVerifySettings().StepTimeoutGrace).Milliseconds()), RequestMessage: message, RequestSignature: ed25519.Sign(verifier, message), ResponseJson: string(response)}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &VerifyOriginalTransition{Body: encoded, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), encoded...))}, key
}

// Metadata and both original wire signatures must agree under caller-pinned keys.
func TestVerifyOriginalAuthenticatesMetadataAndWire(t *testing.T) {
	original, key := testVerifySignedOriginal(t)
	publicKey := key.Public().(ed25519.PublicKey)
	body, err := ValidateVerifyOriginal(original, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	body.Trail.Pending.AssignN++
	changed, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	forged := &VerifyOriginalTransition{Body: changed, Signature: original.Signature}
	if _, err := ValidateVerifyOriginal(forged, publicKey); err == nil {
		t.Fatal("unsigned sample metadata accepted")
	}
	body.RequestSignature[0] ^= 1
	changed, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	forged = &VerifyOriginalTransition{Body: changed, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), changed...))}
	if _, err := ValidateVerifyOriginal(forged, publicKey); err == nil {
		t.Fatal("server receipt replaced verifier authority")
	}
	wrong := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{15}, ed25519.SeedSize))
	if _, err := ValidateVerifyOriginal(original, wrong.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("untrusted historical server key accepted")
	}
}

// An overwritten Redis response does not erase the exact first durable receipt.
func TestVerifyOriginalRetentionIsImmutableAndRequestBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		original, key := testVerifySignedOriginal(t)
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		RetainVerifyOriginal(ctx, original)
		body.Trail.Pending.AssignN++
		changed, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		retry := &VerifyOriginalTransition{Body: changed, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), changed...))}
		if retained := RetainVerifyOriginal(ctx, retry); !bytes.Equal(retained.Body, original.Body) {
			t.Fatal("uncertain retry replaced first assignment")
		}
		body.RequestMessage[0] ^= 1
		changed, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		conflict := &VerifyOriginalTransition{Body: changed, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), changed...))}
		if err := server.HandleError(func() { RetainVerifyOriginal(ctx, conflict) }); err == nil {
			t.Fatal("different request reused transition identity")
		}
		for _, statement := range []string{`UPDATE verify_original_transition SET original_body=original_body WHERE trail_id=$1`, `DELETE FROM verify_original_transition WHERE trail_id=$1`} {
			if err := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, statement, body.Trail.TrailId)) })
			}); err == nil {
				t.Fatal("immutable original mutation succeeded")
			}
		}
		if got := GetLatestVerifyOriginal(ctx, body.Trail.TrailId); got == nil || !bytes.Equal(got.Body, original.Body) {
			t.Fatal("original custody changed")
		}
	})
}

// Pending exposure, sampling metadata and assignment-time network survive expiry.
func TestVerifyExpiredOriginalRetainsPendingExposure(t *testing.T) {
	original, _ := testVerifySignedOriginal(t)
	body, err := DecodeVerifyOriginal(original)
	if err != nil {
		t.Fatal(err)
	}
	row := NewExpiredVerifyTrailRow(body.Trail)
	var retained VerifyTrail
	if err := json.Unmarshal(row.OriginalState, &retained); err != nil {
		t.Fatal(err)
	}
	pending := retained.Pending
	if pending == nil || pending.ClientId != body.Trail.Pending.ClientId || pending.AssignedMs != body.Trail.Pending.AssignedMs || pending.AssignN != body.Trail.Pending.AssignN || pending.NetworkId == nil || *pending.NetworkId != *body.Trail.Pending.NetworkId {
		t.Fatalf("lost pending original: %+v", pending)
	}
}

// A rejected durable expiry leaves its Redis pending/reap obligation untouched.
func TestVerifyExpiryRequiresDurableOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		now := server.NowUtc()
		trail := testVerifyBuildTrail(false, uint64(now.Add(-settings.StepTimeout-settings.StepTimeoutGrace-time.Second).UnixMilli()))
		CreateVerifyTrail(ctx, trail, `{}`, settings)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION verify_test_refuse_expiry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic expiry storage refusal'; END $$; CREATE TRIGGER verify_test_refuse_expiry BEFORE INSERT ON verify_trail FOR EACH ROW EXECUTE FUNCTION verify_test_refuse_expiry()`))
		})
		remove := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER IF EXISTS verify_test_refuse_expiry ON verify_trail; DROP FUNCTION IF EXISTS verify_test_refuse_expiry()`))
			})
		}
		defer remove()
		if err := server.HandleError(func() { SweepExpiredVerifyTrails(ctx, now, settings) }); err == nil {
			t.Fatal("synthetic storage refusal not reached")
		}
		if hot := GetVerifyTrail(ctx, trail.TrailId); hot == nil || hot.Status != VerifyTrailStatusActive || hot.Pending == nil {
			t.Fatalf("expiry lost retryable state: %+v", hot)
		}
		if _, ok := testVerifyReapScore(ctx, trail.TrailId); !ok {
			t.Fatal("expiry dropped unfinished reaper obligation")
		}
		remove()
		if got := SweepExpiredVerifyTrails(ctx, now, settings); got != 1 {
			t.Fatalf("retry expiry=%d", got)
		}
		if row := GetVerifyTrailRow(ctx, trail.TrailId); row == nil || len(row.OriginalState) == 0 {
			t.Fatal("expiry retry lost original")
		}
	})
}

// Owner cancellation remains an unavailable operation, never signature evidence.
func TestVerifyOriginalCanceledReadPreservesCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		recovered := server.HandleError(func() { GetLatestVerifyOriginal(ctx, server.NewId()) })
		cause, ok := recovered.(error)
		if !ok || (!errors.Is(cause, context.Canceled) && !errors.Is(cause, server.DbContextDoneError)) {
			t.Fatalf("canceled original read: %v", recovered)
		}
	})
}

// A committed seed is recoverable even when no Redis key or reap entry exists.
func TestVerifyOriginalOutboxRecoversUnpublishedSeed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		original, _ := testVerifySignedOriginal(t)
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		RetainVerifyOriginal(ctx, original)
		if GetVerifyTrail(ctx, body.Trail.TrailId) != nil {
			t.Fatal("test must start before any Redis publication")
		}
		now := time.UnixMilli(int64(body.RecoveryMs + 1)).UTC()
		if got := SweepExpiredVerifyTrails(ctx, now, DefaultVerifySettings()); got != 1 {
			t.Fatalf("durable continuation=%d", got)
		}
		row := GetVerifyTrailRow(ctx, body.Trail.TrailId)
		if row == nil || row.Status != VerifyTrailRowStatusExpired || len(row.OriginalState) == 0 {
			t.Fatal("unpublished assignment original was lost")
		}
		if ids := dueVerifyOriginalTrails(ctx, now, 100); len(ids) != 0 {
			t.Fatalf("finished continuation remained: %v", ids)
		}
		if got := SweepExpiredVerifyTrails(ctx, now, DefaultVerifySettings()); got != 0 {
			t.Fatalf("finished continuation repeated=%d", got)
		}
	})
}

// Replaying a publication after a lost response cannot increment exposure twice.
func TestVerifyOriginalPublicationIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		original, _ := testVerifySignedOriginal(t)
		body, err := DecodeVerifyOriginal(original)
		if err != nil {
			t.Fatal(err)
		}
		RetainVerifyOriginal(ctx, original)
		settings := DefaultVerifySettings()
		PublishVerifyOriginal(ctx, original, settings)
		PublishVerifyOriginal(ctx, original, settings)
		RollupVerifyProviderStats(ctx, server.NowUtc(), settings)
		var assignments int64
		for _, row := range GetVerifyProviderStats(ctx, body.Trail.Pending.ClientId) {
			assignments += row.Assignments
		}
		if assignments != 1 {
			t.Fatalf("replayed exposure=%d, want1", assignments)
		}
		if hot := GetVerifyTrail(ctx, body.Trail.TrailId); len(hot.Hops) != 1 || hot.Pending.ClientId != body.Trail.Pending.ClientId {
			t.Fatal("publication changed route")
		}
	})
}
