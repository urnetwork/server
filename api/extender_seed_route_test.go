package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

const testExtenderSeedRouteHost = "ur.example"

func installTestExtenderSeedRouteConfig(t *testing.T, seed []byte, publicKey ed25519.PublicKey, enabled bool) *controller.ExtenderConfig {
	t.Helper()
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(fmt.Sprintf(
		"root_private_key_hex: %s\nroot_public_keys_hex:\n  - %s\nnetwork_host: %s\nseed:\n  enabled: %t\n",
		connect.ExtenderKeySeedHex(seed),
		hex.EncodeToString(publicKey),
		testExtenderSeedRouteHost,
		enabled,
	)))
	controller.Testing_ResetExtenderConfig()
	controller.Testing_ResetExtenderSeedOrigin()
	t.Cleanup(func() {
		pop()
		controller.Testing_ResetExtenderConfig()
		controller.Testing_ResetExtenderSeedOrigin()
	})
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// The origin every seed mirror copies, through the production route table
// (connect/EXTENDER-UPDATE2.md §2.3): anyone may fetch it with no credential,
// byte for byte what the publisher stored, under the mirror contract's headers.
// Only the exact path is the bundle, and an off seed answers 404.
func TestExtenderSeedRouteServesTheStoredBundleWithoutACredential(t *testing.T) {
	server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1<<20)
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	config := installTestExtenderSeedRouteConfig(t, seed, publicKey, true)

	issueTime := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	record, _, err := controller.SignExtenderRecord(config, rootPrivateKey, &model.NetworkExtender{
		ExtenderId: server.NewId(),
		PublicKey:  ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey),
		TcpPort:    443,
		Active:     true,
	}, []*model.NetworkExtenderAddress{{
		IpVersion: 4,
		Ip:        netip.MustParseAddr("203.0.113.7"),
		Carriers:  []string{connect.ExtenderCarrierTcp},
		Active:    true,
	}}, issueTime)
	if err != nil {
		t.Fatal(err)
	}
	body, err := connect.NewExtenderSeedBody(
		testExtenderSeedRouteHost,
		uint64(issueTime.UnixMilli()),
		issueTime,
		issueTime.Add(controller.ExtenderRecordExpireTimeout),
		[]*protocol.ExtenderRecord{record},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := connect.SignExtenderSeed(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := connect.MarshalExtenderSeedEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("no blob store")
	}
	stage := filepath.Join(t.TempDir(), "seed.bin")
	if err := os.WriteFile(stage, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), controller.ExtenderSeedLatestKey(store, testExtenderSeedRouteHost), stage, controller.ExtenderSeedContentType); err != nil {
		t.Fatal(err)
	}

	handler := router.NewRouter(t.Context(), Routes())
	serve := func(method string, path string, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, nil)
		for name, value := range header {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	digest := sha256.Sum256(artifact)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`

	// no credential, the exact bytes, the contract's headers
	response := serve(http.MethodGet, controller.ExtenderSeedPath, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", controller.ExtenderSeedPath, response.Code, response.Body.String())
	}
	if !bytes.Equal(response.Body.Bytes(), artifact) {
		t.Fatalf("the route did not serve the stored bytes")
	}
	for name, want := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"Cache-Control":          "public, max-age=300",
		"ETag":                   etag,
		"X-Content-Type-Options": "nosniff",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("a first-party origin redirected to %q", location)
	}

	// a cache's revalidation
	if response := serve(http.MethodGet, controller.ExtenderSeedPath, map[string]string{"If-None-Match": etag}); response.Code != http.StatusNotModified || response.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d with %d bytes, want 304 and none", response.Code, response.Body.Len())
	}

	// an escaped dot and an exact path: nothing else reaches the bundle
	for _, path := range []string{"/v1/seedXbin", "/v1/seed.bin/", "/v1/seed.bin/x", "/v1/seed", "/v1/"} {
		if response := serve(http.MethodGet, path, nil); response.Code == http.StatusOK || bytes.Equal(response.Body.Bytes(), artifact) {
			t.Errorf("GET %s: %d reached the bundle", path, response.Code)
		}
	}

	// off, the origin answers 404 even with a bundle stored
	installTestExtenderSeedRouteConfig(t, seed, publicKey, false)
	if response := serve(http.MethodGet, controller.ExtenderSeedPath, nil); response.Code != http.StatusNotFound {
		t.Fatalf("an off seed: %d, want 404", response.Code)
	}
}
