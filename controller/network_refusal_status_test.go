package controller

// The network routes' refusals declare their statuses, or keep the answer they
// had, now that the router answers an undeclared error with a fixed 500.

import (
	"net/http"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A sign-in token that does not verify keeps its 500, since a provider's
// signing keys failing to load ends the same way, and its fixed text.
func TestNetworkCreateKeepsTheInvalidLoginAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		// the synthetic token fails against an empty key set, with no fetch
		defer session.Testing_SetSsoJwkKeys([]any{})()
		clientSession := session.Testing_CreateClientSession(tb.Context(), nil)
		defer clientSession.Cancel()
		token, tokenType := "synthetic-sso-token", "google"
		_, err := NetworkCreate(model.NetworkCreateArgs{AuthJwt: &token, AuthJwtType: &tokenType, NetworkName: "invalidloginnet", Terms: true}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusInternalServerError, "invalid login")
	})
}

// A user who is not the network's admin cannot remove it.
func TestNetworkRemoveRefusesNonAdminAsForbidden(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		networkId := server.NewId()
		model.Testing_CreateNetwork(tb.Context(), networkId, "removenonadmin", server.NewId())
		clientSession := session.Testing_CreateClientSession(tb.Context(), session.NewByJwt(networkId, server.NewId(), "removenonadmin", false, false))
		defer clientSession.Cancel()
		_, err := NetworkRemove(clientSession)
		requireRefusalAnswer(tb, err, http.StatusForbidden, "Could not remove network")
	})
}

// The removal finds no network the caller administers.
func TestNetworkRemoveRefusesMissingNetworkAsForbidden(t *testing.T) {
	fake := &networkRemoveStepsFake{removeOutcome: model.RemoveNetworkNotFound}
	_, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	requireRefusalAnswer(t, err, http.StatusForbidden, "Could not remove network")
}

// The removal finds a Stripe renewal active after the Stripe step ran.
func TestNetworkRemoveRefusesActiveStripeRenewalAsConflict(t *testing.T) {
	fake := &networkRemoveStepsFake{removeOutcome: model.RemoveNetworkStripeRenewalActive}
	_, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	requireRefusalAnswer(t, err, http.StatusConflict, "Could not remove network")
}

// A concurrent generate creates the seedphrase auth between the existence
// check and the create. The loser was answered with the model's error and a
// 500; it is answered as the check answers.
func TestGenerateSeedphraseRaceAnswersAsItsCheck(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "seedphraserace", userId)
		generateSeedphraseAfterCheck = func() {
			if _, err := model.GenerateSeedphrase(ctx, userId); err != nil {
				tb.Fatal(err)
			}
		}
		defer func() { generateSeedphraseAfterCheck = nil }()
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "seedphraserace", false, false))
		defer clientSession.Cancel()
		result, err := GenerateSeedphrase(GenerateSeedphraseArgs{}, clientSession)
		if err != nil || result == nil || result.Seedphrase != "" || result.Error == nil || result.Error.Message != seedphraseExistsResult().Error.Message {
			tb.Fatalf("the race's loser was not answered as the check: %+v %v", result, err)
		}
	})
}

// Every hour of the deployment's bulk removal budget is spent.
func TestRemoveNetworkClientsRefusesAnExhaustedBudgetAsTooManyRequests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		for range model.MaxBulkClientRemovalLookaheadBuckets {
			if _, _, err := model.ReserveBulkClientRemovalSlot(ctx, server.NewId(), model.MaxBulkClientRemovalsPerBucket); err != nil {
				tb.Fatal(err)
			}
		}
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "removeclients", false, false))
		defer clientSession.Cancel()
		_, err := model.RemoveNetworkClients(&model.RemoveNetworkClientsArgs{ClientIds: []server.Id{server.NewId()}}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusTooManyRequests, "The bulk client removal limit (1000000 per hour, 24000000 per day) has been reached for this deployment. Please try again later.")
	})
}
