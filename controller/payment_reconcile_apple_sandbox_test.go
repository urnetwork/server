package controller

// A deployment that accepts Sandbox App Store notifications credits TestFlight
// and App Review purchases, so its renewals can name sandbox transactions.
// Production answers those with TransactionIdNotFoundError (404, 4040010), and
// on Main every hourly run recorded one status error per TestFlight network,
// so the App Store watermark was never written. These tests pin the lookup:
// production first, then the sandbox, only for 4040010 and only where Sandbox
// is accepted. A sandbox answer is environment evidence, never repair
// authority, and a sandbox miss or failure remains a status error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Apple's body for an unknown transaction id, as production serves it for a
// sandbox transaction.
const appleTestTransactionIdNotFoundBody = `{"errorCode":4040010,"errorMessage":"Transaction id not found."}`

type appleFakeAnswer struct {
	statusCode int
	body       string
}

// appleEnvironmentFake is one App Store Server API environment. A transaction
// it holds neither statuses nor an answer for gets Apple's 404
// TransactionIdNotFoundError.
type appleEnvironmentFake struct {
	mutex    sync.Mutex
	statuses map[string]*appleSubscriptionStatusesResponse
	answers  map[string]appleFakeAnswer
	requests map[string]int

	testServer *httptest.Server
}

func newAppleEnvironmentFake(t testing.TB) *appleEnvironmentFake {
	fake := &appleEnvironmentFake{
		statuses: map[string]*appleSubscriptionStatusesResponse{},
		answers:  map[string]appleFakeAnswer{},
		requests: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inApps/v1/subscriptions/{transactionId}", fake.serveStatuses)
	fake.testServer = httptest.NewServer(mux)
	t.Cleanup(fake.testServer.Close)
	return fake
}

func (self *appleEnvironmentFake) serveStatuses(w http.ResponseWriter, r *http.Request) {
	answer := self.answer(r.PathValue("transactionId"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.statusCode)
	fmt.Fprint(w, answer.body)
}

func (self *appleEnvironmentFake) answer(transactionId string) appleFakeAnswer {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.requests[transactionId] += 1
	if answer, ok := self.answers[transactionId]; ok {
		return answer
	}
	if statuses, ok := self.statuses[transactionId]; ok {
		body, err := json.Marshal(statuses)
		if err != nil {
			return appleFakeAnswer{statusCode: http.StatusInternalServerError, body: err.Error()}
		}
		return appleFakeAnswer{statusCode: http.StatusOK, body: string(body)}
	}
	return appleFakeAnswer{statusCode: http.StatusNotFound, body: appleTestTransactionIdNotFoundBody}
}

func (self *appleEnvironmentFake) setStatuses(transactionId string, statuses *appleSubscriptionStatusesResponse) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.statuses[transactionId] = statuses
}

func (self *appleEnvironmentFake) setAnswer(transactionId string, statusCode int, body string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.answers[transactionId] = appleFakeAnswer{statusCode: statusCode, body: body}
}

func (self *appleEnvironmentFake) requestCount(transactionId string) int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.requests[transactionId]
}

// appleSandboxReconcileTestEnv stands up both App Store environments behind
// the reconciler's seams, with apple.yml accepting the given notification
// environments.
type appleSandboxReconcileTestEnv struct {
	production *appleEnvironmentFake
	sandbox    *appleEnvironmentFake

	mutex        sync.Mutex
	environments []string
}

func newAppleSandboxReconcileTestEnv(t testing.TB, environments ...string) *appleSandboxReconcileTestEnv {
	env := &appleSandboxReconcileTestEnv{
		production:   newAppleEnvironmentFake(t),
		sandbox:      newAppleEnvironmentFake(t),
		environments: environments,
	}

	prevBaseUrl := appleAppStoreServerApiBaseUrl
	prevSandboxBaseUrl := appleAppStoreServerApiSandboxBaseUrl
	prevCredentials := appleReconcileCredentialsFunc
	prevAuthHeader := appleServerApiAuthHeaderFunc
	prevHasCredentials := appleReconcileHasCredentials
	appleAppStoreServerApiBaseUrl = env.production.testServer.URL
	appleAppStoreServerApiSandboxBaseUrl = env.sandbox.testServer.URL
	appleReconcileCredentialsFunc = func() *appleServerApiCredentials {
		return &appleServerApiCredentials{
			KeyId:        "TESTKEY",
			IssuerId:     "test-issuer",
			PrivateKey:   "unused",
			BundleId:     "network.ur.test",
			ProductIds:   []string{"supporter_monthly"},
			Environments: env.acceptedEnvironments(),
		}
	}
	appleServerApiAuthHeaderFunc = func(ctx context.Context, header http.Header) {
		header.Add("Authorization", "Bearer test-token")
	}
	appleReconcileHasCredentials = func() bool { return true }

	t.Cleanup(func() {
		appleAppStoreServerApiBaseUrl = prevBaseUrl
		appleAppStoreServerApiSandboxBaseUrl = prevSandboxBaseUrl
		appleReconcileCredentialsFunc = prevCredentials
		appleServerApiAuthHeaderFunc = prevAuthHeader
		appleReconcileHasCredentials = prevHasCredentials
	})
	return env
}

func (self *appleSandboxReconcileTestEnv) acceptedEnvironments() []string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.environments
}

func (self *appleSandboxReconcileTestEnv) acceptEnvironments(environments ...string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.environments = environments
}

// appleTestActiveSandboxStatuses is the sandbox's answer for an active
// TestFlight subscription: renewed every 24 hours.
func appleTestActiveSandboxStatuses(
	t testing.TB,
	networkId server.Id,
	transactionId string,
	now time.Time,
) *appleSubscriptionStatusesResponse {
	return appleTestStatuses(t, 1, transactionId, appleTestTransactionClaims(
		t,
		networkId, transactionId, "supporter_monthly",
		now.Add(-1*time.Hour), now.Add(23*time.Hour),
	))
}

// appleReconcileStatusErrors returns the error text of each App Store
// status-leg error event.
func appleReconcileStatusErrors(events []*model.PaymentReconciliationEvent) []string {
	messages := []string{}
	for _, event := range events {
		if event.Store != model.SubscriptionMarketApple ||
			event.Action != model.PaymentReconcileActionError ||
			event.Details["leg"] != "status" {
			continue
		}
		message, _ := event.Details["error"].(string)
		messages = append(messages, message)
	}
	return messages
}

// appleReconcileHeartbeatSandboxCount reads the App Store sandbox count from
// the run's durable heartbeat row.
func appleReconcileHeartbeatSandboxCount(t testing.TB, events []*model.PaymentReconciliationEvent) int {
	t.Helper()
	for _, event := range events {
		if event.Store != model.PaymentReconcileStoreAll || event.Action != model.PaymentReconcileActionHeartbeat {
			continue
		}
		stores, _ := event.Details["stores"].(map[string]any)
		apple, _ := stores[model.SubscriptionMarketApple].(map[string]any)
		count, _ := apple["sandbox"].(float64)
		return int(count)
	}
	t.Fatalf("the run wrote no heartbeat")
	return 0
}

// TestPaymentReconcileAppleSandboxTransactionIsNotAStoreError reproduces Main:
// production answers 4040010 for TestFlight transactions. The sandbox lookup
// confirms them, the run completes without a store error and writes the
// watermark, and the sandbox state repairs nothing, though the same answer
// from production would end one renewal and credit the other.
func TestPaymentReconcileAppleSandboxTransactionIsNotAStoreError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		// TestFlight renews every 24 hours, whatever the product's period.
		// The sandbox says this one expired.
		expiredNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, expiredNetworkId, "reconcileapplesandbox1", server.NewId())
		expiredTransactionId := "2000000000000101"
		appleTestCredit(
			t, ctx, expiredNetworkId, expiredTransactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		env.sandbox.setStatuses(expiredTransactionId, appleTestStatuses(t, 2, expiredTransactionId, appleTestTransactionClaims(
			t,
			expiredNetworkId, expiredTransactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)))

		// The sandbox says this one renewed, and no notification delivered the
		// renewal.
		renewedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, renewedNetworkId, "reconcileapplesandbox2", server.NewId())
		renewedTransactionId := "2000000000000201"
		appleTestCredit(
			t, ctx, renewedNetworkId, renewedTransactionId, "supporter_monthly",
			now.Add(-25*time.Hour), now.Add(-1*time.Hour),
		)
		missedTransactionId := "2000000000000202"
		env.sandbox.setStatuses(renewedTransactionId, appleTestStatuses(t, 1, renewedTransactionId, appleTestTransactionClaims(
			t,
			renewedNetworkId, missedTransactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)))

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, result.Ended, 0)
		connect.AssertEqual(t, result.EntitlementsRepaired, 0)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketApple].Sandbox, 2)

		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, len(appleReconcileStatusErrors(events)), 0)
		connect.AssertEqual(t, appleReconcileHeartbeatSandboxCount(t, events), 2)

		// production first, then the sandbox once for each renewal
		for _, transactionId := range []string{expiredTransactionId, renewedTransactionId} {
			connect.AssertEqual(t, env.production.requestCount(transactionId), 1)
			connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 1)
		}

		// the complete, error-free pass wrote the watermark
		_, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
		connect.AssertEqual(t, ok, true)

		// sandbox state changed no entitlement
		connect.AssertEqual(t, model.IsProNetwork(ctx, expiredNetworkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, expiredNetworkId)), 1)
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, missedTransactionId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, renewedNetworkId)), 1)
	})
}

// TestPaymentReconcileAppleSandboxMissRemainsStatusError: only an affirmative,
// valid sandbox answer clears production's 4040010. A sandbox miss, outage,
// authentication failure or malformed status stays one status error that
// names the sandbox, and the watermark stays where it was.
func TestPaymentReconcileAppleSandboxMissRemainsStatusError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapplesandboxmiss", server.NewId())
		transactionId := "2000000000000301"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		priorWatermark := now.Add(-6 * time.Hour)
		err := model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark)
		connect.AssertEqual(t, err, nil)

		for _, test := range []struct {
			name       string
			statusCode int
			body       string
			want       string
		}{
			{name: "not found in either environment", statusCode: http.StatusNotFound, body: appleTestTransactionIdNotFoundBody, want: "4040010"},
			{name: "sandbox unavailable", statusCode: http.StatusInternalServerError, body: `{"errorCode":5000001,"errorMessage":"An unknown error occurred. Please try again."}`, want: "5000001"},
			{name: "sandbox unauthorized", statusCode: http.StatusUnauthorized, body: "", want: "401"},
			{name: "sandbox unsupported status", statusCode: http.StatusOK, body: `{"data":[{"lastTransactions":[{"status":91}]}]}`, want: "unsupported App Store subscription status 91"},
		} {
			env.sandbox.setAnswer(transactionId, test.statusCode, test.body)
			sandboxRequests := env.sandbox.requestCount(transactionId)

			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			if result.Errors != 1 || result.StoreResults[model.SubscriptionMarketApple].Sandbox != 0 {
				t.Fatalf("%s: errors=%d sandbox=%d, want one store error and no sandbox count",
					test.name, result.Errors, result.StoreResults[model.SubscriptionMarketApple].Sandbox)
			}
			messages := appleReconcileStatusErrors(model.GetPaymentReconciliationEvents(ctx, result.RunId))
			if len(messages) != 1 || !strings.HasPrefix(messages[0], "App Store sandbox: ") || !strings.Contains(messages[0], test.want) {
				t.Fatalf("%s: status errors %q do not name the sandbox answer %q", test.name, messages, test.want)
			}
			connect.AssertEqual(t, env.sandbox.requestCount(transactionId), sandboxRequests+1)

			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			if !ok || !watermark.Equal(priorWatermark) {
				t.Fatalf("%s: a sandbox miss moved the App Store watermark", test.name)
			}
			connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
			connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		}
	})
}

// TestPaymentReconcileAppleSandboxLookupRequiresAcceptedEnvironment: a
// deployment whose apple.yml does not accept Sandbox notifications credits no
// sandbox transaction, so a production 4040010 there is a real unknown id. It
// stays a status error and the sandbox is never asked, even when the sandbox
// would know the id.
func TestPaymentReconcileAppleSandboxLookupRequiresAcceptedEnvironment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapplesandboxgate", server.NewId())
		transactionId := "2000000000000401"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		env.sandbox.setStatuses(transactionId, appleTestActiveSandboxStatuses(t, networkId, transactionId, now))
		priorWatermark := now.Add(-6 * time.Hour)
		err := model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark)
		connect.AssertEqual(t, err, nil)

		for _, environments := range [][]string{nil, {"Production"}} {
			env.acceptEnvironments(environments...)

			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Errors, 1)
			messages := appleReconcileStatusErrors(model.GetPaymentReconciliationEvents(ctx, result.RunId))
			if len(messages) != 1 || strings.Contains(messages[0], "sandbox") || !strings.Contains(messages[0], "4040010") {
				t.Fatalf("environments %v: status errors %q, want the production 4040010 alone", environments, messages)
			}
			connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 0)

			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, watermark.Equal(priorWatermark), true)
		}
	})
}

// TestPaymentReconcileAppleSandboxLookupOnlyForTransactionIdNotFound: every
// other production failure (a 404 that is not Apple's 4040010, authentication,
// rate limit, outage) stays a status error without a sandbox request, even
// where Sandbox is accepted and the sandbox would know the id.
func TestPaymentReconcileAppleSandboxLookupOnlyForTransactionIdNotFound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapplesandboxonly", server.NewId())
		transactionId := "2000000000000501"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		env.sandbox.setStatuses(transactionId, appleTestActiveSandboxStatuses(t, networkId, transactionId, now))
		priorWatermark := now.Add(-6 * time.Hour)
		err := model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark)
		connect.AssertEqual(t, err, nil)

		for _, test := range []struct {
			name       string
			statusCode int
			body       string
		}{
			{name: "404 without Apple's error object", statusCode: http.StatusNotFound, body: "not found"},
			{name: "original transaction id not found", statusCode: http.StatusNotFound, body: `{"errorCode":4040005,"errorMessage":"Original transaction id not found."}`},
			{name: "unauthorized", statusCode: http.StatusUnauthorized, body: ""},
			{name: "rate limited", statusCode: http.StatusTooManyRequests, body: `{"errorCode":4290000,"errorMessage":"Rate limit exceeded."}`},
			{name: "unavailable", statusCode: http.StatusInternalServerError, body: `{"errorCode":5000001,"errorMessage":"An unknown error occurred. Please try again."}`},
		} {
			env.production.setAnswer(transactionId, test.statusCode, test.body)

			result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
			connect.AssertEqual(t, err, nil)
			messages := appleReconcileStatusErrors(model.GetPaymentReconciliationEvents(ctx, result.RunId))
			if result.Errors != 1 || len(messages) != 1 || strings.Contains(messages[0], "sandbox") {
				t.Fatalf("%s: errors=%d status errors %q, want one production status error", test.name, result.Errors, messages)
			}
			if env.sandbox.requestCount(transactionId) != 0 {
				t.Fatalf("%s: a production failure other than 4040010 asked the sandbox", test.name)
			}

			watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, watermark.Equal(priorWatermark), true)
		}
	})
}

// TestPaymentReconcileAppleSandboxLookupSpendsStoreBudget: the sandbox lookup
// is a second store API call. With the budget spent on the production call,
// the pass stops incomplete: no error, no sandbox request and no watermark
// advance, so the next run resumes the renewal.
func TestPaymentReconcileAppleSandboxLookupSpendsStoreBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newAppleSandboxReconcileTestEnv(t, "Production", "Sandbox")
		previousBudget := paymentReconcileApiBudget
		paymentReconcileApiBudget = 1
		t.Cleanup(func() { paymentReconcileApiBudget = previousBudget })
		now := server.NowUtc().Truncate(time.Millisecond)

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "reconcileapplesandboxbudget", server.NewId())
		transactionId := "2000000000000601"
		appleTestCredit(
			t, ctx, networkId, transactionId, "supporter_monthly",
			now.Add(-1*time.Hour), now.Add(23*time.Hour),
		)
		env.sandbox.setStatuses(transactionId, appleTestActiveSandboxStatuses(t, networkId, transactionId, now))
		priorWatermark := now.Add(-6 * time.Hour)
		err := model.SetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple, priorWatermark)
		connect.AssertEqual(t, err, nil)

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketApple].BudgetExhausted, true)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketApple].Sandbox, 0)
		connect.AssertEqual(t, env.production.requestCount(transactionId), 1)
		connect.AssertEqual(t, env.sandbox.requestCount(transactionId), 0)

		watermark, ok := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketApple)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, watermark.Equal(priorWatermark), true)
	})
}

// Only Apple's 404 TransactionIdNotFoundError asks the sandbox.
func TestAppleTransactionIdNotFound(t *testing.T) {
	notFound := &server.HttpStatusError{
		StatusCode:   http.StatusNotFound,
		Status:       "404 Not Found",
		ResponseBody: appleTestTransactionIdNotFoundBody,
	}
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "transaction id not found", err: notFound, want: true},
		{name: "wrapped", err: fmt.Errorf("subscription statuses: %w", notFound), want: true},
		{name: "original transaction id not found", err: &server.HttpStatusError{StatusCode: http.StatusNotFound, ResponseBody: `{"errorCode":4040005}`}, want: false},
		{name: "404 without Apple's error object", err: &server.HttpStatusError{StatusCode: http.StatusNotFound, ResponseBody: "not found"}, want: false},
		{name: "code on another status", err: &server.HttpStatusError{StatusCode: http.StatusInternalServerError, ResponseBody: appleTestTransactionIdNotFoundBody}, want: false},
		{name: "transport failure", err: errors.New("dial tcp: i/o timeout"), want: false},
		{name: "no error", err: nil, want: false},
	} {
		if got := appleTransactionIdNotFound(test.err); got != test.want {
			t.Errorf("%s: appleTransactionIdNotFound = %t, want %t", test.name, got, test.want)
		}
	}
}

// The reconciler reads the accepted environments from the same
// app_store_notifications block the webhook verifier enforces. A missing list
// accepts none, and the credentials stay complete either way.
func TestAppleReconcileCredentialsReadAcceptedEnvironments(t *testing.T) {
	for _, test := range []struct {
		name       string
		yaml       string
		sandbox    bool
		production bool
	}{
		{
			name: "production and sandbox",
			yaml: `
app_store_server_api_key_id: key-id
issuer_id: issuer
private_key: key
app_store_notifications:
  bundle_id: app.example
  environments: [Production, Sandbox]
`,
			sandbox:    true,
			production: true,
		},
		{
			name: "production only",
			yaml: `
app_store_server_api_key_id: key-id
issuer_id: issuer
private_key: key
app_store_notifications:
  bundle_id: app.example
  environments: [Production]
`,
			sandbox:    false,
			production: true,
		},
		{
			name: "no environments",
			yaml: `
app_store_server_api_key_id: key-id
issuer_id: issuer
private_key: key
app_store_notifications:
  bundle_id: app.example
`,
			sandbox:    false,
			production: false,
		},
	} {
		pop := server.Vault.PushSimpleResource("apple.yml", []byte(test.yaml))
		creds := appleReconcileCredentialsFromVault()
		pop()
		if creds == nil {
			t.Fatalf("%s: complete Apple credentials were refused", test.name)
		}
		if got := creds.allowsEnvironment("Sandbox"); got != test.sandbox {
			t.Errorf("%s: allows Sandbox = %t, want %t", test.name, got, test.sandbox)
		}
		if got := creds.allowsEnvironment("Production"); got != test.production {
			t.Errorf("%s: allows Production = %t, want %t", test.name, got, test.production)
		}
	}
}
