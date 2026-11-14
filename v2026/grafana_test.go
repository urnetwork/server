package server

// Regression tests keep Grafana endpoint settings in config while requiring a
// role-scoped vault credential on every service metrics push.

import (
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"slices"
	"testing"
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
