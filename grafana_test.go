package server

// Regression tests keep Grafana endpoint settings in config while requiring a
// role-scoped vault credential on every service metrics push.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
