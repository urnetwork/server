package controller

// Hermetic tests (no database) for the side effects of a successful
// /auth/verify: the welcome email and onboarding enrollment belong to a new
// account's sign-up only, never to an email or phone added later to an
// existing account. The product-updates sync runs for every verification.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type authVerifyEffectsRecorder struct {
	enrolled      []string
	welcomed      []string
	parsed        []string
	syncedNetwork []server.Id
}

func newAuthVerifyEffectsRecorder(byJwt *jwt.ByJwt) (*authVerifyEffectsRecorder, authVerifyEffects) {
	recorder := &authVerifyEffectsRecorder{}
	effects := authVerifyEffects{
		enrollOnboarding: func(result *model.AuthVerifyResult, userAuth string, clientSession *session.ClientSession) *jwt.ByJwt {
			recorder.enrolled = append(recorder.enrolled, userAuth)
			return byJwt
		},
		parseByJwt: func(clientSession *session.ClientSession, signedByJwt string) *jwt.ByJwt {
			recorder.parsed = append(recorder.parsed, signedByJwt)
			return byJwt
		},
		sendWelcome: func(userAuth string) {
			recorder.welcomed = append(recorder.welcomed, userAuth)
		},
		syncProductUpdates: func(verifiedSession *session.ClientSession) {
			recorder.syncedNetwork = append(recorder.syncedNetwork, verifiedSession.ByJwt.NetworkId)
		},
	}
	return recorder, effects
}

func authVerifyTestSession(t *testing.T) *session.ClientSession {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return session.NewLocalClientSession(ctx, "", nil)
}

func TestCompleteAuthVerifyNewAccountGetsWelcomeAndEnrollment(t *testing.T) {
	networkId := server.NewId()
	byJwt := &jwt.ByJwt{NetworkId: networkId, UserId: server.NewId()}
	for _, userAuth := range []string{"new@fixture.example", "+15555550100"} {
		recorder, effects := newAuthVerifyEffectsRecorder(byJwt)
		completeAuthVerify(
			&model.AuthVerifyResult{
				Network:    &model.AuthVerifyResultNetwork{ByJwt: "signed"},
				NewAccount: true,
			},
			userAuth,
			authVerifyTestSession(t),
			effects,
		)
		if len(recorder.enrolled) != 1 || recorder.enrolled[0] != userAuth {
			t.Errorf("%s: onboarding enrollments = %v, want one for the sign-up", userAuth, recorder.enrolled)
		}
		if len(recorder.welcomed) != 1 || recorder.welcomed[0] != userAuth {
			t.Errorf("%s: welcome emails = %v, want one for the sign-up", userAuth, recorder.welcomed)
		}
		if len(recorder.syncedNetwork) != 1 || recorder.syncedNetwork[0] != networkId {
			t.Errorf("%s: product-updates syncs = %v, want one for the network", userAuth, recorder.syncedNetwork)
		}
	}
}

// A network created with Apple, Google or a wallet that adds an email sign-in
// verifies it through /auth/verify. That is not a sign-up.
func TestCompleteAuthVerifyAddedSignInGetsNoWelcomeOrEnrollment(t *testing.T) {
	networkId := server.NewId()
	byJwt := &jwt.ByJwt{NetworkId: networkId, UserId: server.NewId()}
	recorder, effects := newAuthVerifyEffectsRecorder(byJwt)
	completeAuthVerify(
		&model.AuthVerifyResult{
			Network:    &model.AuthVerifyResultNetwork{ByJwt: "signed"},
			NewAccount: false,
		},
		"added@fixture.example",
		authVerifyTestSession(t),
		effects,
	)
	if len(recorder.welcomed) != 0 {
		t.Errorf("welcome emails = %v, want none for an added sign-in", recorder.welcomed)
	}
	if len(recorder.enrolled) != 0 {
		t.Errorf("onboarding enrollments = %v, want none for an added sign-in", recorder.enrolled)
	}
	// the existing preference is still synced for the network
	if len(recorder.syncedNetwork) != 1 || recorder.syncedNetwork[0] != networkId {
		t.Errorf("product-updates syncs = %v, want one for the network", recorder.syncedNetwork)
	}
}

func TestCompleteAuthVerifyFailedVerificationHasNoEffects(t *testing.T) {
	byJwt := &jwt.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()}
	recorder, effects := newAuthVerifyEffectsRecorder(byJwt)
	completeAuthVerify(
		&model.AuthVerifyResult{
			Error:      &model.AuthVerifyResultError{Message: "Invalid code."},
			NewAccount: true,
		},
		"new@fixture.example",
		authVerifyTestSession(t),
		effects,
	)
	if len(recorder.enrolled)+len(recorder.welcomed)+len(recorder.parsed)+len(recorder.syncedNetwork) != 0 {
		t.Errorf("failed verification had effects: %+v", recorder)
	}
}
