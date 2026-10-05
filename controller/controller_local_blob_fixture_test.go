package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server"
)

// Resolve only newly owned positive fixture roots. Runtime custody validation
// still receives its original path and rejects intentional symlinks unchanged.
func providerWorkPhysicalTempDir(t testing.TB) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// Select the actual implicit dev-local backend, never the configured-local
// Linux durable-volume backend. Each fixture owns a private physical site and
// vault; it cannot inherit a cloud endpoint or write into the caller's site.
//
// TestEnv already owns active pg/redis resource overrides. Preserve only their
// on-disk baseline resources so its teardown can reconnect to the backing local
// services after popping those overrides. Do not copy minio or other credentials.
func controllerUseLocalBlobStore(t testing.TB) string {
	t.Helper()
	if env, err := server.Env(); err != nil || env != "local" {
		t.Fatal("controller blob fixture requires the local test environment", err)
	}
	root := providerWorkPhysicalTempDir(t)
	vaultRoot, siteRoot := filepath.Join(root, "vault"), filepath.Join(root, "site")
	for _, directory := range []string{vaultRoot, siteRoot} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{server.DefaultPgVaultResourceName, server.MaintenancePgVaultResourceName, "redis.yml"} {
		path, err := server.Vault.ResourcePath(name)
		if errors.Is(err, server.ErrResourceNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vaultRoot, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Real SDK admission hashes its synthetic endpoint IPs. Keep that owner
	// local too, without copying the caller's client identity material.
	if err := os.WriteFile(filepath.Join(vaultRoot, "client.yml"), []byte("client_ip_hash_pepper: provider-work-public-test-pepper\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_VAULT_HOME", vaultRoot)
	t.Setenv("WARP_SITE_HOME", siteRoot)
	// A lingering in-process override must fail the fixture rather than select
	// an external endpoint despite the private vault directory.
	if _, err := server.Vault.SimpleResource("minio.yml"); !errors.Is(err, server.ErrResourceNotFound) {
		t.Fatal("controller fixture inherited a blob configuration", err)
	}
	return filepath.Join(siteRoot, "blob")
}

// An inherited cloud configuration is a trap, not an available fallback. Both
// newly loaded store instances must retain exact bytes in this fixture's root.
func TestProviderWorkFixtureLocalBlobStoreIsPrivateAndColdReadable(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	outside := providerWorkPhysicalTempDir(t)
	vaultRoot, siteRoot := filepath.Join(outside, "vault"), filepath.Join(outside, "site")
	for _, directory := range []string{vaultRoot, siteRoot} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var cloudCalls atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls.Add(1)
		http.Error(w, "unexpected cloud access", http.StatusInternalServerError)
	}))
	t.Cleanup(cloud.Close)
	configuration := fmt.Sprintf("authority: %s\nbucket: external\naccess_key: synthetic\nsecret_key: synthetic\n", strings.TrimPrefix(cloud.URL, "http://"))
	if err := os.WriteFile(filepath.Join(vaultRoot, "minio.yml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	baseline := []byte("authority: local-baseline.invalid:1\n")
	for _, name := range []string{server.DefaultPgVaultResourceName, server.MaintenancePgVaultResourceName, "redis.yml"} {
		if err := os.WriteFile(filepath.Join(vaultRoot, name), baseline, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WARP_VAULT_HOME", vaultRoot)
	t.Setenv("WARP_SITE_HOME", siteRoot)
	if config, present := server.LoadBlobStoreConfig(); !present || config.Local || config.Authority != strings.TrimPrefix(cloud.URL, "http://") {
		t.Fatal("inherited cloud configuration was not exercised")
	}
	active := []byte("authority: active-fixture.invalid:2\n")
	popActive := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, active)
	t.Cleanup(popActive)
	blobRoot := controllerUseLocalBlobStore(t)
	if blobRoot == filepath.Join(siteRoot, "blob") || blobRoot != filepath.Join(server.SiteHomeRoot(), "blob") {
		t.Fatal("fixture did not own a private blob root", blobRoot)
	}
	physicalSite, err := filepath.EvalSymlinks(server.SiteHomeRoot())
	if err != nil || physicalSite != server.SiteHomeRoot() {
		t.Fatal("fixture site root is not physical", err)
	}
	if config, present := server.LoadBlobStoreConfig(); present || config != nil {
		t.Fatal("fixture retained an explicit blob configuration")
	}
	if got := server.Vault.RequireSimpleResource("client.yml").RequireString("client_ip_hash_pepper"); got != "provider-work-public-test-pepper" {
		t.Fatal("fixture inherited external client identity material")
	}
	resource := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
	if got := resource.RequireString("authority"); got != "active-fixture.invalid:2" {
		t.Fatal("fixture replaced the active database owner", got)
	}
	popActive()
	resource = server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
	if got := resource.RequireString("authority"); got != "local-baseline.invalid:1" {
		t.Fatal("fixture did not retain the teardown database baseline", got)
	}
	store, ok := server.LoadBlobStore()
	if !ok || store.Authority() != "local:"+blobRoot || store.Bucket() != "local" {
		t.Fatal("fixture did not load its actual dev-local backend")
	}
	raw := []byte("synthetic original provider-work evidence\n")
	source := filepath.Join(providerWorkPhysicalTempDir(t), "original")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	key := store.Prefix() + "/provider-work/original"
	if created, err := store.PutIfAbsent(t.Context(), key, source, "application/octet-stream"); err != nil || !created {
		t.Fatal("fixture could not retain the original", created, err)
	}
	cold, ok := server.LoadBlobStore()
	if !ok || cold.Authority() != store.Authority() {
		t.Fatal("cold reader lost its exact local owner")
	}
	reader, err := cold.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	readback, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(raw, readback) {
		t.Fatal("cold reader changed retained bytes", err)
	}
	if created, err := cold.PutIfAbsent(t.Context(), key, source, "application/octet-stream"); err != nil || created {
		t.Fatal("cold writer replaced the retained original", created, err)
	}
	if disk, err := os.ReadFile(filepath.Join(blobRoot, key)); err != nil || !bytes.Equal(disk, raw) {
		t.Fatal("retained bytes are outside the owned fixture root", err)
	}
	if calls := cloudCalls.Load(); calls != 0 {
		t.Fatal("fixture contacted the inherited cloud endpoint", calls)
	}
	if entries, err := os.ReadDir(siteRoot); err != nil || len(entries) != 0 {
		t.Fatal("fixture wrote to the caller's site", err)
	}
}

// The local testing substitute must not bless an undeclared production root.
func TestProviderWorkFixtureDoesNotBypassExplicitDurability(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", providerWorkPhysicalTempDir(t))
	root := controllerUseLocalBlobStore(t)
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf("authority: local\npath: %q\n", root)))
	defer pop()
	if config, ok := server.LoadBlobStoreConfig(); !ok || !config.Local || config.LocalPath != root {
		t.Fatal("explicit local configuration was not exercised")
	}
	if store, ok := server.LoadBlobStore(); ok || store != nil {
		t.Fatal("explicit local root without durable declaration was admitted")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected durable configuration provisioned an implicit root", err)
	}
}
