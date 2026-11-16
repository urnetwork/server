// The dedicated issuer uses synthetic entropy and isolated private files. Its
// outputs authenticate normally but can never enter an ordinary shared vault.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Exact synthetic scope is independent of any deployed competition identity.
func patchRetryCredentialScopeForTest() (credentialScope, time.Time) {
	now := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	return credentialScope{Environment: "main", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		RoundId: server.NewId(), JobId: server.NewId(), ExpiresAt: time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)}, now
}

// Real issuer bytes, registration parsing and real controller authentication
// compose without touching a vault, API process, database or hidden evidence.
func TestPatchRetryCredentialIssuerProducesExactPrivateLocalAuthentication(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	scope, now := patchRetryCredentialScopeForTest()
	receipt, err := issuePatchRetryCredential(directory, "registration.json", "bearer", "synthetic-retry", scope,
		bytes.NewReader(bytes.Repeat([]byte{42}, credentialRandomBytes)), now, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "registration.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if receipt.RegistrationSha256 != hex.EncodeToString(digest[:]) {
		t.Fatal("issuer receipt lost the raw registration file pin")
	}
	var registration controller.CompetitionOperatorPatchRetryCredential
	if json.Unmarshal(content, &registration) != nil || registration.RoundId != scope.RoundId || registration.JobId != scope.JobId ||
		!registration.ExpiresAt.Equal(scope.ExpiresAt) || registration.Capability != model.CompetitionOperatorRequestedPatchRetryCapability {
		t.Fatal("issuer changed exact registration scope")
	}
	raw, err := os.ReadFile(filepath.Join(root, "bearer"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	for _, leaf := range []string{"registration.json", "bearer"} {
		info, err := os.Lstat(filepath.Join(root, leaf))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			t.Fatal("issuer created non-private output")
		}
	}
	settings := &controller.Settings{CompetitionId: scope.CompetitionId, AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: scope.Environment, SeasonId: scope.SeasonId}}
	local, err := controller.WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil {
		t.Fatal(err)
	}
	bearer := httptest.NewRequest(http.MethodPost, "/synthetic-grant", nil)
	bearer.Header.Set("Authorization", "Bearer "+strings.TrimSuffix(string(raw), "\n"))
	principal, ok := controller.Authenticate(bearer, local)
	if !ok || principal.Role != operatorPatchRetryCredentialRole || principal.JobId != scope.JobId || principal.RoundId != scope.RoundId {
		t.Fatal("actual issuer/controller authentication contract failed")
	}
	encodedReceipt, _ := json.Marshal(receipt)
	if bytes.Contains(encodedReceipt, bytes.TrimSpace(raw)) || bytes.Contains(content, bytes.TrimSpace(raw)) {
		t.Fatal("raw bearer escaped its dedicated private file")
	}
	if _, err := os.Stat(filepath.Join(root, "vault.yml")); !os.IsNotExist(err) {
		t.Fatal("dedicated issuer created a shared vault")
	}
}

// Partial scope and elapsed expiry fail before entropy or output creation.
// Other roles cannot advertise these job claims as an unenforced restriction.
func TestPatchRetryCredentialIssuerRejectsScopeBeforeEntropyOrFiles(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	scope, now := patchRetryCredentialScopeForTest()
	for _, mutate := range []func(*credentialScope){
		func(s *credentialScope) { s.Environment = "synthetic-other-env" },
		func(s *credentialScope) { s.CompetitionId = "" },
		func(s *credentialScope) { s.SeasonId = "" },
		func(s *credentialScope) { s.RoundId = server.Id{} },
		func(s *credentialScope) { s.JobId = server.Id{} },
		func(s *credentialScope) { s.ExpiresAt = time.Time{} },
		func(s *credentialScope) { s.ExpiresAt = now },
	} {
		changed := scope
		mutate(&changed)
		entropy := bytes.NewReader(bytes.Repeat([]byte{42}, credentialRandomBytes))
		if _, err := issuePatchRetryCredential(directory, "registration.json", "bearer", "synthetic-retry", changed, entropy, now, uint32(os.Geteuid())); err == nil || entropy.Len() != credentialRandomBytes {
			t.Fatal("invalid scope consumed entropy or acquired issuance authority")
		}
	}
	entries, err := directory.ReadDir(-1)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid scope created an output")
	}
	for _, role := range []string{"operator", "submitter", "scheduler", "reviewer", "settlement", "source_promoter", "source_handoff", "review_operator"} {
		if validateCredentialCapability(role, scope) == nil {
			t.Fatal("another role advertised exact patch-retry scope", role)
		}
	}
}

// A short entropy source fails before either leaf is opened. Recovery never
// treats a partially generated secret or registration as an issued capability.
func TestPatchRetryCredentialIssuerEntropyFailureCreatesNoOutputs(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	scope, now := patchRetryCredentialScopeForTest()
	if _, err := issuePatchRetryCredential(directory, "registration.json", "bearer", "synthetic-retry", scope,
		bytes.NewReader(bytes.Repeat([]byte{42}, credentialRandomBytes-1)), now, uint32(os.Geteuid())); err == nil {
		t.Fatal("partial entropy issued a credential")
	}
	entries, err := directory.ReadDir(-1)
	if err != nil || len(entries) != 0 {
		t.Fatal("entropy failure left credential outputs")
	}
}

// Existing leaves and aliases remain byte-identical. If the second exclusive
// create refuses, only the first newly owned leaf is rolled back.
func TestPatchRetryCredentialIssuerRejectsOverwriteAliasesAndUnprotectedOwnership(t *testing.T) {
	scope, now := patchRetryCredentialScopeForTest()
	for _, existing := range []string{"registration", "token", "symlink", "hardlink"} {
		root := t.TempDir()
		marker := []byte("synthetic-existing-private-file")
		if err := os.WriteFile(filepath.Join(root, "original"), marker, 0o600); err != nil {
			t.Fatal(err)
		}
		switch existing {
		case "registration":
			if err := os.WriteFile(filepath.Join(root, "registration.json"), marker, 0o600); err != nil {
				t.Fatal(err)
			}
		case "token":
			if err := os.WriteFile(filepath.Join(root, "bearer"), marker, 0o600); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := os.Symlink("original", filepath.Join(root, "bearer")); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(filepath.Join(root, "original"), filepath.Join(root, "bearer")); err != nil {
				t.Fatal(err)
			}
		}
		directory, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		_, issueErr := issuePatchRetryCredential(directory, "registration.json", "bearer", "synthetic-retry", scope,
			bytes.NewReader(bytes.Repeat([]byte{42}, credentialRandomBytes)), now, uint32(os.Geteuid()))
		directory.Close()
		if issueErr == nil {
			t.Fatal("issuer overwrote an existing leaf", existing)
		}
		retained, err := os.ReadFile(filepath.Join(root, "original"))
		if err != nil || !bytes.Equal(retained, marker) {
			t.Fatal("issuer altered aliased original", existing)
		}
		if existing != "registration" {
			if _, err := os.Lstat(filepath.Join(root, "registration.json")); !os.IsNotExist(err) {
				t.Fatal("failed pair retained its new first leaf")
			}
		}
	}
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if _, err := issuePatchRetryCredential(directory, "registration.json", "bearer", "synthetic-retry", scope, bytes.NewReader(nil), now, uint32(os.Geteuid())+1); err == nil {
		t.Fatal("issuer accepted a different owner")
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := issuePatchRetryCredential(directory, "registration.json", "bearer", "synthetic-retry", scope, bytes.NewReader(nil), now, uint32(os.Geteuid())); err == nil {
		t.Fatal("issuer accepted a writable output parent")
	}
}

// Shared-vault rotation remains a different operation. The new capability can
// neither rewrite that file nor create an ordinary raw credential delivery.
func TestPatchRetryCredentialIssuerCannotRotateIntoSharedVault(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault.yml")
	marker := []byte("synthetic-existing-vault-marker")
	if err := os.WriteFile(vault, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	scope, now := patchRetryCredentialScopeForTest()
	delivery := filepath.Join(root, "delivery.json")
	if _, err := rotateScopedCredentialToken(vault, delivery, "synthetic-retry", operatorPatchRetryCredentialRole, scope, bytes.NewReader(nil), now); err == nil {
		t.Fatal("patch-retry credential entered shared-vault rotation")
	}
	retained, err := os.ReadFile(vault)
	if err != nil || !bytes.Equal(retained, marker) {
		t.Fatal("rejected role changed shared vault")
	}
	if _, err := os.Stat(delivery); !os.IsNotExist(err) {
		t.Fatal("rejected role created a delivery")
	}
}
