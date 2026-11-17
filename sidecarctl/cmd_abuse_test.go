// Tests for `sidecarctl abuse <kind>`: each kind's parser builds the body bringyour.yml documents and refuses what it
// cannot build, each posts its body to an httptest route with the bearer from its file, a refusal reaches stderr with
// its status, no bearer reaches any output, the api defaults to the env WARP_ENV names, and a failed post names the id
// a retry passes back. Nothing here calls a real api.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// A synthetic staff bearer, which no output may hold.
const testAbuseKindBearer = "synthetic-abuse-kind-bearer-7d41c09e5b"

// Synthetic ids for the flags.
const (
	testAbuseKindCaseId     = "11111111-1111-1111-1111-111111111111"
	testAbuseKindActionId   = "22222222-2222-2222-2222-222222222222"
	testAbuseKindAppealId   = "33333333-3333-3333-3333-333333333333"
	testAbuseKindIncidentId = "44444444-4444-4444-4444-444444444444"
	testAbuseKindNetworkId  = "55555555-5555-5555-5555-555555555555"
)

// The nth id a test's deps draw, from 1.
func testAbuseKindDrawnId(n int) string {
	return fmt.Sprintf("dddddddd-dddd-dddd-dddd-%012d", n)
}

// Deps drawing testAbuseKindDrawnId(1), (2), ... in order, the env's api standing at envApiOrigin.
func testAbuseKindDeps(envApiOrigin string) *abuseStaffKindDeps {
	drawnCount := 0
	return &abuseStaffKindDeps{
		newId: func() server.Id {
			drawnCount += 1
			return server.RequireParseId(testAbuseKindDrawnId(drawnCount))
		},
		envApiOrigin: func() (string, error) {
			if envApiOrigin == "" {
				return "", errors.New("no env in this test")
			}
			return envApiOrigin, nil
		},
	}
}

// The spec of kind, failing the test when the binary has none.
func testAbuseKindSpec(t *testing.T, kind string) *abuseStaffKindSpec {
	t.Helper()
	for _, spec := range abuseStaffKindSpecs() {
		if spec.kind == kind {
			return spec
		}
	}
	t.Fatalf("no spec for kind %s", kind)
	return nil
}

// A bearer file holding testAbuseKindBearer, with the line end a file usually has.
func writeTestAbuseKindBearerFile(t *testing.T) string {
	t.Helper()
	bearerPath := filepath.Join(t.TempDir(), "bearer")
	if err := os.WriteFile(bearerPath, []byte(testAbuseKindBearer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return bearerPath
}

// One request the route received.
type testAbuseKindRequest struct {
	path   string
	body   map[string]any
	bearer string
}

// A route that records each request and answers what answer gives: by default {kind, result} with the result
// {"received": <the body>}.
type testAbuseKindRoute struct {
	stateLock sync.Mutex
	requests  []*testAbuseKindRequest
	// the status and body for one request; nil answers the default
	answer func(request *testAbuseKindRequest) (int, []byte)
}

// Records the request and answers it.
func (self *testAbuseKindRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	request := &testAbuseKindRequest{
		path:   r.URL.Path,
		body:   map[string]any{},
		bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
	}
	json.Unmarshal(bodyBytes, &request.body)
	answer := func() func(request *testAbuseKindRequest) (int, []byte) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, request)
		return self.answer
	}()
	status := http.StatusOK
	answerBytes, _ := json.Marshal(map[string]any{"kind": request.body["kind"], "result": map[string]any{"received": request.body}})
	if answer != nil {
		status, answerBytes = answer(request)
	}
	if status != http.StatusOK {
		http.Error(w, string(answerBytes), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(answerBytes)
}

// Sets what the route answers from the next request on; nil answers the default.
func (self *testAbuseKindRoute) setAnswer(answer func(request *testAbuseKindRequest) (int, []byte)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answer = answer
}

// What the route received, in order.
func (self *testAbuseKindRoute) received() []*testAbuseKindRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// A running route, closed when the test ends.
func newTestAbuseKindRoute(t *testing.T) (*testAbuseKindRoute, *httptest.Server) {
	t.Helper()
	route := &testAbuseKindRoute{}
	routeServer := httptest.NewServer(route)
	t.Cleanup(routeServer.Close)
	return route, routeServer
}

// A body as JSON, so that two compare whatever their Go types.
func testAbuseKindJson(t *testing.T, value any) string {
	t.Helper()
	valueBytes, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(valueBytes)
}

// Each kind's flags with every optional one given, and the body they build: what the parser, the route and
// bringyour.yml's fields are all held to.
type testAbuseKindCase struct {
	kind     string
	args     []string
	wantBody map[string]any
	// the flag and id a retry passes back; "" for none
	wantRetryFlagName string
	wantRetryId       string
}

// The cases, every kind at least once and review once per decision.
func testAbuseKindCases() []*testAbuseKindCase {
	return []*testAbuseKindCase{
		{
			kind:     "lookup_case",
			args:     []string{"-case-id=" + testAbuseKindCaseId},
			wantBody: map[string]any{"kind": "lookup_case", "case_id": testAbuseKindCaseId},
		},
		{
			kind:     "lookup_join",
			args:     []string{"-join-kind=host", "-join-key= www.sidecarctl-test.example "},
			wantBody: map[string]any{"kind": "lookup_join", "join_kind": "host", "join_key": "www.sidecarctl-test.example"},
		},
		{
			kind: "action",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", "-reason=A phishing page, reported twice.", "-until=2026-01-01T00:00:00Z", "-action-id=" + testAbuseKindActionId},
			wantBody: map[string]any{
				"kind": "action", "case_id": testAbuseKindCaseId, "action_kind": "host_kill", "target": "www.sidecarctl-test.example",
				"reason": "A phishing page, reported twice.", "until_time_ms": 1767225600000, "action_id": testAbuseKindActionId,
			},
			wantRetryFlagName: "action-id",
			wantRetryId:       testAbuseKindActionId,
		},
		{
			kind: "action",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-action-kind=network_suspend", "-target=" + testAbuseKindNetworkId, "-reason=Closure growth past the floor."},
			wantBody: map[string]any{
				"kind": "action", "case_id": testAbuseKindCaseId, "action_kind": "network_suspend", "target": testAbuseKindNetworkId,
				"reason": "Closure growth past the floor.", "action_id": testAbuseKindDrawnId(1),
			},
			wantRetryFlagName: "action-id",
			wantRetryId:       testAbuseKindDrawnId(1),
		},
		{
			kind: "lift",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-action-id=" + testAbuseKindActionId, "-reason=The host was a false positive.", "-wrong", "-lift-id=" + testAbuseKindAppealId},
			wantBody: map[string]any{
				"kind": "lift", "case_id": testAbuseKindCaseId, "action_id": testAbuseKindActionId, "wrong": true,
				"reason": "The host was a false positive.", "lift_id": testAbuseKindAppealId,
			},
			wantRetryFlagName: "lift-id",
			wantRetryId:       testAbuseKindAppealId,
		},
		{
			kind: "lift",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-action-id=" + testAbuseKindActionId, "-reason=The owner removed the page."},
			wantBody: map[string]any{
				"kind": "lift", "case_id": testAbuseKindCaseId, "action_id": testAbuseKindActionId, "wrong": false,
				"reason": "The owner removed the page.", "lift_id": testAbuseKindDrawnId(1),
			},
			wantRetryFlagName: "lift-id",
			wantRetryId:       testAbuseKindDrawnId(1),
		},
		{
			kind: "review",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-decision=lift", "-action-id=" + testAbuseKindActionId, "-reason=Reviewed: the signal was stale.", "-lift-id=" + testAbuseKindAppealId},
			wantBody: map[string]any{
				"kind": "review", "case_id": testAbuseKindCaseId, "decision": "lift", "action_id": testAbuseKindActionId,
				"reason": "Reviewed: the signal was stale.", "lift_id": testAbuseKindAppealId,
			},
			wantRetryFlagName: "lift-id",
			wantRetryId:       testAbuseKindAppealId,
		},
		{
			kind: "review",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-decision=lift_wrong", "-action-id=" + testAbuseKindActionId, "-reason=Reviewed: wrong host."},
			wantBody: map[string]any{
				"kind": "review", "case_id": testAbuseKindCaseId, "decision": "lift_wrong", "action_id": testAbuseKindActionId,
				"reason": "Reviewed: wrong host.", "lift_id": testAbuseKindDrawnId(1),
			},
			wantRetryFlagName: "lift-id",
			wantRetryId:       testAbuseKindDrawnId(1),
		},
		{
			kind: "review",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-decision=uphold", "-action-id=" + testAbuseKindActionId, "-reason=Reviewed: the page still phishes."},
			wantBody: map[string]any{
				"kind": "review", "case_id": testAbuseKindCaseId, "decision": "uphold", "action_id": testAbuseKindActionId,
				"reason": "Reviewed: the page still phishes.",
			},
		},
		{
			kind: "review",
			args: []string{"-case-id=" + testAbuseKindCaseId, "-decision=no_action", "-reason=Reviewed: nothing to act on."},
			wantBody: map[string]any{
				"kind": "review", "case_id": testAbuseKindCaseId, "decision": "no_action", "reason": "Reviewed: nothing to act on.",
			},
		},
		{
			kind: "decide_appeal",
			args: []string{"-appeal-id=" + testAbuseKindAppealId, "-decision=granted", "-reason=The appellant showed the page was theirs."},
			wantBody: map[string]any{
				"kind": "decide_appeal", "appeal_id": testAbuseKindAppealId, "decision": "granted", "reason": "The appellant showed the page was theirs.",
			},
		},
		{
			kind: "decide_appeal",
			args: []string{"-appeal-id=" + strings.ToUpper(testAbuseKindAppealId), "-decision=denied", "-reason=The page still phishes."},
			wantBody: map[string]any{
				"kind": "decide_appeal", "appeal_id": testAbuseKindAppealId, "decision": "denied", "reason": "The page still phishes.",
			},
		},
		{
			kind: "clear_restore_flag",
			args: []string{"-root=sidecarctl-test.example", "-incident-id=" + testAbuseKindIncidentId, "-network-id=" + testAbuseKindNetworkId},
			wantBody: map[string]any{
				"kind": "clear_restore_flag", "root": "sidecarctl-test.example", "incident_id": testAbuseKindIncidentId, "network_id": testAbuseKindNetworkId,
			},
		},
	}
}

// The parser table: each kind's flags build exactly the body bringyour.yml documents, ids in the form the route
// reads, a drawn id where the kind records one and none was given, and the flag a retry passes it back by.
func TestAbuseStaffKindsBuildTheirBodies(t *testing.T) {
	for _, c := range testAbuseKindCases() {
		client, body, err := parseAbuseStaffKindArgs(testAbuseKindSpec(t, c.kind), c.args, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
		if err != nil {
			t.Errorf("%s %v: %v", c.kind, c.args, err)
			continue
		}
		if got, want := testAbuseKindJson(t, body.nameValues), testAbuseKindJson(t, c.wantBody); got != want {
			t.Errorf("%s %v built %s, want %s", c.kind, c.args, got, want)
		}
		if body.retryFlagName != c.wantRetryFlagName || body.retryId != c.wantRetryId {
			t.Errorf("%s %v: retry by -%s=%s, want -%s=%s", c.kind, c.args, body.retryFlagName, body.retryId, c.wantRetryFlagName, c.wantRetryId)
		}
		if client.apiUrl != "" || client.bearerFile != "" {
			t.Errorf("%s %v: client flags %+v from no -api or -bearer-file", c.kind, c.args, client)
		}
	}
}

// Each refusal is a usage mistake, before any post: a required flag missing, an id in another form, a decision the
// route does not take, a flag review's decision does not take, a reason missing or given twice, a time that is not
// one, a positional, a bearer as an argument and a flag no kind has. None repeats the value refused.
func TestAbuseStaffKindsRefuseWhatTheyCannotBuild(t *testing.T) {
	caseFlag := "-case-id=" + testAbuseKindCaseId
	actionFlag := "-action-id=" + testAbuseKindActionId
	reasonFlag := "-reason=Reasons."
	reasonPath := filepath.Join(t.TempDir(), "reason.txt")
	if err := os.WriteFile(reasonPath, []byte("Reasons from a file.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		kind string
		args []string
		want string
	}{
		{kind: "lookup_case", args: []string{}, want: "-case-id is required"},
		{kind: "lookup_case", args: []string{"-case-id=case-not-an-id"}, want: "-case-id must be an id"},
		{kind: "lookup_case", args: []string{"-case-id=" + strings.ReplaceAll(testAbuseKindCaseId, "-", "")}, want: "-case-id must be an id"},
		{kind: "lookup_case", args: []string{"-case-id=11111111x1111x1111x1111x111111111111"}, want: "-case-id must be an id"},
		{kind: "lookup_case", args: []string{caseFlag, "extra-positional-value"}, want: "takes no positional arguments"},
		{kind: "lookup_case", args: []string{caseFlag, "-bearer=" + testAbuseKindBearer}, want: "a bearer is never an argument"},
		{kind: "lookup_case", args: []string{caseFlag, "--bearer="}, want: "a bearer is never an argument"},
		{kind: "lookup_case", args: []string{caseFlag, "-token=" + testAbuseKindBearer}, want: "flag provided but not defined: -token"},
		{kind: "lookup_case", args: []string{"-case-id", testAbuseKindCaseId}, want: "flag needs an argument"},
		{kind: "lookup_join", args: []string{"-join-kind=host"}, want: "-join-key is required"},
		{kind: "lookup_join", args: []string{"-join-kind=host", "-join-key=  "}, want: "-join-key is required"},
		{kind: "lookup_join", args: []string{"-join-key=www.sidecarctl-test.example"}, want: "-join-kind is required"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", "-target=www.sidecarctl-test.example"}, want: "-reason or -reason-file is required"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", "-reason=  "}, want: "-reason or -reason-file is required"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", reasonFlag, "-reason-file=" + reasonPath}, want: "not both"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", reasonFlag}, want: "-target is required"},
		{kind: "action", args: []string{caseFlag, "-target=www.sidecarctl-test.example", reasonFlag}, want: "-action-kind is required"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", reasonFlag, "-until=next-tuesday"}, want: "-until must be an RFC 3339 time"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", reasonFlag, "-until=1970-01-01T00:00:00Z"}, want: "-until must be an RFC 3339 time after 1970"},
		{kind: "action", args: []string{caseFlag, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", reasonFlag, "-action-id=action-not-an-id"}, want: "-action-id must be an id"},
		{kind: "lift", args: []string{caseFlag, reasonFlag}, want: "-action-id is required"},
		{kind: "lift", args: []string{caseFlag, actionFlag, reasonFlag, "-wrong=maybe"}, want: "invalid boolean value"},
		{kind: "lift", args: []string{caseFlag, actionFlag, reasonFlag, "-lift-id=lift-not-an-id"}, want: "-lift-id must be an id"},
		{kind: "review", args: []string{caseFlag, actionFlag, reasonFlag}, want: "-decision must be one of uphold, lift, lift_wrong, no_action"},
		{kind: "review", args: []string{caseFlag, "-decision=overturn", actionFlag, reasonFlag}, want: "-decision must be one of"},
		{kind: "review", args: []string{caseFlag, "-decision=uphold", reasonFlag}, want: "-action-id is required"},
		{kind: "review", args: []string{caseFlag, "-decision=lift", reasonFlag}, want: "-action-id is required"},
		{kind: "review", args: []string{caseFlag, "-decision=uphold", actionFlag, reasonFlag, "-lift-id=" + testAbuseKindAppealId}, want: "-lift-id is not taken here"},
		{kind: "review", args: []string{caseFlag, "-decision=no_action", actionFlag, reasonFlag}, want: "-action-id is not taken here"},
		{kind: "review", args: []string{caseFlag, "-decision=no_action", reasonFlag, "-lift-id=" + testAbuseKindAppealId}, want: "-lift-id is not taken here"},
		{kind: "decide_appeal", args: []string{"-decision=granted", reasonFlag}, want: "-appeal-id is required"},
		{kind: "decide_appeal", args: []string{"-appeal-id=" + testAbuseKindAppealId, "-decision=pending", reasonFlag}, want: "-decision must be one of granted, denied"},
		{kind: "decide_appeal", args: []string{"-appeal-id=" + testAbuseKindAppealId, "-decision=granted"}, want: "-reason or -reason-file is required"},
		{kind: "clear_restore_flag", args: []string{"-incident-id=" + testAbuseKindIncidentId, "-network-id=" + testAbuseKindNetworkId}, want: "-root is required"},
		{kind: "clear_restore_flag", args: []string{"-root=sidecarctl-test.example", "-network-id=" + testAbuseKindNetworkId}, want: "-incident-id is required"},
		{kind: "clear_restore_flag", args: []string{"-root=sidecarctl-test.example", "-incident-id=" + testAbuseKindIncidentId}, want: "-network-id is required"},
	}
	for _, c := range cases {
		_, _, err := parseAbuseStaffKindArgs(testAbuseKindSpec(t, c.kind), c.args, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%s %v: error %v, want a usage mistake", c.kind, c.args, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: %q, want %q in it", c.kind, c.args, err, c.want)
		}
		for _, secret := range []string{testAbuseKindBearer, "extra-positional-value", "case-not-an-id", "action-not-an-id", "lift-not-an-id"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s %v: the refusal %q repeats %q", c.kind, c.args, err, secret)
			}
		}
	}
}

// -reason-file's reasons are read and trimmed in place of -reason; a file past the bound is refused unread, and a
// file that is not there fails the run.
func TestAbuseStaffKindsReadTheReasonFile(t *testing.T) {
	directory := t.TempDir()
	reasonPath := filepath.Join(directory, "reason.txt")
	if err := os.WriteFile(reasonPath, []byte("\nThe appellant showed the page was theirs.\nSee the ticket.\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-appeal-id=" + testAbuseKindAppealId, "-decision=granted", "-reason-file=" + reasonPath}
	_, body, err := parseAbuseStaffKindArgs(testAbuseKindSpec(t, "decide_appeal"), args, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
	if err != nil {
		t.Fatal(err)
	}
	if body.nameValues["reason"] != "The appellant showed the page was theirs.\nSee the ticket." {
		t.Fatalf("the reason read %q", body.nameValues["reason"])
	}

	settings := defaultAbuseStaffKindSettings()
	settings.ReasonFileMaxByteCount = 16
	_, _, err = parseAbuseStaffKindArgs(testAbuseKindSpec(t, "decide_appeal"), args, settings, testAbuseKindDeps(""))
	usageErr := &sidecarctlUsageError{}
	if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "past 16 bytes") {
		t.Fatalf("a reason file past the bound: %v", err)
	}

	missingArgs := []string{"-appeal-id=" + testAbuseKindAppealId, "-decision=granted", "-reason-file=" + filepath.Join(directory, "missing.txt")}
	_, _, err = parseAbuseStaffKindArgs(testAbuseKindSpec(t, "decide_appeal"), missingArgs, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing reason file: %v", err)
	}
}

// Each kind posts its body to <api>/abuse/action with the bearer from its file, and prints the answer's result as
// JSON, with no bearer in it.
func TestAbuseStaffKindsPostTheirBodiesWithTheBearer(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	bearerPath := writeTestAbuseKindBearerFile(t)
	for index, c := range testAbuseKindCases() {
		args := append(slices.Clone(c.args), "-api="+routeServer.URL+"/", "-bearer-file="+bearerPath)
		out := &bytes.Buffer{}
		if err := runAbuseStaffKind(context.Background(), testAbuseKindSpec(t, c.kind), args, out, defaultAbuseStaffKindSettings(), testAbuseKindDeps("")); err != nil {
			t.Errorf("%s %v: %v", c.kind, c.args, err)
			continue
		}
		received := route.received()
		if len(received) != index+1 {
			t.Fatalf("%s: the route received %d requests, want %d", c.kind, len(received), index+1)
		}
		request := received[index]
		if request.path != "/abuse/action" || request.bearer != testAbuseKindBearer {
			t.Errorf("%s: posted to %s with bearer %q", c.kind, request.path, request.bearer)
		}
		if got, want := testAbuseKindJson(t, request.body), testAbuseKindJson(t, c.wantBody); got != want {
			t.Errorf("%s: the route received %s, want %s", c.kind, got, want)
		}
		printed := map[string]any{}
		if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
			t.Errorf("%s printed %q, which is not JSON", c.kind, out.String())
			continue
		}
		if got, want := testAbuseKindJson(t, printed["received"]), testAbuseKindJson(t, c.wantBody); got != want {
			t.Errorf("%s printed %s, want the result %s", c.kind, got, want)
		}
		if strings.Contains(out.String(), testAbuseKindBearer) {
			t.Errorf("%s printed the bearer", c.kind)
		}
	}
}

// Through the binary's own command line: the answer goes to stdout with exit 0, and a refusal's status and the
// route's message go to stderr with exit 1 and nothing on stdout, the bearer named by UR_ABUSE_STAFF_BEARER_FILE.
func TestAbuseStaffKindRefusalGoesToStderr(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	t.Setenv(abuseStaffBearerFileEnv, writeTestAbuseKindBearerFile(t))

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := sidecarctlCommands.run([]string{"abuse", "lookup_case", "-case-id=" + testAbuseKindCaseId, "-api=" + routeServer.URL}, stdout, stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"case_id": "`+testAbuseKindCaseId+`"`) || stderr.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}

	route.setAnswer(func(request *testAbuseKindRequest) (int, []byte) {
		return http.StatusForbidden, []byte("Your role does not take this kind.")
	})
	stdout.Reset()
	stderr.Reset()
	code = sidecarctlCommands.run([]string{"abuse", "decide_appeal", "-appeal-id=" + testAbuseKindAppealId, "-decision=denied", "-reason=Upheld.", "-api=" + routeServer.URL}, stdout, stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("a refusal exited %d with stdout %q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "sidecarctl abuse decide_appeal: the route refused with 403: Your role does not take this kind.") {
		t.Fatalf("a refusal printed %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = sidecarctlCommands.run([]string{"abuse", "lift", "-case-id=" + testAbuseKindCaseId, "-api=" + routeServer.URL}, stdout, stderr)
	if code != 2 || !strings.Contains(stderr.String(), "sidecarctl abuse lift -case-id=<case_id> -action-id=<action_id>") || len(route.received()) != 2 {
		t.Fatalf("a usage mistake exited %d with stderr %q after %d posts", code, stderr.String(), len(route.received()))
	}
}

// No output holds the bearer: not a route that echoes it in a refusal or in an answer, not a bearer typed in place of
// the bearer file's path, as -bearer or as a positional.
func TestAbuseStaffKindKeepsTheBearerOutOfEveryOutput(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	bearerPath := writeTestAbuseKindBearerFile(t)
	t.Setenv(abuseStaffBearerFileEnv, "")
	lookupArgs := []string{"abuse", "lookup_case", "-case-id=" + testAbuseKindCaseId, "-api=" + routeServer.URL}

	cases := []struct {
		name     string
		answer   func(request *testAbuseKindRequest) (int, []byte)
		args     []string
		wantCode int
		want     string
	}{
		{
			name: "a refusal echoing the bearer",
			answer: func(request *testAbuseKindRequest) (int, []byte) {
				return http.StatusForbidden, []byte("Forbidden for " + request.bearer)
			},
			args:     append(slices.Clone(lookupArgs), "-bearer-file="+bearerPath),
			wantCode: 1,
			want:     "the route refused with 403: Forbidden for <bearer>",
		},
		{
			name: "an answer echoing the bearer",
			answer: func(request *testAbuseKindRequest) (int, []byte) {
				answerBytes, _ := json.Marshal(map[string]any{"kind": "lookup_case", "result": map[string]any{"bearer": request.bearer}})
				return http.StatusOK, answerBytes
			},
			args:     append(slices.Clone(lookupArgs), "-bearer-file="+bearerPath),
			wantCode: 1,
			want:     "the route's answer holds the bearer, so it is not printed",
		},
		{
			name:     "the bearer in place of its file's path",
			args:     append(slices.Clone(lookupArgs), "-bearer-file="+testAbuseKindBearer),
			wantCode: 1,
			want:     "the bearer file -bearer-file or UR_ABUSE_STAFF_BEARER_FILE names cannot be read",
		},
		{
			name:     "the bearer as -bearer",
			args:     append(slices.Clone(lookupArgs), "-bearer="+testAbuseKindBearer),
			wantCode: 2,
			want:     "a bearer is never an argument",
		},
		{
			name:     "the bearer as a positional",
			args:     append(slices.Clone(lookupArgs), testAbuseKindBearer),
			wantCode: 2,
			want:     "takes no positional arguments",
		},
		{
			name:     "no bearer file at all",
			args:     lookupArgs,
			wantCode: 2,
			want:     "name the staff bearer's file with -bearer-file or UR_ABUSE_STAFF_BEARER_FILE",
		},
	}
	for _, c := range cases {
		route.setAnswer(c.answer)
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := sidecarctlCommands.run(c.args, stdout, stderr)
		if code != c.wantCode || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: exit %d, stderr %q; want exit %d with %q", c.name, code, stderr.String(), c.wantCode, c.want)
		}
		if strings.Contains(stdout.String()+stderr.String(), testAbuseKindBearer) {
			t.Errorf("%s: the bearer reached the output: stdout %q, stderr %q", c.name, stdout.String(), stderr.String())
		}
	}
}

// Without -api the route is the env's api, https://api.<domain> of the services.yml the env's vault holds, and with no
// env, or an env whose services.yml gives no domain, the command refuses rather than guessing.
func TestAbuseStaffKindApiDefaultsToTheEnvsApi(t *testing.T) {
	routeUrl, err := abuseStaffKindRouteUrl("", testAbuseKindDeps("https://api.sidecarctl-test.example"))
	if err != nil || routeUrl != "https://api.sidecarctl-test.example/abuse/action" {
		t.Fatalf("the env's route: %q, %v", routeUrl, err)
	}

	t.Setenv("WARP_ENV", "")
	_, err = abuseStaffEnvApiOrigin()
	usageErr := &sidecarctlUsageError{}
	if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "set WARP_ENV") {
		t.Fatalf("no env: %v", err)
	}

	// a fixture vault below the temp directory, which a test binary's resolver reads
	vaultHome := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	t.Setenv("WARP_ENV", "sidecarctltest")
	if err := os.MkdirAll(filepath.Join(vaultHome, "sidecarctltest"), 0o700); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(vaultHome, "sidecarctltest", "services.yml")
	for _, c := range []struct {
		servicesYaml string
		wantOrigin   string
		want         string
	}{
		{servicesYaml: "domain: sidecarctl-test.example\nversions: []\n", wantOrigin: "https://api.sidecarctl-test.example"},
		{servicesYaml: "domain: \" Sidecarctl-Test.example \"\n", wantOrigin: "https://api.Sidecarctl-Test.example"},
		{servicesYaml: "versions: []\n", want: "gives no domain"},
		{servicesYaml: "domain: sidecarctl-test.example/abuse\n", want: "gives no domain"},
		{servicesYaml: "domain: user@sidecarctl-test.example\n", want: "gives no domain"},
		{servicesYaml: "domain: sidecarctl-test..example\n", want: "gives no domain"},
		{servicesYaml: "domain: [sidecarctl-test.example]\n", want: "does not read as yaml"},
	} {
		if err := os.WriteFile(servicesPath, []byte(c.servicesYaml), 0o600); err != nil {
			t.Fatal(err)
		}
		origin, err := abuseStaffEnvApiOrigin()
		if c.wantOrigin != "" {
			if err != nil || origin != c.wantOrigin {
				t.Errorf("%q: origin %q, %v; want %q", c.servicesYaml, origin, err, c.wantOrigin)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: origin %q, %v; want a refusal with %q", c.servicesYaml, origin, err, c.want)
		}
	}

	if err := os.Remove(servicesPath); err != nil {
		t.Fatal(err)
	}
	if origin, err := abuseStaffEnvApiOrigin(); err == nil || !strings.Contains(err.Error(), "cannot be found") {
		t.Fatalf("no services.yml: origin %q, %v", origin, err)
	}
}

// The bearer never crosses a network in the clear: -api is https, or http only to this machine; anything else that is
// not the api's absolute URL is a usage mistake, before the bearer is read.
func TestAbuseStaffKindRefusesTheBearerInTheClear(t *testing.T) {
	cases := []struct {
		apiUrl       string
		wantRouteUrl string
	}{
		{apiUrl: "https://api.sidecarctl-test.example", wantRouteUrl: "https://api.sidecarctl-test.example/abuse/action"},
		{apiUrl: " https://api.sidecarctl-test.example/ ", wantRouteUrl: "https://api.sidecarctl-test.example/abuse/action"},
		{apiUrl: "https://api.sidecarctl-test.example:8443/staff", wantRouteUrl: "https://api.sidecarctl-test.example:8443/staff/abuse/action"},
		{apiUrl: "http://127.0.0.1:50123", wantRouteUrl: "http://127.0.0.1:50123/abuse/action"},
		{apiUrl: "http://[::1]:50123", wantRouteUrl: "http://[::1]:50123/abuse/action"},
		{apiUrl: "http://localhost:50123", wantRouteUrl: "http://localhost:50123/abuse/action"},
		{apiUrl: "http://api.sidecarctl-test.example"},
		{apiUrl: "http://192.0.2.10:8080"},
		{apiUrl: "ftp://api.sidecarctl-test.example"},
		{apiUrl: "api.sidecarctl-test.example"},
		{apiUrl: "https://staff@api.sidecarctl-test.example"},
		{apiUrl: "https://api.sidecarctl-test.example/?kind=lift"},
		{apiUrl: "https://api.sidecarctl-test.example/#top"},
		{apiUrl: "https:api.sidecarctl-test.example"},
	}
	for _, c := range cases {
		routeUrl, err := abuseStaffKindRouteUrl(c.apiUrl, testAbuseKindDeps(""))
		if c.wantRouteUrl != "" {
			if err != nil || routeUrl != c.wantRouteUrl {
				t.Errorf("%q: %q, %v; want %q", c.apiUrl, routeUrl, err, c.wantRouteUrl)
			}
			continue
		}
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%q: %q, %v; want a usage mistake", c.apiUrl, routeUrl, err)
		}
	}
}

// A post that fails names the id the body carried, drawn or given, and a retry passing it back posts the same id, so
// the route records the action once; a kind that carries no id names none.
func TestAbuseStaffKindFailureNamesTheRetryId(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	route.setAnswer(func(request *testAbuseKindRequest) (int, []byte) {
		return http.StatusServiceUnavailable, []byte("Unavailable.")
	})
	bearerPath := writeTestAbuseKindBearerFile(t)
	actionArgs := []string{"-case-id=" + testAbuseKindCaseId, "-action-kind=host_kill", "-target=www.sidecarctl-test.example", "-reason=Phishing.", "-api=" + routeServer.URL, "-bearer-file=" + bearerPath}

	err := runAbuseStaffKind(context.Background(), testAbuseKindSpec(t, "action"), actionArgs, &bytes.Buffer{}, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
	wantRetry := "a retry passing -action-id=" + testAbuseKindDrawnId(1) + " records it at most once"
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), wantRetry) {
		t.Fatalf("a failed action: %v; want %q", err, wantRetry)
	}

	route.setAnswer(nil)
	retryArgs := append(slices.Clone(actionArgs), "-action-id="+testAbuseKindDrawnId(1))
	if err := runAbuseStaffKind(context.Background(), testAbuseKindSpec(t, "action"), retryArgs, &bytes.Buffer{}, defaultAbuseStaffKindSettings(), testAbuseKindDeps("")); err != nil {
		t.Fatal(err)
	}
	received := route.received()
	if len(received) != 2 || received[0].body["action_id"] != testAbuseKindDrawnId(1) || received[1].body["action_id"] != testAbuseKindDrawnId(1) {
		t.Fatalf("the two posts carried %v", received)
	}

	// a route that never answers: the id is named all the same
	routeServer.Close()
	liftArgs := []string{"-case-id=" + testAbuseKindCaseId, "-action-id=" + testAbuseKindActionId, "-reason=Lifted.", "-api=" + routeServer.URL, "-bearer-file=" + bearerPath}
	err = runAbuseStaffKind(context.Background(), testAbuseKindSpec(t, "lift"), liftArgs, &bytes.Buffer{}, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
	if err == nil || !strings.Contains(err.Error(), "the route did not answer") || !strings.Contains(err.Error(), "-lift-id="+testAbuseKindDrawnId(1)) {
		t.Fatalf("a lift to no route: %v", err)
	}
	lookupArgs := []string{"-case-id=" + testAbuseKindCaseId, "-api=" + routeServer.URL, "-bearer-file=" + bearerPath}
	err = runAbuseStaffKind(context.Background(), testAbuseKindSpec(t, "lookup_case"), lookupArgs, &bytes.Buffer{}, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
	if err == nil || strings.Contains(err.Error(), "a retry passing") {
		t.Fatalf("a lookup to no route: %v", err)
	}
}

// The kinds are the route's: each is in controller's kind table and in the binary as `abuse <kind>`, and the fields
// each sends, with every optional flag given, are exactly those connect/api/bringyour.yml documents for it.
func TestAbuseStaffKindsMatchTheRoute(t *testing.T) {
	// go test runs in the package's directory, and connect sits beside server
	data, err := os.ReadFile(filepath.Join("..", "..", "connect", "api", "bringyour.yml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := &struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}{}
	if err := yaml.Unmarshal(data, spec); err != nil {
		t.Fatal(err)
	}
	operationNode, ok := spec.Paths["/abuse/action"]["post"]
	if !ok {
		t.Fatal("bringyour.yml documents no POST /abuse/action")
	}
	operation := &struct {
		Description string `yaml:"description"`
	}{}
	if err := operationNode.Decode(operation); err != nil {
		t.Fatal(err)
	}
	// "`lookup_case` {case_id}": each kind and its body's fields, as the description lists them
	kindDocumentedFields := map[string][]string{}
	for _, match := range regexp.MustCompile("`([a-z_]+)` \\{([^}]*)\\}").FindAllStringSubmatch(operation.Description, -1) {
		fields := []string{}
		for _, field := range strings.Split(match[2], ",") {
			fields = append(fields, strings.TrimSpace(field))
		}
		slices.Sort(fields)
		kindDocumentedFields[match[1]] = fields
	}

	routeKinds := controller.AbuseStaffKindNames()
	kindSentFields := map[string][]string{}
	for _, c := range testAbuseKindCases() {
		_, body, err := parseAbuseStaffKindArgs(testAbuseKindSpec(t, c.kind), c.args, defaultAbuseStaffKindSettings(), testAbuseKindDeps(""))
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		for name := range body.nameValues {
			if name != "kind" && !slices.Contains(kindSentFields[c.kind], name) {
				kindSentFields[c.kind] = append(kindSentFields[c.kind], name)
			}
		}
	}
	for _, kindSpec := range abuseStaffKindSpecs() {
		if !slices.Contains(routeKinds, kindSpec.kind) {
			t.Errorf("the route's table has no kind %s", kindSpec.kind)
		}
		if command, _ := sidecarctlCommands.find([]string{"abuse", kindSpec.kind}); command == nil || command.words != "abuse "+kindSpec.kind {
			t.Errorf("the binary has no abuse %s", kindSpec.kind)
		}
		sentFields := kindSentFields[kindSpec.kind]
		slices.Sort(sentFields)
		documentedFields, ok := kindDocumentedFields[kindSpec.kind]
		if !ok {
			t.Errorf("bringyour.yml documents no fields for %s", kindSpec.kind)
		} else if !slices.Equal(sentFields, documentedFields) {
			t.Errorf("%s sends %v, and bringyour.yml documents %v", kindSpec.kind, sentFields, documentedFields)
		}
	}
	if len(abuseStaffKindSpecs()) != 7 {
		t.Fatalf("%d kinds, want the seven ABUSE-M5-12 documents", len(abuseStaffKindSpecs()))
	}
}
