// External-package tests join the real authentication consumers without
// inverting server's package dependencies. Private children isolate once caches.
package server_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	byjwt "github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
)

// Only explicitly generated roots reach the child's environment. No inherited
// credential, test proxy, or resolver setting can choose its signing authority.
func releaseGateSuiteAuthEnvironment(root, mode string) []string {
	values := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "WARP_") || strings.HasPrefix(name, "BRINGYOUR_") || strings.HasPrefix(name, "RELEASE_GATE_SUITE_AUTH_") || name == "APEX_CONTAINER_EVALUATION" {
			continue
		}
		values = append(values, value)
	}
	return append(values, "WARP_HOME="+root, "WARP_VAULT_HOME="+filepath.Join(root, "vault"), "WARP_CONFIG_HOME="+filepath.Join(root, "config"), "WARP_SITE_HOME="+filepath.Join(root, "site"),
		"WARP_ENV=local", "WARP_SERVICE=test", "WARP_DOMAIN=fixture.example", "WARP_HOST=fixture", "WARP_BLOCK=fixture", "RELEASE_GATE_SUITE_AUTH_CHILD="+mode)
}

// The actual CLI creates all keys. The child then runs the existing server
// loaders in a fresh process; no test callback supplies a successful verdict.
func runReleaseGateSuiteAuthChild(t *testing.T, mode string, mutate func(string)) {
	t.Helper()
	serverSource, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	serverSource, err = filepath.EvalSymlinks(serverSource)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "run", "./scripts/server-fixture", "--suite", "--parent", parent, "--server", serverSource,
		"--postgres-authority", "127.0.0.1:35431", "--redis-authority", "127.0.0.1:36371", "--path-only")
	command.Dir = filepath.Join(serverSource, "..", "sn")
	command.Env = releaseGateSuiteAuthEnvironment(parent, "")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("real complete fixture command: %v\n%s", err, stderr.Bytes())
	}
	root := strings.TrimSuffix(string(output), "\n")
	if filepath.Dir(root) != parent || strings.Contains(root, "\n") {
		t.Fatal("fixture command returned a foreign root")
	}
	if mutate != nil {
		mutate(root)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Dir = serverSource
	child.Env = releaseGateSuiteAuthEnvironment(root, mode)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("actual private authentication adapters: %v\n%s", err, output)
	}
}

// Generated resources must authenticate real tokens and preserve the strict
// expiration and disjoint OAuth/platform-key boundaries, not merely parse yaml.
func TestReleaseGateServicesSuiteAuthenticationAdapters(t *testing.T) {
	if os.Getenv("RELEASE_GATE_SUITE_AUTH_CHILD") == "" {
		runReleaseGateSuiteAuthChild(t, "positive", nil)
		return
	}
	if os.Getenv("RELEASE_GATE_SUITE_AUTH_CHILD") != "positive" {
		t.Fatal("unexpected authentication child mode")
	}
	claims := byjwt.NewByJwt(server.NewId(), server.NewId(), "fixture-network", false, false)
	parsed, err := byjwt.ParseByJwt(t.Context(), claims.Sign())
	if err != nil || parsed.NetworkId != claims.NetworkId || parsed.UserId != claims.UserId {
		t.Fatalf("genuine platform authentication failed: %v", err)
	}
	oauthKey := oauth.SigningKey()
	kid, err := oauth.SignerKid(&oauthKey.PrivateKey.PublicKey)
	if err != nil || kid != oauthKey.Kid || oauth.Issuer() != "https://auth.fixture.example" || len(oauth.VerificationKeys()) != 1 {
		t.Fatalf("dedicated OAuth authority differs: %v", err)
	}
	foreign, err := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims).SignedString(oauthKey.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := byjwt.ParseByJwt(t.Context(), foreign); err == nil {
		t.Fatal("OAuth signer acquired platform authority")
	}
	for _, missing := range []bool{false, true} {
		changed := *claims
		changed.ExpiresAt = gojwt.NewNumericDate(time.Unix(1, 0))
		if missing {
			changed.ExpiresAt = nil
		}
		if _, err := byjwt.ParseByJwt(t.Context(), changed.Sign()); err == nil {
			t.Fatalf("strict expiration boundary relaxed: missing=%t", missing)
		}
	}
	proxyId := server.NewId()
	signed := model.SignProxyId(proxyId)
	if parsed, err := model.ParseSignedProxyId(signed); err != nil || parsed != proxyId {
		t.Fatalf("real proxy signing resource failed: %v", err)
	}
	changed := "0" + signed[1:]
	if signed[0] == '0' {
		changed = "1" + signed[1:]
	}
	if _, err := model.ParseSignedProxyId(changed); err == nil {
		t.Fatal("changed proxy identity retained authority")
	}
	proxyConfig := model.LoadServerProxyConfig()
	if len(proxyConfig.Hosts) != 1 || len(proxyConfig.Hosts["proxy.fixture.example"]["fixture"]) != 5 || len(proxyConfig.Secrets) != 1 {
		t.Fatal("real proxy client allocator has no complete synthetic host block")
	}
	wgBytes, err := base64.StdEncoding.DecodeString(proxyConfig.Wg.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	wgKey, err := ecdh.X25519().NewPrivateKey(wgBytes)
	if err != nil || base64.StdEncoding.EncodeToString(wgKey.PublicKey().Bytes()) != proxyConfig.Wg.PublicKey {
		t.Fatalf("proxy WireGuard key pair differs: %v", err)
	}
	plain := []byte("synthetic private handoff")
	sealed := server.WgHandoffSeal(plain)
	opened, authenticated, err := server.WgHandoffOpen(sealed)
	if err != nil || !authenticated || !bytes.Equal(opened, plain) {
		t.Fatalf("real private handoff encryption failed: %v", err)
	}
	if _, _, err := server.WgHandoffOpen(sealed + "x"); err == nil {
		t.Fatal("changed handoff retained authority")
	}
	if server.ClientIpHashForAddr(netip.MustParseAddr("192.0.2.7")) == ([32]byte{}) {
		t.Fatal("client address hash lacks private key material")
	}
	settings := controller.VerifySettings()
	if len(settings.EgressHashKey) != 32 || settings.SeedRateHardLimit != 40 || settings.ExtendRateHardLimit != 400 || settings.ActiveTrailsHardLimit != 32 {
		t.Fatal("real observed-egress feeder settings lack finite private authority")
	}
	keys, err := controller.GetVerifyKeys(nil)
	if err != nil || len(keys.Keys) != 1 || keys.Keys[0].ServerKeyId != 0 || len(keys.Keys[0].PublicKey) != 32 {
		t.Fatalf("actual public proof-signing key loader failed: %v", err)
	}
}

// Replacing only the private key leaves the original kid wrong. The actual
// loader must refuse that mismatch, even though both keys are genuine P-256.
func TestReleaseGateServicesSuiteRejectsChangedOauthKey(t *testing.T) {
	if os.Getenv("RELEASE_GATE_SUITE_AUTH_CHILD") == "" {
		runReleaseGateSuiteAuthChild(t, "changed-oauth", func(root string) {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalECPrivateKey(key)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "vault", "fixture-oauth.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600); err != nil {
				t.Fatal(err)
			}
		})
		return
	}
	if os.Getenv("RELEASE_GATE_SUITE_AUTH_CHILD") != "changed-oauth" {
		t.Fatal("unexpected authentication child mode")
	}
	defer func() {
		failure := recover()
		if failure == nil || !strings.Contains(fmt.Sprint(failure), "thumbprint") {
			t.Errorf("changed OAuth key did not fail exact authority: %v", failure)
		}
	}()
	oauth.SigningKey()
}
