// Shared app-identity lookup failures must stay visible without raw paths.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Populate only dedicated credentials; each test supplies its app lookup state.
func writeProviderAppLookupCredentials(t *testing.T, root string) {
	t.Helper()
	for name, contents := range map[string]string{
		"google-play-reporting.json": `{"client_email":"reader@synthetic.example","private_key":"synthetic-private-key","private_key_id":"synthetic-key-id","token_uri":"https://oauth.synthetic.example/token"}`,
		"apple-reporting.yml":        "issuer_id: synthetic-issuer-id\nkey_id: synthetic-key-id\nprivate_key: synthetic-private-key\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal("could not write the synthetic dedicated credential")
		}
	}
}

// Follow the real configured-provider failure route, including public rendering.
func requireProviderAppLookupVisibility(t *testing.T, root string) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	settings.GooglePlay = loadGooglePlayReportingSettings()
	settings.AppleReporting = loadAppleReportingSettings()
	if !settings.GooglePlay.Enabled || settings.GooglePlay.LoadError == nil ||
		!settings.AppleReporting.Enabled || settings.AppleReporting.LoadError == nil {
		t.Error("missing configured app identity silently disarmed a provider")
	}
	alerts, err := NewWithSignals(settings,
		NewPlayCrashesSignal(), NewAppleCrashesSignal(), NewSubnetCoverageSignal(),
	).Run(context.Background())
	if err == nil || len(alerts) != 3 {
		t.Error("app-identity failure lost provider visibility or an independent finding")
	}
	for _, provider := range []struct {
		key      string
		number   string
		id       string
		resource string
	}{
		{key: "play-crashes", number: "20.1", id: "mobile/play-crashes", resource: "google.yml"},
		{key: "apple-crashes", number: "20.2", id: "mobile/apple-crashes", resource: "apple.yml"},
	} {
		found := false
		for _, alert := range alerts {
			if alert.SignalKey != provider.key {
				continue
			}
			found = true
			if alert.SignalNumber != provider.number || alert.SignalID != "monitor/visibility" ||
				alert.Class != providerAuthenticationClass || alert.Target != provider.id ||
				alert.Severity != SeverityWarn || alert.Sustain != 1 ||
				!strings.Contains(alert.Markdown(), provider.resource) {
				t.Errorf("%s lost its app-identity visibility contract", provider.key)
			}
		}
		if !found {
			t.Errorf("%s app-identity failure became a quiet provider", provider.key)
		}
	}
	foundIndependent := false
	for _, alert := range alerts {
		if alert.SignalKey == "subnet-coverage" && alert.Class == "subnet-monitor-coverage" {
			foundIndependent = true
		}
	}
	if !foundIndependent {
		t.Error("app-identity failure blocked an unrelated local signal")
	}
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil {
		t.Fatal("could not serialize the synthetic provider alerts")
	}
	for _, value := range []string{root, filepath.Dir(root), "synthetic-private-identity-target", "synthetic-private-key"} {
		if strings.Contains(string(encoded), value) {
			t.Error("public Alert retained private app-identity lookup detail")
		}
		if strings.Contains(alerts.Markdown(), value) {
			t.Error("public Markdown retained private app-identity lookup detail")
		}
		if err != nil && strings.Contains(err.Error(), value) {
			t.Error("public monitor error retained private app-identity lookup detail")
		}
	}
}

// Missing shared app identity differs from an absent optional reporting key.
func TestProviderAppIdentityLookupMissingRemainsVisible(t *testing.T) {
	root := providerCredentialLookupRoot(t)
	writeProviderAppLookupCredentials(t, root)
	for _, resource := range []string{"google.yml", "apple.yml"} {
		_, err := server.Vault.SimpleResource(resource)
		if !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
			t.Fatal("missing app fixture did not reach the not-found resolver branch")
		}
	}
	requireProviderAppLookupVisibility(t, root)
}

// Both unavailable filesystem states carry private paths before sanitization.
func TestProviderAppIdentityLookupUnavailableOmitsPrivatePaths(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		root := providerCredentialLookupRoot(t)
		writeProviderAppLookupCredentials(t, root)
		for _, resource := range []string{"google.yml", "apple.yml"} {
			path := filepath.Join(root, resource)
			var err error
			if dangling {
				err = os.Symlink("synthetic-private-identity-target", path)
			} else {
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal("could not create the unavailable synthetic app identity")
			}
			_, err = server.Vault.SimpleResource(resource)
			if !errors.Is(err, server.ErrResourceUnavailable) || errors.Is(err, server.ErrResourceNotFound) {
				t.Fatal("app fixture did not reach the unavailable resolver branch")
			}
		}
		requireProviderAppLookupVisibility(t, root)
	}
}
