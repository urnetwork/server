// The real public seed path must reuse its first signed response; a valid
// historic request does not depend on current directory or hot projection state.
package controller

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A lost reply cannot produce another provider assignment on the ordinary route.
func TestVerifyOriginalRequestPublicSeedRetryReusesOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		assign, clientId, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifySeedArgs(t, clientId, vpk, key, connect.VerifyMMin)
		for index := 0; index < 2; index++ {
			result, err := Verify(request, testVerifySession(ctx, ips[server.Id(assign.Trail[0])]))
			next, ok := result.(*connect.VerifyAssignResult)
			if err != nil || !ok || next.TrailId != assign.TrailId || !bytes.Equal(next.AssignSig, assign.AssignSig) {
				t.Fatal("same signed seed acquired another original assignment", result, err)
			}
		}
		model.RollupVerifyProviderStats(ctx, server.NowUtc(), verifySettings())
		var assigned int64
		for _, row := range model.GetVerifyProviderStats(ctx, server.Id(assign.NextHop)) {
			assigned += row.Assignments
		}
		if assigned != 1 {
			t.Fatal("identical public seed counted duplicate exposure", assigned)
		}
	})
}

// Original authority remains usable after registration retirement and terminal
// cleanup. Returned old assignment bytes cannot resurrect an expired trail.
func TestVerifyOriginalRequestPublicRetryAfterKeyAndHotCleanup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		assign, clientId, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifySeedArgs(t, clientId, vpk, key, connect.VerifyMMin)
		trail := model.GetVerifyTrail(ctx, server.Id(assign.TrailId))
		model.InsertVerifyTrail(ctx, model.NewExpiredVerifyTrailRow(trail))
		model.ExpireVerifyTrail(ctx, server.Id(assign.TrailId))
		model.RemoveClientPublicKey(ctx, clientId)
		result, err := Verify(request, testVerifySession(ctx, ips[server.Id(assign.Trail[0])]))
		retained, ok := result.(*connect.VerifyAssignResult)
		if err != nil || !ok || !bytes.Equal(retained.AssignSig, assign.AssignSig) {
			t.Fatal("current directory replaced original request authority", result, err)
		}
		if _, err := Verify(testVerifyExtendArgs(t, clientId, vpk, key, assign), testVerifySession(ctx, ips[server.Id(assign.NextHop)])); err == nil {
			t.Fatal("original seed retry resurrected expired work")
		}
	})
}

// Exact lookup is independent of timestamp-index pagination, while missing and
// foreign originals remain null rather than an authenticated zero trial count.
func TestVerifyOriginalRequestPublicExactLookupAndMissing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		assign, clientId, _, _, ips := testVerifyOriginalRoute(t, ctx)
		original := model.GetLatestVerifyOriginal(ctx, server.Id(assign.TrailId))
		body := verifyDecodeOriginal(original)
		locator := &model.VerifyOriginalRequest{Scope: body.Scope, ClientId: clientId, Message: body.RequestMessage, Signature: body.RequestSignature}
		result, err := GetVerifyOriginalRequest(locator, testVerifySession(ctx, ips[server.Id(assign.Trail[0])]))
		if err != nil || result == nil || result.Original == nil || !bytes.Equal(result.Original.Body, original.Body) {
			t.Fatal("bounded exact request lookup lost original bytes", result, err)
		}
		foreign := *locator
		foreign.ClientId = server.NewId()
		missing, err := GetVerifyOriginalRequest(&foreign, testVerifySession(ctx, ips[server.Id(assign.Trail[0])]))
		if err != nil || missing == nil || missing.Original != nil {
			t.Fatal("foreign client borrowed complete original", missing, err)
		}
		foreign = *locator
		foreign.Signature = append([]byte(nil), foreign.Signature...)
		foreign.Signature[0] ^= 1
		if _, err := GetVerifyOriginalRequest(&foreign, testVerifySession(ctx, ips[server.Id(assign.Trail[0])])); err == nil {
			t.Fatal("unsigned request reached original lookup")
		}
	})
}

// The server commits before the transport owner loses its first response.
func TestVerifyOriginalRequestPublicSeedCommitLostReply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		assign, clientId, vpk, key, ips := testVerifyOriginalRoute(t, ctx)
		request := testVerifySeedArgs(t, clientId, vpk, key, connect.VerifyMMin)
		request.ClientNonce[0] ^= 1
		message, err := connect.BuildVerifySeedMessage(request.Vpk, request.ClientNonce, byte(request.M))
		if err != nil {
			t.Fatal(err)
		}
		request.SeedSig = connect.SignVerifyMessage(key, message)
		lost := errors.New("synthetic lost original seed response")
		verifyOriginalAfterCommit = func() { panic(lost) }
		defer func() { verifyOriginalAfterCommit = nil }()
		recovered := server.HandleError(func() {
			_, err := Verify(request, testVerifySession(ctx, ips[server.Id(assign.Trail[0])]))
			server.Raise(err)
		})
		cause, ok := recovered.(error)
		if !ok || !errors.Is(cause, lost) {
			t.Fatal("original seed commit boundary was not reached", recovered)
		}
		verifyOriginalAfterCommit = nil
		original := model.GetVerifyOriginalRequest(ctx, model.VerifyOriginalRequest{Scope: verifyCurrentOriginalScope(), ClientId: clientId, Message: message, Signature: request.SeedSig})
		if original == nil {
			t.Fatal("committed original disappeared with lost reply")
		}
		body := verifyDecodeOriginal(original)
		result, err := Verify(request, testVerifySession(ctx, ips[server.Id(assign.Trail[0])]))
		retry, ok := result.(*connect.VerifyAssignResult)
		if err != nil || !ok || server.Id(retry.TrailId) != body.Trail.TrailId {
			t.Fatal("lost seed reply minted a second durable trail", result, err)
		}
	})
}
