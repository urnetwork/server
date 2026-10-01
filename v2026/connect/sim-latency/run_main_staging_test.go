// Exercises the real staging shell dispatch with synthetic API and worker
// boundaries. Review, promotion, and production creation are forbidden effects.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Records close/drain/create ordering under a mutex because the shell talks to
// the fixture through real HTTP requests handled on separate goroutines.
type stagingHarnessFixture struct {
	stateLock      sync.Mutex
	environment    []string
	events         []string
	epoch          int
	status         string
	winnerJobId    string
	sourcePath     string
	sourceBytes    []byte
	simCommandPath string
}

func newStagingHarnessFixture(t *testing.T, epoch int, winnerJobId string) *stagingHarnessFixture {
	t.Helper()
	root := t.TempDir()
	stubDirectory := filepath.Join(root, "stubs")
	stateDirectory := filepath.Join(root, "state")
	workerPath := filepath.Join(stateDirectory, "bin", "competitionworker")
	for _, directory := range []string{stubDirectory, filepath.Dir(workerPath)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &stagingHarnessFixture{
		epoch:          epoch,
		status:         "open",
		winnerJobId:    winnerJobId,
		sourcePath:     filepath.Join(root, "source.yml"),
		sourceBytes:    []byte("epochs:\n  - epoch: 0\n    significant_improvement_percent: 16.1\n"),
		simCommandPath: filepath.Join(root, "sim-commands.log"),
	}
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer synthetic-staging-token" {
			http.Error(response, "synthetic token required", http.StatusUnauthorized)
			return
		}
		round := func() map[string]any {
			var winner any
			if fixture.status == "finalized" && fixture.winnerJobId != "" {
				winner = fixture.winnerJobId
			}
			return map[string]any{
				"round_id":      fmt.Sprintf("00000000-0000-0000-0000-%012d", fixture.epoch+1),
				"epoch":         fixture.epoch,
				"staging":       true,
				"status":        fixture.status,
				"winner_job_id": winner,
				"opens_at":      "2026-09-01T00:00:00Z",
			}
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /competition/info":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"active_round": nil, "staging_round": round(),
			})
		case "POST /fixture/preflight":
			if fixture.status != "open" {
				http.Error(response, "preflight must precede close", http.StatusConflict)
				return
			}
			fixture.events = append(fixture.events, "preflight")
			_, _ = response.Write([]byte("{}"))
		case "POST /competition/close-staging-round":
			if fixture.status != "open" || !reflect.DeepEqual(fixture.events, []string{"preflight"}) {
				http.Error(response, "close must follow preflight", http.StatusConflict)
				return
			}
			fixture.events = append(fixture.events, "close")
			fixture.status = "grading"
			_ = json.NewEncoder(response).Encode(round())
		case "POST /fixture/drain":
			if fixture.status != "open" && fixture.status != "grading" {
				http.Error(response, "worker must own a nonterminal round", http.StatusConflict)
				return
			}
			fixture.events = append(fixture.events, "drain")
			fixture.status = "finalized"
			_ = json.NewEncoder(response).Encode(round())
		case "POST /competition/generate-staging-round":
			if fixture.status != "finalized" {
				http.Error(response, "accepted work must drain before next round", http.StatusConflict)
				return
			}
			var body struct {
				OpensAt        time.Time `json:"opens_at"`
				ClosesAt       time.Time `json:"closes_at"`
				RevealAt       time.Time `json:"reveal_at"`
				ReplaceCurrent bool      `json:"replace_current"`
			}
			decoder := json.NewDecoder(request.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil || body.ReplaceCurrent ||
				body.ClosesAt.Sub(body.OpensAt) != 2*time.Minute || !body.RevealAt.Equal(body.ClosesAt) {
				t.Errorf("invalid staging successor: %+v, %v", body, err)
				http.Error(response, "invalid successor", http.StatusBadRequest)
				return
			}
			fixture.events = append(fixture.events, "generate-staging")
			fixture.epoch += 1
			fixture.status = "open"
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(round())
		default:
			t.Errorf("unexpected staging harness effect: %s %s", request.Method, request.URL.Path)
			http.Error(response, "forbidden effect", http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	sourceRecordBytes, err := json.Marshal(sourceRecord{
		Schema: 1, Epoch: 0, Branch: stagingEvaluationSourceBranch,
		Repositories: sourceTestCommits(strings.Repeat("a", 40)),
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"operator.token": "synthetic-staging-token\n",
		"source.yml":     string(fixture.sourceBytes),
		"stubs/go": `#!/bin/bash
set -euo pipefail
[[ $# == 6 && $1 == build && $2 == -trimpath && $3 == -buildvcs=true &&
   $4 == -o && $5 == "$RUN_MAIN_FIXTURE_WORKER" && $6 == */cli/competitionworker ]]
`,
		"stubs/sudo": `#!/bin/bash
set -euo pipefail
[[ $# -ge 4 && $1 == -n && $2 == --preserve-env=* && $3 == "$RUN_MAIN_FIXTURE_WORKER" ]]
shift 2
exec "$@"
`,
		"state/bin/competitionworker": `#!/bin/bash
set -euo pipefail
case ${1:-} in
    --check)
        [[ $# == 2 && $2 == --worker_id=sim-latency-staging-preflight ]]
        path=/fixture/preflight
        ;;
    --worker_id=sim-latency-staging-epoch-*)
        [[ $# == 1 ]]
        path=/fixture/drain
        ;;
    *) exit 91 ;;
esac
curl --silent --show-error --fail --request POST \
    --header 'Authorization: Bearer synthetic-staging-token' \
    "$SIM_LATENCY_API_URL$path" >/dev/null
`,
		"sim-latency": `#!/bin/bash
set -euo pipefail
printf '%s\n' "$*" >>"$RUN_MAIN_FIXTURE_SIM_COMMANDS"
[[ $# == 4 && $1 == staging-source-check && $2 == --epoch=0 &&
   $3 == --source-config="$SIM_LATENCY_SOURCE_CONFIG" && $4 == --repos-root=* ]] || exit 92
printf '%s\n' '` + string(sourceRecordBytes) + "'\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.environment = append(os.Environ(),
		"PATH="+stubDirectory+string(os.PathListSeparator)+runMainTestCommandPath(t),
		"SIM_LATENCY_API_URL="+api.URL,
		"SIM_LATENCY_OPERATOR_TOKEN_FILE="+filepath.Join(root, "operator.token"),
		"SIM_LATENCY_SOURCE_CONFIG="+fixture.sourcePath,
		"SIM_LATENCY_BINARY="+filepath.Join(root, "sim-latency"),
		"SIM_LATENCY_STATE_DIR="+stateDirectory,
		"SIM_LATENCY_STAGING_WINDOW_SECONDS=120",
		"SIM_LATENCY_REVIEWER_ID=",
		"RUN_MAIN_FIXTURE_WORKER="+workerPath,
		"RUN_MAIN_FIXTURE_SIM_COMMANDS="+fixture.simCommandPath,
	)
	return fixture
}

// A deadline is only a deadlock backstop; HTTP transition checks prove order.
func (self *stagingHarnessFixture) run(t *testing.T, action string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", "./run-main.sh", action)
	command.Env = self.environment
	command.WaitDelay = time.Second
	var output, diagnostics bytes.Buffer
	command.Stdout = &output
	command.Stderr = &diagnostics
	if err := command.Run(); err != nil {
		t.Fatalf("staging must finish without a review pause: %v\n%s\n%s", err, &output, &diagnostics)
	}
	content, err := os.ReadFile(self.sourcePath)
	if err != nil || !bytes.Equal(content, self.sourceBytes) {
		t.Fatalf("staging changed its frozen source ledger: %q, %v", content, err)
	}
	commands, err := os.ReadFile(self.simCommandPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
		if line != "" && !strings.HasPrefix(line, "staging-source-check --epoch=0 ") {
			t.Fatalf("staging invoked review or promotion: %s", line)
		}
	}
	return output.Bytes()
}

func TestRunMainAdvanceStagingNamesWinnerWithoutReviewOrPromotion(t *testing.T) {
	fixture := newStagingHarnessFixture(t, 7, "00000000-0000-0000-0000-000000000100")
	output := fixture.run(t, "advance-staging")
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if !reflect.DeepEqual(fixture.events, []string{"preflight", "close", "drain", "generate-staging"}) ||
		fixture.epoch != 8 || fixture.status != "open" {
		t.Fatalf("staging did not advance automatically: events=%v epoch=%d status=%s\n%s",
			fixture.events, fixture.epoch, fixture.status, output)
	}
	var next struct {
		Epoch   int  `json:"epoch"`
		Staging bool `json:"staging"`
	}
	if err := json.Unmarshal(output, &next); err != nil || next.Epoch != 8 || !next.Staging {
		t.Fatalf("staging successor output=%s, %v", output, err)
	}
}

func TestRunMainAdvanceStagingWithoutWinnerKeepsFrozenSource(t *testing.T) {
	fixture := newStagingHarnessFixture(t, 0, "")
	fixture.run(t, "advance-staging")
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if !reflect.DeepEqual(fixture.events, []string{"preflight", "close", "drain", "generate-staging"}) ||
		fixture.epoch != 1 || fixture.status != "open" {
		t.Fatalf("no-winner staging did not advance: events=%v epoch=%d status=%s",
			fixture.events, fixture.epoch, fixture.status)
	}
}

func TestRunMainStagingWorkerNamesWinnerWithoutReviewOrPromotion(t *testing.T) {
	fixture := newStagingHarnessFixture(t, 0, "00000000-0000-0000-0000-000000000100")
	fixture.run(t, "staging-worker")
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if !reflect.DeepEqual(fixture.events, []string{"drain"}) || fixture.epoch != 0 || fixture.status != "finalized" {
		t.Fatalf("natural-window staging did not finalize: events=%v epoch=%d status=%s",
			fixture.events, fixture.epoch, fixture.status)
	}
}
