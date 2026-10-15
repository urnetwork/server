package grafana

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A synthetic webhook. Its path stands in for the secret part of a real one.
const syntheticSlackWebhookUrl = "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/syntheticWebhookSecret01"

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
	// status and body forced on writes to the default Slack contact point
	slackContactStatus int
	slackContactBody   string
	// status and body forced on writes to the competition contact point
	competitionContactStatus int
	competitionContactBody   string
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
			if contact.Type == "slack" {
				settings["url"] = "[REDACTED]"
			}
			contact.Settings = settings
			listed = append(listed, contact)
		}
		encode(listed)
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/provisioning/contact-points":
		contact := decodeContact()
		if contact.Uid == DefaultAlertContactUid && self.slackContactStatus != 0 {
			response.WriteHeader(self.slackContactStatus)
			_, _ = io.WriteString(response, self.slackContactBody)
			return
		}
		if contact.Uid == CompetitionAlertContactUid && self.competitionContactStatus != 0 {
			response.WriteHeader(self.competitionContactStatus)
			_, _ = io.WriteString(response, self.competitionContactBody)
			return
		}
		self.contacts = append(self.contacts, contact)
		response.WriteHeader(http.StatusAccepted)
	case request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/api/v1/provisioning/contact-points/"):
		contact := decodeContact()
		uid := strings.TrimPrefix(request.URL.Path, "/api/v1/provisioning/contact-points/")
		if uid == DefaultAlertContactUid && self.slackContactStatus != 0 {
			response.WriteHeader(self.slackContactStatus)
			_, _ = io.WriteString(response, self.slackContactBody)
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
		// Grafana refuses a tree that names a receiver it does not have
		receiver, _ := policy["receiver"].(string)
		if receiver != "empty" && !self.hasReceiverWithLock(receiver) {
			response.WriteHeader(http.StatusBadRequest)
			return
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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.slackContactStatus = status
	self.slackContactBody = body
}

func (self *fakeAlertingGrafana) failCompetitionContactWrites(status int, body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.competitionContactStatus = status
	self.competitionContactBody = body
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

// The live Main shape on 2026-10-13: the root routes to "empty", which has no
// integrations (Grafana lists no contact point for it).
func unroutedToNobodyPolicy() map[string]any {
	return map[string]any{
		"receiver": "empty",
		"group_by": []any{"grafana_folder", "alertname"},
		"routes": []any{
			map[string]any{
				"receiver":        "team-core",
				"object_matchers": []any{[]any{"team", "=", "core"}},
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
	}
	policy := map[string]any{
		"receiver": "main-on-call",
		"group_by": []any{"grafana_folder", "alertname"},
		"routes": []any{
			map[string]any{
				"receiver":        "main-on-call",
				"object_matchers": []any{[]any{"team", "=", "core"}},
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
	if contactWrites != 1 || policyWrites != 2 {
		t.Fatalf("contact writes = %d, policy writes = %d", contactWrites, policyWrites)
	}
	storedContacts, storedPolicy, _ := fake.snapshot()
	if len(storedContacts) != 2 || storedContacts[1].Uid != CompetitionAlertContactUid ||
		storedContacts[1].Settings["addresses"] != CompetitionIncidentContact {
		t.Fatalf("contacts = %+v", storedContacts)
	}
	routes, ok := storedPolicy["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("policy routes = %#v", storedPolicy["routes"])
	}
	unrelated, _ := routes[0].(map[string]any)
	managed, _ := routes[1].(map[string]any)
	if unrelated["receiver"] != "main-on-call" || managed["receiver"] != CompetitionAlertContactName ||
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

func TestCompetitionAlertRoutingRejectsOwnedNameCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`[{"uid":"someone-else","name":"sim-latency-support","type":"email","settings":{"addresses":"other@example.com"}}]`))
	}))
	defer server.Close()
	if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{}); err == nil {
		t.Fatal("contact-point name collision was accepted")
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
		string(routes) != `[{"object_matchers":[["team","=","core"]],"receiver":"team-core"},{"object_matchers":[["service","=","sim-latency"],["severity","=~","warn|page"]],"receiver":"sim-latency-support"}]` {
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
	fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())

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
// whichever Grafana write failed: the competition contact point and policy
// writes quote Grafana's body as well. Redaction comes before the bound, so a
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
		{name: "competition contact point", fail: func(fake *fakeAlertingGrafana) {
			fake.failCompetitionContactWrites(http.StatusBadRequest, quoting)
		}},
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
	settings, err := AlertRoutingSettingsFromMonitorVault([]byte("ssh:\n  user: monitor\nalerts:\n  slack_webhook_url: \" " + syntheticSlackWebhookUrl + " \"\n"))
	if err != nil || settings.SlackWebhookUrl != syntheticSlackWebhookUrl {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
	for _, document := range []string{"", "ssh:\n  user: monitor\n", "alerts:\n  slack_webhook_url: \"\"\n"} {
		settings, err := AlertRoutingSettingsFromMonitorVault([]byte(document))
		if err != nil || settings.SlackWebhookUrl != "" {
			t.Errorf("document %q: settings = %+v, err = %v", document, settings, err)
		}
	}
	// a value under the wrong shape must not be quoted back
	_, err = AlertRoutingSettingsFromMonitorVault([]byte("alerts: " + syntheticSlackWebhookUrl + "\n"))
	if err == nil || strings.Contains(err.Error(), "syntheticWebhookSecret01") || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("malformed document error = %v", err)
	}
}
