package grafana

// Alert routing for the env Grafana, reconciled through Grafana's provisioning
// API as the grafana admin (`bringyourctl grafana reconcile-alerting`, also run
// by `load-defaults`). The file-provisioned alert rules live in the warp repo
// (warp/grafana/alerting); contact points and the notification-policy tree
// live in the Grafana database, so writes here survive Grafana redeploys.
// Writes send X-Disable-Provenance so the routing stays editable in the UI,
// which is also the rollback path.
//
// Two pieces are managed:
//   - the canonical child route for service=sim-latency warn and page alerts,
//     through the existing native main receiver, never an unconfigured email
//     transport; the support address identifies incident ownership;
//   - the Slack default contact point, when vault monitor.yml carries
//     alerts.slack_webhook_url. It becomes the root receiver, so every alert
//     that matches no child route reaches Slack. Before it, the root routed to
//     a receiver without integrations, and no alert reached anyone.
//
// The webhook URL is a credential. It is sent only in the contact-point write,
// and Grafana returns it redacted. Grafana error text can quote a URL, so the
// exported functions redact every error they return once, at their boundary:
// of the webhook, its path, and anything shaped like a Slack webhook. Only
// then is the error bounded to alertRoutingErrorLimit bytes, so the bound
// cannot cut a secret in half. Inside the package, errors quote Grafana's
// body whole.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026"
)

const (
	// Historical email identities remain readable, but reconciliation neither
	// creates nor rewrites this contact point. Only its exact old route migrates.
	CompetitionAlertContactUid  = "sim-latency-support-email"
	CompetitionAlertContactName = "sim-latency-support"
	CompetitionIncidentContact  = "support@ur.xyz"
	// The contact point for every alert that no child route matches. The uid
	// names its single Slack integration; the name is the receiver.
	DefaultAlertContactUid    = "urnetwork-default-slack"
	DefaultAlertContactName   = "urnetwork-default"
	alertRoutingResponseLimit = 1024 * 1024
	// bytes of an error an exported function returns, after redaction
	alertRoutingErrorLimit = 1024
	// Grafana 13 removed the legacy receivers/test route (410). Contact-point
	// tests use the notifications app's receiver test subresource, addressed
	// by the receiver uid (the raw url-safe base64 of its name). Org 1 is the
	// default namespace.
	grafanaReceiversApiPath = "/apis/notifications.alerting.grafana.app/v1beta1/namespaces/default/receivers"
)

// Anything shaped like a Slack incoming webhook, for redaction of text that
// may quote one the caller did not configure.
var slackWebhookPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]*hooks\.slack(?:-gov)?\.com/[^\s"'<>]*`)

type grafanaProvisioningClient struct {
	baseUrl  string
	username string
	password string
	http     *http.Client
}

type grafanaContactPoint struct {
	Uid                   string          `json:"uid"`
	Name                  string          `json:"name"`
	Type                  string          `json:"type"`
	Settings              map[string]any  `json:"settings"`
	SecureFields          map[string]bool `json:"secureFields,omitempty"`
	DisableResolveMessage bool            `json:"disableResolveMessage"`
}

// Optional integrations for the reconcile. An empty SlackWebhookUrl leaves the
// root receiver as it is.
type AlertRoutingSettings struct {
	SlackWebhookUrl string
}

// The reconciled default route, without secrets.
type AlertRoutingResult struct {
	PreviousDefaultReceiver string
	DefaultReceiver         string
	// false when no contact point carries the default receiver's name. Grafana
	// then delivers every unrouted alert to a receiver without integrations,
	// which reaches no one.
	DefaultReceiverHasIntegrations bool
	SlackConfigured                bool
}

// Grafana's verdict on one test notification. Error is redacted.
type AlertTestResult struct {
	Status   string
	Duration string
	Error    string
}

// AlertRoutingSettingsFromMonitorVault reads the default alert webhook from a
// vault monitor.yml document: alerts.slack_webhook_url, the key the monitor's
// Slack notifier also reads (monitor/config.go). A decode failure names only
// line numbers, because YAML errors can quote document values.
func AlertRoutingSettingsFromMonitorVault(data []byte) (AlertRoutingSettings, error) {
	var document struct {
		Alerts struct {
			SlackWebhookUrl string `yaml:"slack_webhook_url"`
		} `yaml:"alerts"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		lines := regexp.MustCompile(`line \d+`).FindAllString(err.Error(), 8)
		if len(lines) == 0 {
			return AlertRoutingSettings{}, errors.New("vault monitor.yml is not valid YAML")
		}
		return AlertRoutingSettings{}, fmt.Errorf("vault monitor.yml is not valid YAML (%s)", strings.Join(lines, ", "))
	}
	return AlertRoutingSettings{SlackWebhookUrl: strings.TrimSpace(document.Alerts.SlackWebhookUrl)}, nil
}

// ReconcileAlertRouting converges the managed contact points and the live
// notification-policy tree. It reads and preserves every unrelated contact
// point and route, then replaces the tree through Grafana's atomic policy
// endpoint. Competition alerts use the existing configured native main Slack
// integration; unrelated and historical email contacts are left unchanged.
// With a Slack webhook, the Slack default contact point is written
// (always: Grafana redacts the stored URL, so a changed vault URL cannot be
// detected) and becomes the root receiver. Without one, the root receiver is
// unchanged and the result reports whether it has any integration. Without a
// configured native main integration, competition reconciliation refuses
// before policy mutation instead of creating an unusable email destination.
func ReconcileAlertRouting(
	ctx context.Context,
	grafanaUrl string,
	username string,
	password string,
	settings AlertRoutingSettings,
) (_ AlertRoutingResult, returnErr error) {
	slackWebhookUrl := strings.TrimSpace(settings.SlackWebhookUrl)
	defer func() {
		if returnErr != nil {
			returnErr = boundaryError(returnErr, slackWebhookUrl)
		}
	}()
	if slackWebhookUrl != "" {
		if err := validateSlackWebhookUrl(slackWebhookUrl); err != nil {
			return AlertRoutingResult{}, err
		}
	}
	client, err := newGrafanaProvisioningClient(grafanaUrl, username, password)
	if err != nil {
		return AlertRoutingResult{}, err
	}
	contacts, err := client.readContactPoints(ctx)
	if err != nil {
		return AlertRoutingResult{}, err
	}
	defaultReceiver := ""
	plannedContacts := contacts
	if slackWebhookUrl != "" {
		defaultReceiver = DefaultAlertContactName
		plannedContacts = make([]grafanaContactPoint, 0, len(contacts)+1)
		found := false
		for _, contact := range contacts {
			if contact.Uid == DefaultAlertContactUid {
				if found {
					return AlertRoutingResult{}, errors.New("Grafana managed main integration identity is duplicated")
				}
				found = true
				plannedContacts = append(plannedContacts, defaultSlackContact(slackWebhookUrl))
			} else {
				plannedContacts = append(plannedContacts, contact)
			}
		}
		if !found {
			plannedContacts = append(plannedContacts, defaultSlackContact(slackWebhookUrl))
		}
	} else if !configuredDefaultAlertContact(contacts) {
		return AlertRoutingResult{}, errors.New("competition alerts require the configured existing main Slack integration")
	}
	policy, previousDefaultReceiver, err := client.competitionPolicy(ctx, defaultReceiver)
	if err != nil {
		return AlertRoutingResult{}, err
	}
	contactsJson, err := json.Marshal(plannedContacts)
	if err != nil {
		return AlertRoutingResult{}, errors.New("Grafana planned integrations cannot be encoded")
	}
	policyJson, err := json.Marshal(policy)
	if err != nil {
		return AlertRoutingResult{}, errors.New("Grafana planned policy cannot be encoded")
	}
	if err := ValidateCompetitionAlertRouting(contactsJson, policyJson); err != nil {
		return AlertRoutingResult{}, err
	}
	if slackWebhookUrl != "" {
		if err := client.reconcileDefaultSlackContact(ctx, contacts, slackWebhookUrl); err != nil {
			return AlertRoutingResult{}, err
		}
	}
	if err := client.writeCompetitionPolicy(ctx, policy); err != nil {
		return AlertRoutingResult{}, err
	}
	result := AlertRoutingResult{
		PreviousDefaultReceiver: previousDefaultReceiver,
		DefaultReceiver:         previousDefaultReceiver,
		SlackConfigured:         slackWebhookUrl != "",
	}
	if result.SlackConfigured {
		result.DefaultReceiver = DefaultAlertContactName
		result.DefaultReceiverHasIntegrations = true
		return result, nil
	}
	for _, contact := range contacts {
		if contact.Name == result.DefaultReceiver {
			result.DefaultReceiverHasIntegrations = true
		}
	}
	return result, nil
}

// SendDefaultAlertTest asks Grafana to send one test notification through the
// default contact point's Slack integration. The request names the stored
// integration and marks its url as a stored secure field, so Grafana posts to
// the webhook it already holds and the credential never crosses this client.
// settings.SlackWebhookUrl, when set, is used only to redact Grafana's error
// text. A delivery failure is a "failure" result, not an error.
func SendDefaultAlertTest(
	ctx context.Context,
	grafanaUrl string,
	username string,
	password string,
	settings AlertRoutingSettings,
) (_ AlertTestResult, returnErr error) {
	slackWebhookUrl := strings.TrimSpace(settings.SlackWebhookUrl)
	defer func() {
		if returnErr != nil {
			returnErr = boundaryError(returnErr, slackWebhookUrl)
		}
	}()
	client, err := newGrafanaProvisioningClient(grafanaUrl, username, password)
	if err != nil {
		return AlertTestResult{}, err
	}
	integrationUid := DefaultAlertContactUid
	request := map[string]any{
		"integration": map[string]any{
			"uid":  integrationUid,
			"type": "slack",
			// empty selects the integration type's current schema version
			"version":      "",
			"settings":     map[string]any{},
			"secureFields": map[string]bool{"url": true},
		},
		"alert": map[string]any{
			"labels": map[string]string{
				"alertname": "UrnetworkDefaultContactTest",
				"severity":  "warn",
			},
			"annotations": map[string]string{
				"summary":     "Test notification for the urnetwork default contact point",
				"description": "Sent by bringyourctl grafana test-alert through Grafana's stored Slack integration. No action is needed.",
			},
		},
	}
	receiverUid := base64.RawURLEncoding.EncodeToString([]byte(DefaultAlertContactName))
	status, body, err := client.request(ctx, http.MethodPost, grafanaReceiversApiPath+"/"+receiverUid+"/test", request)
	if err != nil {
		return AlertTestResult{}, err
	}
	if status != http.StatusOK {
		return AlertTestResult{}, fmt.Errorf("test Grafana default contact point: HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	var response struct {
		Status   string  `json:"status"`
		Duration string  `json:"duration"`
		Error    *string `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return AlertTestResult{}, fmt.Errorf("decode Grafana contact point test: %w", err)
	}
	result := AlertTestResult{
		Status:   response.Status,
		Duration: response.Duration,
	}
	if response.Error != nil {
		result.Error = boundedErrorBody([]byte(redactSlackWebhook(*response.Error, slackWebhookUrl)))
	}
	if result.Status != "success" && result.Status != "failure" {
		return AlertTestResult{}, fmt.Errorf("Grafana contact point test returned status %q", boundedErrorBody([]byte(result.Status)))
	}
	return result, nil
}

func newGrafanaProvisioningClient(grafanaUrl string, username string, password string) (*grafanaProvisioningClient, error) {
	parsed, err := url.Parse(grafanaUrl)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("Grafana URL must be an absolute origin without userinfo or a path")
	}
	hostname := parsed.Hostname()
	address := net.ParseIP(hostname)
	if parsed.Scheme != "https" &&
		!(parsed.Scheme == "http" && (hostname == "localhost" || (address != nil && address.IsLoopback()))) {
		return nil, errors.New("Grafana URL requires HTTPS outside loopback tests")
	}
	if username == "" || password == "" {
		return nil, errors.New("Grafana administrator credentials are required")
	}
	return &grafanaProvisioningClient{
		baseUrl:  strings.TrimSuffix(parsed.String(), "/"),
		username: username,
		password: password,
		http: &http.Client{
			Transport: server.NewHttpTransport(),
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// The webhook must be an absolute https URL without userinfo. The error never
// quotes it, since url.Parse errors embed their input.
func validateSlackWebhookUrl(webhookUrl string) error {
	parsed, err := url.Parse(webhookUrl)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("alerts.slack_webhook_url must be an absolute https URL without userinfo")
	}
	return nil
}

// Removes the configured webhook, its path, and anything shaped like a Slack
// webhook from text that may leave this package.
func redactSlackWebhook(text string, webhookUrl string) string {
	if webhookUrl != "" {
		text = strings.ReplaceAll(text, webhookUrl, "[redacted]")
		if parsed, err := url.Parse(webhookUrl); err == nil && parsed.Path != "" && parsed.Path != "/" {
			text = strings.ReplaceAll(text, parsed.Path, "[redacted]")
		}
	}
	return slackWebhookPattern.ReplaceAllString(text, "[redacted]")
}

func (self *grafanaProvisioningClient) readContactPoints(ctx context.Context) ([]grafanaContactPoint, error) {
	status, body, err := self.request(ctx, http.MethodGet, "/api/v1/provisioning/contact-points", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("read Grafana contact points: HTTP %d", status)
	}
	contacts := []grafanaContactPoint{}
	if err := json.Unmarshal(body, &contacts); err != nil {
		return nil, fmt.Errorf("decode Grafana contact points: %w", err)
	}
	return contacts, nil
}

// Stored secure-field markers and nonempty redacted URLs prove configuration
// without loading a webhook or assigning an operator-owned integration.
func configuredDefaultAlertContact(contacts []grafanaContactPoint) bool {
	found := false
	configured := false
	for _, contact := range contacts {
		if contact.Uid != DefaultAlertContactUid {
			continue
		}
		if found || contact.Name != DefaultAlertContactName || contact.Type != "slack" {
			return false
		}
		found = true
		storedUrl, _ := contact.Settings["url"].(string)
		configured = strings.TrimSpace(storedUrl) != "" || contact.SecureFields["url"]
	}
	return found && configured
}

// Upserts the Slack integration of the default contact point. Resolve messages
// stay on, so a cleared alert is announced as well as a firing one. Only this
// uid is managed: another integration an operator adds under the same name is
// part of the same receiver and is kept.
func (self *grafanaProvisioningClient) reconcileDefaultSlackContact(ctx context.Context, contacts []grafanaContactPoint, slackWebhookUrl string) error {
	desired := defaultSlackContact(slackWebhookUrl)
	found := false
	for _, contact := range contacts {
		if contact.Uid == DefaultAlertContactUid {
			found = true
		}
	}
	method := http.MethodPost
	requestPath := "/api/v1/provisioning/contact-points"
	if found {
		method = http.MethodPut
		requestPath += "/" + DefaultAlertContactUid
	}
	status, body, err := self.request(ctx, method, requestPath, desired)
	if err != nil {
		return err
	}
	if status < 200 || 300 <= status {
		return fmt.Errorf("write Grafana default Slack contact point: HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Planning and the existing native upsert share exactly one managed integration shape.
func defaultSlackContact(slackWebhookUrl string) grafanaContactPoint {
	return grafanaContactPoint{
		Uid:                   DefaultAlertContactUid,
		Name:                  DefaultAlertContactName,
		Type:                  "slack",
		Settings:              map[string]any{"url": slackWebhookUrl},
		DisableResolveMessage: false,
	}
}

// Plans the managed change without mutations so reachability is proved before even a contact write.
func (self *grafanaProvisioningClient) competitionPolicy(ctx context.Context, defaultReceiver string) (map[string]any, string, error) {
	status, body, err := self.request(ctx, http.MethodGet, "/api/v1/provisioning/policies", nil)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("read Grafana notification policy: HTTP %d", status)
	}
	policy := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&policy); err != nil {
		return nil, "", fmt.Errorf("decode Grafana notification policy: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, "", errors.New("Grafana notification policy has trailing JSON")
	}
	previousDefaultReceiver, ok := policy["receiver"].(string)
	if !ok || previousDefaultReceiver == "" {
		return nil, "", errors.New("Grafana notification policy has no default receiver")
	}
	routes := []any{}
	if value, ok := policy["routes"]; ok {
		var routesOk bool
		routes, routesOk = value.([]any)
		if !routesOk {
			return nil, "", errors.New("Grafana notification policy routes are malformed")
		}
	}
	kept := make([]any, 0, len(routes)+1)
	for _, route := range routes {
		if !managedCompetitionRoute(route) {
			kept = append(kept, route)
		}
	}
	kept = append(kept, map[string]any{
		"receiver": DefaultAlertContactName,
		"object_matchers": [][]string{
			{"service", "=", "sim-latency"},
			{"severity", "=~", "warn|page"},
		},
	})
	policy["routes"] = kept
	if defaultReceiver != "" {
		policy["receiver"] = defaultReceiver
	}
	return policy, previousDefaultReceiver, nil
}

// Commits only a completely validated plan; caller validation also precedes any native contact upsert.
func (self *grafanaProvisioningClient) writeCompetitionPolicy(ctx context.Context, policy map[string]any) error {
	status, body, err := self.request(ctx, http.MethodPut, "/api/v1/provisioning/policies", policy)
	if err != nil {
		return err
	}
	if status < 200 || 300 <= status {
		return fmt.Errorf("write Grafana notification policy: HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Only exact canonical old-email/new-main signatures are controller-owned;
// extra matchers, grouping, timing, continuation or child routes are manual.
func managedCompetitionRoute(value any) bool {
	route, ok := value.(map[string]any)
	if !ok || len(route) != 2 {
		return false
	}
	receiver, _ := route["receiver"].(string)
	if receiver != CompetitionAlertContactName && receiver != DefaultAlertContactName {
		return false
	}
	matchers, ok := route["object_matchers"].([]any)
	if !ok || len(matchers) != 2 {
		return false
	}
	service, severity := false, false
	for _, matcherValue := range matchers {
		matcher, ok := matcherValue.([]any)
		if !ok || len(matcher) != 3 {
			return false
		}
		name, _ := matcher[0].(string)
		operator, _ := matcher[1].(string)
		value, _ := matcher[2].(string)
		switch {
		case !service && name == "service" && operator == "=" && value == "sim-latency":
			service = true
		case !severity && name == "severity" && operator == "=~" && value == "warn|page":
			severity = true
		default:
			return false
		}
	}
	return service && severity
}

func (self *grafanaProvisioningClient) request(ctx context.Context, method string, requestPath string, requestBody any) (int, []byte, error) {
	var bodyReader io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, self.baseUrl+requestPath, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	request.SetBasicAuth(self.username, self.password)
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Disable-Provenance", "true")
	}
	response, err := self.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, alertRoutingResponseLimit+1))
	if err != nil {
		return 0, nil, err
	}
	if alertRoutingResponseLimit < len(body) {
		return 0, nil, errors.New("Grafana provisioning response exceeded the size limit")
	}
	return response.StatusCode, body, nil
}

func boundedErrorBody(body []byte) string {
	const limit = 512
	message := strings.TrimSpace(string(body))
	if limit < len(message) {
		message = message[:limit]
	}
	return message
}

// An error leaving an exported function: redacted first, then bounded.
func boundaryError(err error, slackWebhookUrl string) error {
	message := redactSlackWebhook(err.Error(), slackWebhookUrl)
	if alertRoutingErrorLimit < len(message) {
		message = strings.ToValidUTF8(message[:alertRoutingErrorLimit], "")
	}
	return errors.New(message)
}
