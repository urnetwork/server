package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

func TestRunServesWhenOptionalCaptureIsUnavailable(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("test build information unavailable")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
			t.Fatal("control requires -buildvcs=false")
		}
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		for _, which := range []string{"enabled_without_vcs", "unsafe_private_config"} {
			t.Run(which, func(t *testing.T) {
				for key, value := range map[string]string{"WARP_SERVICE": "connect", "WARP_BLOCK": "g1", "WARP_HOST": "fixture-host", "WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080,5080:0"} {
					t.Setenv(key, value)
				}
				t.Cleanup(router.SetWarpStatusReady)
				dir := t.TempDir()
				config := server.ArinShadowRuntimeConfig{RunId: server.NewId(), KeyHex: strings.Repeat("31", 32), Directory: filepath.Join(dir, "endpoints"), ExpiresAt: server.NowUtc().Add(time.Hour), Capacity: 128}
				data, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "capture.json")
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if which == "unsafe_private_config" {
					if err = os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("ARIN_SHADOW_CAPTURE_CONFIG", path)
				if capture, err := startArinShadowCaptureRuntime(context.Background()); err == nil || capture != nil {
					t.Fatal("capture no longer refuses unqualified startup")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				serves, stats := 0, 0
				err = runWithDependencies(ctx, RunOptions{Port: 8080}, func(context.Context) error { return nil }, func(context.Context) func() { stats++; return func() {} }, func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
					serves++
					cancel()
					return nil
				})
				if err != nil || serves != 1 || stats != 1 {
					t.Fatalf("optional capture blocked primary serving: err=%v serves=%d stats=%d", err, serves, stats)
				}
				if _, err = os.Stat(config.Directory); !os.IsNotExist(err) {
					t.Fatal("unavailable capture created endpoint work")
				}
			})
		}
	})
}
