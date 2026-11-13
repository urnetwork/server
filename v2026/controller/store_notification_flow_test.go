package controller

// What became of each store notification (store_notification_flow.go):
// every Play refusal by its outcome, the accepted mark at most once per mark
// interval per process and store, and a mark that fails never failing the
// notification. Hermetic: the token lookup and the durable mark are seams.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Every way a Play push is refused counts as its own outcome, once, and the
// route answers the text it always has.
func TestPlayNotificationRefusalOutcomes(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: "+syntheticPlayAudience+"\n")))
	publisherClaims := func(audience string) map[string]any {
		return map[string]any{
			"email":          "publisher@fixture.example",
			"email_verified": true,
			"aud":            audience,
			"iss":            "https://accounts.google.com",
		}
	}
	type refusalCase struct {
		name    string
		header  string
		claims  map[string]any
		err     error
		outcome string
		message string
	}
	for _, refusal := range []refusalCase{
		{name: "no token", outcome: StoreNotificationRefusedAuth, message: "missing authorization header"},
		{name: "not a bearer token", header: "Basic synthetic", outcome: StoreNotificationRefusedAuth, message: "Missing authorization."},
		{name: "a token Google refuses", header: "Bearer synthetic", err: &server.HttpStatusError{StatusCode: 400, Status: "400 Bad Request"}, outcome: StoreNotificationRefusedAuth},
		{name: "Google unreachable", header: "Bearer synthetic", err: errors.New("synthetic dial failure"), outcome: StoreNotificationError, message: "synthetic dial failure"},
		{name: "another publisher", header: "Bearer synthetic", claims: map[string]any{"email": "someone@fixture.example", "email_verified": true, "aud": syntheticPlayAudience, "iss": "https://accounts.google.com"}, outcome: StoreNotificationRefusedAuth, message: "Missing authorization."},
		{name: "another audience", header: "Bearer synthetic", claims: publisherClaims("https://push.fixture.example/other"), outcome: StoreNotificationRefusedAudience, message: "Missing authorization."},
	} {
		setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
			return refusal.claims, refusal.err
		})
		beforeOutcomeCounts := map[string]float64{}
		for _, outcome := range StoreNotificationOutcomes {
			beforeOutcomeCounts[outcome] = storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, outcome)
		}
		headers := map[string]string{}
		if refusal.header != "" {
			headers["Authorization"] = refusal.header
		}
		_, err := verifyWebhookBody(t, VerifyPlayBody, "{}", headers)
		if err == nil {
			t.Fatalf("%s: the push was accepted", refusal.name)
		}
		if refusal.message != "" && err.Error() != refusal.message {
			t.Errorf("%s: answered %q, want %q", refusal.name, err.Error(), refusal.message)
		}
		for _, outcome := range StoreNotificationOutcomes {
			want := 0.0
			if outcome == refusal.outcome {
				want = 1
			}
			if got := storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, outcome) - beforeOutcomeCounts[outcome]; got != want {
				t.Errorf("%s: %s counted %v, want %v", refusal.name, outcome, got, want)
			}
		}
	}

	// the configured publisher for the configured audience is no refusal
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		return publisherClaims(syntheticPlayAudience), nil
	})
	beforeCount := storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationRefusedAudience)
	_, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic"})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationRefusedAudience), beforeCount)

	// a missing verification config is refused_unavailable
	popGoogle := server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n"))
	defer popGoogle()
	beforeCount = storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationRefusedUnavailable)
	_, err = verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic"})
	connect.AssertEqual(t, errors.Is(err, errWebhookVerificationUnavailable), true)
	connect.AssertEqual(t, storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationRefusedUnavailable)-beforeCount, 1.0)
}

// An accepted notification writes the durable mark at most once per mark
// interval per store; a mark that fails is written by the next accepted
// notification and never fails this one; a refusal marks nothing.
func TestStoreNotificationAcceptedMarksTheFlowOncePerInterval(t *testing.T) {
	markStores := []string{}
	failing := false
	recorder := newStoreNotificationRecorder(
		&StoreNotificationFlowSettings{MarkEvery: time.Hour},
		func(ctx context.Context, store string, acceptedAt time.Time) {
			if failing {
				panic(errors.New("synthetic mark failure"))
			}
			markStores = append(markStores, store)
		},
	)

	ctx := context.Background()
	beforeCount := storeNotificationCountForTest(t, model.SubscriptionMarketApple, StoreNotificationAccepted)
	recorder.record(ctx, model.SubscriptionMarketApple, StoreNotificationAccepted)
	recorder.record(ctx, model.SubscriptionMarketApple, StoreNotificationAccepted)
	recorder.record(ctx, model.SubscriptionMarketGoogle, StoreNotificationAccepted)
	connect.AssertEqual(t, markStores, []string{model.SubscriptionMarketApple, model.SubscriptionMarketGoogle})
	connect.AssertEqual(t, storeNotificationCountForTest(t, model.SubscriptionMarketApple, StoreNotificationAccepted)-beforeCount, 2.0)

	// a failed mark gives its slot back
	failingRecorder := newStoreNotificationRecorder(
		&StoreNotificationFlowSettings{MarkEvery: time.Hour},
		func(ctx context.Context, store string, acceptedAt time.Time) {
			if failing {
				panic(errors.New("synthetic mark failure"))
			}
			markStores = append(markStores, store)
		},
	)
	failing = true
	failingRecorder.record(ctx, model.SubscriptionMarketApple, StoreNotificationAccepted)
	failing = false
	failingRecorder.record(ctx, model.SubscriptionMarketApple, StoreNotificationAccepted)
	connect.AssertEqual(t, markStores, []string{model.SubscriptionMarketApple, model.SubscriptionMarketGoogle, model.SubscriptionMarketApple})

	// a refusal marks nothing
	recorder.record(ctx, model.SubscriptionMarketGoogle, StoreNotificationRefusedAudience)
	connect.AssertEqual(t, len(markStores), 3)

	// a non-positive interval marks every accepted notification
	everyRecorder := newStoreNotificationRecorder(
		&StoreNotificationFlowSettings{MarkEvery: 0},
		func(ctx context.Context, store string, acceptedAt time.Time) {
			markStores = append(markStores, store)
		},
	)
	everyRecorder.record(ctx, model.SubscriptionMarketGoogle, StoreNotificationAccepted)
	everyRecorder.record(ctx, model.SubscriptionMarketGoogle, StoreNotificationAccepted)
	connect.AssertEqual(t, len(markStores), 5)
}

// Every store and outcome series is registered from the start, so a store's
// first refusal is an increase the dashboard and the monitor see.
func TestStoreNotificationSeriesExistFromTheStart(t *testing.T) {
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	storeOutcomes := map[string]bool{}
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != "urnetwork_store_notifications_total" {
			continue
		}
		for _, metric := range metricFamily.Metric {
			labelValues := map[string]string{}
			for _, pair := range metric.Label {
				labelValues[pair.GetName()] = pair.GetValue()
			}
			storeOutcomes[labelValues["store"]+"/"+labelValues["outcome"]] = true
		}
	}
	for _, store := range model.StoreNotificationStores {
		for _, outcome := range StoreNotificationOutcomes {
			if !storeOutcomes[store+"/"+outcome] {
				t.Errorf("the %s %s series is not registered", store, outcome)
			}
		}
	}
	connect.AssertEqual(t, len(storeOutcomes), len(model.StoreNotificationStores)*len(StoreNotificationOutcomes))
}
