// Only synthetic credentials exercise native updater conversion and cleanup.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticMaxMindYaml = "account_id: 42\nlicense_key: SYNTHETIC_TEST_ONLY\nedition_ids: [GeoLite2-City]\n"

func TestMaxMindCredentialReadIsBoundedAndRedacted(t *testing.T) {
	input := strings.NewReader(strings.Repeat("x", int(maximumMaxMindCredentialBytes)*2))
	before := input.Len()
	credential, err := decodeMaxMindCredentials(input)
	if err == nil || credential.LicenseKey != "" || before-input.Len() != int(maximumMaxMindCredentialBytes)+1 {
		t.Fatal("MaxMind decoder exceeded its read bound or retained an invalid credential")
	}
	for _, document := range []string{
		"", "account_id: SYNTHETIC_SECRET\n",
		"account_id: 0\nlicense_key: SYNTHETIC_SECRET\nedition_ids: [GeoLite2-City]\n",
		"account_id: 42\nlicense_key: [SYNTHETIC_SECRET]\nedition_ids: [GeoLite2-City]\n",
		"account_id: 42\nlicense_key: SYNTHETIC_SECRET\nedition_ids: [GeoLite2-ASN]\n",
		"account_id: 42\nlicense_key: \"SYNTHETIC_SECRET\\nDatabaseDirectory /synthetic\"\nedition_ids: [GeoLite2-City]\n",
		"account_id: 42\nlicense_key: SYNTHETIC_SECRET\nedition_ids: [GeoLite2-City, GeoLite2-City]\n",
		syntheticMaxMindYaml + "unknown: SYNTHETIC_SECRET\n",
		syntheticMaxMindYaml + "---\n" + syntheticMaxMindYaml,
	} {
		credential, err := decodeMaxMindCredentials(strings.NewReader(document))
		if err == nil || credential.LicenseKey != "" || strings.Contains(err.Error(), "SYNTHETIC") {
			t.Fatal("invalid MaxMind credential was accepted or exposed in diagnostics")
		}
	}
}

func checkMaxMindTemporaryConfig(t *testing.T, callbackError bool) {
	t.Helper()
	input := writeTestInput(t, filepath.Join(t.TempDir(), "mm-geoip.yml"), []byte(syntheticMaxMindYaml))
	nativePath := ""
	err := withMaxMindNativeConfig(t.Context(), input, func(path string) error {
		nativePath = path
		if path == input {
			t.Fatal("Vault YAML was passed directly to the native updater")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("temporary updater secret was not owner-only")
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "AccountID 42\nLicenseKey SYNTHETIC_TEST_ONLY\nEditionIDs GeoLite2-City\n" {
			t.Fatal("Vault-to-updater conversion changed credential semantics")
		}
		if callbackError {
			return errors.New("synthetic updater echoed SYNTHETIC_TEST_ONLY")
		}
		return nil
	})
	if callbackError != (err != nil) || err != nil && strings.Contains(err.Error(), "SYNTHETIC_TEST_ONLY") {
		t.Fatal("updater failure was lost or leaked credential data")
	}
	if nativePath == "" {
		t.Fatal("updater was not invoked")
	}
	if _, err := os.Stat(nativePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("updater credential survived refresh completion")
	}
	content, err := os.ReadFile(input)
	if err != nil || string(content) != syntheticMaxMindYaml {
		t.Fatal("refresh mutated the persistent Vault credential")
	}
}

func TestMaxMindVaultConversionIsPrivateAndEphemeral(t *testing.T) {
	checkMaxMindTemporaryConfig(t, false)
}

func TestMaxMindUpdaterFailureStillRemovesSecret(t *testing.T) {
	checkMaxMindTemporaryConfig(t, true)
}

func TestMaxMindCancellationDoesNotInvokeUpdater(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := withMaxMindNativeConfig(ctx, "synthetic-unused.yml", func(string) error {
		t.Fatal("canceled request invoked updater")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestMaxMindCancellationDuringUpdaterStillRemovesSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := writeTestInput(t, filepath.Join(t.TempDir(), "mm-geoip.yml"), []byte(syntheticMaxMindYaml))
	nativePath := ""
	err := withMaxMindNativeConfig(ctx, input, func(path string) error {
		nativePath = path
		cancel()
		return errors.New("synthetic canceled updater")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := os.Stat(nativePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled updater retained its temporary credential")
	}
}
