package monitor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// The account message sender (SES, Pinpoint SMS) uses the AWS SDK's default
// chain, which reads the shared credentials file the containers see at
// /root/.aws (the vault's .aws). Until 2026-10-19 this requirement read aws.yml
// instead, a copy no sender loads, whose keys AWS rejected as
// InvalidClientTokenId while the requirement stayed green.
func accountMessageCredentialRequirement(t *testing.T) CredentialRequirement {
	t.Helper()
	for _, requirement := range loadCredentialRequirements("main", false, nil) {
		if requirement.Key == "account-email" {
			return requirement
		}
	}
	t.Fatal("no account-email credential requirement")
	return CredentialRequirement{}
}

func useSyntheticCredentialVault(t *testing.T) {
	t.Helper()
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
}

func TestAccountMessageCredentialsReadTheSharedCredentialsFile(t *testing.T) {
	useSyntheticCredentialVault(t)
	pop := server.Vault.PushSimpleResource(".aws/credentials", []byte(
		"[default]\naws_access_key_id = synthetic-key-id\naws_secret_access_key = synthetic-secret\n",
	))
	defer pop()
	requirement := accountMessageCredentialRequirement(t)
	if requirement.Resource != ".aws/credentials" || !requirement.Present || requirement.Malformed || len(requirement.MissingFields) != 0 {
		t.Fatalf("shared credentials file not read as complete: %+v", requirement)
	}
}

// The vault resolves the resource from its .aws directory on disk, the path
// the containers mount at /root/.aws.
func TestAccountMessageCredentialsResolveTheVaultFile(t *testing.T) {
	useSyntheticCredentialVault(t)
	directory := filepath.Join(os.Getenv("WARP_VAULT_HOME"), "main", ".aws")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "credentials"), []byte(
		"[default]\naws_access_key_id = synthetic-key-id\naws_secret_access_key = synthetic-secret\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	requirement := accountMessageCredentialRequirement(t)
	if !requirement.Present || requirement.Malformed || len(requirement.MissingFields) != 0 {
		t.Fatalf("the vault's .aws/credentials file was not read as complete: %+v", requirement)
	}
}

// A credentials file the vault resolves but the monitor cannot read is
// malformed, not complete.
func TestAccountMessageCredentialsReportAnUnreadableFile(t *testing.T) {
	useSyntheticCredentialVault(t)
	directory := filepath.Join(os.Getenv("WARP_VAULT_HOME"), "main", ".aws")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "credentials")
	if err := os.WriteFile(path, []byte(
		"[default]\naws_access_key_id = synthetic-key-id\naws_secret_access_key = synthetic-secret\n",
	), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this process reads a mode 0000 file")
	}
	requirement := accountMessageCredentialRequirement(t)
	if !requirement.Present || !requirement.Malformed {
		t.Fatalf("an unreadable credentials file was not reported malformed: %+v", requirement)
	}
}

// aws.yml alone no longer certifies the sender: no sender reads it.
func TestAccountMessageCredentialsIgnoreTheUnusedYamlCopy(t *testing.T) {
	useSyntheticCredentialVault(t)
	pop := server.Vault.PushSimpleResource("aws.yml", []byte(
		"aws:\n  access_key_id: synthetic-key-id\n  secret_access_key: synthetic-secret\n",
	))
	defer pop()
	requirement := accountMessageCredentialRequirement(t)
	if requirement.Present || !slices.Contains(requirement.MissingFields, "default.aws_access_key_id") {
		t.Fatalf("aws.yml certified the account message sender: %+v", requirement)
	}
}

func TestAccountMessageCredentialsReportAMissingSecret(t *testing.T) {
	useSyntheticCredentialVault(t)
	pop := server.Vault.PushSimpleResource(".aws/credentials", []byte(
		"[default]\naws_access_key_id = synthetic-key-id\n[other]\naws_secret_access_key = synthetic-secret\n",
	))
	defer pop()
	requirement := accountMessageCredentialRequirement(t)
	if !requirement.Present || requirement.Malformed || !slices.Equal(requirement.MissingFields, []string{"default.aws_secret_access_key"}) {
		t.Fatalf("a default profile without its secret was not reported: %+v", requirement)
	}
}

func TestAccountMessageCredentialsReportAMalformedFile(t *testing.T) {
	for _, content := range []string{
		"aws_access_key_id = outside-any-section\n",
		"[default\naws_access_key_id = synthetic-key-id\n",
		"[default]\nno separator here\n",
		"[default]\naws_access_key_id = one\naws_access_key_id = two\n",
	} {
		useSyntheticCredentialVault(t)
		pop := server.Vault.PushSimpleResource(".aws/credentials", []byte(content))
		requirement := accountMessageCredentialRequirement(t)
		pop()
		if !requirement.Present || !requirement.Malformed {
			t.Errorf("malformed shared credentials %q read as %+v", content, requirement)
		}
	}
}
