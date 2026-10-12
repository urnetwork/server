// Key rotation across process lifetimes, reproduced in fresh processes.
//
// The keys a process signs and verifies with are process state read from the
// vault at first use, so each test runs its scenario in a copy of this test
// binary with its own synthetic vault (WARP_VAULT_HOME). The scenario is the
// 2026-10-12 connect outage: a certificate rotation added a version under the
// jwt tls key path, the api restarted and signed with it, and every verifier
// that had read its keys before the rotation refused those tokens.
package session

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

// selects the scenario a child test binary runs
const byJwtRotationChildEnv = "URNETWORK_TEST_BYJWT_ROTATION_CHILD"

// the synthetic environment and key path of a rotation vault
const byJwtRotationEnv = "test"
const byJwtRotationKeyPath = "tls/test.example/test.example.key"

// Writes a vault root whose jwt.yml lists the synthetic key path.
func newByJwtRotationVault(t *testing.T) string {
	t.Helper()
	vaultRoot := t.TempDir()
	jwtYml := []byte("tls_key_paths:\n  - " + byJwtRotationKeyPath + "\n")
	if err := os.MkdirAll(filepath.Join(vaultRoot, byJwtRotationEnv), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultRoot, byJwtRotationEnv, "jwt.yml"), jwtYml, 0o600); err != nil {
		t.Fatal(err)
	}
	return vaultRoot
}

// The file of a version of the synthetic key path, laid out as the vault lays
// out tls versions: all/tls/<version>/<domain>/<domain>.key.
func byJwtRotationKeyFile(vaultRoot string, version string) string {
	return filepath.Join(vaultRoot, "all", "tls", version, "test.example", "test.example.key")
}

// Writes a new p-256 key as a version of the synthetic key path.
func writeByJwtRotationKey(t testing.TB, vaultRoot string, version string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	writeByJwtKeyFile(t, byJwtRotationKeyFile(vaultRoot, version), key)
	return key
}

// Writes key as a sec 1 pem file, as the vault holds tls keys.
func writeByJwtKeyFile(t testing.TB, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, keyPem, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Reads a key written by writeByJwtKeyFile.
func readByJwtKeyFile(t testing.TB, path string) *ecdsa.PrivateKey {
	t.Helper()
	keyPem, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(keyPem)
	if block == nil {
		t.Fatalf("no pem block in %s", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Signs claims with key as a signer holding it signs: es256, and the kid header
// when withKid.
func signByJwtWithKey(t testing.TB, claims gojwt.Claims, key *ecdsa.PrivateKey, withKid bool) string {
	t.Helper()
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
	if withKid {
		kid, err := jwtKid(publicKey(key))
		if err != nil {
			t.Fatal(err)
		}
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// The kid header of a signed token.
func byJwtTokenKid(t testing.TB, signed string) string {
	t.Helper()
	token, _, err := gojwt.NewParser().ParseUnverified(signed, gojwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := token.Header["kid"].(string)
	return kid
}

// Runs testName's scenario in a copy of this test binary using vaultRoot, and
// fails with the child's output when the scenario fails.
func runByJwtRotationChild(t *testing.T, testName string, vaultRoot string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.count=1", "-test.v")
	child.Env = append(
		os.Environ(),
		byJwtRotationChildEnv+"="+testName,
		"WARP_VAULT_HOME="+vaultRoot,
		"WARP_ENV="+byJwtRotationEnv,
	)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("rotation scenario failed: %s\n%s", err, output)
	}
	// a child that matched no test would pass without running the scenario
	if !strings.Contains(string(output), "--- PASS: "+testName+" ") {
		t.Fatalf("rotation scenario did not run:\n%s", output)
	}
}

// A verifier that read its keys before the vault gained a key version accepts
// a token signed with that version, as a signer started after the rotation
// signs it.
func TestByJwtVerifierAcceptsKeyVersionAddedAfterFirstUse(t *testing.T) {
	if os.Getenv(byJwtRotationChildEnv) == t.Name() {
		ctx := context.Background()
		vaultRoot := os.Getenv("WARP_VAULT_HOME")
		newClaims := func() *ByJwt {
			return NewByJwt(server.NewId(), server.NewId(), "test", false, false)
		}

		// the verifier reads its keys: the vault holds only the first version
		previousKey := readByJwtKeyFile(t, byJwtRotationKeyFile(vaultRoot, "1.0.0"))
		if _, err := ParseByJwt(ctx, signByJwtWithKey(t, newClaims(), previousKey, true)); err != nil {
			t.Fatalf("token signed with the held version: %s", err)
		}

		// the rotation adds a version, and a signer started after it signs with it
		rotatedKey := writeByJwtRotationKey(t, vaultRoot, "2.0.0")
		if _, err := ParseByJwt(ctx, signByJwtWithKey(t, newClaims(), rotatedKey, true)); err != nil {
			t.Fatalf("token signed with the version added after first use: %s", err)
		}
		// still verifies once held, and the held version still verifies
		if _, err := ParseByJwt(ctx, signByJwtWithKey(t, newClaims(), rotatedKey, true)); err != nil {
			t.Fatalf("second token signed with the added version: %s", err)
		}
		if _, err := ParseByJwt(ctx, signByJwtWithKey(t, newClaims(), previousKey, true)); err != nil {
			t.Fatalf("token signed with the previous version after the rotation: %s", err)
		}
		return
	}

	vaultRoot := newByJwtRotationVault(t)
	writeByJwtRotationKey(t, vaultRoot, "1.0.0")
	runByJwtRotationChild(t, t.Name(), vaultRoot)
}

// A signer that starts while the newest key version was only just written to
// the vault keeps signing with the version every verifier already holds.
func TestByJwtSignerWaitsForNewKeyVersionToSettle(t *testing.T) {
	if os.Getenv(byJwtRotationChildEnv) == t.Name() {
		ctx := context.Background()
		vaultRoot := os.Getenv("WARP_VAULT_HOME")
		settledKey := readByJwtKeyFile(t, byJwtRotationKeyFile(vaultRoot, "1.0.0"))
		settledKid, err := jwtKid(publicKey(settledKey))
		if err != nil {
			t.Fatal(err)
		}

		signed := NewByJwt(server.NewId(), server.NewId(), "test", false, false).Testing_Sign()
		if kid := byJwtTokenKid(t, signed); kid != settledKid {
			t.Fatalf("signed with kid %q, want the settled version's %q", kid, settledKid)
		}
		if _, err := ParseByJwt(ctx, signed); err != nil {
			t.Fatalf("token signed with the settled version: %s", err)
		}
		return
	}

	vaultRoot := newByJwtRotationVault(t)
	writeByJwtRotationKey(t, vaultRoot, "1.0.0")
	// the settled version was written to the vault long ago; the new one just now
	settledTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(byJwtRotationKeyFile(vaultRoot, "1.0.0"), settledTime, settledTime); err != nil {
		t.Fatal(err)
	}
	writeByJwtRotationKey(t, vaultRoot, "2.0.0")
	runByJwtRotationChild(t, t.Name(), vaultRoot)
}
