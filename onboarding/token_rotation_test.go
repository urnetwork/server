// A key rotation reproduced in a fresh process: the landing token keys derive
// from the jwt keys, which a process reads from the vault at first use.
package onboarding

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// selects the scenario a child test binary runs
const tokenRotationChildEnv = "URNETWORK_TEST_ONBOARDING_ROTATION_CHILD"

// The file of a version of the synthetic jwt key path.
func tokenRotationKeyFile(vaultRoot string, version string) string {
	return filepath.Join(vaultRoot, "all", "tls", version, "test.example", "test.example.key")
}

// Writes a new p-256 key as a version of the synthetic jwt key path.
func writeTokenRotationKey(t testing.TB, vaultRoot string, version string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := tokenRotationKeyFile(vaultRoot, version)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// The landing token key a signer holding key derives (session.DerivedKeys).
func tokenRotationDerivedKey(t testing.TB, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, der)
	mac.Write([]byte(TokenKeyPurpose))
	return mac.Sum(nil)
}

// A landing-token verifier that read its keys before the vault gained a key
// version accepts a token a signer holding that version minted.
func TestParseAcceptsTokenFromKeyVersionAddedAfterFirstUse(t *testing.T) {
	if os.Getenv(tokenRotationChildEnv) == t.Name() {
		vaultRoot := os.Getenv("WARP_VAULT_HOME")
		now := time.Now()
		newClaims := func() *TokenClaims {
			return &TokenClaims{NetworkId: server.NewId(), Step: "test", ExpiresAt: now.Add(time.Hour).Unix()}
		}

		// the verifier reads its keys: the vault holds only the first version
		if len(session.DerivedKeys(TokenKeyPurpose)) != 1 {
			t.Fatalf("derived keys before the rotation = %d, want 1", len(session.DerivedKeys(TokenKeyPurpose)))
		}

		// the rotation adds a version, and a signer holding it mints a token
		rotatedKey := writeTokenRotationKey(t, vaultRoot, "2.0.0")
		token, err := SignToken(tokenRotationDerivedKey(t, rotatedKey), newClaims())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(token, now); err != nil {
			t.Fatalf("token from the version added after first use: %s", err)
		}
		return
	}

	vaultRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vaultRoot, "test"), 0o700); err != nil {
		t.Fatal(err)
	}
	jwtYml := []byte("tls_key_paths:\n  - tls/test.example/test.example.key\n")
	if err := os.WriteFile(filepath.Join(vaultRoot, "test", "jwt.yml"), jwtYml, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTokenRotationKey(t, vaultRoot, "1.0.0")

	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	child.Env = append(
		os.Environ(),
		tokenRotationChildEnv+"="+t.Name(),
		"WARP_VAULT_HOME="+vaultRoot,
		"WARP_ENV=test",
	)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("rotation scenario failed: %s\n%s", err, output)
	}
	// a child that matched no test would pass without running the scenario
	if !strings.Contains(string(output), "--- PASS: "+t.Name()+" ") {
		t.Fatalf("rotation scenario did not run:\n%s", output)
	}
}
