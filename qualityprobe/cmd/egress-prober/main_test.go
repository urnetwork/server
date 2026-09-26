package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The secrets these tests plant in the environment. They are distinctive
// strings so a substring search over the process output is unambiguous.
const (
	testJwtSecret      = "eyJTOPSECRETJWT-do-not-print-me"
	testOperatorSecret = "SUPERSECRET-hunter2-do-not-print-me"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
	buildDir  string
)

// TestMain removes the directory holding the compiled prober. buildProber
// deliberately does not use t.TempDir (the binary is shared across tests and
// must outlive the first test's cleanup), so without this every run of this
// package leaves a ~36 MB binary behind in the system temp directory.
func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		// Reported rather than discarded: on Windows a still-locked .exe leaves
		// the ~36 MB directory behind, the exact thing this cleanup prevents.
		if err := os.RemoveAll(buildDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove the test build dir %s: %s\n", buildDir, err)
		}
	}
	os.Exit(code)
}

// buildProber compiles the CLI once per test binary and returns the path to
// the executable.
//
// These tests deliberately drive the real process rather than calling an
// internal helper: the defect they guard (secrets rendered into flag.Usage
// output) is a property of what the program writes to its own stderr, and
// asserting it end-to-end means the test is independent of how the flags
// happen to be wired internally -- it fails against the pre-fix code and
// keeps failing for any future refactor that reintroduces the leak by another
// route (e.g. logging the parsed values).
func buildProber(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			buildErr = err
			return
		}
		// Not t.TempDir(): the binary is shared across tests in this package
		// and must outlive the first test's cleanup.
		dir, err := os.MkdirTemp("", "egress-prober-test")
		if err != nil {
			buildErr = err
			return
		}
		// Windows exec refuses an extensionless path (LookPath only tries
		// PATHEXT extensions), so the binary must be named with .exe there.
		buildDir = dir
		bin := filepath.Join(dir, "egress-prober")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("go build output: %s", out)
			return
		}
		builtBin = bin
	})
	if buildErr != nil {
		t.Skipf("cannot build the prober binary in this environment: %s", buildErr)
	}
	return builtBin
}

// runProberWithSecretsInEnv runs the built binary with both secret env vars
// set and returns its combined stdout+stderr.
func runProberWithSecretsInEnv(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(buildProber(t), args...)
	cmd.Env = append(os.Environ(),
		"UR_PROBER_BY_JWT="+testJwtSecret,
		"UR_OPERATOR_SECRET="+testOperatorSecret,
	)
	out, err := cmd.CombinedOutput()
	// A non-zero exit is expected in these tests, but a process that never
	// ran is not: swallowing that error made the content assertions below
	// fail against empty output instead of naming the real problem.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("running the prober: %s", err)
	}
	return string(out)
}

func assertNoSecrets(t *testing.T, what string, output string) {
	t.Helper()
	if strings.Contains(output, testJwtSecret) {
		t.Errorf("%s printed UR_PROBER_BY_JWT verbatim; the env vars are documented as the way to keep secrets out of logs, and this lands in journald/CI logs.\n--- output ---\n%s", what, output)
	}
	if strings.Contains(output, testOperatorSecret) {
		t.Errorf("%s printed UR_OPERATOR_SECRET verbatim; the env vars are documented as the way to keep secrets out of logs, and this lands in journald/CI logs.\n--- output ---\n%s", what, output)
	}
}

// TestHelpDoesNotPrintSecrets is the regression test for the secret
// disclosure: the flags used to take os.Getenv(...) as their flag DEFAULT, and
// flag.PrintDefaults renders a non-zero default as `(default %q)`. Plain -h
// -- which an operator runs routinely -- therefore echoed both secrets to
// stderr.
func TestHelpDoesNotPrintSecrets(t *testing.T) {
	out := runProberWithSecretsInEnv(t, "-h")
	assertNoSecrets(t, "-h", out)

	// The help must still tell the operator the env vars are supported;
	// suppressing the values must not suppress the documentation.
	for _, name := range []string{"UR_PROBER_BY_JWT", "UR_OPERATOR_SECRET"} {
		if !strings.Contains(out, name) {
			t.Errorf("-h output does not mention %s; the env var is supported and must stay documented.\n--- output ---\n%s", name, out)
		}
	}
}

// TestMissingFlagUsageDoesNotPrintSecrets covers the other, wider trigger:
// any missing required flag prints usage too, and that path is hit whenever a
// deployment is misconfigured -- exactly when the output is most likely to be
// copied into a bug report.
//
// It also proves the env fallback still works: with both secrets supplied
// only via the environment, the "missing required flag(s)" line must name
// -api-url alone.
func TestMissingFlagUsageDoesNotPrintSecrets(t *testing.T) {
	out := runProberWithSecretsInEnv(t, "-platform-url", "wss://example.invalid")
	assertNoSecrets(t, "usage after a missing flag", out)

	missingLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "missing required flag") {
			missingLine = line
			break
		}
	}
	if missingLine == "" {
		t.Fatalf("expected a \"missing required flag(s)\" line.\n--- output ---\n%s", out)
	}
	if !strings.Contains(missingLine, "-api-url") {
		t.Errorf("missing-flag line %q should name -api-url", missingLine)
	}
	if strings.Contains(missingLine, "-by-jwt") || strings.Contains(missingLine, "-operator-secret") {
		t.Errorf("missing-flag line %q reports a secret flag as missing, but both were supplied via the environment; the env fallback is broken", missingLine)
	}
}

// Invalid blackhole controls used to fail in unsafe ways: zero concurrency
// deadlocked every worker on an unbuffered semaphore, a non-positive timeout
// removed the network deadline, and a negative interval silently disabled the
// only fleet-wide fast check. Reject each at startup instead of starting a
// healthy-looking but inert service.
func TestInvalidBlackholeFlagsAreRejected(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		value string
		want  string
	}{
		{name: "negative interval", flag: "-blackhole-interval", value: "-1s", want: "-blackhole-interval must not be negative"},
		{name: "zero concurrency", flag: "-blackhole-concurrency", value: "0", want: "-blackhole-concurrency must be positive"},
		{name: "zero timeout", flag: "-blackhole-timeout", value: "0", want: "-blackhole-timeout must be positive"},
		{name: "zero limit", flag: "-blackhole-limit", value: "0", want: "-blackhole-limit must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, code := runProber(t,
				"-skip-confinement-check",
				"-api-url", "http://127.0.0.1:1",
				"-platform-url", "ws://127.0.0.1:1",
				"-interval", "0",
				test.flag, test.value,
			)
			if code != 2 {
				t.Fatalf("%s=%s exited %d, want 2. output:\n%s", test.flag, test.value, code, out)
			}
			if !strings.Contains(out, test.want) {
				t.Fatalf("output does not contain %q:\n%s", test.want, out)
			}
		})
	}
}

// TestListProvidersErrorsWhenEveryLocationFetchFails: the per-location skip
// exists so one hiccup out of hundreds of locations cannot abort a pass, but
// when locations exist and EVERY find-providers2 call failed the enumeration
// accomplished nothing -- and returning ([], nil) there flowed into the
// "nothing to do" exit-0 carve-out, exactly the silent success the exit-code
// contract promises an external cron will never see. The two endpoints can
// genuinely diverge: provider-locations is GET with no auth while
// find-providers2 is an authenticated POST, so a broken route or a revoked
// jwt fails only the second.
func TestListProvidersErrorsWhenEveryLocationFetchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/network/provider-locations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"locations":[{"location_id":"loc-1"},{"location_id":"loc-2"}]}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	ids, err := listProviders(context.Background(), srv.URL, "jwt")
	if err == nil {
		t.Fatalf("listProviders = %v, <nil>: locations exist but every find-providers2 call failed, and that must be an error, not an empty success", ids)
	}
}

// TestListProvidersKeepsThePassWhenOneLocationFails: the resilience the skip
// was written for must survive the all-failed check above -- a partial
// enumeration is still the better outcome.
func TestListProvidersKeepsThePassWhenOneLocationFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/network/provider-locations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"locations":[{"location_id":"loc-bad"},{"location_id":"loc-good"}]}`))
		default:
			var got struct {
				Specs []map[string]string `json:"specs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&got)
			if len(got.Specs) == 1 && got.Specs[0]["location_id"] == "loc-bad" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"providers":[{"client_id":"p-1"}]}`))
		}
	}))
	defer srv.Close()

	ids, err := listProviders(context.Background(), srv.URL, "jwt")
	if err != nil {
		t.Fatalf("listProviders: %s; one failed location out of two must not abort the pass", err)
	}
	if len(ids) != 1 || ids[0] != "p-1" {
		t.Fatalf("listProviders = %v, want [p-1]", ids)
	}
}

// TestFindProvidersAtLocationRequestsForceMinimum is the regression test for
// the enumeration gap: find-providers2 routes candidates through
// loadClientScores, which drops any provider failing PassesMinimums -- a
// user-facing quality gate. A geolocation census wants every provider that
// can accept a contract, not only those meeting that bar. The server exposes
// FindProviders2Args.ForceMinimum (json "force_minimum") for exactly this,
// but the prober never set it, so on beta this returned 1 of 39 providers.
//
// The assertion decodes the actual JSON body rather than inspecting the Go
// request struct: the server only ever sees the JSON, so asserting on the
// struct would let a wrong or missing json tag pass silently.
func TestFindProvidersAtLocationRequestsForceMinimum(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %s", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}))
	defer srv.Close()

	_, err := findProvidersAtLocation(context.Background(), srv.Client(), srv.URL, "jwt", "loc-1")
	if err != nil {
		t.Fatalf("findProvidersAtLocation: %s", err)
	}

	// a geolocation census must not be filtered by the user-facing quality
	// gate; without this the api returned 1 of 39 providers on beta
	if got["force_minimum"] != true {
		t.Errorf("force_minimum = %v, want true", got["force_minimum"])
	}
}
