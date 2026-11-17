// Tests for `sidecarctl abuse production` and `sidecarctl abuse transparency-report` against an httptest route: each
// posts its kind with the bearer from its file, a production lands in a new 0600 file, a report prints, a refusal
// says the route's status, and no bearer reaches any output.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A route that records what each request posted and answers what a test sets.
type testAbuseStaffRoute struct {
	stateLock   sync.Mutex
	bodies      []map[string]any
	bearers     []string
	status      int
	answerBytes []byte
}

// Records the request and answers the set status, or the set answer.
func (self *testAbuseStaffRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	body := map[string]any{}
	bodyBytes, _ := io.ReadAll(r.Body)
	json.Unmarshal(bodyBytes, &body)
	self.bodies = append(self.bodies, body)
	self.bearers = append(self.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if r.URL.Path != "/abuse/action" || r.Method != http.MethodPost {
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	if self.status != http.StatusOK {
		http.Error(w, "The production lacks a sign-off.", self.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(self.answerBytes)
}

// A route answering result for kind, and a bearer file holding bearer.
func newTestAbuseStaffRoute(t *testing.T, kind string, result any) (*testAbuseStaffRoute, *httptest.Server, string) {
	t.Helper()
	answerBytes, err := json.Marshal(map[string]any{"kind": kind, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	route := &testAbuseStaffRoute{status: http.StatusOK, answerBytes: answerBytes}
	server := httptest.NewServer(route)
	t.Cleanup(server.Close)
	bearerPath := filepath.Join(t.TempDir(), "bearer")
	if err := os.WriteFile(bearerPath, []byte("synthetic-staff-bearer-0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return route, server, bearerPath
}

// A production posts kind produce with the request's id and the bearer from its file, lands in a new 0600 file as the
// route answered it, and prints only its section count; a second run onto the same file is refused.
func TestAbuseProductionWritesTheFile(t *testing.T) {
	production := map[string]any{
		"version":    1,
		"request_id": "11111111-1111-1111-1111-111111111111",
		"sections":   []any{map[string]any{"name": "accounts", "records": []any{map[string]any{"kind": "network"}}}, map[string]any{"name": "identities"}},
	}
	route, server, bearerPath := newTestAbuseStaffRoute(t, "produce", production)
	outPath := filepath.Join(t.TempDir(), "production.json")
	out := &bytes.Buffer{}
	args := []string{"-api=" + server.URL, "-request=11111111-1111-1111-1111-111111111111", "-out=" + outPath, "-bearer-file=" + bearerPath}
	if err := runAbuseProductionWith(context.Background(), args, out, defaultAbuseStaffClientSettings()); err != nil {
		t.Fatal(err)
	}
	if len(route.bodies) != 1 || route.bodies[0]["kind"] != "produce" || route.bodies[0]["request_id"] != "11111111-1111-1111-1111-111111111111" || route.bearers[0] != "synthetic-staff-bearer-0123" {
		t.Fatalf("posted %v with %v", route.bodies, route.bearers)
	}
	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	readBack := map[string]any{}
	if err := json.Unmarshal(written, &readBack); err != nil || readBack["request_id"] != production["request_id"] {
		t.Fatalf("the file holds %s", written)
	}
	info, err := os.Stat(outPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the file's mode %v %v", info.Mode(), err)
	}
	if !strings.Contains(out.String(), "2 sections") || strings.Contains(out.String(), `"kind"`) || strings.Contains(out.String(), "synthetic-staff-bearer") {
		t.Fatalf("printed %q", out.String())
	}
	if err := runAbuseProductionWith(context.Background(), args, &bytes.Buffer{}, defaultAbuseStaffClientSettings()); err == nil || len(route.bodies) != 1 {
		t.Fatalf("a second run onto the file: %v, %d posts", err, len(route.bodies))
	}
}

// A report posts transparency_counts over the window in milliseconds and prints the counts.
func TestAbuseTransparencyReportPrintsTheCounts(t *testing.T) {
	route, server, bearerPath := newTestAbuseStaffRoute(t, "transparency_counts", map[string]any{"request_count": 3})
	t.Setenv(abuseStaffBearerFileEnv, bearerPath)
	out := &bytes.Buffer{}
	args := []string{"-api=" + server.URL + "/", "-from=2026-01-01T00:00:00Z", "-to=2026-07-01T00:00:00Z"}
	if err := runAbuseTransparencyReportWith(context.Background(), args, out, defaultAbuseStaffClientSettings()); err != nil {
		t.Fatal(err)
	}
	body := route.bodies[0]
	if body["kind"] != "transparency_counts" || body["from_time_ms"] != float64(1767225600000) || body["to_time_ms"] != float64(1782864000000) {
		t.Fatalf("posted %v", body)
	}
	if !strings.Contains(out.String(), `"request_count": 3`) {
		t.Fatalf("printed %q", out.String())
	}
}

// The parser's refusals, each a usage error and none a post: no -api, no bearer file, a missing flag, a window
// backwards, a positional; and a bearer as an argument is no flag the commands take.
func TestAbuseStaffCommandsRefuseWhatTheyCannotTake(t *testing.T) {
	route, server, bearerPath := newTestAbuseStaffRoute(t, "produce", map[string]any{"sections": []any{}})
	t.Setenv(abuseStaffBearerFileEnv, "")
	outPath := filepath.Join(t.TempDir(), "production.json")
	for name, args := range map[string][]string{
		"no api":             {"-request=x", "-out=" + outPath, "-bearer-file=" + bearerPath},
		"an api with no URL": {"-api=api.example", "-request=x", "-out=" + outPath, "-bearer-file=" + bearerPath},
		"no bearer file":     {"-api=" + server.URL, "-request=x", "-out=" + outPath},
		"no request":         {"-api=" + server.URL, "-out=" + outPath, "-bearer-file=" + bearerPath},
		"no out":             {"-api=" + server.URL, "-request=x", "-bearer-file=" + bearerPath},
		"a positional":       {"-api=" + server.URL, "-request=x", "-out=" + outPath, "-bearer-file=" + bearerPath, "extra"},
		"a bearer argument":  {"-api=" + server.URL, "-request=x", "-out=" + outPath, "-bearer=synthetic-staff-bearer-0123"},
	} {
		err := runAbuseProductionWith(context.Background(), args, &bytes.Buffer{}, defaultAbuseStaffClientSettings())
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, args := range map[string][]string{
		"no window":         {"-api=" + server.URL, "-bearer-file=" + bearerPath},
		"a window backward": {"-api=" + server.URL, "-bearer-file=" + bearerPath, "-from=2026-07-01T00:00:00Z", "-to=2026-01-01T00:00:00Z"},
	} {
		err := runAbuseTransparencyReportWith(context.Background(), args, &bytes.Buffer{}, defaultAbuseStaffClientSettings())
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(route.bodies) != 0 {
		t.Fatalf("a refused command posted %d times", len(route.bodies))
	}
}

// A refusal names the route's status and message and never the bearer, and writes no file.
func TestAbuseProductionRefusalNamesTheStatus(t *testing.T) {
	route, server, bearerPath := newTestAbuseStaffRoute(t, "produce", nil)
	route.status = http.StatusConflict
	outPath := filepath.Join(t.TempDir(), "production.json")
	err := runAbuseProductionWith(context.Background(), []string{"-api=" + server.URL, "-request=x", "-out=" + outPath, "-bearer-file=" + bearerPath}, &bytes.Buffer{}, defaultAbuseStaffClientSettings())
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "sign-off") || strings.Contains(err.Error(), "synthetic-staff-bearer") {
		t.Fatalf("the refusal: %v", err)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("a refused production wrote its file: %v", statErr)
	}
	if command, rest := sidecarctlCommands.find([]string{"abuse", "production", "-request=x"}); command == nil || command.words != "abuse production" || len(rest) != 1 {
		t.Fatal("the binary does not hold abuse production")
	}
	if command, _ := sidecarctlCommands.find([]string{"abuse", "transparency-report"}); command == nil || command.words != "abuse transparency-report" {
		t.Fatal("the binary does not hold abuse transparency-report")
	}
}
