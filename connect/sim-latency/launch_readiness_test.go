// Launch evidence must fail closed on missing local authority, cross-mode
// runtime identity and incomplete readiness, without touching a real daemon.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Both public responses succeed before the old nil dereference, so the test
// reproduces the reporting defect independently of network error behavior.
func TestLaunchApiRejectsMissingManifestBeforeProbing(t *testing.T) {
	var requests atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/competition/healthz" {
			_, _ = response.Write([]byte(`{"status":"alive"}`))
		} else {
			_, _ = response.Write([]byte(`{}`))
		}
	}))
	defer api.Close()
	config := &launchPreflightConfig{ApiUrl: api.URL}
	if _, info, err := checkCompetitionApi(context.Background(), config, nil); err == nil || info != nil || !strings.Contains(err.Error(), "source manifest") {
		t.Fatalf("missing source manifest: info=%+v error=%v", info, err)
	}
	if requests.Load() != 0 {
		t.Fatal("missing source authority reached the live API")
	}
}

// Exercise the composed receipt writer inputs with a missing source and a
// strict local image-inspection shim. No real Docker or blob store is reached.
func TestLaunchPreflightRetainsEveryCheckAfterMissingSource(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\n" +
		"test \"$#\" = 5 && test \"$1\" = docker && test \"$2\" = image && test \"$3\" = inspect && test \"$4\" = '--format={{.Id}}' || exit 97\n" +
		"printf '%s\\n' \"$5\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	openApi := filepath.Join(root, "openapi.yml")
	if err := os.WriteFile(openApi, []byte("openapi: 3.1.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/competition/healthz" {
			_, _ = response.Write([]byte(`{"status":"alive"}`))
		} else {
			_, _ = response.Write([]byte(`{}`))
		}
	}))
	defer api.Close()
	result, err := executeLaunchPreflight(context.Background(), &launchPreflightConfig{
		Timeout: time.Minute, SubprocessWaitDelay: time.Second,
		Epoch: 0, SourceConfig: filepath.Join(root, "missing-source.yml"),
		RepositoriesRoot: root, EvaluatorImageDigest: "sha256:" + strings.Repeat("a", 64),
		OpenApiPath: openApi, ApiUrl: api.URL,
		Now: func() time.Time { return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err == nil || result == nil || result.Passed {
		t.Fatalf("failed preflight receipt: %+v %v", result, err)
	}
	for _, name := range mandatoryLaunchChecks {
		if _, found := result.Checks[name]; !found {
			t.Errorf("missing retained check %s", name)
		}
	}
	if result.Checks["frozen_source"].Passed || result.Checks["competition_api"].Passed || !result.Checks["evaluator_image"].Passed {
		t.Fatalf("independent source/API/image results changed: %+v", result.Checks)
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatal("failed receipt cannot be encoded:", err)
	}
}

// All identities are synthetic. A complete live contract is independent of
// source/image equality; it also binds source mode, mapping and policy.
func launchReadinessTestInfo(manifest *sourceManifest) (*controller.InfoResult, *controller.ReadinessResult) {
	generation := manifest.EvaluationSource.Generations[0]
	info := &controller.InfoResult{
		CompetitionId: "synthetic-competition", Enabled: true, ScoreSchema: controller.ScoreSchema, ScorerVersion: controller.ScorerVersion,
		BaseSha: generation.Repositories.Server.Commit, EvaluatorImageDigest: "sha256:" + strings.Repeat("d", 64),
		DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionRuntimeBlobCapability,
		EvaluationPolicy: controller.EvaluationPolicy{TakeoverMargin: generation.SignificantImprovementPercent / 100},
		AutomationPolicy: &model.CompetitionAutomationPolicy{
			Enabled: true, Version: 2, Environment: "synthetic", SeasonId: "synthetic-season",
			ElectionRule: model.CompetitionElectionRule, RequiredApiCapability: model.CompetitionRuntimeBlobCapability,
			PolicyApprovalSha256: strings.Repeat("e", 64), SourceGeneration: 0, SourceLockSha256: generation.SourceLockSha256, SourceActivationGeneration: 1,
			ReviewCriteriaVersion: "synthetic-review-v1", SettlementPolicyVersion: "synthetic-settlement-v1", BurnPolicyVersion: "synthetic-burn-v1",
			RepeatRewardPolicy: "allow", SettlementMode: "live",
		},
		ActiveRound: &controller.RoundResult{
			RoundId: server.Id{0: 1}, Epoch: 1, DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionRuntimeBlobCapability,
		},
	}
	ready := &controller.ReadinessResult{Ready: true, Checks: map[string]bool{}}
	for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "authoritative_evaluator_host", "artifact_storage", "host_rebaseline", "api_image_identity", "artifact_archive"} {
		ready.Checks[name] = true
	}
	return info, ready
}

// The real HTTP decoder and operator-bearing readiness probe remain enabled;
// only the remote endpoint is replaced by a strict loopback contract server.
func launchReadinessTestApi(t *testing.T, info *controller.InfoResult, ready *controller.ReadinessResult) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		var value any
		switch request.URL.Path {
		case "/competition/healthz":
			value = controller.HealthResult{Status: "alive"}
		case "/competition/info":
			value = info
		case "/competition/readyz":
			if request.Header.Get("Authorization") != "Bearer synthetic-operator-token" {
				t.Error("readiness probe lost its operator credential")
				response.WriteHeader(http.StatusUnauthorized)
				return
			}
			value = ready
		case "/competition/leaderboard":
			value = controller.SeasonLeaderboardResult{CompetitionId: info.CompetitionId}
		default:
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(response).Encode(value); err != nil {
			t.Error(err)
		}
	}))
}

// A carry-forward's epoch is not its source generation. The preflight follows
// the exact committed mapping rather than deriving one index from the other.
func TestLaunchApiAuthenticatesV2CarryForwardMapping(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	mapping := manifest.EvaluationSource.Rounds[0]
	mapping.Epoch, mapping.ActivationGeneration = 2, 2
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, mapping)
	info, ready := launchReadinessTestInfo(manifest)
	info.ActiveRound.Epoch, info.AutomationPolicy.SourceActivationGeneration = 2, 2
	api := launchReadinessTestApi(t, info, ready)
	defer api.Close()
	evidence, actual, err := checkCompetitionApi(context.Background(), &launchPreflightConfig{
		Epoch: 0, ApiUrl: api.URL, OperatorToken: "synthetic-operator-token", EvaluatorImageDigest: info.EvaluatorImageDigest,
	}, manifest)
	if err != nil || actual == nil || evidence["source_generation"] != 0 || evidence["automation_policy"] == nil {
		t.Fatalf("exact carry-forward evidence: %+v %+v %v", evidence, actual, err)
	}
}

// Production and staging can share a base and evaluator image. Neither those
// pins nor a ready boolean authenticate a changed source/runtime contract.
func TestLaunchApiRejectsV2IdentityMismatch(t *testing.T) {
	cases := []struct {
		name   string
		change func(*controller.InfoResult)
	}{
		{name: "missing-policy", change: func(info *controller.InfoResult) { info.AutomationPolicy = nil }},
		{name: "disabled-policy", change: func(info *controller.InfoResult) { info.AutomationPolicy.Enabled = false }},
		{name: "missing-approval", change: func(info *controller.InfoResult) { info.AutomationPolicy.PolicyApprovalSha256 = "" }},
		{name: "staging-policy", change: func(info *controller.InfoResult) {
			info.AutomationPolicy.Staging, info.AutomationPolicy.SettlementMode, info.AutomationPolicy.StagingRoundWindowSeconds = true, "shadow", 3600
		}},
		{name: "source-generation", change: func(info *controller.InfoResult) { info.AutomationPolicy.SourceGeneration = 1 }},
		{name: "source-lock", change: func(info *controller.InfoResult) { info.AutomationPolicy.SourceLockSha256 = strings.Repeat("f", 64) }},
		{name: "source-margin", change: func(info *controller.InfoResult) { info.EvaluationPolicy.TakeoverMargin += .01 }},
		{name: "decision-contract", change: func(info *controller.InfoResult) { info.DecisionMode = "" }},
		{name: "capability-contract", change: func(info *controller.InfoResult) { info.RequiredApiCapability = model.CompetitionAutomationCapability }},
		{name: "missing-round", change: func(info *controller.InfoResult) { info.ActiveRound = nil }},
		{name: "missing-round-id", change: func(info *controller.InfoResult) { info.ActiveRound.RoundId = server.Id{} }},
		{name: "staging-round", change: func(info *controller.InfoResult) { info.ActiveRound.Staging = true }},
		{name: "round-capability", change: func(info *controller.InfoResult) {
			info.ActiveRound.RequiredApiCapability = model.CompetitionAutomationCapability
		}},
		{name: "round-mapping", change: func(info *controller.InfoResult) { info.ActiveRound.Epoch = 2 }},
		{name: "source-activation", change: func(info *controller.InfoResult) { info.AutomationPolicy.SourceActivationGeneration = 2 }},
	}
	for _, c := range cases {
		manifest := reviewedSourceTestManifest(t)
		info, ready := launchReadinessTestInfo(manifest)
		c.change(info)
		api := launchReadinessTestApi(t, info, ready)
		_, _, err := checkCompetitionApi(context.Background(), &launchPreflightConfig{
			Epoch: 0, ApiUrl: api.URL, OperatorToken: "synthetic-operator-token", EvaluatorImageDigest: info.EvaluatorImageDigest,
		}, manifest)
		api.Close()
		if err == nil {
			t.Errorf("accepted v2 identity mismatch %s", c.name)
		}
	}
}

// Every required readiness fact must be present and true. A missing fact is
// not an empty set of failures, and an unknown negative fact cannot be ignored.
func TestLaunchApiRejectsIncompleteReadiness(t *testing.T) {
	cases := []struct {
		name   string
		change func(*controller.ReadinessResult)
	}{
		{name: "empty", change: func(ready *controller.ReadinessResult) { ready.Checks = map[string]bool{} }},
		{name: "missing-host", change: func(ready *controller.ReadinessResult) { delete(ready.Checks, "authoritative_evaluator_host") }},
		{name: "failed-host", change: func(ready *controller.ReadinessResult) { ready.Checks["authoritative_evaluator_host"] = false }},
		{name: "unknown-failure", change: func(ready *controller.ReadinessResult) { ready.Checks["synthetic-new-gate"] = false }},
	}
	for _, c := range cases {
		manifest := reviewedSourceTestManifest(t)
		info, ready := launchReadinessTestInfo(manifest)
		c.change(ready)
		api := launchReadinessTestApi(t, info, ready)
		_, _, err := checkCompetitionApi(context.Background(), &launchPreflightConfig{
			Epoch: 0, ApiUrl: api.URL, OperatorToken: "synthetic-operator-token", EvaluatorImageDigest: info.EvaluatorImageDigest,
		}, manifest)
		api.Close()
		if err == nil {
			t.Errorf("accepted readiness case %s", c.name)
		}
	}
}

// A canceled owner cannot allocate source clones or start another Git process.
func TestLaunchSourcePreflightHonorsCanceledOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manifest := reviewedSourceTestManifest(t)
	if err := verifyRemoteSourceEpochModeContext(ctx, time.Second, manifest, 0, t.TempDir(), manifest.EvaluationSource.Branch, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled source preflight: %v", err)
	}
}

// Explicit staging remains supported, but only through its separate source
// manifest and current staging round, never a production round alias.
func TestLaunchApiAuthenticatesExplicitV2Staging(t *testing.T) {
	manifest := stagingSourceTestManifest(t)
	info, ready := launchReadinessTestInfo(manifest)
	info.AutomationPolicy.Staging, info.AutomationPolicy.SettlementMode, info.AutomationPolicy.StagingRoundWindowSeconds = true, "shadow", 3600
	info.ActiveRound.Staging = true
	info.StagingRound, info.ActiveRound = info.ActiveRound, nil
	api := launchReadinessTestApi(t, info, ready)
	defer api.Close()
	if _, _, err := checkCompetitionApi(context.Background(), &launchPreflightConfig{
		Epoch: 0, ApiUrl: api.URL, OperatorToken: "synthetic-operator-token", EvaluatorImageDigest: info.EvaluatorImageDigest,
	}, manifest); err != nil {
		t.Fatal("exact staging runtime was refused:", err)
	}
}

// Additive v2 checks do not reinterpret historical schema-one source records.
func TestLaunchApiPreservesLegacySourceContract(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	info, ready := launchReadinessTestInfo(manifest)
	manifest.Schema, manifest.EvaluationSource.Generations, manifest.EvaluationSource.Rounds = 1, nil, nil
	info.AutomationPolicy, info.ActiveRound = nil, nil
	info.DecisionMode, info.RequiredApiCapability = "", ""
	api := launchReadinessTestApi(t, info, ready)
	defer api.Close()
	if _, _, err := checkCompetitionApi(context.Background(), &launchPreflightConfig{
		Epoch: 0, ApiUrl: api.URL, OperatorToken: "synthetic-operator-token", EvaluatorImageDigest: info.EvaluatorImageDigest,
	}, manifest); err != nil {
		t.Fatal("legacy source record was reinterpreted:", err)
	}
}
