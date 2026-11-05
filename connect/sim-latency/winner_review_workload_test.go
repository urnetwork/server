// These synthetic tests prove mandatory context/check enforcement, not model
// detection quality. No provider, production artifact or measured run is used.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Ordinary protocol fixtures need readable synthetic source, not production
// identities. Mechanism-specific tests below provide their own actual behavior.
func winnerWorkloadContextFixture(commit string) *winnerWorkloadReviewContext {
	value := &winnerWorkloadReviewContext{Version: winnerWorkloadReviewVersion, Addendum: winnerWorkloadReviewInstructions,
		AddendumSha256: winnerWorkloadReviewAddendumSha256(), ServerCommit: commit}
	for _, path := range winnerWorkloadInstrumentPaths() {
		content := "package main\n\n// Synthetic frozen instrument context for " + path + ".\n"
		digest := sha256.Sum256([]byte(content))
		value.InstrumentFiles = append(value.InstrumentFiles, winnerReviewSourceFile{Path: path, Content: content, Sha256: hex.EncodeToString(digest[:])})
	}
	return value
}

// Even legacy-G2 synthetic reviews carry both frozen compiled binary pins.
func winnerWorkloadInstrumentFixture(commit, imageDigest string) *controller.CompetitionReviewInstrument {
	simulator, scorer := strings.Repeat("7", 64), strings.Repeat("8", 64)
	return &controller.CompetitionReviewInstrument{Schema: 1, ServerCommit: commit, ImageDigest: imageDigest,
		SimulatorSha256: simulator, ScorerSha256: scorer,
		EvaluationPolicy: model.CompetitionEvaluationPolicy{SimulatorSha256: simulator, ScorerSha256: scorer}}
}

// The same retained-input and model-output binding used in production, with
// explicitly synthetic provider metadata and no transport operation.
func winnerBrokerAuthenticatedFixture(t *testing.T) (winnerReviewBrokerResult, winnerReviewBrokerRequest) {
	t.Helper()
	request, judgment := winnerModelFixture()
	input, err := winnerModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input)
	providerModel := winnerReviewProviderModelName(request.Request.ReviewerModel)
	evidence, err := json.Marshal(winnerModelEvidence{ProviderId: "synthetic-workload-review", RequestedModel: providerModel,
		PolicyModel: request.Request.ReviewerModel, Model: providerModel, Reasoning: request.Request.ReviewerReasoning,
		InputSha256: hex.EncodeToString(digest[:]), Usage: json.RawMessage(`{"input_tokens":10,"output_tokens":10}`), Judgment: judgment, ProviderOutput: string(output)})
	if err != nil {
		t.Fatal(err)
	}
	return *winnerModelBrokerResult(request, judgment, evidence), request
}

// The exact locked server commit supplies untouched protected files even when
// the worktree changes afterward and the patch targets a sibling repository.
func TestWinnerWorkloadReviewLoadsUntouchedFrozenInstrument(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	serverRoot := filepath.Join(root, "server")
	fixture := winnerWorkloadContextFixture(commits["server"])
	for index, file := range fixture.InstrumentFiles {
		if winnerWorkloadInstrumentMayBeAbsent(file.Path) {
			digest := sha256.Sum256(nil)
			fixture.InstrumentFiles[index] = winnerReviewSourceFile{Path: file.Path, Sha256: hex.EncodeToString(digest[:]), AbsentAtBase: true}
			continue
		}
		path := filepath.Join(root, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.Content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sourceTestGit(t, serverRoot, "add", "--", "connect/sim-latency")
	sourceTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic frozen instrument")
	commits["server"] = sourceTestGit(t, serverRoot, "rev-parse", "HEAD")
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	packet := &controller.CompetitionSourceReviewPacket{BaseSha: commits["server"], SourceCommits: commits,
		PatchPolicy: controller.PatchPolicy{PathBase: "mono_root_v1", AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}},
		Entry:       model.CompetitionSealedEntry{BaseSourceLockSha256: lock}}
	packet.EvaluatedImageDigest = "sha256:" + strings.Repeat("9", 64)
	packet.Instrument = winnerWorkloadInstrumentFixture(packet.BaseSha, packet.EvaluatedImageDigest)
	if err := os.WriteFile(filepath.Join(root, fixture.InstrumentFiles[0].Path), []byte("moving worktree must never become review evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "connect/sim-latency/work_coverage.go"), []byte("moving helper must remain absent at the frozen base"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := loadWinnerWorkloadReviewContext(t.Context(), root, packet, defaultWinnerWorkloadReviewSettings())
	if err != nil || value == nil || value.ServerCommit != commits["server"] || len(value.InstrumentFiles) != len(fixture.InstrumentFiles) {
		t.Fatal("untouched frozen context was not materialized", err)
	}
	for index, file := range value.InstrumentFiles {
		if file != fixture.InstrumentFiles[index] {
			t.Fatal("context came from moving source or another repository", file.Path)
		}
	}
	settings := defaultWinnerWorkloadReviewSettings()
	settings.MaximumBytes = len(value.InstrumentFiles[0].Content)
	if _, err := loadWinnerWorkloadReviewContext(t.Context(), root, packet, settings); err == nil {
		t.Fatal("oversized context was truncated or accepted")
	}
	packet.SourceCommits["connect"] = strings.Repeat("f", 40)
	if _, err := loadWinnerWorkloadReviewContext(t.Context(), root, packet, defaultWinnerWorkloadReviewSettings()); err == nil {
		t.Fatal("changed frozen source lock supplied instrument evidence")
	}
}

// An unavailable protected file is a review hold, not permission to guess the
// crawl/scoring contract or silently fall back to the current checkout.
func TestWinnerWorkloadReviewMissingInstrumentFailsClosed(t *testing.T) {
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "server")
	packet := &controller.CompetitionSourceReviewPacket{BaseSha: commit, EvaluatedImageDigest: "sha256:" + strings.Repeat("9", 64)}
	packet.Instrument = winnerWorkloadInstrumentFixture(packet.BaseSha, packet.EvaluatedImageDigest)
	if _, err := loadWinnerWorkloadReviewContext(t.Context(), root, packet, defaultWinnerWorkloadReviewSettings()); err == nil {
		t.Fatal("missing frozen instrument context accepted")
	}
}

// Both model transports refuse a missing, substituted or old clarification
// before a provider call. Digests and source identities are checked separately.
func TestWinnerWorkloadReviewContextRequiredBeforeEveryProvider(t *testing.T) {
	for _, snow := range []bool{false, true} {
		for _, mutate := range []func(*winnerReviewBrokerRequest){
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview = nil },
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.Version = "synthetic-old/0" },
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.Addendum += " altered" },
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.AddendumSha256 = strings.Repeat("f", 64) },
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.ServerCommit = strings.Repeat("b", 40) },
			func(r *winnerReviewBrokerRequest) {
				r.WorkloadReview.InstrumentFiles = r.WorkloadReview.InstrumentFiles[:1]
			},
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.InstrumentFiles[0].Content += "tampered" },
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.InstrumentFiles[0].AbsentAtBase = true },
			func(r *winnerReviewBrokerRequest) { r.WorkloadReview.InstrumentFiles[0].Path = "connect/client.go" },
		} {
			request, _ := winnerModelFixture()
			if snow {
				request, _ = winnerSnowJudgmentFixture()
			}
			mutate(&request)
			calls := 0
			client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("synthetic forbidden provider call")
			})}
			var err error
			if snow {
				directory := t.TempDir()
				if err := os.Chmod(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				_, err = executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), defaultWinnerSnowProviderSettings(), strings.Repeat("a", 64), directory)
			} else {
				_, err = executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings())
			}
			if err == nil || calls != 0 || !strings.Contains(err.Error(), "traffic-preservation") {
				t.Fatalf("snow=%t context mutation reached provider: calls=%d error=%v", snow, calls, err)
			}
		}
	}
}

// The provider schema and native member validator both require an explicit
// versioned check; six generic passing criteria are no longer sufficient.
func TestWinnerWorkloadReviewRejectsMissingOrUncertainApproval(t *testing.T) {
	for _, mutate := range []func(*winnerModelJudgment){
		func(j *winnerModelJudgment) { j.WorkloadPreservation = nil },
		func(j *winnerModelJudgment) { j.WorkloadPreservation.Version = "synthetic-old/0" },
		func(j *winnerModelJudgment) { j.WorkloadPreservation.Explanation = " " },
		func(j *winnerModelJudgment) { j.WorkloadPreservation.Disposition = "uncertain" },
		func(j *winnerModelJudgment) { j.WorkloadPreservation.Disposition = "fail" },
	} {
		request, judgment := winnerSnowJudgmentFixture()
		mutate(&judgment)
		if validateWinnerSnowMember(judgment, request) == nil {
			t.Fatal("missing, old or unresolved check authorized a member")
		}
	}
	request, judgment := winnerSnowJudgmentFixture()
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	var instance map[string]any
	if err := json.Unmarshal(content, &instance); err != nil {
		t.Fatal(err)
	}
	delete(instance, "workload_preservation")
	for _, schema := range []map[string]any{winnerModelSchema(), winnerSnowModelSchema(request.Request.Policy)} {
		if winnerSnowResolvedSchema(t, schema).Validate(instance) == nil {
			t.Fatal("provider schema allowed a skipped traffic-preservation check")
		}
	}
}

// Every union member receives and must satisfy the same version. One complete
// review cannot cover for another member's missing or uncertain subcheck.
func TestWinnerWorkloadReviewUnionRequiresEveryMemberCheck(t *testing.T) {
	for _, mutate := range []func(*winnerModelJudgment){
		func(j *winnerModelJudgment) { j.WorkloadPreservation = nil },
		func(j *winnerModelJudgment) { j.WorkloadPreservation.Version = "synthetic-old/0" },
		func(j *winnerModelJudgment) {
			j.Decision = "uncertain"
			j.WorkloadPreservation.Disposition = "uncertain"
		},
	} {
		request, members := winnerSnowMemberFixture(t)
		// Existing fixtures intentionally share immutable judgments; own this
		// member's check before mutating it so the sibling is a real control.
		check := *members[1].Judgment.WorkloadPreservation
		members[1].Judgment.WorkloadPreservation = &check
		mutate(&members[1].Judgment)
		content, err := json.Marshal(members[1].Judgment)
		if err != nil {
			t.Fatal(err)
		}
		members[1].ProviderOutput = string(content)
		if aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request).Decision != "uncertain" {
			t.Fatal("one member's omitted traffic-preservation review authorized the union")
		}
	}
}

// Rehashed context is still a changed non-clock identity. Accepted judgments
// from before the clarification cannot bypass it or trigger a model reroll.
func TestWinnerWorkloadReviewCacheBindsAddendumAndInstrument(t *testing.T) {
	result, request := winnerBrokerAuthenticatedFixture(t)
	output, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "accepted.json")
		original := request
		if legacy {
			original.WorkloadReview = nil
		}
		if _, _, err := winnerReviewResponse(path, original, func() ([]byte, error) { return output, nil }); err != nil {
			t.Fatal(err)
		}
		if err := cacheWinnerReviewJudgment(path, output); err != nil {
			t.Fatal(err)
		}
		changed := request
		if !legacy {
			changed.WorkloadReview = winnerWorkloadContextFixture(request.Packet.BaseSha)
			changed.WorkloadReview.InstrumentFiles[0].Content += "// changed frozen evidence\n"
			digest := sha256.Sum256([]byte(changed.WorkloadReview.InstrumentFiles[0].Content))
			changed.WorkloadReview.InstrumentFiles[0].Sha256 = hex.EncodeToString(digest[:])
		}
		if _, _, err := winnerReviewResponse(path, changed, func() ([]byte, error) {
			t.Fatal("changed traffic-preservation context rerolled an accepted judgment")
			return nil, nil
		}); err == nil {
			t.Fatal("old or changed context reused accepted review authority")
		}
	}
	request.WorkloadReview = nil
	if validateWinnerReviewBrokerResult(&result, request.Template, request.Request.Policy, request) == nil {
		t.Fatal("old input authorized a new single-provider result")
	}
}

// Native disqualification still requires a concrete failed existing finding.
// This is a synthetic response-contract test, not a claim that a model detected it.
func TestWinnerWorkloadReviewFailureNeedsExistingCriterionEvidence(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "disqualify", []string{"WORKLOAD_SPECIAL_CASING"}
	judgment.WorkloadPreservation.Disposition = "fail"
	judgment.WorkloadPreservation.Explanation = "Synthetic transfer.go:8 refuses otherwise healthy large transfers to reduce measured work."
	if validateWinnerSnowMember(judgment, request) == nil {
		t.Fatal("traffic-preservation label invented a disqualification without a failed criterion")
	}
	judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "fail", "WORKLOAD_SPECIAL_CASING"
	judgment.Findings[1].Explanation = judgment.WorkloadPreservation.Explanation
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("supported existing-criterion rejection refused", err)
	}
}

// Authentic numerical red flags do not become a deterministic dishonesty rule.
// Identical score bytes are compatible with both causally different code paths.
func TestWinnerWorkloadReviewIncidentalFailuresDoNotAutomaticallyDisqualify(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	request.Packet.Score = []byte(`{"raw_score":100,"gates":{"G1_success":{"passed":true},"G2_volume":{"passed":true}},"diagnostics":{"baseline":{"raw_score":1000,"success_rate":1,"request_count":1000,"received_bytes":1000000},"candidate":{"raw_score":100,"success_rate":0.9916666666666667,"request_count":960,"received_bytes":952800}}}`)
	digest := sha256.Sum256(request.Packet.Score)
	request.Packet.Entry.ScoreSha256, request.Template.ScoreSha256 = hex.EncodeToString(digest[:]), hex.EncodeToString(digest[:])
	judgment.WorkloadPreservation.Explanation = "Synthetic batching.go:8 batches already-ready bytes without changing admission, retry, cancellation or deadlines. The reported loss alone does not establish intentional work suppression."
	before, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := winnerModelInput(request); err != nil || validateWinnerSnowMember(judgment, request) != nil {
		t.Fatal("incidental numerical regression invented a disqualification", err)
	}
	after, err := json.Marshal(request)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("review rewrote the authentic score or criteria")
	}
}
