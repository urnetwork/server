package taskworker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

func TestRunStartsWorkerWhenOptionalCaptureIsUnavailable(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("test build information unavailable")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
			t.Fatal("control requires -buildvcs=false")
		}
	}
	for _, which := range []string{"enabled_without_vcs", "unsafe_private_config"} {
		t.Run(which, func(t *testing.T) {
			for key, value := range map[string]string{"WARP_ENV": "test", "WARP_VERSION": "2026.10.4+1", "WARP_HOST": "fixture-host", "WARP_SERVICE": "taskworker", "WARP_BLOCK": "g1", "WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080"} {
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
			loaded, err := server.LoadArinShadowRuntimeConfig()
			if runtime.GOOS != "linux" {
				// Protected capture is deliberately disabled outside Linux. The
				// primary worker must still start; Linux admission remains a
				// separate required check on a Linux host.
				t.Logf("UNSUPPORTED_HOST: %s has no protected capture runtime; verifying disabled capture and primary-worker startup only", runtime.GOOS)
				if err != nil || loaded != nil {
					t.Fatal("unsupported host enabled optional capture", loaded, err)
				}
				capture, startErr := server.StartArinShadowRuntime(context.Background(), &config, "native", func(context.Context) (server.ArinShadowRPCHandler, func(), error) {
					t.Fatal("unsupported host ran the capture factory")
					return nil, nil, nil
				})
				if capture != nil || !errors.Is(startErr, server.ErrArinShadowInput) {
					t.Fatal("unsupported host accepted configured capture", capture, startErr)
				}
			} else if which == "enabled_without_vcs" {
				if err != nil || loaded == nil {
					t.Fatal("fresh capture fixture not enabled")
				}
				runtime, startErr := server.StartArinShadowRuntime(context.Background(), loaded, "native", func(context.Context) (server.ArinShadowRPCHandler, func(), error) {
					t.Fatal("unqualified capture factory ran")
					return nil, nil, nil
				})
				if startErr == nil || runtime != nil {
					t.Fatal("capture lost strict unavailable-source refusal")
				}
			} else if err == nil {
				t.Fatal("unsafe config no longer refused")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runtime := &taskworkerLifecycleRuntime{drainStarted: make(chan struct{}), handbackDone: make(chan struct{})}
			started := make(chan struct{})
			starts, serves, stats := 0, 0, 0
			err = runWithDependencies(ctx, RunOptions{Port: 8080, Count: 1, BatchSize: 1}, func(context.Context) error { return nil },
				func(context.Context) func() { stats++; return func() {} },
				func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
					<-started
					serves++
					cancel()
					<-runtime.drainStarted
					<-runtime.handbackDone
					return nil
				},
				func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
					starts++
					close(started)
					return runtime, nil
				})
			if err != nil || starts != 1 || serves != 1 || stats != 1 {
				t.Fatalf("optional capture blocked primary runtime: err=%v starts=%d serves=%d stats=%d", err, starts, serves, stats)
			}
			if _, err = os.Stat(config.Directory); !os.IsNotExist(err) {
				t.Fatal("unavailable capture created endpoint work")
			}
		})
	}
}
