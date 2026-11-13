// Production seed and extension routes must honor a committed request fence.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A canceled original owner keeps both its standard cancellation and actual
// cause without acquiring SQL state or manufacturing a signature contradiction.
func TestVerifyRequestClosureCanceledOwnerPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("synthetic request closure owner stopped")
	cancel(cause)
	_, err := CloseVerifyOriginalRequest(&protocol.ProviderAttemptRequestClosure{}, testVerifySession(ctx, "192.0.2.11"))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || errors.Is(err, protocol.ErrProviderAttemptsIntegrity) {
		t.Fatal("request closure lost actual owner cancellation", err)
	}
}

// The deployment is independently configured before either request is signed.
func verifyClosureControllerConfig(t testing.TB) {
	t.Helper()
	priorConfig, priorKeys, priorSettings := stConfigInstance, verifyServerKeysInstance, verifySettingsInstance
	t.Cleanup(func() { SetStConfig(priorConfig); SetVerifyServerKeys(priorKeys); SetVerifySettings(priorSettings) })
	SetStConfig(&StConfig{Enabled: true, Profile: "testnet", ChainId: 945, GenesisHash: [32]byte{1}, ContractAddress: common.Address{1}, DeploymentId: "closure-controller-test", PolicyHash: [32]byte{2}, Netuid: 521, NoId: 1})
}

// Build closure from exact wire bytes produced by the ordinary client helper.
func verifyClosureControllerValue(t testing.TB, client server.Id, message, signature []byte, key ed25519.PrivateKey) *protocol.ProviderAttemptRequestClosure {
	t.Helper()
	scope, err := verifyRequestClosureScope()
	if err != nil {
		t.Fatal(err)
	}
	value, err := protocol.SealProviderAttemptRequestClosure(t.Context(), protocol.ProviderAttemptRequestClosure{Schema: protocol.ProviderAttemptRequestCloseDomain, Scope: scope, ClientId: connect.Id(client), Message: message, RequestSignature: signature, CutHash: [32]byte{3}, Epoch: 7, EndBlock: 20}, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// A late seed cannot allocate a trail; retired directory state cannot revoke
// the exact historical tombstone or cause a new server signature on retry.
func TestVerifyRequestClosureActualSeedAndRetiredKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureControllerConfig(t)
		ctx := t.Context()
		assign, client, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifySeedArgs(t, client, vpk, key, connect.VerifyMMin)
		request.ClientNonce[0] ^= 1
		message, err := connect.BuildVerifySeedMessage(vpk, request.ClientNonce, byte(request.M))
		if err != nil {
			t.Fatal(err)
		}
		request.SeedSig = ed25519.Sign(key, message)
		closure := verifyClosureControllerValue(t, client, message, request.SeedSig, key)
		owner := testVerifySession(ctx, ips[server.Id(assign.Trail[0])])
		result, err := CloseVerifyOriginalRequest(closure, owner)
		if err != nil || result == nil || result.ClosedUnreceived == nil || result.Original != nil {
			t.Fatal("actual seed closure did not commit", result, err)
		}
		recovered := server.HandleError(func() { _, err := Verify(request, owner); server.Raise(err) })
		cause, ok := recovered.(error)
		if !ok || !errors.Is(cause, model.ErrVerifyRequestClosed) {
			t.Fatal("ordinary seed ignored closed request", recovered)
		}
		if got := model.ListVerifyOriginals(ctx, time.Unix(0, 0), server.NowUtc().Add(time.Hour), 100); len(got) != 1 {
			t.Fatal("closed seed created another original", len(got))
		}
		model.RemoveClientPublicKey(ctx, client)
		retry, err := CloseVerifyOriginalRequest(closure, owner)
		if err != nil || retry == nil || retry.ClosedUnreceived == nil || !bytes.Equal(retry.ClosedUnreceived.Body, result.ClosedUnreceived.Body) || !bytes.Equal(retry.ClosedUnreceived.Signature, result.ClosedUnreceived.Signature) {
			t.Fatal("retired key lost original closure receipt", retry, err)
		}
		bad := *closure
		bad.Scope.NoId++
		foreign, err := protocol.SealProviderAttemptRequestClosure(ctx, bad, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CloseVerifyOriginalRequest(foreign, owner); err == nil {
			t.Fatal("foreign deployment closed a local request")
		}
	})
}

// Closing an outstanding extension does not remove its already assigned hop,
// and a late extension cannot confirm it or allocate another provider.
func TestVerifyRequestClosureActualExtendKeepsPendingExposure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureControllerConfig(t)
		ctx := t.Context()
		assign, client, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifyExtendArgs(t, client, vpk, key, assign)
		message, err := connect.BuildVerifyExtendMessage(assign.TrailId, assign.ServerNonce, vpk, byte(assign.M), verifyConnectIds(request.Trail))
		if err != nil {
			t.Fatal(err)
		}
		closure := verifyClosureControllerValue(t, client, message, request.ExtendSig, key)
		owner := testVerifySession(ctx, ips[server.Id(assign.NextHop)])
		result, err := CloseVerifyOriginalRequest(closure, owner)
		if err != nil || result == nil || result.ClosedUnreceived == nil {
			t.Fatal("actual extend closure did not commit", result, err)
		}
		recovered := server.HandleError(func() { _, err := Verify(request, owner); server.Raise(err) })
		cause, ok := recovered.(error)
		if !ok || !errors.Is(cause, model.ErrVerifyRequestClosed) {
			t.Fatal("ordinary extension ignored closed request", recovered)
		}
		trail := model.GetVerifyTrail(ctx, server.Id(assign.TrailId))
		if trail == nil || trail.Pending == nil || trail.Pending.ClientId != server.Id(assign.NextHop) || len(trail.Hops) != 1 {
			t.Fatal("closing extension erased or confirmed previous exposure", trail)
		}
		if got := model.ListVerifyOriginals(ctx, time.Unix(0, 0), server.NowUtc().Add(time.Hour), 100); len(got) != 1 {
			t.Fatal("closed extension created another original", len(got))
		}
	})
}

// Exact recovery reads admit canonical extensions as well as seeds, without
// selecting a timestamp page or asking mutable client registration again.
func TestVerifyOriginalRequestPublicExactExtendAndClosureOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureControllerConfig(t)
		ctx := t.Context()
		assign, client, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifyExtendArgs(t, client, vpk, key, assign)
		owner := testVerifySession(ctx, ips[server.Id(assign.NextHop)])
		if _, err := Verify(request, owner); err != nil {
			t.Fatal(err)
		}
		original := model.GetLatestVerifyOriginal(ctx, server.Id(assign.TrailId))
		body := verifyDecodeOriginal(original)
		locator := &model.VerifyOriginalRequest{Scope: body.Scope, ClientId: client, Message: body.RequestMessage, Signature: body.RequestSignature}
		read, err := GetVerifyOriginalRequest(locator, owner)
		if err != nil || read == nil || read.Original == nil || !bytes.Equal(read.Original.Body, original.Body) {
			t.Fatal("public exact lookup rejected original extension", read, err)
		}
		closure := verifyClosureControllerValue(t, client, body.RequestMessage, body.RequestSignature, key)
		result, err := CloseVerifyOriginalRequest(closure, owner)
		if err != nil || result == nil || result.Original == nil || result.ClosedUnreceived != nil || !bytes.Equal(result.Original.Body, original.Body) {
			t.Fatal("closure replaced received extension with absence", result, err)
		}
	})
}
