// Synthetic native-log fixtures exercise evidence parsing without running Go,
// opening a provider socket, reading native paths or claiming real observations.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// Mock compiler logs are visibly synthetic and never mark a provider qualified.
func currentQgFixtureMockEvidence(t *testing.T, sources []currentQgQualificationSource, clock string) currentQgNativeFixtureEvidence {
	t.Helper()
	planSha256, err := currentQgNativeFixturePlanSha256(sources)
	if err != nil {
		t.Fatal(err)
	}
	evidence := currentQgNativeFixtureEvidence{Schema: 1, Kind: "current-qg-review-native-fixture-observations/v1", SourcePlanSha256: planSha256, GoVersion: "go1.27.3",
		GoExecutableSha256: strings.Repeat("f", 64), GoVersionOutput: "go version go1.27.3 linux/amd64\n", Runs: []currentQgNativeFixtureRun{}}
	for index, source := range sources {
		for variant, content := range []string{source.Before, source.After} {
			probe, err := currentQgNativeFixtureProbe(content)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "synthetic_probe_test.go", probe, parser.AllErrors); err != nil {
				t.Fatal("embedded current probe is not complete source", err)
			}
			var stream bytes.Buffer
			writeEvent := func(value map[string]any) {
				value["Time"], value["Elapsed"] = clock, 0.001
				content, _ := json.Marshal(value)
				stream.Write(content)
				stream.WriteByte('\n')
			}
			exitCode := 0
			if index+1 == 6 || index+1 == 11 || index+1 == 18 {
				exitCode = 1
				writeEvent(map[string]any{"Action": "build-output", "ImportPath": "command-line-arguments [command-line-arguments.test]", "Output": "./source.go:3: undefined: synthetic_unavailable_helper\n"})
				writeEvent(map[string]any{"Action": "build-fail", "ImportPath": "command-line-arguments"})
				writeEvent(map[string]any{"Action": "fail", "Package": "command-line-arguments"})
			} else {
				writeEvent(map[string]any{"Action": "start", "Package": "command-line-arguments"})
				writeEvent(map[string]any{"Action": "run", "Package": "command-line-arguments", "Test": "TestFixtureObservation"})
				writeEvent(map[string]any{"Action": "output", "Package": "command-line-arguments", "Test": "TestFixtureObservation", "Output": fmt.Sprintf("fixture_observation={\"synthetic_offline_result\":%d}\n", index+1)})
				writeEvent(map[string]any{"Action": "pass", "Package": "command-line-arguments", "Test": "TestFixtureObservation"})
				writeEvent(map[string]any{"Action": "pass", "Package": "command-line-arguments"})
			}
			evidence.Runs = append(evidence.Runs, currentQgNativeFixtureRun{Position: index + 1, Variant: []string{"pristine_base", "patched_source"}[variant], SourceSha256: currentQgQualificationSha256([]byte(content)),
				ProbeSha256: currentQgQualificationSha256(probe), ExitCode: exitCode, Stdout: stream.String(), StdoutSha256: currentQgQualificationSha256(stream.Bytes()), StderrSha256: currentQgQualificationSha256(nil)})
		}
	}
	return evidence
}

// All 38 source/probe/log identities are mandatory. Old twelve-variant/native
// receipts, substituted outputs, skipped tests and missing failures are refused.
func TestCurrentQgQualificationFixtureEvidenceBindsEveryVariant(t *testing.T) {
	plan, expectations, sources, _ := currentQgExecutionFixture(t)
	requests := []winnerReviewBrokerRequest{}
	for _, item := range plan.Cases {
		requests = append(requests, item.Request)
	}
	original, _ := json.Marshal(requests)
	evidence := currentQgFixtureMockEvidence(t, sources, "synthetic-original-clock")
	content, _ := json.Marshal(evidence)
	bound, returned, err := bindCurrentQgNativeFixtureEvidence(requests, sources, content, expectations)
	if err != nil || len(bound) != 19 || !reflect.DeepEqual(returned, expectations) {
		t.Fatal("complete synthetic native evidence shape refused", err)
	}
	after, _ := json.Marshal(requests)
	if !bytes.Equal(original, after) {
		t.Fatal("binding mutated preliminary immutable preparation requests")
	}
	for index, request := range bound {
		request.Operation = "review_elected_winner"
		input, err := winnerModelInput(request)
		if err != nil || currentQgQualificationContainsExpectedLabels(input) || !bytes.Contains(request.Packet.Score, []byte("native_source_observations")) ||
			request.Template.ScoreSha256 == requests[index].Template.ScoreSha256 || request.Template.ScoreSha256 != request.Packet.Entry.ScoreSha256 ||
			strings.Contains(request.Instructions, "No test, compiler, production deployment or timing result has been observed yet.") {
			t.Fatal("bound native source evidence lost current readable hash/claim identity", err)
		}
	}
	for _, mutate := range []func(*currentQgNativeFixtureEvidence){
		func(e *currentQgNativeFixtureEvidence) { e.SourcePlanSha256 = strings.Repeat("0", 64) },
		func(e *currentQgNativeFixtureEvidence) { e.Runs = e.Runs[:12] },
		func(e *currentQgNativeFixtureEvidence) { e.GoVersionOutput = "go version go1.26 linux/amd64\n" },
		func(e *currentQgNativeFixtureEvidence) { e.Runs[0], e.Runs[1] = e.Runs[1], e.Runs[0] },
		func(e *currentQgNativeFixtureEvidence) { e.Runs[0].Stdout += "altered" },
		func(e *currentQgNativeFixtureEvidence) {
			e.Runs[0].Stdout = strings.Replace(e.Runs[0].Stdout, `"Action":"pass"`, `"Action":"skip"`, 1)
			e.Runs[0].StdoutSha256 = currentQgQualificationSha256([]byte(e.Runs[0].Stdout))
		},
		func(e *currentQgNativeFixtureEvidence) { e.Runs[10].ExitCode = 0 },
	} {
		var changed currentQgNativeFixtureEvidence
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		changedBytes, _ := json.Marshal(changed)
		if _, _, err := bindCurrentQgNativeFixtureEvidence(requests, sources, changedBytes, expectations); err == nil {
			t.Fatal("altered/incomplete native source evidence entered final current QG input")
		}
	}
}

// Raw execution clocks remain retained, but changing them cannot create a new
// semantic input/draw. Function/compiler facts, sources and probes still bind.
func TestCurrentQgQualificationFixtureClocksStayOutsideSemanticInput(t *testing.T) {
	plan, expectations, sources, _ := currentQgExecutionFixture(t)
	requests := []winnerReviewBrokerRequest{}
	for _, item := range plan.Cases {
		requests = append(requests, item.Request)
	}
	inputs := [][]winnerReviewBrokerRequest{}
	for _, clock := range []string{"synthetic-first-clock", "synthetic-second-clock"} {
		content, _ := json.Marshal(currentQgFixtureMockEvidence(t, sources, clock))
		bound, _, err := bindCurrentQgNativeFixtureEvidence(requests, sources, content, expectations)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, bound)
	}
	if !reflect.DeepEqual(inputs[0], inputs[1]) {
		t.Fatal("raw native Time/Elapsed fields allowed a new semantic provider input")
	}
}
