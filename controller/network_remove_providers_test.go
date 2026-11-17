package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// networkRemoveStepsFake records the order of the deletion steps. Every
// store answer is fixed by the test, so the outcome depends on nothing but
// the deletion logic.
type networkRemoveStepsFake struct {
	// the team the first check finds the caller the last Owner of; nil for none
	lastOwnerTeam *model.LastOwnerTeam
	storeSnapshot *model.RemoveNetworkStoreSnapshot
	appleRenewing bool
	appleErr      error
	stripeErr     error
	playErr       error
	// the removal's outcome; empty means removed
	removeOutcome model.RemoveNetworkOutcome
	// the team the removal names with RemoveNetworkLastTeamOwner
	removeLastOwnerTeam *model.LastOwnerTeam

	calls   []string
	removed bool
	// what each step was handed
	appleTransactionIds []string
	playPurchaseTokens  []string
	removeStoreSnapshot *model.RemoveNetworkStoreSnapshot
}

func (self *networkRemoveStepsFake) steps() *networkRemoveSteps {
	return &networkRemoveSteps{
		lastOwnerTeam: func(clientSession *session.ClientSession) *model.LastOwnerTeam {
			self.calls = append(self.calls, "last-owner")
			return self.lastOwnerTeam
		},
		storeSnapshot: func(clientSession *session.ClientSession) *model.RemoveNetworkStoreSnapshot {
			self.calls = append(self.calls, "snapshot")
			if self.storeSnapshot == nil {
				return &model.RemoveNetworkStoreSnapshot{}
			}
			return self.storeSnapshot
		},
		appleRenewing: func(clientSession *session.ClientSession, originalTransactionIds []string) (bool, error) {
			self.calls = append(self.calls, "apple")
			self.appleTransactionIds = originalTransactionIds
			return self.appleRenewing, self.appleErr
		},
		unsubscribeStripe: func(clientSession *session.ClientSession) error {
			self.calls = append(self.calls, "stripe")
			return self.stripeErr
		},
		cancelPlay: func(clientSession *session.ClientSession, purchaseTokens []string) error {
			self.calls = append(self.calls, "play")
			self.playPurchaseTokens = purchaseTokens
			return self.playErr
		},
		removeNetwork: func(clientSession *session.ClientSession, storeSnapshot *model.RemoveNetworkStoreSnapshot) *model.RemoveNetworkResult {
			self.calls = append(self.calls, "remove")
			self.removeStoreSnapshot = storeSnapshot
			if self.removeOutcome != "" && self.removeOutcome != model.RemoveNetworkRemoved {
				return &model.RemoveNetworkResult{
					Outcome:       self.removeOutcome,
					LastOwnerTeam: self.removeLastOwnerTeam,
				}
			}
			self.removed = true
			return &model.RemoveNetworkResult{
				Outcome:   model.RemoveNetworkRemoved,
				UserAuths: map[string]bool{"synthetic@example.invalid": true},
			}
		},
		scheduleRemoveProductUpdates: func(clientSession *session.ClientSession, userAuths map[string]bool) {
			self.calls = append(self.calls, "product-updates")
		},
	}
}

func networkRemoveStepsTestSession() *session.ClientSession {
	return &session.ClientSession{
		Ctx:   context.Background(),
		ByJwt: session.NewByJwt(server.NewId(), server.NewId(), "synthetic", false, false),
	}
}

// An active Google Play subscription must be cancelled before the network is
// removed; before the fix deletion never touched Play and the subscription
// kept billing a deleted account.
func TestNetworkRemoveCancelsPlayBeforeRemoval(t *testing.T) {
	fake := &networkRemoveStepsFake{}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("remove result=%+v err=%v, want success", result, err)
	}
	want := []string{"last-owner", "snapshot", "apple", "stripe", "play", "remove", "product-updates"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("steps = %v, want %v", fake.calls, want)
	}
}

// The App Store check, the Play cancellation and the removal work from one
// read of the store renewals, so the removal can tell a renewal credited
// after the store steps from one they checked.
func TestNetworkRemoveFeedsOneStoreSnapshotToEveryStep(t *testing.T) {
	storeSnapshot := &model.RemoveNetworkStoreSnapshot{
		AppleTransactionIds: []string{"synthetic-apple-1"},
		PlayPurchaseTokens:  []string{"synthetic-play-1", "synthetic-play-2"},
	}
	fake := &networkRemoveStepsFake{storeSnapshot: storeSnapshot}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("remove result=%+v err=%v, want success", result, err)
	}
	if !reflect.DeepEqual(fake.appleTransactionIds, storeSnapshot.AppleTransactionIds) {
		t.Fatalf("App Store check got %v, want %v", fake.appleTransactionIds, storeSnapshot.AppleTransactionIds)
	}
	if !reflect.DeepEqual(fake.playPurchaseTokens, storeSnapshot.PlayPurchaseTokens) {
		t.Fatalf("Play cancellation got %v, want %v", fake.playPurchaseTokens, storeSnapshot.PlayPurchaseTokens)
	}
	if fake.removeStoreSnapshot != storeSnapshot {
		t.Fatalf("removal got snapshot %+v, want the one the store steps used", fake.removeStoreSnapshot)
	}
}

// A store renewal credited while the deletion ran keeps the network and asks
// the customer to retry; the retry's store steps see the new renewal.
func TestNetworkRemoveStoreRenewalChangedAsksForRetry(t *testing.T) {
	fake := &networkRemoveStepsFake{removeOutcome: model.RemoveNetworkStoreRenewalUnchecked}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil {
		t.Fatalf("remove err = %v, want a result error", err)
	}
	if fake.removed {
		t.Fatalf("network removed although a store renewal changed (steps %v)", fake.calls)
	}
	if result == nil || result.Error == nil || result.Error.Message != networkRemoveStoreRenewalChangedMessage {
		t.Fatalf("remove result = %+v, want the retry message", result)
	}
	if want := []string{"last-owner", "snapshot", "apple", "stripe", "play", "remove"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("steps = %v, want %v (no product-update removal)", fake.calls, want)
	}
}

// A failed Play cancellation leaves the network in place so the customer can
// retry, like a failed Stripe cancellation.
func TestNetworkRemovePlayCancelFailureBlocksRemoval(t *testing.T) {
	fake := &networkRemoveStepsFake{playErr: errors.New("synthetic play failure")}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil {
		t.Fatalf("remove err = %v, want a result error", err)
	}
	if fake.removed {
		t.Fatalf("network removed after the Play cancellation failed (steps %v)", fake.calls)
	}
	if result == nil || result.Error == nil || result.Error.Message != networkRemovePlayFailedMessage {
		t.Fatalf("remove result = %+v, want the Play failure message", result)
	}
}

// The server cannot cancel an App Store subscription, so deletion is refused
// while one renews, before any store or local state changes.
func TestNetworkRemoveAppleRenewingBlocksRemoval(t *testing.T) {
	for name, fake := range map[string]*networkRemoveStepsFake{
		"renewing":      {appleRenewing: true},
		"lookup failed": {appleErr: errors.New("synthetic App Store failure")},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
			if err != nil {
				t.Fatalf("remove err = %v, want a result error", err)
			}
			if fake.removed {
				t.Fatalf("network removed while an App Store subscription renews (steps %v)", fake.calls)
			}
			if !reflect.DeepEqual(fake.calls, []string{"last-owner", "snapshot", "apple"}) {
				t.Fatalf("steps = %v, want only the last Owner check, the snapshot and the App Store check", fake.calls)
			}
			if result == nil || result.Error == nil || result.Error.Message != networkRemoveAppleRenewingMessage {
				t.Fatalf("remove result = %+v, want the App Store message", result)
			}
		})
	}
}

func TestNetworkRemoveStripeFailureBlocksRemoval(t *testing.T) {
	fake := &networkRemoveStepsFake{stripeErr: errors.New("synthetic stripe failure")}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	if err != nil || result == nil || result.Error == nil || result.Error.Message != networkRemoveStripeFailedMessage {
		t.Fatalf("remove result=%+v err=%v, want the Stripe failure message", result, err)
	}
	if !reflect.DeepEqual(fake.calls, []string{"last-owner", "snapshot", "apple", "stripe"}) {
		t.Fatalf("steps = %v, want the last Owner check, the snapshot, the App Store check and Stripe only", fake.calls)
	}
}

// Requires the refusal of a team's last Owner, naming team, or no team for nil.
func requireNetworkRemoveLastTeamOwner(t testing.TB, result *NetworkRemoveResult, err error, team *model.LastOwnerTeam) {
	t.Helper()
	if err != nil || result == nil || result.Error == nil {
		t.Fatalf("remove result=%+v err=%v, want the last Owner's refusal", result, err)
	}
	refusal := result.Error
	if refusal.Message != networkRemoveLastTeamOwnerMessage || refusal.Code != networkRemoveLastTeamOwnerCode {
		t.Fatalf("refusal = %+v, want message %q and code %q", refusal, networkRemoveLastTeamOwnerMessage, networkRemoveLastTeamOwnerCode)
	}
	if team == nil {
		if refusal.TeamNetworkId != nil || refusal.TeamNetworkName != "" {
			t.Fatalf("refusal names team %v %q, want none", refusal.TeamNetworkId, refusal.TeamNetworkName)
		}
		return
	}
	if refusal.TeamNetworkId == nil || *refusal.TeamNetworkId != team.NetworkId || refusal.TeamNetworkName != team.NetworkName {
		t.Fatalf("refusal names team %v %q, want %s %q", refusal.TeamNetworkId, refusal.TeamNetworkName, team.NetworkId, team.NetworkName)
	}
}

// §7.5's last Owner (owner decision 329): the check runs before every store
// step, so its refusal reads and cancels nothing at any store and removes
// nothing.
func TestNetworkRemoveChecksTheLastOwnerBeforeAnyStoreStep(t *testing.T) {
	team := &model.LastOwnerTeam{NetworkId: server.NewId(), NetworkName: "synthetic-team"}
	fake := &networkRemoveStepsFake{
		lastOwnerTeam: team,
		storeSnapshot: &model.RemoveNetworkStoreSnapshot{
			AppleTransactionIds: []string{"synthetic-apple-1"},
			PlayPurchaseTokens:  []string{"synthetic-play-1"},
		},
	}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	requireNetworkRemoveLastTeamOwner(t, result, err, team)
	if fake.removed {
		t.Fatal("the last Owner's network was removed")
	}
	if want := []string{"last-owner"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("steps = %v, want %v", fake.calls, want)
	}
}

// An Owner who left the team after the first check leaves the caller the last
// one under the removal's lock: the removal refuses, after the store steps,
// with the same answer, and no product-update removal is scheduled.
func TestNetworkRemoveRefusesALastOwnerFoundUnderTheLock(t *testing.T) {
	team := &model.LastOwnerTeam{NetworkId: server.NewId(), NetworkName: "synthetic-team"}
	fake := &networkRemoveStepsFake{
		removeOutcome:       model.RemoveNetworkLastTeamOwner,
		removeLastOwnerTeam: team,
	}
	result, err := networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	requireNetworkRemoveLastTeamOwner(t, result, err, team)
	if fake.removed {
		t.Fatal("the last Owner's network was removed")
	}
	if want := []string{"last-owner", "snapshot", "apple", "stripe", "play", "remove"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("steps = %v, want %v (no product-update removal)", fake.calls, want)
	}

	// a removal that names no team still answers the refusal, without a team
	fake = &networkRemoveStepsFake{removeOutcome: model.RemoveNetworkLastTeamOwner}
	result, err = networkRemoveWithSteps(networkRemoveStepsTestSession(), fake.steps())
	requireNetworkRemoveLastTeamOwner(t, result, err, nil)
}

// The route's existing error shape carries the refusal's code and team beside
// its message; every other refusal answers byte for byte what it did.
func TestNetworkRemoveLastTeamOwnerAnswerShape(t *testing.T) {
	teamNetworkId := server.NewId()
	teamNetworkIdJson, err := json.Marshal(teamNetworkId)
	if err != nil {
		t.Fatal(err)
	}
	messageJson, err := json.Marshal(networkRemoveLastTeamOwnerMessage)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(networkRemoveLastTeamOwnerError(&model.LastOwnerTeam{NetworkId: teamNetworkId, NetworkName: "synthetic-team"}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":{"message":` + string(messageJson) + `,"code":"last_team_owner","team_network_id":` + string(teamNetworkIdJson) + `,"team_network_name":"synthetic-team"}}`
	if string(body) != want {
		t.Fatalf("refusal body = %s, want %s", body, want)
	}

	for _, message := range []string{
		networkRemoveStripeFailedMessage,
		networkRemovePlayFailedMessage,
		networkRemoveAppleRenewingMessage,
		networkRemoveStoreRenewalChangedMessage,
	} {
		messageJson, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(networkRemoveError(message))
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"error":{"message":` + string(messageJson) + `}}`; string(body) != want {
			t.Errorf("refusal body = %s, want %s", body, want)
		}
	}
	if body, err := json.Marshal(&NetworkRemoveResult{}); err != nil || string(body) != `{}` {
		t.Fatalf("removal body = %s %v, want {}", body, err)
	}
}

func TestAppleSubscriptionsRenewing(t *testing.T) {
	yes, no := true, false
	states := map[string]*subscriptionStoreState{
		"renewing":  {AutoRenew: &yes, Active: true},
		"cancelled": {AutoRenew: &no, CancelAtPeriodEnd: true, Active: true},
		"unknown":   {Active: true},
	}
	lookup := func(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error) {
		if originalTransactionId == "failing" {
			return nil, errors.New("synthetic lookup failure")
		}
		return states[originalTransactionId], nil
	}
	for _, c := range []struct {
		ids      []string
		renewing bool
		err      bool
	}{
		{ids: nil, renewing: false},
		{ids: []string{"cancelled", "cancelled"}, renewing: false},
		{ids: []string{"cancelled", "renewing"}, renewing: true},
		{ids: []string{"unknown"}, renewing: true},
		{ids: []string{"failing"}, renewing: true, err: true},
		{ids: []string{""}, renewing: true, err: true},
	} {
		renewing, err := appleSubscriptionsRenewing(context.Background(), c.ids, lookup)
		if renewing != c.renewing || (err != nil) != c.err {
			t.Errorf("ids %v: renewing=%v err=%v, want renewing=%v err=%v", c.ids, renewing, err, c.renewing, c.err)
		}
	}
}

// playCancelFake is a synthetic Android Publisher API on a loopback test
// server.
type playCancelFake struct {
	lock       sync.Mutex
	subs       map[string]*PlaySubscription
	gone       map[string]bool
	cancelFail map[string]bool
	// the tokens looked up, in order
	looked     []string
	cancelled  []string
	cancelBody []map[string]any
}

func newPlayCancelFake(t testing.TB) *playCancelFake {
	t.Helper()
	fake := &playCancelFake{
		subs:       map[string]*PlaySubscription{},
		gone:       map[string]bool{},
		cancelFail: map[string]bool{},
	}
	mux := http.NewServeMux()
	const prefix = "/androidpublisher/v3/applications/synthetic.package/purchases/subscriptionsv2/tokens/"
	mux.HandleFunc("GET "+prefix+"{token}", func(response http.ResponseWriter, request *http.Request) {
		fake.lock.Lock()
		defer fake.lock.Unlock()
		token := request.PathValue("token")
		fake.looked = append(fake.looked, token)
		if fake.gone[token] {
			http.Error(response, "gone", http.StatusGone)
			return
		}
		sub, ok := fake.subs[token]
		if !ok {
			http.Error(response, "not found", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		json.NewEncoder(response).Encode(sub)
	})
	mux.HandleFunc("POST "+prefix+"{tokenCancel}", func(response http.ResponseWriter, request *http.Request) {
		fake.lock.Lock()
		defer fake.lock.Unlock()
		tokenCancel := request.PathValue("tokenCancel")
		const suffix = ":cancel"
		if len(tokenCancel) <= len(suffix) || tokenCancel[len(tokenCancel)-len(suffix):] != suffix {
			http.Error(response, "unexpected method", http.StatusNotFound)
			return
		}
		token := tokenCancel[:len(tokenCancel)-len(suffix)]
		body := map[string]any{}
		bodyBytes, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("cancel body %q: %v", bodyBytes, err)
		}
		fake.cancelBody = append(fake.cancelBody, body)
		if fake.cancelFail[token] {
			http.Error(response, "synthetic failure", http.StatusInternalServerError)
			return
		}
		fake.cancelled = append(fake.cancelled, token)
		response.WriteHeader(http.StatusOK)
	})
	testServer := httptest.NewServer(mux)

	previousBaseUrl := playPublisherApiBaseUrl
	previousPackageName := playPackageNameFunc
	previousAuthHeader := playAuthHeaderFunc
	playPublisherApiBaseUrl = testServer.URL
	playPackageNameFunc = func() string { return "synthetic.package" }
	playAuthHeaderFunc = func(ctx context.Context, header http.Header) {}
	t.Cleanup(func() {
		playPublisherApiBaseUrl = previousBaseUrl
		playPackageNameFunc = previousPackageName
		playAuthHeaderFunc = previousAuthHeader
		testServer.Close()
	})
	return fake
}

func playCancelTestSubscription(state string, autoRenew *bool) *PlaySubscription {
	item := &PlaySubscriptionPurchaseLineItem{
		ProductId:  "supporter",
		ExpiryTime: "2030-01-01T00:00:00Z",
	}
	if autoRenew != nil {
		item.AutoRenewingPlan = &PlayAutoRenewingPlan{AutoRenewEnabled: *autoRenew}
	}
	return &PlaySubscription{
		SubscriptionState: state,
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item},
	}
}

func TestPlayCancelDeletionSubscriptionsCancelsOnlyRenewing(t *testing.T) {
	fake := newPlayCancelFake(t)
	yes, no := true, false
	fake.subs["renewing"] = playCancelTestSubscription("SUBSCRIPTION_STATE_ACTIVE", &yes)
	fake.subs["grace"] = playCancelTestSubscription("SUBSCRIPTION_STATE_IN_GRACE_PERIOD", &yes)
	fake.subs["user-cancelled"] = playCancelTestSubscription("SUBSCRIPTION_STATE_ACTIVE", &no)
	fake.subs["canceled"] = playCancelTestSubscription("SUBSCRIPTION_STATE_CANCELED", &no)
	fake.subs["expired"] = playCancelTestSubscription("SUBSCRIPTION_STATE_EXPIRED", &yes)
	fake.subs["prepaid"] = playCancelTestSubscription("SUBSCRIPTION_STATE_ACTIVE", nil)
	fake.gone["gone"] = true

	err := playCancelDeletionSubscriptions(context.Background(), []string{
		"renewing", "renewing", "grace", "user-cancelled", "canceled", "expired", "prepaid", "gone",
	})
	if err != nil {
		t.Fatalf("cancel err = %v", err)
	}
	if want := []string{"renewing", "grace"}; !reflect.DeepEqual(fake.cancelled, want) {
		t.Fatalf("cancelled %v, want %v", fake.cancelled, want)
	}
	for _, body := range fake.cancelBody {
		cancellationContext, _ := body["cancellationContext"].(map[string]any)
		if cancellationContext["cancellationType"] != playCancellationTypeDeveloperStopPayments {
			t.Fatalf("cancel body %v, want cancellationType %s", body, playCancellationTypeDeveloperStopPayments)
		}
	}
}

func TestPlayCancelDeletionSubscriptionsFailsClosed(t *testing.T) {
	fake := newPlayCancelFake(t)
	yes := true
	fake.subs["renewing"] = playCancelTestSubscription("SUBSCRIPTION_STATE_ACTIVE", &yes)
	fake.cancelFail["renewing"] = true

	if err := playCancelDeletionSubscriptions(context.Background(), []string{"renewing"}); err == nil {
		t.Fatalf("cancel failure was not reported")
	}
	if err := playCancelDeletionSubscriptions(context.Background(), []string{"missing"}); err == nil {
		t.Fatalf("lookup failure was not reported")
	}
	if err := playCancelDeletionSubscriptions(context.Background(), []string{""}); err == nil {
		t.Fatalf("empty purchase token was not reported")
	}
}
