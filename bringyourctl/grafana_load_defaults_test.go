package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/grafana"
)

// A minimal Grafana for load-defaults: the folder exists, every dashboard
// upsert succeeds, no dashboard is shared yet, and every request to the
// alerting provisioning API is counted.
type grafanaLoadDefaultsFake struct {
	stateLock            sync.Mutex
	dashboardPosts       int
	provisioningRequests int
}

func newGrafanaLoadDefaultsFake(t *testing.T) (*grafanaLoadDefaultsFake, string) {
	fake := &grafanaLoadDefaultsFake{}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (self *grafanaLoadDefaultsFake) serve(response http.ResponseWriter, request *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, _ = io.ReadAll(request.Body)
	response.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(request.URL.Path, "/api/v1/provisioning/"):
		self.provisioningRequests += 1
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
		_, _ = io.WriteString(response, `{"status":"success"}`)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/public-dashboards/"):
		response.WriteHeader(http.StatusNotFound)
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/public-dashboards/"):
		_, _ = io.WriteString(response, `{"uid":"shared","accessToken":"token","isEnabled":true}`)
	default:
		response.WriteHeader(http.StatusNotFound)
	}
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
	if err := grafanaLoadDefaultsTo(context.Background(), io.Discard, fullUrl, "secret", true, grafana.AlertRoutingSettings{}); err != nil {
		t.Fatal(err)
	}
	if full.provisioningRequestCount() == 0 {
		t.Fatal("a full load did not reconcile the alert routing")
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
