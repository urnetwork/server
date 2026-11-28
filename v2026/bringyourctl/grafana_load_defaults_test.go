// Dashboard-loading fixtures separate dashboard ownership from fail-closed native alert routing and never model an unconfigured receiver as a successful full load.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026/grafana"
)

// A minimal Grafana for load-defaults: the folder exists, every dashboard
// upsert succeeds, and every request to the alerting provisioning API is
// counted. Public dashboard shares are kept by dashboard uid: none exists
// until a test adds one or load-defaults publishes one. Dashboard upserts and
// share changes are logged in order, as "upsert <uid>", "publish <uid>" and
// "delete <uid> <share uid>".
type grafanaLoadDefaultsFake struct {
	stateLock            sync.Mutex
	dashboardPosts       int
	provisioningRequests int
	provisioningWrites   int
	shares               map[string]grafanaLoadDefaultsShare
	events               []string
}

// A public dashboard share, in the fields of Grafana's public dashboard api.
type grafanaLoadDefaultsShare struct {
	Uid         string `json:"uid"`
	AccessToken string `json:"accessToken"`
	IsEnabled   bool   `json:"isEnabled"`
}

func newGrafanaLoadDefaultsFake(t *testing.T) (*grafanaLoadDefaultsFake, string) {
	fake := &grafanaLoadDefaultsFake{
		shares: map[string]grafanaLoadDefaultsShare{},
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (self *grafanaLoadDefaultsFake) serve(response http.ResponseWriter, request *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	body, _ := io.ReadAll(request.Body)
	response.Header().Set("Content-Type", "application/json")
	// /api/dashboards/uid/<uid>/public-dashboards/[<share uid>]
	sharePath := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	shareRequest := 5 <= len(sharePath) && sharePath[0] == "api" && sharePath[1] == "dashboards" && sharePath[2] == "uid" && sharePath[4] == "public-dashboards"
	switch {
	case strings.HasPrefix(request.URL.Path, "/api/v1/provisioning/"):
		self.provisioningRequests += 1
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			self.provisioningWrites += 1
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/contact-points":
			_, _ = io.WriteString(response, `[]`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/policies":
			_, _ = io.WriteString(response, `{"receiver":"empty","routes":[]}`)
		default:
			response.WriteHeader(http.StatusAccepted)
		}
	case request.Method == http.MethodGet && request.URL.Path == "/api/folders/"+grafana.FolderUid:
		_, _ = io.WriteString(response, `{"uid":"`+grafana.FolderUid+`"}`)
	case request.Method == http.MethodPost && request.URL.Path == "/api/dashboards/db":
		self.dashboardPosts += 1
		var upsert struct {
			Dashboard struct {
				Uid string `json:"uid"`
			} `json:"dashboard"`
		}
		_ = json.Unmarshal(body, &upsert)
		self.events = append(self.events, "upsert "+upsert.Dashboard.Uid)
		_, _ = io.WriteString(response, `{"status":"success"}`)
	case shareRequest && request.Method == http.MethodGet:
		share, ok := self.shares[sharePath[3]]
		if !ok {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		shareJson, _ := json.Marshal(share)
		_, _ = response.Write(shareJson)
	case shareRequest && request.Method == http.MethodPost:
		share := grafanaLoadDefaultsShare{
			Uid:         "synthetic-share-" + sharePath[3],
			AccessToken: fmt.Sprintf("syntheticToken%02d", len(self.events)),
			IsEnabled:   true,
		}
		self.shares[sharePath[3]] = share
		self.events = append(self.events, "publish "+sharePath[3])
		shareJson, _ := json.Marshal(share)
		_, _ = response.Write(shareJson)
	case shareRequest && request.Method == http.MethodDelete && len(sharePath) == 6:
		share, ok := self.shares[sharePath[3]]
		if !ok || share.Uid != sharePath[5] {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		delete(self.shares, sharePath[3])
		self.events = append(self.events, "delete "+sharePath[3]+" "+sharePath[5])
		_, _ = io.WriteString(response, `{}`)
	default:
		response.WriteHeader(http.StatusNotFound)
	}
}

func (self *grafanaLoadDefaultsFake) eventLog() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

func (self *grafanaLoadDefaultsFake) share(dashboardUid string) (grafanaLoadDefaultsShare, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	share, ok := self.shares[dashboardUid]
	return share, ok
}

func (self *grafanaLoadDefaultsFake) dashboardPostCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.dashboardPosts
}

func (self *grafanaLoadDefaultsFake) provisioningRequestCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provisioningRequests
}

// Dashboard upserts are deliberately separate from notification-policy and contact mutations.
func (self *grafanaLoadDefaultsFake) provisioningWriteCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provisioningWrites
}

// A dashboards-only load upserts every default dashboard and sends nothing to
// the alerting provisioning API, so the alert routing another owner manages is
// left as it is. A full load still reconciles the routing.
func TestGrafanaLoadDefaultsDashboardsOnlyLeavesAlertRoutingUntouched(t *testing.T) {
	dashboardsOnly, dashboardsOnlyUrl := newGrafanaLoadDefaultsFake(t)
	var out bytes.Buffer
	if err := grafanaLoadDefaultsTo(context.Background(), &out, dashboardsOnlyUrl, "secret", false, grafana.AlertRoutingSettings{}); err != nil {
		t.Fatal(err)
	}
	if dashboardsOnly.dashboardPostCount() == 0 {
		t.Fatal("a dashboards-only load upserted no dashboards")
	}
	if got := dashboardsOnly.provisioningRequestCount(); got != 0 {
		t.Fatalf("a dashboards-only load sent %d requests to the alerting provisioning API", got)
	}
	if !strings.Contains(out.String(), "Alert routing left unchanged") {
		t.Fatalf("dashboards-only output = %q", out.String())
	}

	full, fullUrl := newGrafanaLoadDefaultsFake(t)
	if err := grafanaLoadDefaultsTo(context.Background(), io.Discard, fullUrl, "secret", true, grafana.AlertRoutingSettings{SlackWebhookUrl: "https://hooks.slack.example/services/synthetic-fixture-only"}); err != nil {
		t.Fatal(err)
	}
	if full.provisioningRequestCount() == 0 {
		t.Fatal("a full load did not reconcile the alert routing")
	}
	if got := full.provisioningWriteCount(); got != 2 {
		t.Fatalf("configured full load alerting writes = %d, want one contact and one policy", got)
	}
}

// An unconfigured full load can update dashboards before routing refuses, but cannot mutate a contact or policy or report routing success.
func TestGrafanaLoadDefaultsUnconfiguredRoutingRefusesPolicyWrites(t *testing.T) {
	fake, grafanaUrl := newGrafanaLoadDefaultsFake(t)
	var out bytes.Buffer
	if err := grafanaLoadDefaultsTo(context.Background(), &out, grafanaUrl, "secret", true, grafana.AlertRoutingSettings{}); err == nil {
		t.Fatal("unconfigured full-load routing was accepted")
	}
	if fake.dashboardPostCount() == 0 {
		t.Fatal("fixture did not reach routing after the existing dashboard-upsert phase")
	}
	if got := fake.provisioningWriteCount(); got != 0 {
		t.Fatalf("unconfigured full load mutated alerting %d times", got)
	}
	if strings.Contains(out.String(), "Competition warn/page") {
		t.Fatalf("unconfigured full load reported routing success: %q", out.String())
	}
}

// load-defaults accepts --dashboards_only.
func TestGrafanaLoadDefaultsUsageAcceptsDashboardsOnly(t *testing.T) {
	opts, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, []string{"grafana", "load-defaults", "--dashboards_only"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if dashboardsOnly, _ := opts.Bool("--dashboards_only"); !dashboardsOnly {
		t.Fatal("--dashboards_only was not parsed")
	}
}

// The case seen on main on 2026-11-09: the internal proxy test dashboard, which
// has no public tag, had been shared outside load-defaults (in the Grafana ui),
// so the public directory listed it. The next `load-defaults --dashboards_only`
// deletes that share before it uploads the dashboard, publishes the shared
// copy, and prints the copy's link as unlisted, apart from the listed public
// dashboards and the directory.
func TestGrafanaLoadDefaultsDashboardsOnlyUnpublishesAnUntaggedShareAndPrintsTheUnlistedLink(t *testing.T) {
	fake, grafanaUrl := newGrafanaLoadDefaultsFake(t)
	fake.shares["urnetwork-proxy-test"] = grafanaLoadDefaultsShare{Uid: "synthetic-ui-share", AccessToken: "syntheticUiShareToken01", IsEnabled: true}
	var out bytes.Buffer
	if err := grafanaLoadDefaultsTo(context.Background(), &out, grafanaUrl, "secret", false, grafana.AlertRoutingSettings{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.provisioningRequestCount(); got != 0 {
		t.Fatalf("a dashboards-only load sent %d requests to the alerting provisioning API", got)
	}

	events := fake.eventLog()
	deleted := slices.Index(events, "delete urnetwork-proxy-test synthetic-ui-share")
	uploaded := slices.Index(events, "upsert urnetwork-proxy-test")
	if deleted < 0 || uploaded < deleted {
		t.Fatalf("the untagged dashboard's share was not deleted before its upload: %q", events)
	}
	if _, ok := fake.share("urnetwork-proxy-test"); ok {
		t.Fatal("the untagged dashboard is still shared")
	}
	sharedUploaded := slices.Index(events, "upsert urnetwork-proxy-test-shared")
	sharedPublished := slices.Index(events, "publish urnetwork-proxy-test-shared")
	shared, ok := fake.share("urnetwork-proxy-test-shared")
	if sharedUploaded < 0 || sharedPublished < sharedUploaded || !ok || !shared.IsEnabled {
		t.Fatalf("the shared copy was not published after its upload: %q", events)
	}
	stats, ok := fake.share("urnetwork-public-traffic")
	if !ok {
		t.Fatal("the network stats dashboard was not published")
	}

	output := out.String()
	unlistedAt := strings.Index(output, "\nUnlisted public dashboards (read-only, no login; not in "+grafanaUrl+"/stats or /stats.json")
	if unlistedAt < 0 {
		t.Fatalf("no unlisted section in the output:\n%s", output)
	}
	listedSection, unlistedSection := output[:unlistedAt], output[unlistedAt:]
	if !strings.Contains(unlistedSection, "\n- urnetwork / proxy test (shared) (unlisted)\n    "+grafanaUrl+"/public-dashboards/"+shared.AccessToken+"\n") {
		t.Errorf("the unlisted section does not give the shared copy's link:\n%s", output)
	}
	if !strings.Contains(listedSection, "\nPublic dashboards (read-only, no login):\n- urnetwork / network stats\n    "+grafanaUrl+"/public-dashboards/"+stats.AccessToken+"\n") ||
		!strings.Contains(listedSection, "\nPublic directory: "+grafanaUrl+"/stats\n") {
		t.Errorf("the listed section does not give the network stats link and the directory:\n%s", output)
	}
	if strings.Contains(listedSection, shared.AccessToken) || strings.Contains(unlistedSection, stats.AccessToken) {
		t.Errorf("a link is printed in the other section:\n%s", output)
	}
	if strings.Contains(output, "syntheticUiShareToken01") {
		t.Errorf("the deleted share's link is printed:\n%s", output)
	}
}
