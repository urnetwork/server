package connect

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func privateHeapConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", home)
	t.Setenv("WARP_ENV", "test")
	return home
}

func TestPrivateHeapConfigAbsentAndDisabled(t *testing.T) {
	home := privateHeapConfigHome(t)
	for _, raw := range []string{"", "target: disabled\n", "target: ''\n", "{}\n"} {
		if raw != "" {
			if err := os.WriteFile(filepath.Join(home, privateHeapProfileConfigResource), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if target, err := privateHeapProfileStartupTarget(""); err != nil || target != "" {
			t.Fatalf("default-off resource: target=%q err=%v", target, err)
		}
	}
}

func TestPrivateHeapConfigExactInstanceOnly(t *testing.T) {
	home := privateHeapConfigHome(t)
	path := filepath.Join(home, privateHeapProfileConfigResource)
	raw := []byte("target: by-us-fmt-5-edge-4/g3\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	target, err := privateHeapProfileStartupTarget("")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"} {
		for _, block := range []string{"beta", "g1", "g2", "g3", "g4"} {
			t.Setenv("WARP_HOST", host)
			t.Setenv("WARP_BLOCK", block)
			settings := exchangeSettingsForRun(RunOptions{PrivateHeapProfileTarget: target})
			want := host == "by-us-fmt-5-edge-4" && block == "g3"
			if (settings.SDKPayloadOwnerLedger != nil) != want {
				t.Fatalf("incorrect accounting scope for %s/%s", host, block)
			}
			if !want {
				profile, err := startPrivateHeapProfile(context.Background(), target, nil, nil)
				if err != nil || profile != nil {
					t.Fatalf("non-target opened private profile: %s/%s", host, block)
				}
			}
		}
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
		t.Fatal("startup lookup changed the resource")
	}
}

func TestPrivateHeapConfigExplicitOverrideAndStartupSnapshot(t *testing.T) {
	home := privateHeapConfigHome(t)
	path := filepath.Join(home, privateHeapProfileConfigResource)
	if err := os.WriteFile(path, []byte("target: by-us-fmt-5-edge-4/g3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for explicit, want := range map[string]string{"disabled": "", "by-us-fmt-5-edge-3/g2": "by-us-fmt-5-edge-3/g2"} {
		if target, err := privateHeapProfileStartupTarget(explicit); err != nil || target != want {
			t.Fatalf("explicit override: target=%q err=%v", target, err)
		}
	}
	target, err := privateHeapProfileStartupTarget("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("target: disabled\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_HOST", "by-us-fmt-5-edge-4")
	t.Setenv("WARP_BLOCK", "g3")
	if exchangeSettingsForRun(RunOptions{PrivateHeapProfileTarget: target}).SDKPayloadOwnerLedger == nil {
		t.Fatal("later Config edit changed the captured startup target")
	}
	if next, err := privateHeapProfileStartupTarget(""); err != nil || next != "" {
		t.Fatal("next startup did not observe disabled Config")
	}
}

func TestPrivateHeapConfigRefusesInvalidAndUnavailable(t *testing.T) {
	home := privateHeapConfigHome(t)
	path := filepath.Join(home, privateHeapProfileConfigResource)
	for _, raw := range []string{
		"target: by-us-fmt-5-edge-5/g3\n", "target: by-us-fmt-5-edge-4/beta\n",
		"target: by-us-fmt-5-edge-4/g3/extra\n", "target: ['by-us-fmt-5-edge-4/g3']\n",
		"target: by-us-fmt-5-edge-4/g3\ntarget: disabled\n", "unknown: true\n",
		"target: by-us-fmt-5-edge-4/g3\n---\ntarget: disabled\n", strings.Repeat("#", 4097),
	} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if target, err := privateHeapProfileStartupTarget(""); err == nil || target != "" {
			t.Fatal("invalid resource enabled diagnostics")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if target, err := privateHeapProfileStartupTarget(""); err == nil || target != "" {
		t.Fatal("unavailable resource became a target")
	}
	if target, err := privateHeapProfileStartupTarget("disabled"); err != nil || target != "" {
		t.Fatal("explicit disable consulted invalid Config")
	}
}

func TestPrivateHeapConfigFailurePreservesPrimaryStartup(t *testing.T) {
	home := privateHeapConfigHome(t)
	if err := os.WriteFile(filepath.Join(home, privateHeapProfileConfigResource), []byte("target: invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"WARP_VERSION": "2026.10.5+1", "WARP_HOST": "by-us-fmt-5-edge-4", "WARP_SERVICE": "connect", "WARP_BLOCK": "g3",
		"WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080",
	} {
		t.Setenv(key, value)
	}
	checks, serves := 0, 0
	err := runWithDependencies(context.Background(), RunOptions{Port: 8080},
		func(context.Context) error { checks++; return errors.New("local readiness sentinel") },
		func(context.Context) func() { t.Fatal("not-ready fixture published metrics"); return nil },
		func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
			serves++
			return nil
		},
	)
	if err != nil || checks != 1 || serves != 1 {
		t.Fatalf("optional config blocked primary startup: err=%v checks=%d serves=%d", err, checks, serves)
	}
}
