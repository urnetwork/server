package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Start a fresh test process so the real package initializer reads synthetic
// settings before any test can install overrides or suppress its diagnostics.
func TestEnvInitializerPrivacy(t *testing.T) {
	preparedGenerator := os.Getenv(releaseGateSuiteGeneratorEnvironment)
	if preparedGenerator == "" {
		t.Fatal("missing parent-prepared suite generator proof")
	}
	for _, scenario := range []string{"map", "invalid", "precedence", "absent"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			config := ""
			site := ""
			switch scenario {
			case "map":
				config = "all:\n  nested:\n    label: fakeMapLabelCanary\n    endpoint: https://fakeMapUrlCanary.invalid/?q=fakeMapQueryCanary\n    authorization: fakeMapAuthCanary\nprivacy-host:\n  env_vars:\n    INIT_PRIVACY_VALID: valid\n"
			case "invalid":
				config = "all:\n  env_vars:\n    fakeInvalidKeyCanary:\n      nested: fakeInvalidValueCanary\n    INIT_PRIVACY_VALID: valid\n"
			case "precedence":
				config = "all:\n  initializer_all: all\n  initializer_choice: all\n  initializer_host_choice: all\n  env_vars:\n    INIT_PRIVACY_ALL: all\nprivacy-host:\n  initializer_choice: host\n  initializer_host_choice: host\n  env_vars:\n    INIT_PRIVACY_HOST: host\n"
				site = "initializer_choice: site\ninitializer_site: site\nenv_vars:\n  INIT_PRIVACY_SITE: site\n"
			}
			for name, body := range map[string]string{"config": config, "site": site} {
				if body == "" {
					continue
				}
				home := filepath.Join(root, name)
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal("create synthetic settings directory")
				}
				if err := os.WriteFile(filepath.Join(home, "settings.yml"), []byte(body), 0600); err != nil {
					t.Fatal("write synthetic settings")
				}
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal("resolve child test executable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestEnvInitializerPrivacyChild$", "-test.count=1")
			child.WaitDelay = time.Second
			child.Env = []string{
				"HOME=" + root, "TMPDIR=" + root,
				"WARP_HOME=" + root, "WARP_ENV=privacy", "WARP_HOST=privacy-host",
				"WARP_CONFIG_HOME=" + filepath.Join(root, "config"),
				"WARP_SITE_HOME=" + filepath.Join(root, "site"),
				"WARP_VAULT_HOME=" + filepath.Join(root, "vault"),
				releaseGateSuiteGeneratorEnvironment + "=" + preparedGenerator,
				"BY_LOG_LOGTOSTDERR=true", "BY_LOG_STDERRTHRESHOLD=INFO", "BY_LOG_V=0",
				"URNETWORK_INITIALIZER_PRIVACY_CHILD=" + scenario,
			}
			// Retain only the qualified parent's tool/build environment. TestMain
			// validates the borrowed generator; no ambient settings roots are copied.
			for _, key := range []string{
				"PATH", "GOCACHE", "GOENV", "GOFLAGS", "GOMAXPROCS", "GOMODCACHE",
				"GOPROXY", "GOSUMDB", "GOTMPDIR", "GOTOOLCHAIN", "GOVCS", "GOWORK",
				"LANG", "LC_ALL", "TZ", "CC", "CXX", "CGO_ENABLED", "GOAMD64",
				"GOARCH", "GOOS", "GOROOT", "GOPATH", "GORACE",
			} {
				if value, ok := os.LookupEnv(key); ok {
					child.Env = append(child.Env, key+"="+value)
				}
			}
			stdout := &initializerPrivacyOutput{remaining: 64 * 1024}
			stderr := &initializerPrivacyOutput{remaining: 64 * 1024}
			child.Stdout, child.Stderr = stdout, stderr
			err = child.Run()
			if ctx.Err() != nil || err != nil || stdout.exceeded || stderr.exceeded {
				t.Fatal("synthetic initializer child did not complete within bounds")
			}
			for _, canary := range []string{"fakeMapLabelCanary", "fakeMapUrlCanary", "fakeMapQueryCanary", "fakeMapAuthCanary", "fakeInvalidKeyCanary", "fakeInvalidValueCanary"} {
				if bytes.Contains(stdout.Bytes(), []byte(canary)) || bytes.Contains(stderr.Bytes(), []byte(canary)) {
					t.Fatal("initializer exposed synthetic settings canary")
				}
			}
			if !strings.Contains(stderr.String(), "[env]settings loaded") {
				t.Fatal("missing code-owned settings diagnostic")
			}
			invalidDiagnostic := strings.Contains(stderr.String(), "[env]ignored non-string environment value")
			if invalidDiagnostic != (scenario == "invalid") {
				t.Fatal("invalid environment diagnostic did not match synthetic input")
			}
		})
	}
}

// Assert functionality only after init has consumed the synthetic files; do not
// print values, including on failure. The parent separately inspects both streams.
func TestEnvInitializerPrivacyChild(t *testing.T) {
	scenario := os.Getenv("URNETWORK_INITIALIZER_PRIVACY_CHILD")
	if scenario == "" {
		t.Skip("synthetic initializer child only")
	}
	settings := GetSettings()
	switch scenario {
	case "map", "invalid":
		if os.Getenv("INIT_PRIVACY_VALID") != "valid" {
			t.Fatal("valid environment assignment was not retained")
		}
		if _, ok := os.LookupEnv("fakeInvalidKeyCanary"); ok {
			t.Fatal("invalid environment value was assigned")
		}
	case "precedence":
		for key, expected := range map[string]string{
			"initializer_all": "all", "initializer_host_choice": "host",
			"initializer_choice": "site", "initializer_site": "site",
		} {
			if settings[key] != expected {
				t.Fatal("settings precedence or independent setting changed")
			}
		}
		if os.Getenv("INIT_PRIVACY_SITE") != "site" {
			t.Fatal("site environment assignment was not retained")
		}
		for _, key := range []string{"INIT_PRIVACY_ALL", "INIT_PRIVACY_HOST"} {
			if _, ok := os.LookupEnv(key); ok {
				t.Fatal("environment maps were merged instead of replaced")
			}
		}
	case "absent":
		if len(settings) != 0 {
			t.Fatal("absent settings did not remain empty")
		}
	default:
		t.Fatal("unknown synthetic initializer scenario")
	}
}

// Bound each child stream while retaining actual diagnostics for the canary
// assertions. An overrun fails the control rather than discarding log evidence.
type initializerPrivacyOutput struct {
	bytes.Buffer
	remaining int
	exceeded  bool
}

// Refuse bytes beyond the finite synthetic stream budget.
func (self *initializerPrivacyOutput) Write(data []byte) (int, error) {
	if len(data) > self.remaining {
		self.exceeded = true
		return 0, errors.New("synthetic initializer output limit")
	}
	self.remaining -= len(data)
	return self.Buffer.Write(data)
}
