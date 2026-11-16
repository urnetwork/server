// Local synthetic Grafana fixtures exercise native reconciliation, secret redaction and one-shot notification transport without external calls.
package grafana

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// A synthetic webhook. Its path stands in for the secret part of a real one.
const syntheticSlackWebhookUrl = "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/syntheticWebhookSecret01"

// A synthetic heartbeat check's ping url. Its path stands in for the check's
// secret uuid.
const syntheticWatchdogHeartbeatUrl = "https://heartbeat.example/ping/syntheticHeartbeatSecret01"

// One recorded mutation of the fake Grafana.
type fakeGrafanaWrite struct {
	method string
	path   string
	body   string
}

// In-memory Grafana provisioning and receiver-test API. GET responses redact
// stored Slack urls the way Grafana does; writes are recorded in order.
type fakeAlertingGrafana struct {
	t *testing.T

	stateLock    sync.Mutex
	contacts     []grafanaContactPoint
	policy       map[string]any
	writes       []fakeGrafanaWrite
	requestCount int
	// status and body forced on writes to the contact point with this uid
	failedContactUid    string
	failedContactStatus int
	failedContactBody   string
	// status and body forced on policy writes
	policyStatus int
	policyBody   string
	testResponse string
}

func newFakeAlertingGrafana(t *testing.T, contacts []grafanaContactPoint, policy map[string]any) (*fakeAlertingGrafana, *httptest.Server) {
	fake := &fakeAlertingGrafana{
		t:            t,
		contacts:     contacts,
		policy:       policy,
		testResponse: `{"status":"success","duration":"85ms"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	return fake, server
}

func (self *fakeAlertingGrafana) serve(response http.ResponseWriter, request *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requestCount++
	username, password, ok := request.BasicAuth()
	if !ok || username != "admin" || password != "secret" {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	if request.Method != http.MethodGet && request.Header.Get("X-Disable-Provenance") != "true" {
		self.t.Error("mutation did not preserve Grafana UI editability")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Error(err)
	}
	if request.Method != http.MethodGet {
		self.writes = append(self.writes, fakeGrafanaWrite{method: request.Method, path: request.URL.Path, body: string(body)})
	}
	response.Header().Set("Content-Type", "application/json")
	encode := func(value any) {
		if err := json.NewEncoder(response).Encode(value); err != nil {
			self.t.Error(err)
		}
	}
	decodeContact := func() grafanaContactPoint {
		contact := grafanaContactPoint{}
		if err := json.Unmarshal(body, &contact); err != nil {
			self.t.Error(err)
		}
		return contact
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/contact-points":
		listed := []grafanaContactPoint{}
		for _, contact := range self.contacts {
			settings := map[string]any{}
			for key, value := range contact.Settings {
				settings[key] = value
			}
			if storedUrl, ok := settings["url"].(string); contact.Type == "slack" && ok && storedUrl != "" {
				settings["url"] = "[REDACTED]"
			}
			contact.Settings = settings
			listed = append(listed, contact)
		}
		encode(listed)
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/provisioning/contact-points":
		contact := decodeContact()
		if contact.Uid == self.failedContactUid && self.failedContactStatus != 0 {
			response.WriteHeader(self.failedContactStatus)
			_, _ = io.WriteString(response, self.failedContactBody)
			return
		}
		self.contacts = append(self.contacts, contact)
		response.WriteHeader(http.StatusAccepted)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/api/v1/provisioning/contact-points/"):
		contact := decodeContact()
		uid := strings.TrimPrefix(request.URL.Path, "/api/v1/provisioning/contact-points/")
		if uid == self.failedContactUid && self.failedContactStatus != 0 {
			response.WriteHeader(self.failedContactStatus)
			_, _ = io.WriteString(response, self.failedContactBody)
			return
		}
		for i := range self.contacts {
			if self.contacts[i].Uid == uid {
				self.contacts[i] = contact
			}
		}
		response.WriteHeader(http.StatusAccepted)
	case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/policies":
		encode(self.policy)
	case request.Method == http.MethodPut && request.URL.Path == "/api/v1/provisioning/policies":
		if self.policyStatus != 0 {
			response.WriteHeader(self.policyStatus)
			_, _ = io.WriteString(response, self.policyBody)
			return
		}
		policy := map[string]any{}
		if err := json.Unmarshal(body, &policy); err != nil {
			self.t.Error(err)
		}
		// Grafana refuses a tree that names a receiver it does not have. The
		// fixtures' manual routes name receivers they do not model, so child
		// routes are checked only for the heartbeat's.
		receiver, _ := policy["receiver"].(string)
		if receiver != "empty" && !self.hasReceiverWithLock(receiver) {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		routes, _ := policy["routes"].([]any)
		for _, value := range routes {
			route, _ := value.(map[string]any)
			if route["receiver"] == WatchdogAlertContactName && !self.hasReceiverWithLock(WatchdogAlertContactName) {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		self.policy = policy
		response.WriteHeader(http.StatusAccepted)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, grafanaReceiversApiPath+"/") && strings.HasSuffix(request.URL.Path, "/test"):
		_, _ = io.WriteString(response, self.testResponse)
	default:
		response.WriteHeader(http.StatusNotFound)
	}
}

func (self *fakeAlertingGrafana) hasReceiverWithLock(name string) bool {
	for _, contact := range self.contacts {
		if contact.Name == name {
			return true
		}
	}
	return false
}

// The integrations Grafana would notify for an alert that matches no child
// route: those of the root receiver.
func (self *fakeAlertingGrafana) unroutedIntegrations() []grafanaContactPoint {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	receiver, _ := self.policy["receiver"].(string)
	integrations := []grafanaContactPoint{}
	for _, contact := range self.contacts {
		if contact.Name == receiver {
			integrations = append(integrations, contact)
		}
	}
	return integrations
}

func (self *fakeAlertingGrafana) recordedWrites() []fakeGrafanaWrite {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]fakeGrafanaWrite(nil), self.writes...)
}

// A snapshot of the stored contact points, policy and request count.
func (self *fakeAlertingGrafana) snapshot() ([]grafanaContactPoint, map[string]any, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]grafanaContactPoint(nil), self.contacts...), self.policy, self.requestCount
}

func (self *fakeAlertingGrafana) failSlackContactWrites(status int, body string) {
	self.failContactWrites(DefaultAlertContactUid, status, body)
}

func (self *fakeAlertingGrafana) failContactWrites(uid string, status int, body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failedContactUid = uid
	self.failedContactStatus = status
	self.failedContactBody = body
}

// The integrations Grafana would notify for an alert with labels: those of
// the receiver of the first top-level route whose matchers all match, else
// the root's. Nested routes and continue are not modeled; the managed tree
// has neither.
func (self *fakeAlertingGrafana) integrationsFor(labels map[string]string) []grafanaContactPoint {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	receiver, _ := self.policy["receiver"].(string)
	routes, _ := self.policy["routes"].([]any)
	for _, value := range routes {
		route, _ := value.(map[string]any)
		matchers, _ := route["object_matchers"].([]any)
		matched := true
		for _, matcherValue := range matchers {
			matcher, _ := matcherValue.([]any)
			if len(matcher) != 3 {
				self.t.Fatalf("matcher = %#v", matcherValue)
			}
			name, _ := matcher[0].(string)
			operator, _ := matcher[1].(string)
			pattern, _ := matcher[2].(string)
			actual := labels[name]
			switch operator {
			case "=":
				matched = matched && actual == pattern
			case "!=":
				matched = matched && actual != pattern
			case "=~":
				matched = matched && regexp.MustCompile("^(?:"+pattern+")$").MatchString(actual)
			case "!~":
				matched = matched && !regexp.MustCompile("^(?:"+pattern+")$").MatchString(actual)
			default:
				self.t.Fatalf("matcher operator = %q", operator)
			}
		}
		if matched {
			receiver, _ = route["receiver"].(string)
			break
		}
	}
	integrations := []grafanaContactPoint{}
	for _, contact := range self.contacts {
		if contact.Name == receiver {
			integrations = append(integrations, contact)
		}
	}
	return integrations
}

func (self *fakeAlertingGrafana) failPolicyWrites(status int, body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.policyStatus = status
	self.policyBody = body
}

func (self *fakeAlertingGrafana) respondToTests(body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.testResponse = body
}

// Synthetic unmatched alerts have no integration; the unrelated team route
// explicitly excludes competition so readiness need not invent team labels.
func unroutedToNobodyPolicy() map[string]any {
	return map[string]any{
		"receiver": "empty",
		"group_by": []any{"grafana_folder", "alertname"},
		"routes": []any{
			map[string]any{
				"receiver":        "team-core",
				"object_matchers": []any{[]any{"team", "=", "core"}, []any{"service", "!=", "sim-latency"}},
			},
		},
	}
}

func teamCoreContacts() []grafanaContactPoint {
	return []grafanaContactPoint{
		{Uid: "team-core", Name: "team-core", Type: "webhook", Settings: map[string]any{"url": "https://alerts.example/core"}},
	}
}

func TestCompetitionAlertRoutingPreservesTheMainPolicyTree(t *testing.T) {
	contacts := []grafanaContactPoint{
		{Uid: "main-on-call", Name: "main-on-call", Type: "webhook", Settings: map[string]any{"url": "https://alerts.invalid/main"}},
		{Uid: DefaultAlertContactUid, Name: DefaultAlertContactName, Type: "slack", Settings: map[string]any{"url": syntheticSlackWebhookUrl}},
	}
	policy := map[string]any{
		"receiver": "main-on-call",
		"group_by": []any{"grafana_folder", "alertname"},
		"routes": []any{
			map[string]any{
				"receiver":        "main-on-call",
				"object_matchers": []any{[]any{"team", "=", "core"}, []any{"service", "!=", "sim-latency"}},
			},
		},
	}
	fake, server := newFakeAlertingGrafana(t, contacts, policy)

	for i := 0; i < 2; i++ {
		result, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{})
		if err != nil {
			t.Fatal(err)
		}
		if result.SlackConfigured || result.DefaultReceiver != "main-on-call" || !result.DefaultReceiverHasIntegrations {
			t.Fatalf("result = %+v", result)
		}
	}
	contactWrites := 0
	policyWrites := 0
	for _, write := range fake.recordedWrites() {
		switch write.path {
		case "/api/v1/provisioning/contact-points":
			contactWrites++
		case "/api/v1/provisioning/policies":
			policyWrites++
		}
	}
	if contactWrites != 0 || policyWrites != 2 {
		t.Fatalf("contact writes = %d, policy writes = %d", contactWrites, policyWrites)
	}
	storedContacts, storedPolicy, _ := fake.snapshot()
	if len(storedContacts) != 2 || storedContacts[1].Uid != DefaultAlertContactUid ||
		storedContacts[1].Settings["url"] != syntheticSlackWebhookUrl {
		t.Fatalf("contacts = %+v", storedContacts)
	}
	routes, ok := storedPolicy["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("policy routes = %#v", storedPolicy["routes"])
	}
	unrelated, _ := routes[0].(map[string]any)
	managed, _ := routes[1].(map[string]any)
	if unrelated["receiver"] != "main-on-call" || managed["receiver"] != DefaultAlertContactName ||
		storedPolicy["receiver"] != "main-on-call" {
		t.Fatalf("policy = %#v", storedPolicy)
	}
	encoded, err := json.Marshal(managed["object_matchers"])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `[["service","=","sim-latency"],["severity","=~","warn|page"]]` {
		t.Fatalf("managed matchers = %s", encoded)
	}
}

// The retired email name is no longer managed; an operator-owned integration remains untouched.
func TestCompetitionAlertRoutingPreservesLegacyNameCollision(t *testing.T) {
	contacts := []grafanaContactPoint{
		{Uid: "synthetic-other-integration", Name: CompetitionAlertContactName, Type: "email", Settings: map[string]any{"addresses": "operator@notify.example"}},
		{Uid: DefaultAlertContactUid, Name: DefaultAlertContactName, Type: "slack", Settings: map[string]any{"url": syntheticSlackWebhookUrl}},
	}
	fake, server := newFakeAlertingGrafana(t, contacts, map[string]any{"receiver": DefaultAlertContactName})
	if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{}); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := fake.snapshot()
	if len(stored) != len(contacts) || stored[0].Uid != contacts[0].Uid || stored[0].Settings["addresses"] != contacts[0].Settings["addresses"] {
		t.Fatal("unmanaged email integration changed")
	}
	for _, write := range fake.recordedWrites() {
		if write.path != "/api/v1/provisioning/policies" {
			t.Fatalf("contact write: %+v", write)
		}
	}
}

func TestCompetitionAlertRoutingRejectsInsecureRemoteEndpointsAndRedirects(t *testing.T) {
	if _, err := newGrafanaProvisioningClient("http://grafana.example.com", "admin", "secret"); err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatalf("insecure Grafana endpoint error = %v", err)
	}

	redirectFollowed := false
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirectFollowed = true
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/credential-capture", http.StatusFound)
	}))
	defer origin.Close()
	if _, err := ReconcileAlertRouting(context.Background(), origin.URL, "admin", "secret", AlertRoutingSettings{}); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirecting Grafana endpoint error = %v", err)
	}
	if redirectFollowed {
		t.Fatal("Grafana provisioning followed a credential-bearing redirect")
	}
}

// Root cause of "no alert reaches anyone" (owner decisions 2026-10-13 §10d):
// the root policy routed to a receiver without integrations and nothing ever
// replaced it. With the vault webhook set, an unrouted alert must reach a
// Slack integration that carries that webhook.
func TestAlertRoutingRoutesUnmatchedAlertsToSlack(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())
	if integrations := fake.unroutedIntegrations(); len(integrations) != 0 {
		t.Fatalf("fixture already routes unmatched alerts: %+v", integrations)
	}

	result, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{
		SlackWebhookUrl: " " + syntheticSlackWebhookUrl + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := AlertRoutingResult{
		PreviousDefaultReceiver:        "empty",
		DefaultReceiver:                DefaultAlertContactName,
		DefaultReceiverHasIntegrations: true,
		SlackConfigured:                true,
	}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}

	integrations := fake.unroutedIntegrations()
	if len(integrations) != 1 {
		t.Fatalf("unrouted alerts reach %d integrations: %+v", len(integrations), integrations)
	}
	slack := integrations[0]
	if slack.Uid != DefaultAlertContactUid || slack.Type != "slack" || slack.DisableResolveMessage ||
		slack.Settings["url"] != syntheticSlackWebhookUrl || len(slack.Settings) != 1 {
		t.Fatalf("default integration = %+v", slack)
	}

	// The rest of the tree is preserved: grouping, the unrelated route, and
	// the managed competition route after it.
	_, storedPolicy, _ := fake.snapshot()
	group, _ := json.Marshal(storedPolicy["group_by"])
	routes, _ := json.Marshal(storedPolicy["routes"])
	if string(group) != `["grafana_folder","alertname"]` ||
		string(routes) != `[{"object_matchers":[["team","=","core"],["service","!=","sim-latency"]],"receiver":"team-core"},{"object_matchers":[["service","=","sim-latency"],["severity","=~","warn|page"]],"receiver":"urnetwork-default"}]` {
		t.Fatalf("policy = %#v", storedPolicy)
	}

	// The contact point is written before the policy that names it.
	writes := fake.recordedWrites()
	slackWrite, policyWrite := -1, -1
	for i, write := range writes {
		if strings.Contains(write.body, `"uid":"`+DefaultAlertContactUid+`"`) {
			slackWrite = i
		}
		if write.path == "/api/v1/provisioning/policies" {
			policyWrite = i
		}
	}
	if slackWrite < 0 || policyWrite < slackWrite {
		t.Fatalf("write order = %+v", writes)
	}
}

// An empty webhook (the vault template) must not change routing or write a
// Slack contact point, and the result must say that unrouted alerts reach no
// one so the operator sees it.
func TestAlertRoutingWithoutSlackWebhookKeepsDefaultReceiver(t *testing.T) {
	contacts := append(teamCoreContacts(), grafanaContactPoint{Uid: DefaultAlertContactUid, Name: DefaultAlertContactName, Type: "slack", Settings: map[string]any{"url": syntheticSlackWebhookUrl}})
	fake, server := newFakeAlertingGrafana(t, contacts, unroutedToNobodyPolicy())

	result, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: "  "})
	if err != nil {
		t.Fatal(err)
	}
	want := AlertRoutingResult{
		PreviousDefaultReceiver:        "empty",
		DefaultReceiver:                "empty",
		DefaultReceiverHasIntegrations: false,
		SlackConfigured:                false,
	}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
	if _, storedPolicy, _ := fake.snapshot(); storedPolicy["receiver"] != "empty" {
		t.Fatalf("root receiver changed to %v", storedPolicy["receiver"])
	}
	for _, write := range fake.recordedWrites() {
		if strings.Contains(write.body, DefaultAlertContactUid) || strings.Contains(write.body, `"slack"`) {
			t.Fatalf("empty webhook wrote a Slack contact point: %+v", write)
		}
	}
}

// Grafana returns the stored url redacted, so every reconcile rewrites the
// existing integration in place: a rotated vault webhook must propagate. An
// integration an operator added under the same name is kept.
func TestAlertRoutingRewritesTheStoredSlackWebhook(t *testing.T) {
	contacts := append(teamCoreContacts(),
		grafanaContactPoint{Uid: DefaultAlertContactUid, Name: DefaultAlertContactName, Type: "slack", Settings: map[string]any{"url": "https://hooks.slack.example/services/OLD/OLD/old"}},
		grafanaContactPoint{Uid: "operator-added-email", Name: DefaultAlertContactName, Type: "email", Settings: map[string]any{"addresses": "oncall@example.com"}},
	)
	policy := unroutedToNobodyPolicy()
	policy["receiver"] = DefaultAlertContactName
	fake, server := newFakeAlertingGrafana(t, contacts, policy)

	for i := 0; i < 2; i++ {
		if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: syntheticSlackWebhookUrl}); err != nil {
			t.Fatal(err)
		}
	}
	slackPuts := 0
	for _, write := range fake.recordedWrites() {
		if write.method == http.MethodPost && strings.Contains(write.body, DefaultAlertContactUid) {
			t.Fatalf("existing Slack integration was created again: %+v", write)
		}
		if write.method == http.MethodPut && write.path == "/api/v1/provisioning/contact-points/"+DefaultAlertContactUid {
			slackPuts++
		}
	}
	if slackPuts != 2 {
		t.Fatalf("Slack integration rewrites = %d", slackPuts)
	}
	integrations := fake.unroutedIntegrations()
	if len(integrations) != 2 || integrations[0].Settings["url"] != syntheticSlackWebhookUrl || integrations[1].Uid != "operator-added-email" {
		t.Fatalf("default receiver integrations = %+v", integrations)
	}
}

// Grafana error text can quote the webhook (url.Parse and net/http errors
// embed their URL). No returned error may carry it or its secret path.
func TestAlertRoutingRedactsTheSlackWebhookFromErrors(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())
	parsed, err := url.Parse(syntheticSlackWebhookUrl)
	if err != nil {
		t.Fatal(err)
	}
	fake.failSlackContactWrites(http.StatusBadRequest, `{"message":"invalid settings: parse \"`+syntheticSlackWebhookUrl+`\": rejected path `+parsed.Path+`"}`)

	_, err = ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: syntheticSlackWebhookUrl})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), syntheticSlackWebhookUrl) || strings.Contains(err.Error(), parsed.Path) || strings.Contains(err.Error(), "syntheticWebhookSecret01") {
		t.Fatalf("error quotes the webhook: %v", err)
	}
	if _, storedPolicy, _ := fake.snapshot(); storedPolicy["receiver"] != "empty" {
		t.Fatal("policy was rewritten after the contact point write failed")
	}

	// An unconfigured webhook-shaped URL, such as a stale stored one, is
	// redacted by shape.
	redacted := redactSlackWebhook(`Post "https://hooks.slack.com/services/T0SYNTHETIC/B0SYNTHETIC/staleSecret": EOF`, syntheticSlackWebhookUrl)
	if strings.Contains(redacted, "staleSecret") || !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("shape redaction = %q", redacted)
	}
}

// Every error ReconcileAlertRouting returns is redacted at its boundary,
// whichever Grafana write failed: the native Slack contact point and policy
// writes quote Grafana's body. Redaction comes before the bound, so a
// webhook that a 512-byte cut would split is not left half quoted.
func TestAlertRoutingRedactsEveryErrorAtTheBoundary(t *testing.T) {
	parsed, err := url.Parse(syntheticSlackWebhookUrl)
	if err != nil {
		t.Fatal(err)
	}
	quoting := `{"message":"rejected ` + syntheticSlackWebhookUrl + `, ` + parsed.Path +
		` and https://hooks.slack.com/services/T0SYNTHETIC/B0SYNTHETIC/staleSecret"}`
	// a 512-byte cut lands inside the webhook's secret
	split := strings.Repeat("x", 512-80) + syntheticSlackWebhookUrl
	for _, c := range []struct {
		name string
		fail func(*fakeAlertingGrafana)
	}{
		{name: "policy", fail: func(fake *fakeAlertingGrafana) { fake.failPolicyWrites(http.StatusBadRequest, quoting) }},
		{name: "long policy", fail: func(fake *fakeAlertingGrafana) { fake.failPolicyWrites(http.StatusBadRequest, split) }},
		{name: "long Slack contact point", fail: func(fake *fakeAlertingGrafana) {
			fake.failSlackContactWrites(http.StatusBadRequest, split)
		}},
	} {
		fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())
		c.fail(fake)
		_, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: syntheticSlackWebhookUrl})
		if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
			t.Errorf("%s: error = %v", c.name, err)
			continue
		}
		for _, secret := range []string{syntheticSlackWebhookUrl, parsed.Path, "syntheticWebhookSe", "staleSecret", "hooks.slack.com/services", "services/T0SYNTHETIC"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error quotes %q: %v", c.name, secret, err)
			}
		}
		if alertRoutingErrorLimit < len(err.Error()) {
			t.Errorf("%s: error is %d bytes", c.name, len(err.Error()))
		}
	}
}

// A non-https webhook is refused before any request, without quoting it.
func TestAlertRoutingRejectsANonHttpsSlackWebhook(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())
	for i, webhookUrl := range []string{
		"http://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/plainSecret",
		"https://user:pass@hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/userinfoSecret",
		"hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/relativeSecret",
		"https://%zz/services/escapeSecret",
	} {
		_, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: webhookUrl})
		if err == nil || strings.Contains(err.Error(), "Secret") {
			t.Errorf("webhook %d: error = %v", i, err)
		}
	}
	if _, _, requestCount := fake.snapshot(); requestCount != 0 {
		t.Fatalf("invalid webhooks reached Grafana %d times", requestCount)
	}
}

// The test notification names the stored integration and asks Grafana to use
// its stored url; the webhook never crosses the client.
func TestDefaultAlertTestUsesTheStoredSlackIntegration(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())

	result, err := SendDefaultAlertTest(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: syntheticSlackWebhookUrl})
	if err != nil {
		t.Fatal(err)
	}
	if result != (AlertTestResult{Status: "success", Duration: "85ms"}) {
		t.Fatalf("result = %+v", result)
	}
	writes := fake.recordedWrites()
	if len(writes) != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	wantPath := grafanaReceiversApiPath + "/" + base64.RawURLEncoding.EncodeToString([]byte(DefaultAlertContactName)) + "/test"
	if writes[0].method != http.MethodPost || writes[0].path != wantPath {
		t.Fatalf("test request = %s %s, want POST %s", writes[0].method, writes[0].path, wantPath)
	}
	if strings.Contains(writes[0].body, "hooks.slack") || strings.Contains(writes[0].body, "syntheticWebhookSecret01") {
		t.Fatalf("test request carries the webhook: %s", writes[0].body)
	}
	var request struct {
		Integration struct {
			Uid          string          `json:"uid"`
			Type         string          `json:"type"`
			Settings     map[string]any  `json:"settings"`
			SecureFields map[string]bool `json:"secureFields"`
		} `json:"integration"`
		Alert struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"alert"`
	}
	if err := json.Unmarshal([]byte(writes[0].body), &request); err != nil {
		t.Fatal(err)
	}
	if request.Integration.Uid != DefaultAlertContactUid || request.Integration.Type != "slack" ||
		len(request.Integration.Settings) != 0 || !request.Integration.SecureFields["url"] ||
		request.Alert.Labels["alertname"] == "" || request.Alert.Annotations["summary"] == "" {
		t.Fatalf("test request = %+v", request)
	}

	// A delivery failure is a result, and Grafana's error text is redacted.
	fake.respondToTests(`{"status":"failure","duration":"1s","error":"failed to send Slack message: Post \"` + syntheticSlackWebhookUrl + `\": dial tcp: lookup failed"}`)
	result, err = SendDefaultAlertTest(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{SlackWebhookUrl: syntheticSlackWebhookUrl})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failure" || !strings.Contains(result.Error, "dial tcp") ||
		strings.Contains(result.Error, "hooks.slack") || strings.Contains(result.Error, "syntheticWebhookSecret01") {
		t.Fatalf("failure result = %+v", result)
	}
}

func TestAlertRoutingSettingsFromMonitorVault(t *testing.T) {
	settings, err := AlertRoutingSettingsFromMonitorVault([]byte("ssh:\n  user: monitor\nalerts:\n  slack_webhook_url: \" " + syntheticSlackWebhookUrl + " \"\n" +
		"  watchdog_heartbeat_url: \" " + syntheticWatchdogHeartbeatUrl + "\"\n"))
	if err != nil || settings.SlackWebhookUrl != syntheticSlackWebhookUrl || settings.WatchdogHeartbeatUrl != syntheticWatchdogHeartbeatUrl {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
	for _, document := range []string{"", "ssh:\n  user: monitor\n", "alerts:\n  slack_webhook_url: \"\"\n  watchdog_heartbeat_url: \"\"\n"} {
		settings, err := AlertRoutingSettingsFromMonitorVault([]byte(document))
		if err != nil || settings.SlackWebhookUrl != "" || settings.WatchdogHeartbeatUrl != "" {
			t.Errorf("document %q: settings = %+v, err = %v", document, settings, err)
		}
	}
	// a value under the wrong shape must not be quoted back
	_, err = AlertRoutingSettingsFromMonitorVault([]byte("alerts: " + syntheticSlackWebhookUrl + "\n"))
	if err == nil || strings.Contains(err.Error(), "syntheticWebhookSecret01") || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("malformed document error = %v", err)
	}
}

// The labels Grafana gives the always-firing Watchdog rule (warp
// grafana/alerting/watchdog.yml): its title as alertname, its own labels and
// its folder.
func watchdogAlertLabels() map[string]string {
	return map[string]string{
		"alertname":      WatchdogAlertName,
		"service":        WatchdogAlertService,
		"severity":       "warn",
		"grafana_folder": "urnetwork alerts",
	}
}

func slackRootContacts() []grafanaContactPoint {
	return append(teamCoreContacts(), grafanaContactPoint{
		Uid: DefaultAlertContactUid, Name: DefaultAlertContactName, Type: "slack", Settings: map[string]any{"url": syntheticSlackWebhookUrl},
	})
}

func slackRootPolicy() map[string]any {
	policy := unroutedToNobodyPolicy()
	policy["receiver"] = DefaultAlertContactName
	return policy
}

// The dead man's switch (owner, 2026-11-16). With no route of its own, the
// always-firing Watchdog would reach Slack every 4 hours and no external
// check would hear it stop. With the vault heartbeat set it reaches only the
// heartbeat's webhook, every 5 minutes, and nothing else does: not its own
// DatasourceError, which must reach Slack when Mimir stops answering, not
// another rule's alerts, not competition alerts. Reconciling again keeps one
// Watchdog route and rewrites the webhook in place.
func TestAlertRoutingSendsTheWatchdogOnlyToItsHeartbeat(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, slackRootContacts(), slackRootPolicy())
	if reached := fake.integrationsFor(watchdogAlertLabels()); len(reached) != 1 || reached[0].Uid != DefaultAlertContactUid {
		t.Fatalf("fixture routes the Watchdog to %+v", reached)
	}

	for i := 0; i < 2; i++ {
		result, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{
			WatchdogHeartbeatUrl: " " + syntheticWatchdogHeartbeatUrl + "\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := AlertRoutingResult{
			PreviousDefaultReceiver:        DefaultAlertContactName,
			DefaultReceiver:                DefaultAlertContactName,
			DefaultReceiverHasIntegrations: true,
			WatchdogConfigured:             true,
		}
		if result != want {
			t.Fatalf("result = %+v, want %+v", result, want)
		}
	}

	reached := fake.integrationsFor(watchdogAlertLabels())
	if len(reached) != 1 {
		t.Fatalf("the Watchdog reaches %+v", reached)
	}
	heartbeat := reached[0]
	if heartbeat.Uid != WatchdogAlertContactUid || heartbeat.Name != WatchdogAlertContactName || heartbeat.Type != "webhook" ||
		heartbeat.Settings["url"] != syntheticWatchdogHeartbeatUrl || heartbeat.Settings["httpMethod"] != http.MethodPost ||
		len(heartbeat.Settings) != 2 || !heartbeat.DisableResolveMessage {
		t.Fatalf("heartbeat integration = %+v", heartbeat)
	}
	for _, labels := range []map[string]string{
		{"alertname": "DatasourceError", "rulename": WatchdogAlertName, "service": WatchdogAlertService, "severity": "warn"},
		{"alertname": "DbArchiveRunOver168h", "service": "db-archive", "severity": "page"},
		{"alertname": "SimLatencyStale", "service": "sim-latency", "severity": "page"},
		{"alertname": WatchdogAlertName, "service": "db-archive", "severity": "warn"},
	} {
		reached := fake.integrationsFor(labels)
		if len(reached) != 1 || reached[0].Uid != DefaultAlertContactUid {
			t.Errorf("%v reaches %+v, want only the Slack default", labels, reached)
		}
	}

	_, storedPolicy, _ := fake.snapshot()
	routes, _ := json.Marshal(storedPolicy["routes"])
	if string(routes) != `[{"group_by":["alertname"],"group_interval":"1m","group_wait":"30s","object_matchers":[["alertname","=","Watchdog"],["service","=","watchdog"]],"receiver":"urnetwork-watchdog","repeat_interval":"5m"},`+
		`{"object_matchers":[["team","=","core"],["service","!=","sim-latency"]],"receiver":"team-core"},`+
		`{"object_matchers":[["service","=","sim-latency"],["severity","=~","warn|page"]],"receiver":"urnetwork-default"}]` {
		t.Fatalf("routes = %s", routes)
	}

	// created once, then rewritten in place, each time before the policy
	creates, rewrites := 0, 0
	lastContact, lastPolicy := -1, -1
	for i, write := range fake.recordedWrites() {
		switch {
		case write.method == http.MethodPost && strings.Contains(write.body, `"uid":"`+WatchdogAlertContactUid+`"`):
			creates++
			lastContact = i
		case write.method == http.MethodPut && write.path == "/api/v1/provisioning/contact-points/"+WatchdogAlertContactUid:
			rewrites++
			lastContact = i
		case write.path == "/api/v1/provisioning/policies":
			lastPolicy = i
		}
	}
	if creates != 1 || rewrites != 1 || lastPolicy < lastContact {
		t.Fatalf("heartbeat creates = %d, rewrites = %d, writes = %+v", creates, rewrites, fake.recordedWrites())
	}
}

// Without the vault heartbeat, a heartbeat an earlier reconcile set up stays:
// its contact point is not written and its route keeps its place, so an
// environment's routing does not change with a vault that lacks the url.
func TestAlertRoutingWithoutHeartbeatLeavesTheWatchdogAsItIs(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, slackRootContacts(), slackRootPolicy())
	if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{WatchdogHeartbeatUrl: syntheticWatchdogHeartbeatUrl}); err != nil {
		t.Fatal(err)
	}
	_, before, _ := fake.snapshot()
	beforeRoutes, _ := json.Marshal(before["routes"])
	writes := len(fake.recordedWrites())

	result, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{WatchdogHeartbeatUrl: "  "})
	if err != nil {
		t.Fatal(err)
	}
	if result.WatchdogConfigured {
		t.Fatalf("result = %+v", result)
	}
	for _, write := range fake.recordedWrites()[writes:] {
		if write.path != "/api/v1/provisioning/policies" {
			t.Fatalf("write without the heartbeat: %+v", write)
		}
	}
	_, after, _ := fake.snapshot()
	afterRoutes, _ := json.Marshal(after["routes"])
	if string(afterRoutes) != string(beforeRoutes) {
		t.Fatalf("routes = %s, want %s", afterRoutes, beforeRoutes)
	}
	if reached := fake.integrationsFor(watchdogAlertLabels()); len(reached) != 1 || reached[0].Uid != WatchdogAlertContactUid {
		t.Fatalf("the Watchdog reaches %+v", reached)
	}
}

// The heartbeat url is a credential: a Grafana error that quotes it, or its
// path, is redacted at the boundary, and the policy that would name its
// contact point is not written.
func TestAlertRoutingRedactsTheHeartbeatFromErrors(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, slackRootContacts(), slackRootPolicy())
	parsed, err := url.Parse(syntheticWatchdogHeartbeatUrl)
	if err != nil {
		t.Fatal(err)
	}
	fake.failContactWrites(WatchdogAlertContactUid, http.StatusBadRequest, `{"message":"invalid url \"`+syntheticWatchdogHeartbeatUrl+`\": rejected path `+parsed.Path+`"}`)

	_, err = ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{WatchdogHeartbeatUrl: syntheticWatchdogHeartbeatUrl})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "Watchdog heartbeat") {
		t.Fatalf("error = %v", err)
	}
	for _, secret := range []string{syntheticWatchdogHeartbeatUrl, parsed.Path, "syntheticHeartbeatSecret01"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error quotes %q: %v", secret, err)
		}
	}
	for _, write := range fake.recordedWrites() {
		if write.path == "/api/v1/provisioning/policies" {
			t.Fatalf("policy written after the heartbeat write failed: %+v", write)
		}
	}
}

// A non-https heartbeat is refused before any request, without quoting it,
// and so is a heartbeat receiver that carries an integration this reconcile
// does not manage, which would reach someone every 5 minutes.
func TestAlertRoutingRefusesAnUnsafeHeartbeat(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, slackRootContacts(), slackRootPolicy())
	for i, heartbeatUrl := range []string{
		"http://heartbeat.example/ping/plainSecret",
		"https://user:pass@heartbeat.example/ping/userinfoSecret",
		"heartbeat.example/ping/relativeSecret",
	} {
		_, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{WatchdogHeartbeatUrl: heartbeatUrl})
		if err == nil || strings.Contains(err.Error(), "Secret") {
			t.Errorf("heartbeat %d: error = %v", i, err)
		}
	}
	if _, _, requestCount := fake.snapshot(); requestCount != 0 {
		t.Fatalf("invalid heartbeats reached Grafana %d times", requestCount)
	}

	contacts := append(slackRootContacts(), grafanaContactPoint{
		Uid: "operator-email", Name: WatchdogAlertContactName, Type: "email", Settings: map[string]any{"addresses": "oncall@example.com"},
	})
	fake, server = newFakeAlertingGrafana(t, contacts, slackRootPolicy())
	_, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{WatchdogHeartbeatUrl: syntheticWatchdogHeartbeatUrl})
	if err == nil || !strings.Contains(err.Error(), "does not manage") {
		t.Fatalf("error = %v", err)
	}
	if writes := fake.recordedWrites(); len(writes) != 0 {
		t.Fatalf("writes = %+v", writes)
	}
}
