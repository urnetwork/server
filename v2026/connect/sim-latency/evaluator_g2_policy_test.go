// The actual new shell predicates and CLI grammar are exercised with synthetic
// metadata only; no container, service, compilation or workload is started.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026/model"
)

// Extracts a complete real function, never a rewritten copy of the predicate.
func g2ScriptFunction(t *testing.T, path, name string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := "\n" + name + "() {\n"
	start := strings.Index(string(content), marker)
	if start < 0 {
		t.Fatal("g2 script function is missing", name)
	}
	end := strings.Index(string(content[start+1:]), "\n}\n")
	if end < 0 {
		t.Fatal("g2 script function is incomplete", name)
	}
	return string(content[start+1 : start+1+end+3])
}

// New readers may replay either exact contract, but never mix their identities.
func TestEvaluatorG2WorkerSchemaRejectsPolicyVersionMismatch(t *testing.T) {
	function := g2ScriptFunction(t, filepath.Join("evaluator", "container", "evaluator-minimum-work.sh"), "validate_worker_request_schema")
	for _, c := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{name: "new policy", valid: true},
		{name: "legacy omission", valid: true, mutate: func(request map[string]any) {
			delete(request["evaluation_policy"].(map[string]any), "g2_volume_policy")
			request["scorer_version"] = model.CompetitionScorerVersion
		}},
		{name: "wrong version", mutate: func(request map[string]any) { request["scorer_version"] = model.CompetitionScorerVersion }},
		{name: "missing policy", mutate: func(request map[string]any) {
			delete(request["evaluation_policy"].(map[string]any), "g2_volume_policy")
		}},
		{name: "unknown policy", mutate: func(request map[string]any) {
			request["evaluation_policy"].(map[string]any)["g2_volume_policy"] = "unknown/1"
		}},
		{name: "explicit empty policy", mutate: func(request map[string]any) { request["evaluation_policy"].(map[string]any)["g2_volume_policy"] = "" }},
		{name: "null policy", mutate: func(request map[string]any) { request["evaluation_policy"].(map[string]any)["g2_volume_policy"] = nil }},
		{name: "nonstring policy", mutate: func(request map[string]any) { request["evaluation_policy"].(map[string]any)["g2_volume_policy"] = 1 }},
		{name: "unknown extension", mutate: func(request map[string]any) { request["evaluation_policy"].(map[string]any)["unknown"] = 1 }},
	} {
		request := evaluatorRequestSchemaFixture()
		request["scorer_version"] = model.CompetitionMinimumWorkScorerVersion
		request["evaluation_policy"].(map[string]any)["g2_volume_policy"] = string(model.CompetitionG2VolumePolicyMinimumWork)
		if c.mutate != nil {
			c.mutate(request)
		}
		content, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+function+"\nvalidate_worker_request_schema \"$1\"", "synthetic-g2-request", path)
		command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		if output, err := command.CombinedOutput(); (err == nil) != c.valid {
			t.Errorf("%s: accepted=%t expected=%t: %s %v", c.name, err == nil, c.valid, output, err)
		}
	}
}

// Source-policy preflight rejects relabeling before paying for any R9 controls.
func TestOfficialRunG2SourceGenerationPolicyMismatchFailsClosed(t *testing.T) {
	function := g2ScriptFunction(t, "official-run.sh", "validate_source_epoch_g2_policy")
	for _, c := range []struct {
		policy, record string
		valid          bool
	}{
		{record: `{}`, valid: true},
		{policy: "minimum-work/1", record: `{"g2_volume_policy":"minimum-work/1"}`, valid: true},
		{policy: "minimum-work/1", record: `{}`},
		{record: `{"g2_volume_policy":"minimum-work/1"}`},
		{policy: "minimum-work/1", record: `{"g2_volume_policy":"unknown/1"}`},
		{policy: "minimum-work/1", record: `{"g2_volume_policy":null}`},
		{record: `{"g2_volume_policy":""}`},
		{policy: "unknown/1", record: `{"g2_volume_policy":"unknown/1"}`},
	} {
		body := "set -Eeuo pipefail\ndie(){ exit 71; }\n" + function + "\nvalidate_source_epoch_g2_policy \"$SOURCE_RECORD\"\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{"PATH=/usr/bin:/bin", "APEX_G2_VOLUME_POLICY=" + c.policy, "SOURCE_RECORD=" + c.record}
		if output, err := command.CombinedOutput(); (err == nil) != c.valid {
			t.Errorf("policy=%q record=%s: accepted=%t expected=%t: %s %v", c.policy, c.record, err == nil, c.valid, output, err)
		}
	}
}

// A new CLI option must be allowed by the actual explicit run grammar, yet
// omission must never become an empty new entry in legacy frozen run flags.
func TestSimulationG2RunGrammarAndLegacyFlagOmission(t *testing.T) {
	for _, policy := range []string{"", "minimum-work/1"} {
		args := []string{"run", "--epoch=0"}
		if policy != "" {
			args = append(args, "--g2-volume-policy="+policy)
		}
		opts, err := docopt.ParseArgs(simulationUsage(), args, "synthetic-test")
		if err != nil || optString(opts, "--g2-volume-policy", "") != policy {
			t.Fatal("run policy grammar disagrees with the official runner", policy, err)
		}
	}
	path := filepath.Join(t.TempDir(), "providers.yml")
	if err := os.WriteFile(path, []byte("synthetic providers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := &RunOptions{ConfigPath: path, Duration: time.Second, RequestTimeout: time.Millisecond}
	legacy, err := newRunStats(options, &Config{Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	options.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	future, err := newRunStats(options, &Config{Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Flags, future.Flags) || legacy.ScorerVersion != model.CompetitionScorerVersion || future.ScorerVersion != model.CompetitionMinimumWorkScorerVersion {
		t.Fatal("policy changed historical run flags or lost its implementation version")
	}
	for name := range legacy.Flags {
		if strings.Contains(name, "g2") {
			t.Fatal("empty policy leaked into frozen run flags")
		}
	}
	content, err := json.Marshal(legacy)
	if err != nil || bytes.Contains(content, []byte("g2_volume_policy")) {
		t.Fatal("legacy run metadata acquired a new field", err)
	}
}

// The current producer remains the exact sealed source, while the future
// baseline-only program pins the new producer instead of changing that seal.
func TestEvaluatorG2PreparationPinsSeparateProducerBytes(t *testing.T) {
	legacy, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := sha256.Sum256(legacy)
	if hex.EncodeToString(legacyDigest[:]) != "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b" {
		t.Fatal("legacy sealed evaluator changed")
	}
	future, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-minimum-work.sh"))
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := os.ReadFile(filepath.Join("evaluator", "container", "prepare-round-baseline-minimum-work.sh"))
	if err != nil {
		t.Fatal(err)
	}
	futureDigest := sha256.Sum256(future)
	if !bytes.Contains(preparer, []byte(hex.EncodeToString(futureDigest[:]))) || bytes.Contains(preparer, []byte(hex.EncodeToString(legacyDigest[:]))) ||
		!bytes.Contains(preparer, []byte(`.evaluation_policy.g2_volume_policy == "minimum-work/1"`)) ||
		!bytes.Contains(future, []byte(`"APEX_G2_VOLUME_POLICY=$g2_volume_policy"`)) ||
		!bytes.Contains(future, []byte(`score-policy --g2-volume-policy "$g2_volume_policy"`)) {
		t.Fatal("future preparation or runtime omitted its independent policy binding")
	}
}
