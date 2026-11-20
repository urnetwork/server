package server

// Regression tests keep Grafana endpoint settings in config while requiring a
// role-scoped vault credential on every service metrics push.

import (
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestSourceBuildInfoFromSettings(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "-buildmode", Value: "exe"},
		{Key: "vcs.revision", Value: "dc40916a2c6c6e576d77f29aef8634fa45be5a8f"},
		{Key: "vcs.modified", Value: "false"},
	}
	info, ok := sourceBuildInfoFromSettings(settings)
	if !ok {
		t.Fatal("complete Go VCS settings were rejected")
	}
	if info.revision != "dc40916a2c6c6e576d77f29aef8634fa45be5a8f" || info.modified {
		t.Fatalf("source info = %+v", info)
	}
	labels := sourceInfoLabelValues(
		info,
		"  sha256:042255119828a004024a4dc5e57d97373a8bf399aca6074ca98804dec2b3156a  ",
	)
	expectedLabels := []string{
		"dc40916a2c6c6e576d77f29aef8634fa45be5a8f",
		"false",
		"sha256:042255119828a004024a4dc5e57d97373a8bf399aca6074ca98804dec2b3156a",
	}
	if !slices.Equal(labels, expectedLabels) {
		t.Fatalf("source labels = %q, want %q", labels, expectedLabels)
	}

	for _, incomplete := range [][]debug.BuildSetting{
		{{Key: "vcs.modified", Value: "false"}},
		{{Key: "vcs.revision", Value: "dc40916a"}},
		{{Key: "vcs.revision", Value: "dc40916a"}, {Key: "vcs.modified", Value: "not-a-bool"}},
	} {
		if info, ok := sourceBuildInfoFromSettings(incomplete); ok {
			t.Fatalf("incomplete settings produced source info %+v", info)
		}
	}
}

// The port comes only from config even if the scoped secret repeats a value.
func TestGrafanaPushSettingsSeparatesConfigAndSecret(t *testing.T) {
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
local_port: 3111
users:
  - name: writer
    roles: [push]
`))
	defer popConfig()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
local_port: 9999
users:
  - name: writer
    password: secret
`))
	defer popVault()

	localPort, username, password, err := grafanaPushSettings()
	if err != nil {
		t.Fatal(err)
	}
	if localPort != 3111 || username != "writer" || password != "secret" {
		t.Fatalf("settings = %d %q %q", localPort, username, password)
	}
}

// A query-only credential cannot silently authorize ingestion.
func TestGrafanaPushSettingsRequiresPushRole(t *testing.T) {
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
local_port: 3111
users:
  - name: reader
    roles: [query]
`))
	defer popConfig()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: reader
    password: secret
    roles: [push]
`))
	defer popVault()

	if _, _, _, err := grafanaPushSettings(); err == nil {
		t.Fatal("expected a missing push credential to fail")
	}
}

// A vault-only identity cannot add itself to the ingestion policy.
func TestGrafanaPushSettingsRejectsSecretOnlyUser(t *testing.T) {
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: reader
    roles: [query]
`))
	defer popConfig()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: reader
    password: reader-secret
  - name: attacker
    password: attacker-secret
`))
	defer popVault()

	if _, _, _, err := grafanaPushSettings(); err == nil {
		t.Fatal("expected a vault-only identity to fail")
	}
}

// The real Prometheus pusher sends HTTP basic auth, reproducing the formerly
// unauthenticated local-publisher request path.
func TestNewStatsPusherSendsBasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "writer" || password != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	if err := newStatsPusher(server.URL, "api", "writer", "secret").Push(); err != nil {
		t.Fatal(err)
	}
}

// A metric with a label of a reserved name makes the push client refuse the
// whole push of its process, as the lb_alt front's request counter, labeled
// block, stopped every metric of connect on its first requests. Other names
// push.
func TestStatsPushRefusesReservedLabelNames(t *testing.T) {
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer pushServer.Close()

	pushLabeled := func(labelName string) error {
		registry := prometheus.NewRegistry()
		counter := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "stats_push_label_test_total",
			Help: "A counter with one label, pushed under the process grouping",
		}, []string{labelName})
		registry.MustRegister(counter)
		counter.WithLabelValues("synthetic").Inc()
		return newGroupedStatsPusher(
			pushServer.URL,
			"env-test",
			"service-test",
			"block-test",
			"host-test",
			"instance-test",
			"writer",
			"secret",
		).Gatherer(registry).Push()
	}

	for _, labelName := range StatsPushReservedLabelNames() {
		if err := pushLabeled(labelName); err == nil {
			t.Errorf("a metric labeled %s pushed; the push client should refuse it", labelName)
		}
	}
	for _, labelName := range []string{"upstream_service", "upstream_block"} {
		if err := pushLabeled(labelName); err != nil {
			t.Errorf("a metric labeled %s did not push: %s", labelName, err)
		}
	}
}

// The reserved names are exactly the job and the grouping labels the process
// pusher keys a push by, in order, so the list can't drift from the grouping.
func TestStatsPushReservedLabelNamesMatchGrouping(t *testing.T) {
	pushPaths := make(chan string, 1)
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case pushPaths <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer pushServer.Close()

	err := newGroupedStatsPusher(
		pushServer.URL,
		"env-test",
		"service-test",
		"block-test",
		"host-test",
		"instance-test",
		"writer",
		"secret",
	).Gatherer(prometheus.NewRegistry()).Push()
	if err != nil {
		t.Fatal(err)
	}

	// /metrics/job/<job>/env/<env>/..., name and value in turn
	pushPath := <-pushPaths
	pathParts := strings.Split(strings.TrimPrefix(pushPath, "/metrics/"), "/")
	labelNames := []string{}
	for i := 0; i+1 < len(pathParts); i += 2 {
		labelNames = append(labelNames, pathParts[i])
	}
	if !slices.Equal(labelNames, StatsPushReservedLabelNames()) {
		t.Fatalf("push keyed by %v, reserved names %v", labelNames, StatsPushReservedLabelNames())
	}
}
