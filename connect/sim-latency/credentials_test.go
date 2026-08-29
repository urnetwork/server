package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
)

func TestCredentialLifecycleRotatesWithoutAnAuthenticationGap(t *testing.T) {
	temporaryRoot := t.TempDir()
	vaultPath := filepath.Join(temporaryRoot, "competition.yml")
	initialDeliveryPath := filepath.Join(temporaryRoot, "initial-delivery.json")
	now := time.Date(2026, time.August, 29, 3, 0, 0, 0, time.UTC)

	if _, err := generateCredentialVault(
		vaultPath,
		initialDeliveryPath,
		"apex-submit-v1",
		"operations-v1",
		bytes.NewReader(deterministicCredentialEntropy(3*credentialRandomBytes)),
		now,
	); err != nil {
		t.Fatal(err)
	}
	assertPrivateRegularFile(t, vaultPath)
	assertPrivateRegularFile(t, initialDeliveryPath)
	initialDelivery := readCredentialDeliveryForTest(t, initialDeliveryPath)
	if initialDelivery.Schema != credentialDeliverySchema || !initialDelivery.GeneratedAt.Equal(now) || len(initialDelivery.Credentials) != 2 {
		t.Fatalf("initial delivery = %+v", initialDelivery)
	}
	initialVault, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivered := range initialDelivery.Credentials {
		if credentialTokenDigest(delivered.Token) != delivered.Sha256 || !credentialAuthenticates(initialVault, delivered.Token, delivered.Role) {
			t.Fatalf("delivered credential %s does not authenticate", delivered.Name)
		}
	}
	oldSubmitter := initialDelivery.Credentials[0]
	if oldSubmitter.Role != "submitter" {
		t.Fatalf("first credential role = %q", oldSubmitter.Role)
	}
	seedBefore := initialVault.SeedKeyBase64

	rotationDeliveryPath := filepath.Join(temporaryRoot, "submitter-v2.json")
	if _, err := rotateCredentialToken(
		vaultPath,
		rotationDeliveryPath,
		"apex-submit-v2",
		"submitter",
		bytes.NewReader(deterministicCredentialEntropy(credentialRandomBytes)),
		now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	newSubmitter := readCredentialDeliveryForTest(t, rotationDeliveryPath).Credentials[0]
	rotatedVault, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if rotatedVault.SeedKeyBase64 != seedBefore || !credentialAuthenticates(rotatedVault, oldSubmitter.Token, "submitter") ||
		!credentialAuthenticates(rotatedVault, newSubmitter.Token, "submitter") {
		t.Fatal("token rotation did not preserve old/new overlap or changed the seed")
	}

	if _, err := revokeCredentialToken(vaultPath, oldSubmitter.Name); err != nil {
		t.Fatal(err)
	}
	revokedVault, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if credentialAuthenticates(revokedVault, oldSubmitter.Token, "submitter") || !credentialAuthenticates(revokedVault, newSubmitter.Token, "submitter") {
		t.Fatal("revocation did not remove only the old credential")
	}
	if _, err := revokeCredentialToken(vaultPath, newSubmitter.Name); err == nil {
		t.Fatal("revocation removed the final submitter credential")
	}

	if _, err := rotateCredentialSeed(vaultPath, bytes.NewReader(bytes.Repeat([]byte{0xa5}, credentialRandomBytes))); err != nil {
		t.Fatal(err)
	}
	seedRotatedVault, err := readCredentialVault(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if seedRotatedVault.SeedKeyBase64 == seedBefore || !credentialAuthenticates(seedRotatedVault, newSubmitter.Token, "submitter") {
		t.Fatal("seed rotation did not change only the seed")
	}
}

func TestCredentialMutationsRejectUnsafeFilesAndDuplicateNames(t *testing.T) {
	temporaryRoot := t.TempDir()
	vaultPath := filepath.Join(temporaryRoot, "competition.yml")
	deliveryPath := filepath.Join(temporaryRoot, "delivery.json")
	if _, err := generateCredentialVault(
		vaultPath,
		deliveryPath,
		"submitter-v1",
		"operator-v1",
		bytes.NewReader(deterministicCredentialEntropy(3*credentialRandomBytes)),
		time.Unix(1, 0),
	); err != nil {
		t.Fatal(err)
	}

	duplicateDeliveryPath := filepath.Join(temporaryRoot, "duplicate.json")
	if _, err := rotateCredentialToken(
		vaultPath,
		duplicateDeliveryPath,
		"submitter-v1",
		"submitter",
		bytes.NewReader(deterministicCredentialEntropy(credentialRandomBytes)),
		time.Unix(2, 0),
	); err == nil {
		t.Fatal("duplicate credential name was accepted")
	}
	if _, err := os.Stat(duplicateDeliveryPath); !os.IsNotExist(err) {
		t.Fatal("failed rotation retained a raw credential delivery")
	}

	if err := os.Chmod(vaultPath, 0644); err != nil {
		t.Fatal(err)
	}
	unsafeDeliveryPath := filepath.Join(temporaryRoot, "unsafe.json")
	if _, err := rotateCredentialToken(
		vaultPath,
		unsafeDeliveryPath,
		"submitter-v2",
		"submitter",
		bytes.NewReader(deterministicCredentialEntropy(credentialRandomBytes)),
		time.Unix(3, 0),
	); err == nil {
		t.Fatal("group-readable credential vault was accepted")
	}
	if _, err := os.Stat(unsafeDeliveryPath); !os.IsNotExist(err) {
		t.Fatal("unsafe vault failure retained a raw credential delivery")
	}
}

func deterministicCredentialEntropy(size int) []byte {
	result := make([]byte, size)
	for i := range result {
		result[i] = byte(i + 1)
	}
	return result
}

func readCredentialDeliveryForTest(t *testing.T, filePath string) *credentialDelivery {
	t.Helper()
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	delivery := &credentialDelivery{}
	if err := json.Unmarshal(content, delivery); err != nil {
		t.Fatal(err)
	}
	return delivery
}

func assertPrivateRegularFile(t *testing.T, filePath string) {
	t.Helper()
	info, err := os.Lstat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("%s mode = %v", filePath, info.Mode())
	}
}

func credentialTokenDigest(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

func credentialAuthenticates(vault *credentialVault, raw string, role string) bool {
	tokens := make([]controller.Token, 0, len(vault.Tokens))
	for _, token := range vault.Tokens {
		tokens = append(tokens, controller.Token{Name: token.Name, Role: token.Role, Sha256: token.Sha256})
	}
	request, err := http.NewRequest(http.MethodGet, "https://api.bringyour.com/competition/info", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+raw)
	principal, ok := controller.Authenticate(request, &controller.Settings{Tokens: tokens})
	return ok && principal.Role == role
}
