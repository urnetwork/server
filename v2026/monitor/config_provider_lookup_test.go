// Exercise optional provider arming through real, isolated resource lookup.
package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// No installed Vault resource or in-memory resource override participates.
func providerCredentialLookupRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "synthetic-private-credential-root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal("could not create the isolated credential fixture")
	}
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "")
	return root
}

// Preserve both provider failures and an unrelated local signal's finding.
func requireProviderCredentialLookupVisibility(t *testing.T, root, reason string, privateValues ...string) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	settings.GooglePlay = loadGooglePlayReportingSettings()
	settings.AppleReporting = loadAppleReportingSettings()
	for _, provider := range []struct {
		resource string
		enabled  bool
		loadErr  error
	}{
		{resource: "google-play-reporting.json", enabled: settings.GooglePlay.Enabled, loadErr: settings.GooglePlay.LoadError},
		{resource: "apple-reporting.yml", enabled: settings.AppleReporting.Enabled, loadErr: settings.AppleReporting.LoadError},
	} {
		if !provider.enabled || provider.loadErr == nil {
			t.Errorf("%s lookup failure silently disarmed its provider", provider.resource)
		} else if provider.loadErr.Error() != provider.resource+" "+reason {
			t.Errorf("%s lookup failure lost its fixed sanitized reason", provider.resource)
		}
	}

	alerts, err := NewWithSignals(settings,
		NewPlayCrashesSignal(), NewAppleCrashesSignal(), NewSubnetCoverageSignal(),
	).Run(context.Background())
	if err == nil {
		t.Error("unavailable credentials did not retain a nonzero monitor result")
	}
	if len(alerts) != 3 {
		t.Error("provider failures did not coexist with the unrelated signal finding")
	}
	for _, provider := range []struct {
		key      string
		number   string
		id       string
		resource string
	}{
		{key: "play-crashes", number: "20.1", id: "mobile/play-crashes", resource: "google-play-reporting.json"},
		{key: "apple-crashes", number: "20.2", id: "mobile/apple-crashes", resource: "apple-reporting.yml"},
	} {
		found := false
		for _, alert := range alerts {
			if alert.SignalKey != provider.key {
				continue
			}
			found = true
			if alert.SignalNumber != provider.number || alert.SignalID != "monitor/visibility" ||
				alert.Class != providerAuthenticationClass || alert.Target != provider.id ||
				alert.Severity != SeverityWarn || alert.Sustain != 1 {
				t.Errorf("%s lost its provider visibility identity", provider.key)
			}
			if !strings.Contains(alert.Markdown(), provider.resource+" "+reason) {
				t.Errorf("%s Markdown lost the fixed lookup reason", provider.key)
			}
		}
		if !found {
			t.Errorf("%s lookup failure became a quiet provider", provider.key)
		}
	}
	foundIndependent := false
	for _, alert := range alerts {
		if alert.SignalKey == "subnet-coverage" && alert.Class == "subnet-monitor-coverage" {
			foundIndependent = true
		}
	}
	if !foundIndependent {
		t.Error("provider lookup failure blocked an unrelated signal")
	}
	privateValues = append(privateValues, root, filepath.Dir(root))
	for _, value := range privateValues {
		if strings.Contains(alerts.Markdown(), value) || (err != nil && strings.Contains(err.Error(), value)) {
			t.Error("provider visibility retained private fixture detail")
		}
	}
}

// Missing optional resources remain unarmed, not failed authentication.
func TestProviderCredentialLookupAbsentRemainsUnarmed(t *testing.T) {
	providerCredentialLookupRoot(t)
	for _, resource := range []string{"google-play-reporting.json", "apple-reporting.yml"} {
		_, err := server.Vault.SimpleResource(resource)
		if !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
			t.Fatal("absent fixture did not produce the resolver's not-found classification")
		}
	}
	settings := syntheticSettings(&syntheticSource{})
	settings.GooglePlay = loadGooglePlayReportingSettings()
	settings.AppleReporting = loadAppleReportingSettings()
	if settings.GooglePlay.Enabled || settings.GooglePlay.LoadError != nil ||
		settings.AppleReporting.Enabled || settings.AppleReporting.LoadError != nil {
		t.Fatal("genuinely absent optional credentials did not remain unarmed")
	}
	alerts, err := NewWithSignals(settings, NewPlayCrashesSignal(), NewAppleCrashesSignal()).Run(context.Background())
	if err != nil || len(alerts) != 0 {
		t.Fatal("absent optional credentials performed provider work or emitted a failure")
	}
}

// A regular resource still loads its dedicated key and separate app identity.
func TestProviderCredentialLookupRegularResourcesRemainEnabled(t *testing.T) {
	root := providerCredentialLookupRoot(t)
	for name, contents := range map[string]string{
		"google-play-reporting.json": `{"client_email":"reader@synthetic.example","private_key":"synthetic-private-key","private_key_id":"synthetic-key-id","token_uri":"https://oauth.synthetic.example/token"}`,
		"google.yml":                 "webhook:\n  package_name: example.synthetic.app\n",
		"apple-reporting.yml":        "issuer_id: synthetic-issuer-id\nkey_id: synthetic-key-id\nprivate_key: synthetic-private-key\n",
		"apple.yml":                  "app_store_notifications:\n  app_apple_id: 900000000000000001\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal("could not write a synthetic regular resource")
		}
	}
	play := loadGooglePlayReportingSettings()
	apple := loadAppleReportingSettings()
	if !play.Enabled || play.LoadError != nil || validateGooglePlayReportingSettings(play) != nil ||
		play.PackageName != "example.synthetic.app" || play.ClientEmail != "reader@synthetic.example" ||
		play.PrivateKey != "synthetic-private-key" || play.PrivateKeyID != "synthetic-key-id" ||
		play.TokenURL != "https://oauth.synthetic.example/token" {
		t.Error("regular Google resource did not retain its configured synthetic fields")
	}
	if !apple.Enabled || apple.LoadError != nil || validateAppleReportingSettings(apple) != nil ||
		apple.AppID != "900000000000000001" || apple.IssuerID != "synthetic-issuer-id" ||
		apple.KeyID != "synthetic-key-id" || apple.PrivateKey != "synthetic-private-key" {
		t.Error("regular Apple resource did not retain its configured synthetic fields")
	}
}

// A present directory is unavailable even though it has the optional basename.
func TestProviderCredentialLookupNonregularRemainsVisible(t *testing.T) {
	root := providerCredentialLookupRoot(t)
	for _, resource := range []string{"google-play-reporting.json", "apple-reporting.yml"} {
		if err := os.Mkdir(filepath.Join(root, resource), 0o700); err != nil {
			t.Fatal("could not create a synthetic nonregular credential")
		}
		_, err := server.Vault.SimpleResource(resource)
		if !errors.Is(err, server.ErrResourceUnavailable) || errors.Is(err, server.ErrResourceNotFound) {
			t.Fatal("nonregular fixture did not produce the resolver's unavailable classification")
		}
	}
	requireProviderCredentialLookupVisibility(t, root, "is unavailable")
}

// A broken symlink is a present unusable resource, not an absent opt-in.
func TestProviderCredentialLookupDanglingRemainsVisible(t *testing.T) {
	root := providerCredentialLookupRoot(t)
	const target = "synthetic-private-missing-credential-target"
	for _, resource := range []string{"google-play-reporting.json", "apple-reporting.yml"} {
		if err := os.Symlink(target, filepath.Join(root, resource)); err != nil {
			t.Fatal("could not create a synthetic dangling credential")
		}
		_, err := server.Vault.SimpleResource(resource)
		if !errors.Is(err, server.ErrResourceUnavailable) || errors.Is(err, server.ErrResourceNotFound) {
			t.Fatal("dangling fixture did not produce the resolver's unavailable classification")
		}
	}
	requireProviderCredentialLookupVisibility(t, root, "is unavailable", target)
}

// Readable malformed credentials keep the existing fixed diagnostic path.
func TestProviderCredentialLookupMalformedRemainsVisible(t *testing.T) {
	root := providerCredentialLookupRoot(t)
	const privateValue = "synthetic-unregistered-private-credential-detail"
	for _, resource := range []string{"google-play-reporting.json", "apple-reporting.yml"} {
		if err := os.WriteFile(filepath.Join(root, resource), []byte("private_key: ["+privateValue+"\n"), 0o600); err != nil {
			t.Fatal("could not write a synthetic malformed credential")
		}
		if _, err := server.Vault.SimpleResource(resource); err != nil {
			t.Fatal("malformed fixture failed lookup before reaching its decoder")
		}
	}
	requireProviderCredentialLookupVisibility(t, root, "is unreadable or malformed", privateValue)
}
