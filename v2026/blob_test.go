package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

func TestLocalBlobStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStore(root, "stats")
	ctx := context.Background()

	if store.Prefix() != "stats" {
		t.Fatalf("prefix = %q, want stats", store.Prefix())
	}
	if store.Bucket() != "local" {
		t.Fatalf("bucket = %q, want local", store.Bucket())
	}

	srcPath := filepath.Join(t.TempDir(), "seg.pb.zst")
	content := []byte("hello blob content")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	key := "stats/local/findproviders2/2026-07-19/inst/1234-0.pb.zst"
	if err := store.Put(ctx, key, srcPath, "application/zstd"); err != nil {
		t.Fatalf("put: %s", err)
	}

	objects, err := store.List(ctx, "stats/local/")
	if err != nil {
		t.Fatalf("list: %s", err)
	}
	if len(objects) != 1 || objects[0].Key != key || objects[0].Size != int64(len(content)) {
		t.Fatalf("list = %+v, want one object key=%s size=%d", objects, key, len(content))
	}

	reader, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %s", err)
	}
	got, _ := io.ReadAll(reader)
	reader.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("get content = %q, want %q", got, content)
	}

	// a non-matching prefix returns empty (not everything)
	if none, err := store.List(ctx, "stats/other/"); err != nil || len(none) != 0 {
		t.Fatalf("non-matching prefix: got %v err=%v, want empty", none, err)
	}

	// a partial (in-progress) write is never listed
	if err := os.WriteFile(store.(*localBlobStore).pathFor(key)+blobPartialSuffix, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if objs, _ := store.List(ctx, "stats/local/"); len(objs) != 1 {
		t.Fatalf("partial write leaked into list: %+v", objs)
	}

	// a non-existent root is empty, not an error
	if objs, err := NewLocalBlobStore(filepath.Join(root, "nope"), "").List(ctx, ""); err != nil || len(objs) != 0 {
		t.Fatalf("non-existent root: got %v err=%v, want empty", objs, err)
	}
}

func TestLocalBlobStoreReaper(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStore(root, "stats").(*localBlobStore)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := filepath.Join(t.TempDir(), "seg.pb.zst")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	expiredKey := "stats/local/findproviders2/2026-07-10/inst/1-0.pb.zst" // aged, has a rule -> reaped
	freshKey := "stats/local/findproviders2/2026-07-19/inst/2-0.pb.zst"   // recent, has a rule -> kept
	noRuleKey := "stats/local/otherstream/2026-07-10/inst/1-0.pb.zst"     // aged, no rule -> kept
	for _, k := range []string{expiredKey, freshKey, noRuleKey} {
		if err := store.Put(ctx, k, src, "application/zstd"); err != nil {
			t.Fatal(err)
		}
	}
	// age the two "old" objects to 48h ago (by mtime, which the reaper uses)
	old := time.Now().Add(-48 * time.Hour)
	for _, k := range []string{expiredKey, noRuleKey} {
		if err := os.Chtimes(store.pathFor(k), old, old); err != nil {
			t.Fatal(err)
		}
	}

	// findproviders2 keeps 24h; otherstream has no rule
	if err := store.SetLifecycle(ctx, []BlobLifecycleRule{
		{KeyPrefix: "stats/local/findproviders2/", TTL: 24 * time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	store.reapPass()

	if _, err := os.Stat(store.pathFor(expiredKey)); !os.IsNotExist(err) {
		t.Fatalf("expired findproviders2 object should be deleted (err=%v)", err)
	}
	if _, err := os.Stat(store.pathFor(freshKey)); err != nil {
		t.Fatalf("fresh findproviders2 object should be kept: %s", err)
	}
	if _, err := os.Stat(store.pathFor(noRuleKey)); err != nil {
		t.Fatalf("object of a stream with no rule should be kept: %s", err)
	}
}

func TestLocalBlobStoreCapacity(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "stats", 5)
	ctx := context.Background()

	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(first, []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("56"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Put(ctx, "stats/a", first, "application/octet-stream"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := store.Put(ctx, "stats/b", second, "application/octet-stream"); err == nil {
		t.Fatal("put exceeding aggregate cap succeeded")
	}
	if err := store.Put(ctx, "stats/a", replacement, "application/octet-stream"); err != nil {
		t.Fatalf("smaller replacement: %v", err)
	}
	if err := store.Put(ctx, "stats/b", second, "application/octet-stream"); err != nil {
		t.Fatalf("put after freeing capacity: %v", err)
	}
}

func TestLoadBlobStoreConfigBackendSelection(t *testing.T) {
	// explicit local backend
	cleanup := Vault.PushSimpleResource("minio.yml", []byte("authority: local\npath: /tmp/sim-blob\nprefix: stats\n"))
	config, present := LoadBlobStoreConfig()
	if !present {
		cleanup()
		t.Fatal("expected present")
	}
	if !config.Local || config.LocalPath != "/tmp/sim-blob" {
		cleanup()
		t.Fatalf("authority 'local' should select local backend at /tmp/sim-blob, got %+v", config)
	}
	if config.LocalMaxBytes != DefaultLocalBlobMaxBytes {
		t.Fatalf("local max bytes = %d, want %d", config.LocalMaxBytes, DefaultLocalBlobMaxBytes)
	}
	store, ok := LoadBlobStore()
	if !ok || store.Authority() != "local:/tmp/sim-blob" {
		cleanup()
		t.Fatalf("LoadBlobStore local: ok=%t authority=%q", ok, store.Authority())
	}
	cleanup()

	// a real authority selects MinIO (not local)
	cleanup = Vault.PushSimpleResource("minio.yml", []byte("authority: minio.example.com:9000\nbucket: stats\naccess_key: k\nsecret_key: s\n"))
	defer cleanup()
	config, present = LoadBlobStoreConfig()
	if !present {
		t.Fatal("expected present")
	}
	if config.Local {
		t.Fatal("a real authority should not select the local backend")
	}
	if config.Bucket != "stats" {
		t.Fatalf("bucket = %q, want stats", config.Bucket)
	}
}

// SetLifecycle must replace only code-owned rules (blobLifecycleRuleIdPrefix)
// and preserve every foreign rule on the bucket — ops-set ILM and other envs'
// rules survive a retention apply.
func TestMergeOwnedLifecycleRules(t *testing.T) {
	opsRule := lifecycle.Rule{
		ID:         "ops-backup-expiry",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "backups/"},
		Expiration: lifecycle.Expiration{Days: 30},
	}
	otherEnvRule := lifecycle.Rule{
		ID:         blobLifecycleRuleIdPrefix + "stats-other-findproviders2",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "stats/other/findproviders2/"},
		Expiration: lifecycle.Expiration{Days: 7},
	}
	staleOwnedRule := lifecycle.Rule{
		ID:         blobLifecycleRuleIdPrefix + "stats-main-findproviders2",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "stats/main/findproviders2/"},
		Expiration: lifecycle.Expiration{Days: 14},
	}
	existing := lifecycle.NewConfiguration()
	existing.Rules = []lifecycle.Rule{opsRule, otherEnvRule, staleOwnedRule}

	owned := []lifecycle.Rule{
		{
			ID:         blobLifecycleRuleIdPrefix + "stats-main-findproviders2",
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: "stats/main/findproviders2/"},
			Expiration: lifecycle.Expiration{Days: 7},
		},
	}

	merged := mergeOwnedLifecycleRules(existing, owned)

	// replacement is by exact ID: the ops rule and another env's code-owned
	// rule (different deterministic ID) both survive; only the rule being
	// rewritten is replaced
	ids := map[string]lifecycle.Rule{}
	for _, rule := range merged {
		ids[rule.ID] = rule
	}
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged rules, got %d", len(merged))
	}
	if _, ok := ids["ops-backup-expiry"]; !ok {
		t.Fatal("foreign (ops) rule must be preserved")
	}
	if _, ok := ids[blobLifecycleRuleIdPrefix+"stats-other-findproviders2"]; !ok {
		t.Fatal("another env's code-owned rule must be preserved")
	}
	updated, ok := ids[blobLifecycleRuleIdPrefix+"stats-main-findproviders2"]
	if !ok {
		t.Fatal("owned rule must be present")
	}
	if updated.Expiration.Days != 7 {
		t.Fatalf("owned rule must be replaced with the new TTL, got %d days", updated.Expiration.Days)
	}

	// a nil existing config merges to exactly the owned set
	merged = mergeOwnedLifecycleRules(nil, owned)
	if len(merged) != 1 || merged[0].ID != owned[0].ID {
		t.Fatal("nil existing config must merge to the owned rules")
	}
}

// resolveBlobAuthority: env interpolation per the vault convention plus the
// config settings routes mapping (hostname -> route ip), fail-safe on a
// missing env var.
func TestResolveBlobAuthority(t *testing.T) {
	routes := map[string]string{
		"test-minio-host": "192.168.1.3",
	}

	// plain passthrough (raw ip, not in routes)
	resolved, ok := resolveBlobAuthority("10.0.0.9:23900", routes)
	if !ok || resolved != "10.0.0.9:23900" {
		t.Fatalf("passthrough: %q %v", resolved, ok)
	}

	// hostname in routes maps to the route ip, port kept
	resolved, ok = resolveBlobAuthority("test-minio-host:23900", routes)
	if !ok || resolved != "192.168.1.3:23900" {
		t.Fatalf("routes: %q %v", resolved, ok)
	}

	// env interpolation then routes mapping
	t.Setenv("TEST_BLOB_MINIO_HOSTNAME", "test-minio-host")
	resolved, ok = resolveBlobAuthority("{{ env:TEST_BLOB_MINIO_HOSTNAME }}:23900", routes)
	if !ok || resolved != "192.168.1.3:23900" {
		t.Fatalf("env+routes: %q %v", resolved, ok)
	}

	// a missing env var disables the store instead of panicking
	_, ok = resolveBlobAuthority("{{ env:TEST_BLOB_MINIO_HOSTNAME_UNSET }}:23900", routes)
	if ok {
		t.Fatal("missing env var must resolve to not-ok")
	}

	// empty and "local" pass through for backend selection
	resolved, ok = resolveBlobAuthority("", routes)
	if !ok || resolved != "" {
		t.Fatalf("empty: %q %v", resolved, ok)
	}
}
