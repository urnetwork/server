package monitor

import (
	"strings"
	"testing"

	server "github.com/urnetwork/server"
)

func TestGooglePlayReportingSettingsLoadDedicatedCredentialAndSharedAppIdentity(t *testing.T) {
	popCredential := server.Vault.PushSimpleResource("google-play-reporting.json", []byte(`{
  "client_email": "monitor@example.invalid",
  "private_key": "private-key",
  "private_key_id": "key-1",
  "token_uri": "https://oauth2.googleapis.com/token"
}`))
	defer popCredential()
	popApp := server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  package_name: com.example.app\n"))
	defer popApp()

	settings := loadGooglePlayReportingSettings()
	if !settings.Enabled || settings.LoadError != nil {
		t.Fatalf("settings enabled=%v error=%v", settings.Enabled, settings.LoadError)
	}
	if settings.PackageName != "com.example.app" || settings.ClientEmail != "monitor@example.invalid" || settings.PrivateKeyID != "key-1" {
		t.Fatalf("unexpected Google settings: %+v", settings)
	}
}

func TestAppleReportingSettingsLoadDedicatedCredentialAndSharedAppIdentity(t *testing.T) {
	popCredential := server.Vault.PushSimpleResource("apple-reporting.yml", []byte("issuer_id: issuer-1\nkey_id: key-1\nprivate_key: private-key\n"))
	defer popCredential()
	popApp := server.Vault.PushSimpleResource("apple.yml", []byte("app_store_notifications:\n  app_apple_id: 6741000606\n"))
	defer popApp()

	settings := loadAppleReportingSettings()
	if !settings.Enabled || settings.LoadError != nil {
		t.Fatalf("settings enabled=%v error=%v", settings.Enabled, settings.LoadError)
	}
	if settings.AppID != "6741000606" || settings.IssuerID != "issuer-1" || settings.KeyID != "key-1" {
		t.Fatalf("unexpected Apple settings: %+v", settings)
	}
}

func TestPresentMalformedProviderCredentialDoesNotSilentlyDisableProbe(t *testing.T) {
	popCredential := server.Vault.PushSimpleResource("apple-reporting.yml", []byte("issuer_id: [\n"))
	defer popCredential()
	settings := loadAppleReportingSettings()
	if !settings.Enabled || settings.LoadError == nil {
		t.Fatalf("malformed present credential enabled=%v error=%v", settings.Enabled, settings.LoadError)
	}
	if !strings.Contains(settings.LoadError.Error(), "apple-reporting.yml") {
		t.Fatalf("load error lacks resource identity: %v", settings.LoadError)
	}
}
