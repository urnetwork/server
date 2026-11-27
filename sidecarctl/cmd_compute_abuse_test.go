// Tests for `sidecarctl compute suspend` and `sidecarctl compute resume` (CARCH-M12-01): each builds the body
// bringyour.yml documents for its kind and refuses what it cannot build without repeating it, posts its body to an
// httptest route with the bearer from its file, prints the answer's JSON, and keeps the bearer out of every output,
// argv included: a bearer as -bearer, as the positional, or in place of its file's path. Nothing here calls a real api.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/controller"
)

// Each command's arguments and the body they build.
func TestComputeAbuseCommandsBuildTheirBodies(t *testing.T) {
	cases := []struct {
		kind string
		args []string
		want map[string]any
	}{
		{
			kind: controller.AbuseStaffKindComputeSuspend,
			args: []string{testAbuseKindNetworkId, "--reason=mining"},
			want: map[string]any{"kind": "compute_suspend", "network_id": testAbuseKindNetworkId, "reason": "mining"},
		},
		{
			kind: controller.AbuseStaffKindComputeSuspend,
			args: []string{"--reason=takedown", "--api=https://api.example", strings.ToUpper(testAbuseKindNetworkId)},
			want: map[string]any{"kind": "compute_suspend", "network_id": testAbuseKindNetworkId, "reason": "takedown"},
		},
		{
			kind: controller.AbuseStaffKindComputeResume,
			args: []string{testAbuseKindNetworkId},
			want: map[string]any{"kind": "compute_resume", "network_id": testAbuseKindNetworkId},
		},
	}
	for _, c := range cases {
		_, body, err := parseComputeAbuseArgs(c.kind, c.args)
		if err != nil {
			t.Fatalf("%s %v: %v", c.kind, c.args, err)
		}
		if testAbuseKindJson(t, body) != testAbuseKindJson(t, c.want) {
			t.Errorf("%s %v built %v, want %v", c.kind, c.args, body, c.want)
		}
	}
}

// What a command cannot build is refused before anything is read or posted, a usage mistake exiting 2, and no refusal
// repeats what it refused; a web search suspension's clear waits for SRCH-M13-08 and is refused, sending nothing.
func TestComputeAbuseCommandsRefuseWhatTheyCannotBuild(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	bearerPath := writeTestAbuseKindBearerFile(t)
	client := []string{"--api=" + routeServer.URL, "--bearer-file=" + bearerPath}
	notAnId := "synthetic-not-an-id-3f9c2a"
	cases := []struct {
		args     []string
		wantCode int
		want     string
	}{
		{args: []string{"compute", "suspend", "--reason=mining"}, wantCode: 2, want: "takes one positional argument"},
		{args: []string{"compute", "suspend", testAbuseKindNetworkId, testAbuseKindCaseId, "--reason=mining"}, wantCode: 2, want: "takes one positional argument"},
		{args: []string{"compute", "suspend", notAnId, "--reason=mining"}, wantCode: 2, want: "the network must be its id"},
		{args: []string{"compute", "suspend", "00000000-0000-0000-0000-000000000000", "--reason=mining"}, wantCode: 2, want: "the network must be its id"},
		{args: []string{"compute", "suspend", testAbuseKindNetworkId}, wantCode: 2, want: "--reason must be one of mining, scanning, spam"},
		{args: []string{"compute", "suspend", testAbuseKindNetworkId, "--reason=none"}, wantCode: 2, want: "--reason must be one of"},
		{args: []string{"compute", "suspend", testAbuseKindNetworkId, "--reason=Mining"}, wantCode: 2, want: "--reason must be one of"},
		{args: []string{"compute", "suspend", testAbuseKindNetworkId, "--reason", "mining"}, wantCode: 2, want: "takes one positional argument"},
		{args: []string{"compute", "suspend", testAbuseKindNetworkId, "--reason=spam", "--search"}, wantCode: 2, want: "--search and --review are resume's"},
		{args: []string{"compute", "resume", testAbuseKindNetworkId, "--reason=spam"}, wantCode: 2, want: "--reason is suspend's"},
		{args: []string{"compute", "resume", testAbuseKindNetworkId, "--review=case-17"}, wantCode: 2, want: "--review goes with --search"},
		{args: []string{"compute", "resume", testAbuseKindNetworkId, "--search"}, wantCode: 2, want: "--search takes --review"},
		{args: []string{"compute", "resume", testAbuseKindNetworkId, "--search", "--review=case-17"}, wantCode: 1, want: "waits for SRCH-M13-08's compute_search_flag: nothing was sent"},
		{args: []string{"compute", "resume", testAbuseKindNetworkId, "--fast"}, wantCode: 2, want: "flag provided but not defined"},
	}
	for _, c := range cases {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := sidecarctlCommands.run(append(slices.Clone(c.args), client...), stdout, stderr)
		if code != c.wantCode || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d with %q", c.args, code, stderr.String(), c.wantCode, c.want)
		}
		if strings.Contains(stderr.String(), notAnId) || stdout.Len() != 0 {
			t.Errorf("%v: the refusal repeated what it refused, or printed: %q %q", c.args, stdout.String(), stderr.String())
		}
	}
	if received := route.received(); len(received) != 0 {
		t.Fatalf("a refused command posted %d requests", len(received))
	}
}

// Each command posts its body to <api>/abuse/action with the bearer from its file and prints the result as JSON.
func TestComputeAbuseCommandsPostWithTheBearer(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	bearerPath := writeTestAbuseKindBearerFile(t)
	t.Setenv(abuseStaffBearerFileEnv, bearerPath)
	for _, args := range [][]string{
		{"compute", "suspend", testAbuseKindNetworkId, "--reason=provider_complaint", "--api=" + routeServer.URL},
		{"compute", "resume", testAbuseKindNetworkId, "--api=" + routeServer.URL},
	} {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := sidecarctlCommands.run(args, stdout, stderr); code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
		printed := map[string]any{}
		if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || printed["received"] == nil {
			t.Fatalf("%v printed %q: %v", args, stdout.String(), err)
		}
	}
	received := route.received()
	if len(received) != 2 {
		t.Fatalf("the route received %d requests", len(received))
	}
	want := []map[string]any{
		{"kind": "compute_suspend", "network_id": testAbuseKindNetworkId, "reason": "provider_complaint"},
		{"kind": "compute_resume", "network_id": testAbuseKindNetworkId},
	}
	for i, request := range received {
		if request.path != "/abuse/action" || request.bearer != testAbuseKindBearer || testAbuseKindJson(t, request.body) != testAbuseKindJson(t, want[i]) {
			t.Errorf("request %d: %s %q %v", i, request.path, request.bearer, request.body)
		}
	}
}

// No output holds the bearer: not a refusal or an answer that echoes it, not a bearer typed as -bearer, as the
// positional or in place of its file's path.
func TestComputeAbuseCommandsKeepTheBearerOutOfEveryOutput(t *testing.T) {
	route, routeServer := newTestAbuseKindRoute(t)
	bearerPath := writeTestAbuseKindBearerFile(t)
	t.Setenv(abuseStaffBearerFileEnv, "")
	suspendArgs := []string{"compute", "suspend", testAbuseKindNetworkId, "--reason=fraud", "--api=" + routeServer.URL}
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
			args:     append(slices.Clone(suspendArgs), "--bearer-file="+bearerPath),
			wantCode: 1,
			want:     "the route refused with 403: Forbidden for <bearer>",
		},
		{
			name: "an answer echoing the bearer",
			answer: func(request *testAbuseKindRequest) (int, []byte) {
				answerBytes, _ := json.Marshal(map[string]any{"kind": "compute_suspend", "result": map[string]any{"bearer": request.bearer}})
				return http.StatusOK, answerBytes
			},
			args:     append(slices.Clone(suspendArgs), "--bearer-file="+bearerPath),
			wantCode: 1,
			want:     "the route's answer holds the bearer, so it is not printed",
		},
		{
			name:     "the bearer in place of its file's path",
			args:     append(slices.Clone(suspendArgs), "--bearer-file="+testAbuseKindBearer),
			wantCode: 1,
			want:     "the bearer file --bearer-file or UR_ABUSE_STAFF_BEARER_FILE names cannot be read",
		},
		{
			name:     "the bearer as --bearer",
			args:     append(slices.Clone(suspendArgs), "--bearer="+testAbuseKindBearer),
			wantCode: 2,
			want:     "a bearer is never an argument",
		},
		{
			name:     "the bearer as -bearer, of resume",
			args:     []string{"compute", "resume", testAbuseKindNetworkId, "-bearer=" + testAbuseKindBearer, "--api=" + routeServer.URL},
			wantCode: 2,
			want:     "a bearer is never an argument",
		},
		{
			name:     "the bearer as the network",
			args:     []string{"compute", "suspend", testAbuseKindBearer, "--reason=fraud", "--api=" + routeServer.URL, "--bearer-file=" + bearerPath},
			wantCode: 2,
			want:     "the network must be its id",
		},
		{
			name:     "no bearer file at all",
			args:     suspendArgs,
			wantCode: 2,
			want:     "name the staff bearer's file with -bearer-file or UR_ABUSE_STAFF_BEARER_FILE",
		},
		{
			name: "a route that did not answer",
			answer: func(request *testAbuseKindRequest) (int, []byte) {
				return http.StatusServiceUnavailable, []byte("Unavailable.")
			},
			args:     append(slices.Clone(suspendArgs), "--bearer-file="+bearerPath),
			wantCode: 1,
			want:     "the same command again acts at most once",
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
	// no command whose argv carried the bearer posted it
	for _, request := range route.received() {
		if request.body["network_id"] == testAbuseKindBearer {
			t.Fatal("a bearer given as the network was posted")
		}
	}
}

// Both commands' kinds are in the route's table, and each body's fields are the ones bringyour.yml documents for it.
func TestComputeAbuseCommandsMatchTheRoute(t *testing.T) {
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
	for kind, args := range map[string][]string{
		controller.AbuseStaffKindComputeSuspend: {testAbuseKindNetworkId, "--reason=spam"},
		controller.AbuseStaffKindComputeResume:  {testAbuseKindNetworkId},
	} {
		if !slices.Contains(routeKinds, kind) {
			t.Errorf("the route's table has no kind %s", kind)
		}
		_, body, err := parseComputeAbuseArgs(kind, args)
		if err != nil {
			t.Fatal(err)
		}
		sent := []string{}
		for name := range body {
			if name != "kind" {
				sent = append(sent, name)
			}
		}
		slices.Sort(sent)
		if documented, ok := kindDocumentedFields[kind]; !ok || !slices.Equal(sent, documented) {
			t.Errorf("%s sends %v, and bringyour.yml documents %v", kind, sent, documented)
		}
	}
	for _, word := range []string{"suspend", "resume"} {
		if command, _ := sidecarctlCommands.find([]string{"compute", word}); command == nil || command.words != "compute "+word {
			t.Errorf("the binary has no compute %s", word)
		}
	}
}
