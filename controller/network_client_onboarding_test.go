// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
)

func TestPostPrimaryOnboardingSessionDropsCancellationButKeepsBound(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	clientSession := session.NewLocalClientSession(parentCtx, "", &jwt.ByJwt{
		NetworkId: server.NewId(),
		UserId:    server.NewId(),
	})
	cancelParent()
	defer clientSession.Cancel()

	startedAt := time.Now()
	postSession, cancelPost := newPostPrimaryOnboardingSession(clientSession)
	defer cancelPost()
	if clientSession.Ctx.Err() != context.Canceled {
		t.Fatalf("request context error = %v, want context canceled", clientSession.Ctx.Err())
	}
	if err := postSession.Ctx.Err(); err != nil {
		t.Fatalf("post-auth context inherited request cancellation: %v", err)
	}
	deadline, ok := postSession.Ctx.Deadline()
	if !ok {
		t.Fatal("post-auth context has no finite deadline")
	}
	if !deadline.After(startedAt) || deadline.After(time.Now().Add(onboardingPostPrimaryTimeout)) {
		t.Fatalf("post-primary context deadline = %s, want a current %s bound", deadline, onboardingPostPrimaryTimeout)
	}
	if postSession == clientSession {
		t.Fatal("post-auth work reused the request-owned session")
	}
}

// Client creation is already committed before this callback runs. Prove that
// an already-canceled request cannot discard either optional onboarding write,
// and that replaying the callback still writes each logical row only once.
func TestRecordAuthNetworkClientOnboardingPersistsOnceAfterCallerCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		queryCtx := context.Background()
		networkId := server.NewId()
		adminUserId := server.NewId()
		server.Tx(queryCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				queryCtx,
				`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
				networkId,
				"synthetic-post-auth-onboarding-"+networkId.String(),
				adminUserId,
			))
		})

		now := server.NowUtc()
		if err := model.AddOnboardingEvent(queryCtx, &model.OnboardingEvent{
			NetworkId:  networkId,
			Name:       model.EventLandingClicked,
			At:         now.Add(-time.Minute),
			ReceivedAt: now.Add(-time.Minute),
			Props: map[string]any{
				"step":      "synthetic-step",
				"flow_step": "synthetic-flow-step",
			},
		}); err != nil {
			t.Fatal(err)
		}

		callerCtx, cancelCaller := context.WithCancel(queryCtx)
		clientSession := session.NewLocalClientSession(callerCtx, "", &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    adminUserId,
		})
		cancelCaller()
		defer clientSession.Cancel()
		authClient := &model.AuthNetworkClientArgs{
			DeviceSpec: "synthetic Android device",
			TimeZone:   "Etc/UTC",
			Locale:     "en-US",
		}

		recordAuthNetworkClientOnboarding(authClient, clientSession)
		recordAuthNetworkClientOnboarding(authClient, clientSession)

		row := model.GetNetworkOnboarding(queryCtx, networkId)
		if row == nil {
			t.Fatal("canceled-parent campaign enrollment was not persisted")
		}
		if !row.Exited() || row.ExitReason != onboarding.ExitNoEmail {
			t.Fatalf("campaign enrollment exit = (%t, %q), want no_email", row.Exited(), row.ExitReason)
		}
		if count := onboardingEventCount(t, queryCtx, networkId, model.EventAppOpened); count != 1 {
			t.Fatalf("canceled-parent attributed app opens = %d, want 1", count)
		}

		contextNetworkId := server.NewId()
		server.Tx(queryCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				queryCtx,
				`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
				contextNetworkId,
				"synthetic-post-auth-context-"+contextNetworkId.String(),
				server.NewId(),
			))
		})
		if !model.CreateNetworkOnboarding(queryCtx, &model.NetworkOnboarding{
			NetworkId: contextNetworkId,
			CreatedAt: now,
			Email:     true,
		}) {
			t.Fatal("could not create active client-context fixture")
		}
		contextSession := session.NewLocalClientSession(callerCtx, "", &jwt.ByJwt{
			NetworkId: contextNetworkId,
			UserId:    server.NewId(),
		})
		defer contextSession.Cancel()
		recordAuthNetworkClientOnboarding(authClient, contextSession)
		recordAuthNetworkClientOnboarding(authClient, contextSession)
		contextRow := model.GetNetworkOnboarding(queryCtx, contextNetworkId)
		if contextRow == nil {
			t.Fatal("client-context fixture disappeared")
		}
		if contextRow.TimeZone != "Etc/UTC" || contextRow.Locale != "en-US" || contextRow.Platform != "android" {
			t.Fatalf(
				"canceled-parent client context = (%q, %q, %q), want (Etc/UTC, en-US, android)",
				contextRow.TimeZone,
				contextRow.Locale,
				contextRow.Platform,
			)
		}
	})
}

// NetworkCreate and AuthVerify have distinct result shapes but share the
// account-path enrollment boundary. Exercise both concrete adapters with an
// already-canceled request and prove their real PostgreSQL write is idempotent.
func TestAccountEnrollmentCallsitesPersistAfterCallerCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		queryCtx := context.Background()
		callerCtx, cancelCaller := context.WithCancel(queryCtx)
		cancelCaller()

		createNetworkId := server.NewId()
		createUserId := server.NewId()
		createUserAuth := model.Testing_CreateNetwork(
			queryCtx,
			createNetworkId,
			"synthetic-create-onboarding-"+createNetworkId.String(),
			createUserId,
		)
		createSession := session.NewLocalClientSession(callerCtx, "", &jwt.ByJwt{
			NetworkId: createNetworkId,
			UserId:    createUserId,
		})
		defer createSession.Cancel()
		createResult := &model.NetworkCreateResult{
			Network:  &model.NetworkCreateResultNetwork{NetworkId: createNetworkId},
			UserAuth: &createUserAuth,
		}
		enrollNetworkCreateOnboardingPostPrimary(createResult, createSession)
		enrollNetworkCreateOnboardingPostPrimary(createResult, createSession)
		if row := model.GetNetworkOnboarding(queryCtx, createNetworkId); row == nil {
			t.Fatal("NetworkCreate post-primary enrollment was not persisted")
		}
		pendingNetworkId := server.NewId()
		enrollNetworkCreateOnboardingPostPrimary(&model.NetworkCreateResult{
			Network: &model.NetworkCreateResultNetwork{NetworkId: pendingNetworkId},
			VerificationRequired: &model.NetworkCreateResultVerification{
				UserAuth: "synthetic-pending-auth",
			},
		}, createSession)
		if row := model.GetNetworkOnboarding(queryCtx, pendingNetworkId); row != nil {
			t.Fatal("NetworkCreate enrolled before required verification completed")
		}

		verifyNetworkId := server.NewId()
		verifyUserId := server.NewId()
		verifyUserAuth := model.Testing_CreateNetwork(
			queryCtx,
			verifyNetworkId,
			"synthetic-verify-onboarding-"+verifyNetworkId.String(),
			verifyUserId,
		)
		verifySession := session.NewLocalClientSession(callerCtx, "", &jwt.ByJwt{
			NetworkId: verifyNetworkId,
			UserId:    verifyUserId,
		})
		defer verifySession.Cancel()
		verifyToken := jwt.NewByJwt(
			verifyNetworkId,
			verifyUserId,
			"synthetic-verify-onboarding",
			false,
			false,
		).Sign()
		verifyResult := &model.AuthVerifyResult{
			Network: &model.AuthVerifyResultNetwork{ByJwt: verifyToken},
		}
		parsedByJwt := enrollAuthVerifyOnboardingPostPrimary(verifyResult, verifyUserAuth, verifySession)
		if parsedByJwt == nil || parsedByJwt.NetworkId != verifyNetworkId {
			t.Fatal("AuthVerify post-primary adapter did not derive the verified network")
		}
		enrollAuthVerifyOnboardingPostPrimary(verifyResult, verifyUserAuth, verifySession)
		if row := model.GetNetworkOnboarding(queryCtx, verifyNetworkId); row == nil {
			t.Fatal("AuthVerify post-primary enrollment was not persisted")
		}
		invalidNetworkId := server.NewId()
		invalidResult := &model.AuthVerifyResult{
			Network: &model.AuthVerifyResultNetwork{ByJwt: "synthetic-invalid-token"},
		}
		invalidSession := session.NewLocalClientSession(callerCtx, "", &jwt.ByJwt{
			NetworkId: invalidNetworkId,
			UserId:    server.NewId(),
		})
		defer invalidSession.Cancel()
		if byJwt := enrollAuthVerifyOnboardingPostPrimary(
			invalidResult,
			"synthetic@fixture.example",
			invalidSession,
		); byJwt != nil {
			t.Fatal("AuthVerify enrolled after invalid JWT derivation")
		}
		if row := model.GetNetworkOnboarding(queryCtx, invalidNetworkId); row != nil {
			t.Fatal("invalid AuthVerify token created an onboarding row")
		}
	})
}

func onboardingEventCount(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	name string,
) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT count(*) FROM network_onboarding_event WHERE network_id = $1 AND name = $2`,
			networkId,
			name,
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatal("onboarding event count returned no row")
			}
			server.Raise(result.Scan(&count))
		})
	})
	return count
}
