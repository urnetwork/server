// Scoped credential fixtures are synthetic and exercise the same authentication
// types as the API. No generated test bearer leaves its private temporary root.
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
)

// Every new capability requires all three claims before consuming entropy or
// creating a delivery file. Legacy tokens cannot acquire scope by implication.
func TestCredentialCapabilitiesRequireExactScope(t *testing.T) {
	scope := credentialScope{Environment: "synthetic-stage", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season"}
	for _, role := range []string{"scheduler", "reviewer", "settlement", "source_promoter"} {
		for _, incomplete := range []credentialScope{{}, {Environment: scope.Environment}, {Environment: scope.Environment, CompetitionId: scope.CompetitionId}} {
			if _, err := generateScopedCredentialToken("synthetic-actor", role, incomplete, bytes.NewReader(nil)); err == nil {
				t.Errorf("unscoped %s accepted", role)
			}
		}
		if _, err := generateScopedCredentialToken("synthetic-actor", role, scope, bytes.NewReader(bytes.Repeat([]byte{42}, credentialRandomBytes))); err != nil {
			t.Errorf("scoped %s refused: %v", role, err)
		}
	}
	if _, err := generateScopedCredentialToken("synthetic-actor", "operator", scope, bytes.NewReader(nil)); err == nil {
		t.Fatal("legacy operator misleadingly advertised as a scoped v2 capability")
	}
}

// Rotation and seed updates preserve scope/revocation fields instead of either
// rejecting a v2 vault or silently upgrading its keys to unscoped credentials.
func TestCredentialScopedRotationPreservesClaimsAndRevocation(t *testing.T) {
	root := t.TempDir()
	vaultPath := filepath.Join(root, "vault.yml")
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	if _, err := generateCredentialVault(vaultPath, filepath.Join(root, "initial.json"), "synthetic-submit", "synthetic-operator",
		bytes.NewReader(deterministicCredentialEntropy(3*credentialRandomBytes)), now); err != nil {
		t.Fatal(err)
	}
	scope := credentialScope{Environment: "synthetic-stage", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season"}
	for index, role := range []string{"scheduler", "reviewer", "settlement", "source_promoter"} {
		name := "synthetic-" + role
		deliveryPath := filepath.Join(root, name+".json")
		if _, err := rotateScopedCredentialToken(vaultPath, deliveryPath, name, role, scope,
			bytes.NewReader(bytes.Repeat([]byte{byte(index + 40)}, credentialRandomBytes)), now); err != nil {
			t.Fatal(err)
		}
		delivered := readCredentialDeliveryForTest(t, deliveryPath).Credentials[0]
		vault, err := readCredentialVault(vaultPath)
		if err != nil {
			t.Fatal(err)
		}
		if delivered.Environment != scope.Environment || delivered.CompetitionId != scope.CompetitionId || delivered.SeasonId != scope.SeasonId ||
			!credentialAuthenticates(vault, delivered.Token, role) {
			t.Fatalf("scoped %s delivery did not retain its claims", role)
		}
		var configured controller.Token
		for _, token := range vault.Tokens {
			if token.Name == name {
				configured = controller.Token{Name: token.Name, Role: token.Role, Sha256: token.Sha256, Environment: token.Environment, CompetitionId: token.CompetitionId, SeasonId: token.SeasonId}
			}
		}
		request := httptest.NewRequest(http.MethodGet, "https://competition.example/competition/v2/events", nil)
		request.Header.Set("Authorization", "Bearer "+delivered.Token)
		principal, ok := controller.Authenticate(request, &controller.Settings{Tokens: []controller.Token{configured}})
		if !ok || principal.Environment != scope.Environment || principal.CompetitionId != scope.CompetitionId || principal.SeasonId != scope.SeasonId {
			t.Fatalf("API lost exact scope for %s", role)
		}
	}
	vault, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := range vault.Tokens {
		if vault.Tokens[index].Role == "reviewer" {
			vault.Tokens[index].Revoked = true
		}
	}
	if err := writeCredentialVault(vaultPath, vault); err != nil {
		t.Fatal(err)
	}
	if _, err := rotateCredentialSeed(vaultPath, bytes.NewReader(bytes.Repeat([]byte{99}, credentialRandomBytes))); err != nil {
		t.Fatal(err)
	}
	reloaded, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range reloaded.Tokens {
		if token.Role == "reviewer" && (!token.Revoked || token.Environment != scope.Environment || token.SeasonId != scope.SeasonId) {
			t.Fatal("seed rotation erased a scoped revocation")
		}
	}
	if _, err := revokeCredentialToken(vaultPath, "synthetic-settlement"); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentialVault(vaultPath); err != nil {
		t.Fatal(err)
	}
}

// A revoked legacy key cannot satisfy the overlap guarantee for the last live
// operator/submitter. Metadata preservation must not create an auth outage.
func TestCredentialVaultDoesNotCountRevokedKeysAsAvailable(t *testing.T) {
	root := t.TempDir()
	vaultPath := filepath.Join(root, "vault.yml")
	if _, err := generateCredentialVault(vaultPath, filepath.Join(root, "delivery.json"), "synthetic-submit", "synthetic-operator",
		bytes.NewReader(deterministicCredentialEntropy(3*credentialRandomBytes)), time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	vault, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"submitter", "operator"} {
		copyVault := *vault
		copyVault.Tokens = append([]credentialToken{}, vault.Tokens...)
		for index := range copyVault.Tokens {
			if copyVault.Tokens[index].Role == role {
				copyVault.Tokens[index].Revoked = true
			}
		}
		if validateCredentialVault(&copyVault) == nil {
			t.Fatalf("revoked %s counted as an available credential", role)
		}
	}
}
