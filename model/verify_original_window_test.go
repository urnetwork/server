// Internal window metadata preserves the public canonical receipt grammar and
// leaves historical originals without a retained window on their legacy path.
package model

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// Receipts that predate window metadata still verify and round-trip exactly;
// recovery leaves their existing Redis window and legacy fallback unchanged.
func TestVerifyOriginalHeadWindowHistoricalCompatibility(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		original, key := testVerifySignedOriginal(t)
		if bytes.Contains(original.Body, []byte(`"head_window"`)) {
			t.Fatal("historical fixture unexpectedly contains a seed window")
		}
		body, err := ValidateVerifyOriginal(original, key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(body)
		if err != nil || !bytes.Equal(encoded, original.Body) {
			t.Fatal("historical original canonical bytes changed", err)
		}
		settings := DefaultVerifySettings()
		settings.CohortDeploymentKey = StDeploymentKey("101:synthetic-historical-window")
		SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &StEpochSummary{Epoch: 41}, time.Hour)
		RetainVerifyOriginal(ctx, original)
		SetVerifyTrailHeadWindow(ctx, body.Trail.TrailId, body.Trail.M, settings)
		SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &StEpochSummary{Epoch: 42}, time.Hour)
		PublishVerifyOriginal(ctx, original, settings)
		if window := GetVerifyTrailHeadWindow(ctx, body.Trail.TrailId, settings); window != "e41" {
			t.Fatalf("historical recovery replaced an existing window: %s", window)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, verifyTrailHeadWindowKey(body.Trail.TrailId)).Err())
		})
		PublishVerifyOriginal(ctx, original, settings)
		if window := GetVerifyTrailHeadWindow(ctx, body.Trail.TrailId, settings); window != "e42" {
			t.Fatalf("historical missing-window compatibility changed: %s", window)
		}
		retained := GetLatestVerifyOriginal(ctx, body.Trail.TrailId)
		if !bytes.Equal(retained.Body, original.Body) || !bytes.Equal(retained.Signature, original.Signature) {
			t.Fatal("historical recovery re-signed an inferred seed window")
		}
	})
}

// Recovery metadata cannot extend the public body grammar: the existing SN
// consumer must keep accepting the exact retained body and signature bytes.
func TestVerifyOriginalHeadWindowPublicGrammarUnchanged(t *testing.T) {
	original, key := testVerifySignedOriginal(t)
	if _, err := protocol.ValidateProviderAttemptReceipt(&protocol.ProviderAttemptReceipt{Body: original.Body, Signature: original.Signature}, key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal("existing SN receipt grammar changed", err)
	}
	withWindow := append(bytes.Clone(original.Body[:len(original.Body)-1]), []byte(`,"head_window":"e41"}`)...)
	signed := &VerifyOriginalTransition{Body: withWindow, Signature: ed25519.Sign(key, append([]byte(VerifyOriginalDomain), withWindow...))}
	if _, err := ValidateVerifyOriginal(signed, key.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("internal window metadata silently changed the signed original domain")
	}
}
