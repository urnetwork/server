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

// A service's own push file comes first, and a vault grafana.yml beside it is
// not read: a new block mounts only grafana-push-<service>.yml
// (plan-OPS-small-readings #5).
func TestGrafanaPushSettingsReadsTheServicesPushFile(t *testing.T) {
	t.Setenv("WARP_SERVICE", "evs")
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
local_port: 3111
users:
  - name: push-evs
    roles: [push]
  - name: writer
    roles: [push]
`))
	defer popConfig()
	popPush := Vault.PushSimpleResource("grafana-push-evs.yml", []byte(`
users:
  - name: push-evs
    password: evs-secret
`))
	defer popPush()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: writer
    password: shared-secret
`))
	defer popVault()

	localPort, username, password, err := grafanaPushSettings()
	if err != nil {
		t.Fatal(err)
	}
	if localPort != 3111 || username != "push-evs" || password != "evs-secret" {
		t.Fatalf("settings = %d %q %q", localPort, username, password)
	}
}

// Without a push file of its own, a service reads vault's grafana.yml as
// existing services do.
func TestGrafanaPushSettingsFallsBackToGrafanaYmlForExistingServices(t *testing.T) {
	t.Setenv("WARP_SERVICE", "api")
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: writer
    roles: [push]
`))
	defer popConfig()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: writer
    password: secret
`))
	defer popVault()

	_, username, password, err := grafanaPushSettings()
	if err != nil {
		t.Fatal(err)
	}
	if username != "writer" || password != "secret" {
		t.Fatalf("settings = %q %q", username, password)
	}
}

// A push file that does not hold exactly one push user fails, and never falls
// back to grafana.yml's credential.
func TestGrafanaPushSettingsRefusesABadPushFile(t *testing.T) {
	t.Setenv("WARP_SERVICE", "gkm")
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: push-gkm
    roles: [push]
  - name: reader
    roles: [query]
  - name: writer
    roles: [push]
`))
	defer popConfig()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: writer
    password: shared-secret
`))
	defer popVault()

	for name, pushFile := range map[string]string{
		"two users": `
users:
  - name: push-gkm
    password: one
  - name: writer
    password: two
`,
		"no users": `
users: []
`,
		"no password": `
users:
  - name: push-gkm
`,
		"its own roles": `
users:
  - name: push-gkm
    password: secret
    roles: [push]
`,
		"a user config gives no push role": `
users:
  - name: reader
    password: secret
`,
		"a user config does not name": `
users:
  - name: stranger
    password: secret
`,
	} {
		popPush := Vault.PushSimpleResource("grafana-push-gkm.yml", []byte(pushFile))
		if _, username, _, err := grafanaPushSettings(); err == nil {
			t.Errorf("%s: pushed as %q", name, username)
		}
		popPush()
	}
}

// A service name that could leave the vault's directory names no push file.
func TestGrafanaPushSettingsRefusesAServiceNameThatIsNoFileName(t *testing.T) {
	t.Setenv("WARP_SERVICE", "../evs")
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: writer
    roles: [push]
`))
	defer popConfig()
	popVault := Vault.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: writer
    password: secret
`))
	defer popVault()
	if _, _, _, err := grafanaPushSettings(); err == nil {
		t.Fatal("a service name with a path pushed")
	}
}

// StatsPushUnavailable says why nothing would be pushed: no warp identity, then
// no push credential, and nil once both are there.
func TestStatsPushUnavailableNamesWhy(t *testing.T) {
	t.Setenv("WARP_ENV", "")
	if err := StatsPushUnavailable(); err == nil {
		t.Fatal("a process outside warp would push")
	}
	t.Setenv("WARP_ENV", "test")
	t.Setenv("WARP_SERVICE", "sockets")
	t.Setenv("WARP_BLOCK", "g1-1")
	t.Setenv("WARP_HOST", "host-1")
	popConfig := Config.PushSimpleResource("grafana.yml", []byte(`
users:
  - name: push-sockets
    roles: [push]
`))
	defer popConfig()
	if err := StatsPushUnavailable(); err == nil {
		t.Fatal("a process with no push credential would push")
	}
	popPush := Vault.PushSimpleResource("grafana-push-sockets.yml", []byte(`
users:
  - name: push-sockets
    password: secret
`))
	defer popPush()
	if err := StatsPushUnavailable(); err != nil {
		t.Fatal(err)
	}
}
