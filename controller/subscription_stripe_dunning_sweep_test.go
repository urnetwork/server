package controller

// Hermetic tests of the hourly dunning sweep
// (subscription_stripe_dunning_sweep.go), with no database.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A local or test env has no Stripe token: the sweep reads nothing from Stripe
// and says why, so its hourly run is not an error there.
func TestStripeDunningSweepSkipsAnEnvWithoutStripeCredentials(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("stripe.yml", []byte("skus:\n  pro: price_test\n")))
	stripeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the sweep read Stripe without credentials: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer stripeServer.Close()
	previousBaseUrl := stripeApiBaseUrl
	stripeApiBaseUrl = stripeServer.URL
	defer func() {
		stripeApiBaseUrl = previousBaseUrl
	}()

	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := SweepStripeDunning(&SweepStripeDunningArgs{}, clientSession)
	if err != nil || result == nil || result.Skip == "" || result.SubscriptionCount != 0 {
		t.Fatalf("a sweep without credentials: %+v %v", result, err)
	}
}
