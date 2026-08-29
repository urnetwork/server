package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CompetitionAlertContactUid  = "sim-latency-support-email"
	CompetitionAlertContactName = "sim-latency-support"
	CompetitionIncidentContact  = "support@ur.xyz"
	alertRoutingResponseLimit   = 1024 * 1024
)

type grafanaProvisioningClient struct {
	baseUrl  string
	username string
	password string
	http     *http.Client
}

type grafanaContactPoint struct {
	Uid                   string         `json:"uid"`
	Name                  string         `json:"name"`
	Type                  string         `json:"type"`
	Settings              map[string]any `json:"settings"`
	DisableResolveMessage bool           `json:"disableResolveMessage"`
}

// ReconcileCompetitionAlertRouting adds one managed child route to the live
// notification-policy tree. It reads and preserves every unrelated route
// before replacing the tree through Grafana's atomic policy endpoint.
func ReconcileCompetitionAlertRouting(
	ctx context.Context,
	grafanaUrl string,
	username string,
	password string,
) error {
	client, err := newGrafanaProvisioningClient(grafanaUrl, username, password)
	if err != nil {
		return err
	}
	if err := client.reconcileCompetitionContact(ctx); err != nil {
		return err
	}
	return client.reconcileCompetitionPolicy(ctx)
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
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (self *grafanaProvisioningClient) reconcileCompetitionContact(ctx context.Context) error {
	status, body, err := self.request(ctx, http.MethodGet, "/api/v1/provisioning/contact-points", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("read Grafana contact points: HTTP %d", status)
	}
	contacts := []grafanaContactPoint{}
	if err := json.Unmarshal(body, &contacts); err != nil {
		return fmt.Errorf("decode Grafana contact points: %w", err)
	}
	desired := grafanaContactPoint{
		Uid:                   CompetitionAlertContactUid,
		Name:                  CompetitionAlertContactName,
		Type:                  "email",
		Settings:              map[string]any{"addresses": CompetitionIncidentContact},
		DisableResolveMessage: false,
	}
	found := false
	needsUpdate := false
	for _, contact := range contacts {
		if contact.Name == CompetitionAlertContactName && contact.Uid != CompetitionAlertContactUid {
			return fmt.Errorf("Grafana contact point name %q is owned by uid %q", CompetitionAlertContactName, contact.Uid)
		}
		if contact.Uid == CompetitionAlertContactUid {
			found = true
			needsUpdate = contact.Name != desired.Name || contact.Type != desired.Type ||
				contact.Settings["addresses"] != CompetitionIncidentContact || contact.DisableResolveMessage
		}
	}
	if found && !needsUpdate {
		return nil
	}
	method := http.MethodPost
	requestPath := "/api/v1/provisioning/contact-points"
	if found {
		method = http.MethodPut
		requestPath += "/" + CompetitionAlertContactUid
	}
	status, body, err = self.request(ctx, method, requestPath, desired)
	if err != nil {
		return err
	}
	if status < 200 || 300 <= status {
		return fmt.Errorf("write Grafana competition contact point: HTTP %d: %s", status, boundedErrorBody(body))
	}
	return nil
}

func (self *grafanaProvisioningClient) reconcileCompetitionPolicy(ctx context.Context) error {
	status, body, err := self.request(ctx, http.MethodGet, "/api/v1/provisioning/policies", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("read Grafana notification policy: HTTP %d", status)
	}
	policy := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&policy); err != nil {
		return fmt.Errorf("decode Grafana notification policy: %w", err)
	}
	if receiver, ok := policy["receiver"].(string); !ok || receiver == "" {
		return errors.New("Grafana notification policy has no default receiver")
	}
	routes := []any{}
	if value, ok := policy["routes"]; ok {
		var routesOk bool
		routes, routesOk = value.([]any)
		if !routesOk {
			return errors.New("Grafana notification policy routes are malformed")
		}
	}
	kept := make([]any, 0, len(routes)+1)
	for _, route := range routes {
		if !managedCompetitionRoute(route) {
			kept = append(kept, route)
		}
	}
	kept = append(kept, map[string]any{
		"receiver": CompetitionAlertContactName,
		"object_matchers": [][]string{
			{"service", "=", "sim-latency"},
			{"severity", "=~", "warn|page"},
		},
	})
	policy["routes"] = kept
	status, body, err = self.request(ctx, http.MethodPut, "/api/v1/provisioning/policies", policy)
	if err != nil {
		return err
	}
	if status < 200 || 300 <= status {
		return fmt.Errorf("write Grafana competition notification policy: HTTP %d: %s", status, boundedErrorBody(body))
	}
	return nil
}

func managedCompetitionRoute(value any) bool {
	route, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if receiver, _ := route["receiver"].(string); receiver == CompetitionAlertContactName {
		return true
	}
	matchers, ok := route["object_matchers"].([]any)
	if !ok {
		return false
	}
	for _, matcherValue := range matchers {
		matcher, ok := matcherValue.([]any)
		if !ok || len(matcher) != 3 {
			continue
		}
		name, _ := matcher[0].(string)
		operator, _ := matcher[1].(string)
		value, _ := matcher[2].(string)
		if name == "service" && operator == "=" && value == "sim-latency" {
			return true
		}
	}
	return false
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
