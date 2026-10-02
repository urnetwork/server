package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shadowRuntimeConfig() ArinShadowRuntimeConfig {
	return ArinShadowRuntimeConfig{RunId: NewId(), KeyHex: strings.Repeat("31", 32), ExpiresAt: NowUtc().Add(time.Minute), Capacity: 128}
}

func TestArinRuntimePrivateConfigRejectsUnsafeInputsAndExpiresDisabled(t *testing.T) {
	config := shadowRuntimeConfig()
	path := filepath.Join(t.TempDir(), "private.json")
	write := func() {
		b, _ := json.Marshal(config)
		if os.WriteFile(path, b, 0600) != nil {
			t.Fatal("write")
		}
	}
	write()
	if loaded, err := loadArinShadowRuntimeConfig(path); err != nil || loaded.RunId != config.RunId {
		t.Fatal("private valid config rejected")
	}
	if os.Chmod(path, 0644) != nil {
		t.Fatal("chmod")
	}
	if _, err := loadArinShadowRuntimeConfig(path); err == nil {
		t.Fatal("public credential file accepted")
	}
	os.Chmod(path, 0600)
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(path, link)
	if _, err := loadArinShadowRuntimeConfig(link); err == nil {
		t.Fatal("config symlink accepted")
	}
	config.CandidateResource = "../secret"
	write()
	if _, err := loadArinShadowRuntimeConfig(path); err == nil {
		t.Fatal("resource traversal accepted")
	}
	config.CandidateResource = ""
	config.ExpiresAt = NowUtc().Add(5 * time.Hour)
	write()
	if _, err := loadArinShadowRuntimeConfig(path); err == nil {
		t.Fatal("unbounded lease accepted")
	}
	config.ExpiresAt = NowUtc().Add(-time.Second)
	write()
	if loaded, err := loadArinShadowRuntimeConfig(path); err != nil || loaded != nil {
		t.Fatal("expired lease did not disable itself")
	}
	if os.WriteFile(path, []byte(strings.Repeat(" ", 16385)), 0600) != nil {
		t.Fatal("write")
	}
	if _, err := loadArinShadowRuntimeConfig(path); err == nil {
		t.Fatal("oversized credential input accepted")
	}
}

func TestArinRuntimeUnixLifecycleJoinsAndLeavesNoCandidateWorkWhenDisabled(t *testing.T) {
	var opened, closed atomic.Int64
	entered := make(chan struct{})
	factory := func(context.Context) (ArinShadowRPCHandler, func(), error) {
		opened.Add(1)
		return func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}, func() { closed.Add(1) }, nil
	}
	if runtime, err := StartArinShadowRuntime(context.Background(), nil, "connect", factory); err != nil || runtime != nil || opened.Load() != 0 {
		t.Fatal("disabled capture did work")
	}
	base, err := os.MkdirTemp("", "arin-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	config := shadowRuntimeConfig()
	config.Directory = base
	runtime, err := startArinShadowRuntime(context.Background(), &config, "connect", factory, sourceBuildInfo{revision: strings.Repeat("d", 40)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	dirs, _ := os.ReadDir(base)
	if len(dirs) != 1 {
		t.Fatal("private endpoint missing")
	}
	dir := filepath.Join(base, dirs[0].Name())
	for _, name := range []string{"capture.sock", "identity.json"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("endpoint permission")
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	var identity ArinShadowRPCIdentity
	if json.Unmarshal(b, &identity) != nil {
		t.Fatal("identity")
	}
	key := [32]byte{}
	for i := range key {
		key[i] = 0x31
	}
	client, _ := NewArinShadowRPCClient(config.RunId, key, identity, ArinShadowUnixRoundTrip(filepath.Join(dir, "capture.sock")))
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out struct{}
		_ = client.Call(context.Background(), "capture", struct{}{}, &out)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not enter")
	}
	runtime.Close()
	<-done
	if opened.Load() != 1 || closed.Load() != 1 {
		t.Fatal("runtime did not join cleanup")
	}
	if dirs, _ := os.ReadDir(base); len(dirs) != 0 {
		t.Fatal("private endpoint survived close")
	}
}
