package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type launchPlaybook struct {
	Schema int `yaml:"schema"`
	Season struct {
		EpochCount               int `yaml:"epoch_count"`
		SubmissionWindowSeconds  int `yaml:"submission_window_seconds"`
		PreparationWindowSeconds int `yaml:"preparation_window_seconds"`
		QueueLimit               int `yaml:"queue_limit"`
		ScoreTimeoutSeconds      int `yaml:"score_timeout_seconds"`
	} `yaml:"season"`
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

	runnerBytes, err := os.ReadFile("run-main.sh")
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
			t.Errorf("run-main.sh is missing %q", required)
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
	if strings.Contains(strings.ToLower(tests), "python") {
		t.Fatal("tests.sh must remain Go-only")
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
		playbook.Season.QueueLimit != 10 || playbook.Season.ScoreTimeoutSeconds != 49392 {
		t.Fatalf("launch season is not frozen: %+v", playbook.Season)
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
		"six_epoch_batch_lifecycle",
		"immutable_artifact_implementation",
		"grafana_implementation",
		"runtime_control_plane_identity",
		"winner_source_policy",
	} {
		if !strings.HasPrefix(statuses[completeId], "complete") {
			t.Errorf("checklist item %q = %q, want complete", completeId, statuses[completeId])
		}
	}
}
