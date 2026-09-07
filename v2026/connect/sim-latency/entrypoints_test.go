package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		"/competition/generate-staging-round",
		".staging == true",
		"/competition/generate-round",
		".staging == false",
		"competitionworker",
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
