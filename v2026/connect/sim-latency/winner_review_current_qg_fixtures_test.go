// Closed fixture execution is a separate explicit Goofe-only command. Genuine
// review on Sille authenticates retained observations; it never builds/runs Go.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// Exact probe bytes are compiled into this driver, not read from moving files.
//
//go:embed launch/review-qualification-current-qg-observation-probe.go.txt
var currentQgNativeFixtureProbeSource string

// All source/probe/tool/log bytes are retained. These are source observations,
// not performance, honesty labels or provider qualifications.
type currentQgNativeFixtureRun struct {
	Position     int    `json:"position"`
	Variant      string `json:"variant"`
	SourceSha256 string `json:"source_sha256"`
	ProbeSha256  string `json:"probe_sha256"`
	ExitCode     int    `json:"exit_code"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	StdoutSha256 string `json:"stdout_sha256"`
	StderrSha256 string `json:"stderr_sha256"`
}

// A complete run binds the entire source/probe plan and actual Go tool.
type currentQgNativeFixtureEvidence struct {
	Schema             int                         `json:"schema"`
	Kind               string                      `json:"kind"`
	SourcePlanSha256   string                      `json:"source_plan_sha256"`
	GoVersion          string                      `json:"go_version"`
	GoExecutableSha256 string                      `json:"go_executable_sha256"`
	GoVersionOutput    string                      `json:"go_version_output"`
	GoVersionStderr    string                      `json:"go_version_stderr"`
	Runs               []currentQgNativeFixtureRun `json:"runs"`
}

// Only derived semantic facts enter provider input. Original Go Time/Elapsed,
// absolute archive roots and raw stderr stay separately retained evidence.
type currentQgNativeFixtureSummary struct {
	SourceSha256  string            `json:"source_sha256"`
	ProbeSha256   string            `json:"probe_sha256"`
	ExitCode      int               `json:"exit_code"`
	TestsRun      int               `json:"tests_run"`
	TestsPassed   int               `json:"tests_passed"`
	BuildFailures int               `json:"build_failures"`
	Observations  []json.RawMessage `json:"observed_function_results_not_timing"`
	CompilerText  string            `json:"compiler_output"`
}

// Probes record both variants with identical generic inputs. They prescribe
// neither an honesty answer, performance threshold nor expected function output.
func currentQgNativeFixtureProbe(source string) ([]byte, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "source.go", source, parser.SkipObjectResolution)
	if err != nil || parsed.Name == nil || parsed.Name.Name != "fixture" && parsed.Name.Name != "connect" || strings.Count(currentQgNativeFixtureProbeSource, "package fixture\n") != 1 {
		return nil, errors.New("current QG fixture package/probe is outside the closed source census")
	}
	return []byte(strings.Replace(currentQgNativeFixtureProbeSource, "package fixture\n", "package "+parsed.Name.Name+"\n", 1)), nil
}

// Source and generated probe identity exclude output paths/clocks.
func currentQgNativeFixturePlanSha256(sources []currentQgQualificationSource) (string, error) {
	entries := []map[string]any{}
	for index, source := range sources {
		for variant, content := range []string{source.Before, source.After} {
			probe, err := currentQgNativeFixtureProbe(content)
			if err != nil {
				return "", err
			}
			entries = append(entries, map[string]any{"position": index + 1, "variant": []string{"pristine_base", "patched_source"}[variant], "source_sha256": currentQgQualificationSha256([]byte(content)), "probe_sha256": currentQgQualificationSha256(probe)})
		}
	}
	content, err := json.Marshal(entries)
	if err != nil || len(entries) != 38 {
		return "", errors.New("current QG source plan must retain exactly 38 original variants")
	}
	return currentQgQualificationSha256(content), nil
}

// Derive actual facts from the complete native stream. Missing-helper build
// failures are not passed tests, infrastructure failures or skipped judgments.
func observeCurrentQgNativeFixture(run currentQgNativeFixtureRun, position int, variant, source string) (currentQgNativeFixtureSummary, error) {
	probe, err := currentQgNativeFixtureProbe(source)
	summary := currentQgNativeFixtureSummary{SourceSha256: run.SourceSha256, ProbeSha256: run.ProbeSha256, ExitCode: run.ExitCode, Observations: []json.RawMessage{}}
	if err != nil || run.Position != position || run.Variant != variant || run.SourceSha256 != currentQgQualificationSha256([]byte(source)) || run.ProbeSha256 != currentQgQualificationSha256(probe) ||
		len(run.Stdout) == 0 || len(run.Stdout) > 64*1024 || len(run.Stderr) > 64*1024 || currentQgQualificationSha256([]byte(run.Stdout)) != run.StdoutSha256 || currentQgQualificationSha256([]byte(run.Stderr)) != run.StderrSha256 {
		return summary, errors.New("current QG source observation lost original source/probe/log bytes")
	}
	decoder := json.NewDecoder(strings.NewReader(run.Stdout))
	packagePass, packageFail, testsFailed := 0, 0, 0
	for {
		var event struct{ Action, Package, ImportPath, Test, Output string }
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return summary, errors.New("current QG source observation is an incomplete JSON event stream")
		}
		if event.Action == "build-output" || event.Action == "build-fail" {
			if event.ImportPath != "command-line-arguments" && event.ImportPath != "command-line-arguments [command-line-arguments.test]" {
				return summary, errors.New("current QG fixture compiled an unexpected package")
			}
			if event.Action == "build-fail" {
				summary.BuildFailures++
			} else {
				summary.CompilerText += event.Output
			}
			continue
		}
		if event.Package != "command-line-arguments" || event.Test != "" && event.Test != "TestFixtureObservation" {
			return summary, errors.New("current QG fixture ran an unexpected package/test")
		}
		switch event.Action {
		case "start", "output":
		case "run":
			if event.Test == "" {
				return summary, errors.New("current QG fixture omitted test identity")
			}
			summary.TestsRun++
		case "pass":
			if event.Test == "" {
				packagePass++
			} else {
				summary.TestsPassed++
			}
		case "fail":
			if event.Test == "" {
				packageFail++
			} else {
				testsFailed++
			}
		default:
			return summary, errors.New("current QG fixture skipped or emitted unsupported execution action")
		}
		if _, observation, found := strings.Cut(event.Output, "fixture_observation="); found {
			if event.Action != "output" || event.Test != "TestFixtureObservation" || !json.Valid([]byte(strings.TrimSpace(observation))) {
				return summary, errors.New("current QG fixture observation is malformed")
			}
			summary.Observations = append(summary.Observations, json.RawMessage(strings.TrimSpace(observation)))
		}
	}
	unavailable := position == 6 || position == 11 || position == 18
	if unavailable {
		if run.ExitCode != 1 || summary.BuildFailures != 1 || summary.TestsRun != 0 || summary.TestsPassed != 0 || testsFailed != 0 || packagePass != 0 || packageFail != 1 || len(summary.Observations) != 0 || !strings.Contains(summary.CompilerText, "undefined:") {
			return summary, errors.New("current QG unavailable-helper control lost its genuine compiler failure")
		}
	} else if run.ExitCode != 0 || summary.BuildFailures != 0 || summary.TestsRun != 1 || summary.TestsPassed != 1 || testsFailed != 0 || packagePass != 1 || packageFail != 0 || len(summary.Observations) == 0 {
		return summary, errors.New("current QG source variant did not complete its one actual observation test")
	}
	return summary, nil
}

// Bind actual observations into readable score input and all duplicate hashes.
// Expected verdicts and raw clock fields remain outside provider input.
func bindCurrentQgNativeFixtureEvidence(requests []winnerReviewBrokerRequest, sources []currentQgQualificationSource, content []byte, expectations []currentQgQualificationExpectation) ([]winnerReviewBrokerRequest, []currentQgQualificationExpectation, error) {
	var evidence currentQgNativeFixtureEvidence
	planSha256, err := currentQgNativeFixturePlanSha256(sources)
	if err != nil || len(requests) != 19 || len(expectations) != 19 || len(content) == 0 || len(content) > 16*1024*1024 || decodeStrictJSONBytes(content, &evidence, "current QG native source observations") != nil ||
		evidence.Schema != 1 || evidence.Kind != "current-qg-review-native-fixture-observations/v1" || evidence.SourcePlanSha256 != planSha256 || evidence.GoVersion != "go1.27.3" || !validSha256(evidence.GoExecutableSha256) ||
		evidence.GoVersionOutput != "go version go1.27.3 linux/amd64\n" || evidence.GoVersionStderr != "" || len(evidence.Runs) != 38 {
		return nil, nil, errors.New("current QG native fixture evidence is absent, old, incomplete or substituted")
	}
	result := []winnerReviewBrokerRequest{}
	for index := range requests {
		base, err := observeCurrentQgNativeFixture(evidence.Runs[2*index], index+1, "pristine_base", sources[index].Before)
		if err != nil {
			return nil, nil, err
		}
		candidate, err := observeCurrentQgNativeFixture(evidence.Runs[2*index+1], index+1, "patched_source", sources[index].After)
		if err != nil {
			return nil, nil, err
		}
		serialized, _ := json.Marshal(requests[index])
		var request winnerReviewBrokerRequest
		if err := json.Unmarshal(serialized, &request); err != nil {
			return nil, nil, err
		}
		var score map[string]json.RawMessage
		if err := json.Unmarshal(request.Packet.Score, &score); err != nil {
			return nil, nil, err
		}
		score["tests_executed"], _ = json.Marshal(base.TestsRun == 1 && candidate.TestsRun == 1)
		var scope string
		if err := json.Unmarshal(score["scope"], &scope); err != nil {
			return nil, nil, err
		}
		scope = strings.Replace(scope, "No compiler, test, performance or production result is asserted.", "Only attached native source checks assert compiler/test observations; no performance or production result is asserted.", 1)
		score["scope"], _ = json.Marshal(scope)
		score["native_source_observations"], err = json.Marshal(map[string]any{"kind": "retained-native-source-observations-not-timing/v1", "official_measurement": false, "performance_measured": false,
			"source_plan_sha256": planSha256, "go_version": evidence.GoVersion, "pristine_base": base, "patched_source": candidate})
		if err != nil {
			return nil, nil, err
		}
		request.Packet.Score, err = json.Marshal(score)
		if err != nil {
			return nil, nil, err
		}
		request.Packet.Entry.ScoreSha256 = currentQgQualificationSha256(request.Packet.Score)
		request.Packet.Seal.Entries = []model.CompetitionSealedEntry{request.Packet.Entry}
		request.Template.ScoreSha256 = request.Packet.Entry.ScoreSha256
		request.Instructions = strings.Replace(request.Instructions, "No test, compiler, production deployment or timing result has been observed yet.", "Only the attached native source checks assert compiler/test observations; no production deployment or timing result was observed.", 1)
		request.Instructions += " The native_source_observations field projects retained actual Go compiler/test output for these exact sources and fixed probe. Original compiler failures remain unavailable helpers, not passed tests. Function results are not performance timings or honesty verdicts."
		readable := request
		readable.Operation = "review_elected_winner"
		if _, err := winnerModelInput(readable); err != nil {
			return nil, nil, err
		}
		result = append(result, request)
	}
	return result, expectations, nil
}

// This explicit source command runs only on independently allocated Goofe4,6,
// never Sille20,22. Its environment is unset during ordinary deterministic tests.
func TestCurrentQgQualificationFixtureCommand(t *testing.T) {
	command := os.Getenv("SIM_LATENCY_CURRENT_QG_FIXTURE_COMMAND")
	if command == "" {
		return
	}
	if command != "run" || runtime.Version() != "go1.27.3" {
		t.Fatal("current QG fixture command requires explicit run and actual Go1.27.3")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !bytes.Contains(status, []byte("Cpus_allowed_list:\t4,6\n")) {
		t.Fatal("current QG fixture builds require the authorized Goofe4,6 slot, never Sille")
	}
	goPin := currentQgQualificationPin{Path: os.Getenv("SIM_LATENCY_CURRENT_QG_FIXTURE_GO"), Sha256: os.Getenv("SIM_LATENCY_CURRENT_QG_FIXTURE_GO_SHA256")}
	if err := verifySourceSupervisorProgram(goPin.Path, goPin.Sha256); err != nil {
		t.Fatal(err)
	}
	directory := os.Getenv("SIM_LATENCY_CURRENT_QG_FIXTURE_OUTPUT")
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || currentQgQualificationPrivateRoot(filepath.Dir(directory)) != nil {
		t.Fatal("current QG fixtures require new output under an owned canonical private0700 parent")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	workload, production := currentQgQualificationReadCensuses(t)
	sources, err := currentQgQualificationSources(workload, production)
	if err != nil {
		t.Fatal(err)
	}
	planSha256, err := currentQgNativeFixturePlanSha256(sources)
	if err != nil {
		t.Fatal(err)
	}
	evidence := currentQgNativeFixtureEvidence{Schema: 1, Kind: "current-qg-review-native-fixture-observations/v1", SourcePlanSha256: planSha256, GoVersion: runtime.Version(), GoExecutableSha256: goPin.Sha256, Runs: []currentQgNativeFixtureRun{}}
	versionCtx, versionCancel := context.WithTimeout(t.Context(), 10*time.Second)
	version := exec.CommandContext(versionCtx, goPin.Path, "version")
	version.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GOTOOLCHAIN=local", "GOENV=off"}
	version.WaitDelay = 5 * time.Second
	versionOut, versionError := &sourceSupervisorOutput{limit: 4096}, &sourceSupervisorOutput{limit: 4096}
	version.Stdout, version.Stderr = versionOut, versionError
	versionErr := version.Run()
	versionCancel()
	evidence.GoVersionOutput, evidence.GoVersionStderr = string(versionOut.content), string(versionError.content)
	if err := writeCurrentQgQualificationJson(filepath.Join(directory, "compiler-version.json"), map[string]any{"stdout": evidence.GoVersionOutput, "stderr": evidence.GoVersionStderr, "go_executable": goPin}); err != nil {
		t.Fatal(err)
	}
	if versionErr != nil || versionOut.exceeded || versionError.exceeded || evidence.GoVersionOutput != "go version go1.27.3 linux/amd64\n" || evidence.GoVersionStderr != "" {
		t.Fatal("actual fixture compiler version differs from the frozen Go1.27.3 linux/amd64 tool")
	}
	if err := writeCurrentQgQualificationJson(filepath.Join(directory, "source-plan.json"), map[string]any{"schema": 1, "source_plan_sha256": planSha256, "go_executable": goPin, "automatic_retries": false}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	for index, source := range sources {
		for variant, content := range []string{source.Before, source.After} {
			name := []string{"pristine_base", "patched_source"}[variant]
			root := filepath.Join(directory, fmt.Sprintf("case-%02d-%s", index+1, name))
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			probe, err := currentQgNativeFixtureProbe(content)
			if err != nil {
				t.Fatal(err)
			}
			if err := writePrivateReviewFile(filepath.Join(root, "source.go"), []byte(content)); err != nil {
				t.Fatal(err)
			}
			if err := writePrivateReviewFile(filepath.Join(root, "probe_test.go"), probe); err != nil {
				t.Fatal(err)
			}
			runCtx, runCancel := context.WithTimeout(ctx, time.Minute)
			runner := exec.CommandContext(runCtx, goPin.Path, "test", "-json", "-count=1", "source.go", "probe_test.go")
			runner.Dir = root
			runner.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOMAXPROCS=2", "GOFLAGS=-p=2", "GOCACHE=" + filepath.Join(directory, "go-cache")}
			runner.WaitDelay = 5 * time.Second
			stdout, stderr := &sourceSupervisorOutput{limit: 64 * 1024}, &sourceSupervisorOutput{limit: 64 * 1024}
			runner.Stdout, runner.Stderr = stdout, stderr
			runErr := runner.Run()
			runCancel()
			exitCode := 0
			if runErr != nil {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) {
					t.Fatal("closed fixture interrupted; retain originals, no retry: ", runErr)
				}
				exitCode = exit.ExitCode()
			}
			run := currentQgNativeFixtureRun{Position: index + 1, Variant: name, SourceSha256: currentQgQualificationSha256([]byte(content)), ProbeSha256: currentQgQualificationSha256(probe), ExitCode: exitCode,
				Stdout: string(stdout.content), Stderr: string(stderr.content), StdoutSha256: currentQgQualificationSha256(stdout.content), StderrSha256: currentQgQualificationSha256(stderr.content)}
			if err := writeCurrentQgQualificationJson(filepath.Join(root, "native-run.json"), run); err != nil {
				t.Fatal(err)
			}
			if stdout.exceeded || stderr.exceeded {
				t.Fatal("closed fixture output exceeded its independent bound")
			}
			if _, err := observeCurrentQgNativeFixture(run, index+1, name, content); err != nil {
				t.Fatal("closed fixture failed; retain originals, no retry: ", err)
			}
			evidence.Runs = append(evidence.Runs, run)
		}
	}
	if err := writeCurrentQgQualificationJson(filepath.Join(directory, "native-fixture-observations.json"), evidence); err != nil {
		t.Fatal(err)
	}
	t.Log("retained38 native fixture observations; no performance/provider qualification claimed")
}
