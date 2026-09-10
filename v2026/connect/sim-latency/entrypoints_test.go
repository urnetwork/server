package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type launchPlaybook struct {
	Schema int `yaml:"schema"`
	Season struct {
		EpochCount               int    `yaml:"epoch_count"`
		SubmissionWindowSeconds  int    `yaml:"submission_window_seconds"`
		PreparationWindowSeconds int    `yaml:"preparation_window_seconds"`
		SubmissionFeeUsd         int    `yaml:"submission_fee_usd"`
		QueueLimit               int    `yaml:"queue_limit"`
		ScoreTimeoutSeconds      int    `yaml:"score_timeout_seconds"`
		BaselineSourcePolicy     string `yaml:"baseline_source_policy"`
	} `yaml:"season"`
	Significance struct {
		Authority                 string  `yaml:"authority"`
		Method                    string  `yaml:"method"`
		Alpha                     float64 `yaml:"alpha"`
		InitialImprovementPercent float64 `yaml:"initial_improvement_percent"`
		ThresholdScope            string  `yaml:"threshold_scope"`
		NoWinnerPolicy            string  `yaml:"no_winner_policy"`
	} `yaml:"significance"`
	Checklist []struct {
		Id     string `yaml:"id"`
		Status string `yaml:"status"`
	} `yaml:"checklist"`
}

func TestHostBuildAndRunEntrypoints(t *testing.T) {
	makefileBytes, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(makefileBytes)
	for _, required := range []string{
		"all: clean build",
		"BUILD_DIR := build/$(GOOS)/$(GOARCH)",
		"CGO_ENABLED=0",
		"$(GO) build",
		"test:",
		"./tests.sh",
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("Makefile is missing %q", required)
		}
	}

	runnerBytes, err := os.ReadFile("run-local-main.sh")
	if err != nil {
		t.Fatal(err)
	}
	runner := string(runnerBytes)
	for _, required := range []string{
		"WARP_ENV=\"$env\"",
		"BRINGYOUR_POSTGRES_HOSTNAME=\"127.0.0.1\"",
		"BRINGYOUR_REDIS_HOSTNAME=\"127.0.0.1\"",
		"exec go run . \"$@\"",
	} {
		if !strings.Contains(runner, required) {
			t.Errorf("run-local-main.sh is missing %q", required)
		}
	}

	testsBytes, err := os.ReadFile("tests.sh")
	if err != nil {
		t.Fatal(err)
	}
	tests := string(testsBytes)
	if !strings.Contains(tests, "go test") || !strings.Contains(tests, "-race") {
		t.Fatal("tests.sh must run the Go package under the race detector")
	}
	if !strings.Contains(tests, "^TestRunMainCompleteSixEpochLifecycle$") {
		t.Fatal("tests.sh must always run the deterministic six-epoch lifecycle tier")
	}
	if strings.Contains(strings.ToLower(tests), "python") {
		t.Fatal("tests.sh must remain Go-only")
	}

	seasonHarnessBytes, err := os.ReadFile("run-main.sh")
	if err != nil {
		t.Fatal(err)
	}
	seasonHarness := string(seasonHarnessBytes)
	for _, required := range []string{
		"set -euo pipefail",
		"WARP_HOME=${WARP_HOME:-$workspace_root}",
		"/competition/generate-staging-round",
		"staging-worker",
		"--replace-current",
		"SIM_LATENCY_STAGING_WINDOW_SECONDS",
		".staging == true",
		"/competition/generate-round",
		".staging == false",
		"competitionworker",
		"--check",
		"sudo -n --preserve-env=",
		"epoch-review",
		"pending_review",
		"promote --epoch=",
		"--no-winner",
		"return 20",
	} {
		if !strings.Contains(seasonHarness, required) {
			t.Errorf("run-main.sh is missing fail-closed lifecycle contract %q", required)
		}
	}
	if !strings.Contains(seasonHarness, "advance_staging() {\n    preflight_worker staging\n    close_staging_round") {
		t.Fatal("advance-staging must preflight the worker before closing admission")
	}
	if strings.Contains(strings.ToLower(seasonHarness), "python") {
		t.Fatal("run-main.sh must remain Go/shell-only")
	}
	seasonRunbook, err := os.ReadFile("RUN-MAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Mandatory candidate review",
		"Terra with max reasoning",
		"Sol with max reasoning",
		"fabricated measurements",
		"status 20",
		"mode-0700 temporary directory",
		"After epoch 6",
	} {
		if !strings.Contains(string(seasonRunbook), required) {
			t.Errorf("RUN-MAIN.md is missing agent handoff contract %q", required)
		}
	}
}

func TestRunMainAdvancesAndExplicitlyReplacesStagingRounds(t *testing.T) {
	cases := []struct {
		name           string
		currentStatus  string
		replaceCurrent bool
	}{
		{name: "advance finalized", currentStatus: "finalized", replaceCurrent: false},
		{name: "replace open", currentStatus: "open", replaceCurrent: true},
	}
	for _, c := range cases {
		requestCount := 0
		var stagingRequest struct {
			OpensAt        time.Time `json:"opens_at"`
			ClosesAt       time.Time `json:"closes_at"`
			RevealAt       time.Time `json:"reveal_at"`
			ReplaceCurrent bool      `json:"replace_current"`
		}
		apiServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			if request.Header.Get("Authorization") != "Bearer synthetic-stage-token" {
				response.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/competition/info":
				_ = json.NewEncoder(response).Encode(map[string]any{
					"active_round": nil,
					"staging_round": map[string]any{
						"round_id": "00000000-0000-0000-0000-000000000001",
						"epoch":    0, "staging": true, "status": c.currentStatus,
					},
				})
			case request.Method == http.MethodPost && request.URL.Path == "/competition/generate-staging-round":
				requestCount++
				decoder := json.NewDecoder(request.Body)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&stagingRequest); err != nil {
					t.Errorf("%s staging request: %v", c.name, err)
				}
				response.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(response).Encode(map[string]any{
					"round_id": "00000000-0000-0000-0000-000000000002",
					"epoch":    1, "staging": true, "status": "open",
				})
			default:
				response.WriteHeader(http.StatusNotFound)
			}
		}))
		tokenPath := filepath.Join(t.TempDir(), "operator.token")
		if err := os.WriteFile(tokenPath, []byte("synthetic-stage-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sourceConfigPath := filepath.Join(t.TempDir(), "sim-latency.yml")
		if err := os.WriteFile(sourceConfigPath, []byte("epochs:\n  - epoch: 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		binaryPath := filepath.Join(t.TempDir(), "sim-latency")
		binaryFixture := `#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == staging-source-check ]]
printf '%s\n' '{"schema":1,"epoch":0,"branch":"sim-latency-staging","repositories":{"server":"a","connect":"b","sdk":"c","proxy":"d","glog":"e","goidenticons":"f","userwireguard":"g","sn":"h"}}'
`
		if err := os.WriteFile(binaryPath, []byte(binaryFixture), 0o700); err != nil {
			t.Fatal(err)
		}
		arguments := []string{"./run-main.sh", "staging"}
		if c.replaceCurrent {
			arguments = append(arguments, "--replace-current")
		}
		command := exec.Command("/bin/bash", arguments...)
		command.Env = append(os.Environ(),
			"SIM_LATENCY_API_URL="+apiServer.URL,
			"SIM_LATENCY_OPERATOR_TOKEN_FILE="+tokenPath,
			"SIM_LATENCY_SOURCE_CONFIG="+sourceConfigPath,
			"SIM_LATENCY_BINARY="+binaryPath,
			"SIM_LATENCY_STATE_DIR="+t.TempDir(),
			"SIM_LATENCY_STAGING_WINDOW_SECONDS=120",
		)
		output, err := command.CombinedOutput()
		apiServer.Close()
		if err != nil {
			t.Fatalf("%s run-main staging: %v\n%s", c.name, err, output)
		}
		if requestCount != 1 || stagingRequest.ReplaceCurrent != c.replaceCurrent ||
			!stagingRequest.RevealAt.Equal(stagingRequest.ClosesAt) ||
			stagingRequest.ClosesAt.Sub(stagingRequest.OpensAt) != 2*time.Minute {
			t.Errorf("%s request count=%d body=%+v", c.name, requestCount, stagingRequest)
		}
	}
}

func TestLaunchPlaybookFreezesWeeklySixEpochContract(t *testing.T) {
	bytes, err := os.ReadFile("playbook.yml")
	if err != nil {
		t.Fatal(err)
	}
	var playbook launchPlaybook
	if err := yaml.Unmarshal(bytes, &playbook); err != nil {
		t.Fatal(err)
	}
	if playbook.Schema != 1 || playbook.Season.EpochCount != 6 ||
		playbook.Season.SubmissionWindowSeconds != 7*24*60*60 ||
		playbook.Season.PreparationWindowSeconds != 16*60*60 ||
		playbook.Season.SubmissionFeeUsd != 20 || playbook.Season.QueueLimit != 0 ||
		playbook.Season.ScoreTimeoutSeconds != 10800 ||
		playbook.Season.BaselineSourcePolicy != "promote_significant_winner_or_carry_forward_unchanged" {
		t.Fatalf("launch season is not frozen: %+v", playbook.Season)
	}
	if playbook.Significance.Authority != "config/main/sim-latency.yml" ||
		playbook.Significance.Method != scoreSignificanceMethod ||
		playbook.Significance.Alpha != scoreSignificanceAlpha ||
		playbook.Significance.InitialImprovementPercent != 16.1 ||
		playbook.Significance.ThresholdScope != "per_source_epoch" ||
		playbook.Significance.NoWinnerPolicy != "carry_commits_and_threshold_forward_when_none_significant_or_all_rejected" {
		t.Fatalf("launch significance policy is not frozen: %+v", playbook.Significance)
	}
	statuses := map[string]string{}
	for _, item := range playbook.Checklist {
		if statuses[item.Id] != "" {
			t.Errorf("duplicate checklist id %q", item.Id)
		}
		statuses[item.Id] = item.Status
	}
	for _, completeId := range []string{
		"main_postgres_redis_restore",
		"api_migration_worker_ordering",
		"public_ingress_controls",
		"competition_api_and_leaderboard",
		"six_epoch_immediate_fifo_lifecycle",
		"immutable_artifact_implementation",
		"grafana_implementation",
		"runtime_control_plane_identity",
		"winner_source_policy",
		"winner_honesty_review_gate",
	} {
		if !strings.HasPrefix(statuses[completeId], "complete") {
			t.Errorf("checklist item %q = %q, want complete", completeId, statuses[completeId])
		}
	}
}

func TestOnlyCurrentEntrypointsRemainAtPackageRoot(t *testing.T) {
	archived := []string{
		"APEX-CALIBRATION.md",
		"APEX-SCORE-SPEC.md",
		"EVALUATION2.md",
		"FINALIZATION-STATUS.md",
		"FINALIZE.md",
		"eval-48.sh",
		"eval-frontier-12c.sh",
		"finalize-local-baseline.sh",
		"run-reserved-boundary-baseline.sh",
		"sample-host-resources.sh",
		"sample-rss.sh",
		"sample-service-resources.sh",
		"summarize-baseline.py",
		"summarize-frontier.py",
		"verify-local-baseline.sh",
		"final-baseline2.html",
		"final-preview.html",
	}
	for _, name := range archived {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("archived file remains at package root: %s", name)
		}
		if info, err := os.Stat(filepath.Join("old", name)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("archived file is not preserved under old/: %s", name)
		}
	}

	for _, name := range []string{
		"README.md",
		"OFFICIAL-RUN.md",
		"PLAYBOOK.md",
		"RUN-MAIN.md",
		"run-main.sh",
		"playbook.yml",
		"official-run.sh",
		"baseline/README.md",
		"baseline/final-baseline.html",
		"baseline/verify.sh",
	} {
		if info, err := os.Stat(name); err != nil || !info.Mode().IsRegular() {
			t.Errorf("current package file is missing: %s", name)
		}
	}
	for _, name := range []string{
		"launch/ONBOARDING.md",
		"launch/INCIDENT-RESPONSE.md",
	} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Errorf("launch document is missing: %s: %v", name, err)
			continue
		}
		if !strings.Contains(string(content), "support@ur.xyz") {
			t.Errorf("launch document %s does not identify the operations owner", name)
		}
	}

	pythonFiles, err := filepath.Glob("*.py")
	if err != nil {
		t.Fatal(err)
	}
	if len(pythonFiles) != 0 {
		t.Errorf("live package root contains Python utilities: %v", pythonFiles)
	}
}

func TestCurrentDocumentationDoesNotLinkArchivedContracts(t *testing.T) {
	documents := []string{"README.md", "OFFICIAL-RUN.md", "PLAYBOOK.md"}
	archivedReferences := []string{
		"APEX-CALIBRATION.md",
		"APEX-SCORE-SPEC.md",
		"EVALUATION2.md",
		"FINALIZATION-STATUS.md",
		"FINALIZE.md",
		"final-preview.html",
		"final-baseline2.html",
	}
	for _, document := range documents {
		contents, err := os.ReadFile(document)
		if err != nil {
			t.Fatal(err)
		}
		for _, archived := range archivedReferences {
			if strings.Contains(string(contents), archived) {
				t.Errorf("%s still references archived %s", document, archived)
			}
		}
	}
}
