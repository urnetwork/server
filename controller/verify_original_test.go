// Crash boundaries prove original custody and replay without timing assumptions.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Build a real route using only synthetic identities and documentation IPs.
func testVerifyOriginalRoute(t testing.TB, ctx context.Context) (*connect.VerifyAssignResult, server.Id, ed25519.PublicKey, ed25519.PrivateKey, map[server.Id]string) {
	settings := model.DefaultVerifySettings()
	SetVerifySettings(settings)
	testVerifyInstallServerKey()
	ips := map[server.Id]string{}
	var seedId server.Id
	for index := 0; index < connect.VerifyMMin; index++ {
		ip := fmt.Sprintf("192.0.2.%d", 10+index)
		clientId := testVerifyProvider(ctx, netip.MustParseAddr(ip), settings)
		ips[clientId] = ip
		if index == 0 {
			seedId = clientId
		}
	}
	validatorId, vpk, key := testVerifyValidator(ctx)
	result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, key, connect.VerifyMMin), testVerifySession(ctx, ips[seedId]))
	if err != nil {
		t.Fatal(err)
	}
	assign, ok := result.(*connect.VerifyAssignResult)
	if !ok {
		t.Fatalf("seed response %T", result)
	}
	return assign, validatorId, vpk, key, ips
}

// A committed assignment survives a process failure before Redis advances.
func TestVerifyOriginalCommitRecoversAssignment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		assign, validatorId, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifyExtendArgs(t, validatorId, vpk, key, assign)
		session := testVerifySession(ctx, ips[server.Id(assign.NextHop)])
		crash := errors.New("synthetic failure after original commit")
		verifyOriginalAfterCommit = func() { panic(crash) }
		defer func() { verifyOriginalAfterCommit = nil }()
		if err := server.HandleError(func() { _, err := Verify(request, session); server.Raise(err) }); !errors.Is(err, crash) {
			t.Fatalf("crash boundary: %v", err)
		}
		verifyOriginalAfterCommit = nil
		trailId := server.Id(assign.TrailId)
		if hot := model.GetVerifyTrail(ctx, trailId); hot == nil || len(hot.Hops) != 1 {
			t.Fatalf("Redis advanced before publication: %+v", hot)
		}
		original := model.GetLatestVerifyOriginal(ctx, trailId)
		retained := verifyDecodeOriginal(original)
		if retained.PreviousDepth != 1 || len(retained.Trail.Hops) != 2 || retained.Trail.Pending == nil || !bytes.Equal(retained.RequestSignature, request.ExtendSig) {
			t.Fatalf("missing original transition: %+v", retained)
		}
		expected, err := verifyDecodeCachedResponse(retained.ResponseJson)
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 2; index++ {
			result, err := Verify(request, session)
			if err != nil {
				t.Fatal(err)
			}
			next, ok := result.(*connect.VerifyAssignResult)
			if !ok || !bytes.Equal(next.AssignSig, expected.(*connect.VerifyAssignResult).AssignSig) {
				t.Fatalf("replay replaced exact original: %T", result)
			}
		}
		if hot := model.GetVerifyTrail(ctx, trailId); len(hot.Hops) != 2 {
			t.Fatalf("duplicate confirmation: %d", len(hot.Hops))
		}
		model.RollupVerifyProviderStats(ctx, server.NowUtc(), verifySettings())
		var confirmations int64
		for _, row := range model.GetVerifyProviderStats(ctx, server.Id(assign.NextHop)) {
			confirmations += row.Confirmations
		}
		if confirmations != 1 {
			t.Fatalf("confirmation projection=%d, want1", confirmations)
		}
		originals := model.ListVerifyOriginals(ctx, time.UnixMilli(int64(retained.Trail.CreateMs)).Add(-time.Second), server.NowUtc().Add(time.Second), 100)
		if len(originals) != 2 {
			t.Fatalf("original assignment census=%d, want2", len(originals))
		}
	})
}

// The final proof is reconstructed from committed bytes after a failed publish.
func TestVerifyOriginalCommitRecoversFinal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		assign, validatorId, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		for len(assign.Trail) < connect.VerifyMMin-1 {
			request := testVerifyExtendArgs(t, validatorId, vpk, key, assign)
			result, err := Verify(request, testVerifySession(ctx, ips[server.Id(assign.NextHop)]))
			if err != nil {
				t.Fatal(err)
			}
			assign = result.(*connect.VerifyAssignResult)
		}
		request := testVerifyExtendArgs(t, validatorId, vpk, key, assign)
		session := testVerifySession(ctx, ips[server.Id(assign.NextHop)])
		crash := errors.New("synthetic final publication failure")
		verifyOriginalAfterCommit = func() { panic(crash) }
		defer func() { verifyOriginalAfterCommit = nil }()
		if err := server.HandleError(func() { _, err := Verify(request, session); server.Raise(err) }); !errors.Is(err, crash) {
			t.Fatalf("crash boundary: %v", err)
		}
		verifyOriginalAfterCommit = nil
		trailId := server.Id(assign.TrailId)
		if row := model.GetVerifyTrailRow(ctx, trailId); row != nil {
			t.Fatal("terminal projection committed before crash boundary")
		}
		original := model.GetLatestVerifyOriginal(ctx, trailId)
		retained := verifyDecodeOriginal(original)
		result, err := Verify(request, session)
		if err != nil {
			t.Fatal(err)
		}
		final, ok := result.(*connect.VerifyFinalResult)
		if !ok {
			t.Fatalf("recovered %T", result)
		}
		expected, err := verifyDecodeCachedResponse(retained.ResponseJson)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(final.Proof.FinalSig, expected.(*connect.VerifyFinalResult).Proof.FinalSig) {
			t.Fatal("recovery signed a different final")
		}
		row := model.GetVerifyTrailRow(ctx, trailId)
		if row == nil || len(row.OriginalState) == 0 || !bytes.Equal(row.FinalSig, final.Proof.FinalSig) {
			t.Fatal("final response escaped durable original custody")
		}
		if hot := model.GetVerifyTrail(ctx, trailId); len(hot.Hops) != connect.VerifyMMin || hot.Status != model.VerifyTrailStatusComplete {
			t.Fatalf("recovered state: %+v", hot)
		}
	})
}

// Expiry committed before a lost Redis write cannot later become a final.
func TestVerifyOriginalExpiryCannotResume(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		assign, validatorId, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		trailId := server.Id(assign.TrailId)
		trail := model.GetVerifyTrail(ctx, trailId)
		model.InsertVerifyTrail(ctx, model.NewExpiredVerifyTrailRow(trail))
		request := testVerifyExtendArgs(t, validatorId, vpk, key, assign)
		if _, err := Verify(request, testVerifySession(ctx, ips[server.Id(assign.NextHop)])); err == nil {
			t.Fatal("durably expired assignment resumed")
		}
		if hot := model.GetVerifyTrail(ctx, trailId); hot.Status != model.VerifyTrailStatusExpired || len(hot.Hops) != 1 {
			t.Fatalf("expired projection: %+v", hot)
		}
	})
}

// An empty key index cannot authenticate a self-selected binding key.
func TestSnBindingOriginalRequiresRegisteredKey(t *testing.T) {
	clientId := server.NewId()
	key := [32]byte{7}
	cases := []struct {
		keys map[server.Id][32]byte
		want bool
	}{
		{keys: nil, want: false},
		{keys: map[server.Id][32]byte{server.NewId(): key}, want: false},
		{keys: map[server.Id][32]byte{clientId: {}}, want: false},
		{keys: map[server.Id][32]byte{clientId: {8}}, want: false},
		{keys: map[server.Id][32]byte{clientId: key}, want: true},
	}
	for index, c := range cases {
		if got := snBindingClientKeyMatches(c.keys, clientId, key); got != c.want {
			t.Fatalf("case%d: got%t want%t", index, got, c.want)
		}
	}
}
