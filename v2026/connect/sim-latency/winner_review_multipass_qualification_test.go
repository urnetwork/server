// Complete-coverage qualification is a new prospectively bounded protocol. The
// fixed historical control sources/expectations remain immutable, but their old
// flat execution reports cannot qualify the new provider or containment release.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// A new group binds actual release artifacts and expected deployment provenance.
// Served model aliases are observable; checkpoint revisions are trusted declared
// pins, not independent weight attestations fabricated from /v1/models.
type winnerMultipassQualificationRelease struct {
	Protocol               string                                        `json:"protocol"`
	SourceCommit           string                                        `json:"source_commit"`
	TrustedCli             currentQgQualificationPin                     `json:"trusted_cli"`
	NativeDriver           currentQgQualificationPin                     `json:"native_driver"`
	Wrapper                currentQgQualificationPin                     `json:"wrapper"`
	Configuration          currentQgQualificationPin                     `json:"configuration"`
	ProviderSettings       currentQgQualificationPin                     `json:"provider_settings"`
	Image                  currentQgQualificationPin                     `json:"image"`
	ImageId                string                                        `json:"image_id"`
	OfflineQualification   currentQgQualificationPin                     `json:"offline_qualification"`
	Preparation            currentQgQualificationPin                     `json:"preparation"`
	NativeFixtureEvidence  currentQgQualificationPin                     `json:"native_fixture_evidence"`
	DeploymentDeclaration  currentQgQualificationPin                     `json:"deployment_declaration"`
	CheckpointEvidenceKind string                                        `json:"checkpoint_evidence_kind"`
	TokenizationProfiles   map[string]winnerMultipassTokenizationProfile `json:"tokenization_profiles"`
}

// Shape validation is not file authentication or runtime qualification. The
// native command separately reads all root pins and actual executable identities.
func validateWinnerMultipassQualificationReleaseShape(release winnerMultipassQualificationRelease) error {
	return validateWinnerMultipassReleaseShape(release, true)
}

// Only fixed synthetic runtime probes may use a prospective empty observation.
// Native artifact authentication is identical, not a fabricated proof digest.
func validateWinnerMultipassRuntimeProbeReleaseShape(release winnerMultipassQualificationRelease) error {
	return validateWinnerMultipassReleaseShape(release, false)
}

func validateWinnerMultipassReleaseShape(release winnerMultipassQualificationRelease, observed bool) error {
	if release.Protocol != winnerMultipassVersion || !sourceGitShaPattern.MatchString(release.SourceCommit) || !strings.HasPrefix(release.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(release.ImageId, "sha256:")) ||
		release.CheckpointEvidenceKind != "trusted_deployment_declaration_not_weight_attestation" || len(release.TokenizationProfiles) != 2 {
		return errors.New("multi-pass qualification release identity is incomplete or overclaims checkpoint attestation")
	}
	for _, pin := range []currentQgQualificationPin{release.TrustedCli, release.NativeDriver, release.Wrapper, release.Configuration, release.ProviderSettings, release.Image, release.OfflineQualification, release.Preparation, release.NativeFixtureEvidence, release.DeploymentDeclaration} {
		if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || !validSha256(pin.Sha256) {
			return errors.New("multi-pass qualification release has an unpinned artifact")
		}
	}
	for index, member := range defaultWinnerSnowProviderSettings().Members {
		profile, found := release.TokenizationProfiles[member.Name]
		maximum := []int{98304, 131072}[index]
		if !found || profile.Member != member || profile.MaximumContextTokens != maximum || profile.TokenizerContractSha256 != winnerMultipassTokenizerContractSha256() || observed && !validSha256(profile.RuntimeEvidenceSha256) || !observed && profile.RuntimeEvidenceSha256 != "" {
			return errors.New("multi-pass release changed fixed membership, runtime context or rendering contract")
		}
	}
	return nil
}

// Expectations never enter Bundle, Plan, Payload or an invocation envelope.
type winnerMultipassQualificationCase struct {
	Position    int                               `json:"position"`
	Expectation currentQgQualificationExpectation `json:"private_expectation"`
	Bundle      winnerMultipassBundle             `json:"bundle"`
	Plan        *winnerMultipassPlan              `json:"plan"`
}

// Every tokenizer, cancellation and generation invocation is declared before
// first input release. Model loads are explicit non-inference tokenizer steps.
type winnerMultipassQualificationInvocation struct {
	Position       int    `json:"position"`
	CasePosition   int    `json:"case_position"`
	CallId         string `json:"call_id"`
	Operation      string `json:"operation"`
	Phase          string `json:"phase"`
	AllowModelLoad bool   `json:"allow_model_load"`
}

// One immutable clock covers all controls and both member batches. Per-case
// clocks are exact copies, never separately restarted 30-minute review owners.
type winnerMultipassQualificationPlan struct {
	Schema               int                                      `json:"schema"`
	Kind                 string                                   `json:"kind"`
	Owner                winnerMultipassOwner                     `json:"owner"`
	Release              winnerMultipassQualificationRelease      `json:"release"`
	Settings             winnerMultipassSettings                  `json:"settings"`
	ExpectationsSha256   string                                   `json:"expectations_sha256"`
	Cases                []winnerMultipassQualificationCase       `json:"cases"`
	Invocations          []winnerMultipassQualificationInvocation `json:"invocations"`
	GenerationCalls      int                                      `json:"generation_calls"`
	TokenizerInvocations int                                      `json:"tokenizer_invocations"`
	ArtifactBytes        int64                                    `json:"artifact_bytes"`
}

// Only the exact previously reviewed source censuses determine control labels.
// This reads immutable checked-in fixtures, never a provider-selected corpus.
func winnerMultipassQualificationSources() ([]currentQgQualificationSource, error) {
	workload, err := os.ReadFile(filepath.Join("testdata", "workload-review-qualification-v1.json"))
	if err != nil {
		return nil, err
	}
	production, err := os.ReadFile(filepath.Join("testdata", "production-benefit-review-qualification-v1.json"))
	if err != nil {
		return nil, err
	}
	return currentQgQualificationSources(workload, production)
}

// Preparation consumes independently authenticated original requests. It neither
// queries models nor resets the original fixture/scoring/submission authority.
// Optional groups come from prior non-inference token packing and are rechecked.
func prepareWinnerMultipassQualification(requests []winnerReviewBrokerRequest, owner winnerMultipassOwner, settings winnerMultipassSettings, release winnerMultipassQualificationRelease, groups [][]winnerMultipassScanGroup) (*winnerMultipassQualificationPlan, error) {
	if err := validateWinnerMultipassQualificationReleaseShape(release); err != nil {
		return nil, err
	}
	if validateWinnerMultipassSettings(settings) != nil || !reflect.DeepEqual(settings.TokenizationProfiles, release.TokenizationProfiles) || len(requests) != 19 || len(groups) != 0 && len(groups) != 19 || owner.Id == "" || len(owner.Id) > 64 || owner.StartedAt.IsZero() || !owner.Deadline.After(owner.StartedAt) || owner.Deadline.Sub(owner.StartedAt) > settings.MaximumOwnerDuration {
		return nil, errors.New("multi-pass qualification needs all nineteen controls and one fixed bounded owner")
	}
	sources, err := winnerMultipassQualificationSources()
	if err != nil {
		return nil, err
	}
	plan := &winnerMultipassQualificationPlan{Schema: 1, Kind: "winner-review-multipass-qualification-plan/v1", Owner: owner, Release: release, Settings: settings, Cases: []winnerMultipassQualificationCase{}, Invocations: []winnerMultipassQualificationInvocation{}}
	expectations := []currentQgQualificationExpectation{}
	for index, request := range requests {
		source := sources[index]
		requestBytes, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		if currentQgQualificationContainsExpectedLabels(requestBytes) || !request.Request.StartedAt.IsZero() || !request.Request.Deadline.IsZero() || len(request.SourceFiles) != 1 || request.SourceFiles[0].Path != currentQgFixturePath || request.SourceFiles[0].Content != source.Before ||
			(index == 0 && request.Operation != "qualification_cancel_before_provider") || (index != 0 && request.Operation != "review_elected_winner") {
			return nil, errors.New("qualification input changed its fixed control, labels, operation or semantic clock")
		}
		request.Operation = "review_elected_winner"
		bundle := winnerMultipassBundle{Request: request, SupplementalFiles: []winnerMultipassSourceFile{}, RequiredSupplementalPaths: []string{}, UnresolvedContext: []string{}}
		caseOwner := winnerMultipassOwner{Id: winnerMultipassSha256([]byte(fmt.Sprintf("%s/control-%02d", owner.Id, index+1))), StartedAt: owner.StartedAt, Deadline: owner.Deadline}
		casePlan, err := prepareWinnerMultipass(bundle, caseOwner, settings, defaultWinnerSnowProviderSettings())
		if err != nil {
			return nil, err
		}
		if len(groups) != 0 {
			if err := setWinnerMultipassScanGroups(casePlan, groups[index]); err != nil {
				return nil, err
			}
		}
		artifacts, err := winnerMultipassArtifacts(bundle, settings)
		if err != nil {
			return nil, err
		}
		patched := ""
		for _, artifact := range artifacts {
			plan.ArtifactBytes += int64(len(artifact.Content))
			if artifact.Kind == "candidate" && artifact.Path == currentQgFixturePath {
				patched = artifact.Content
			}
		}
		if patched != source.After {
			return nil, errors.New("qualification patch no longer reconstructs its fixed control source")
		}
		plan.Cases = append(plan.Cases, winnerMultipassQualificationCase{Position: index + 1, Expectation: source.Expectation, Bundle: bundle, Plan: casePlan})
		expectations = append(expectations, source.Expectation)
		if index != 0 {
			plan.GenerationCalls += len(casePlan.Calls)
		}
	}
	if plan.ArtifactBytes > int64(settings.MaximumBundleBytes) || plan.GenerationCalls > settings.MaximumCalls {
		return nil, errors.New("whole qualification group exceeds complete-artifact or original model-call budget; prospectively pack before freezing")
	}
	expectedBytes, _ := json.Marshal(expectations)
	plan.ExpectationsSha256 = winnerMultipassSha256(expectedBytes)
	appendInvocation := func(casePosition int, call winnerMultipassCall, operation, phase string, load bool) {
		plan.Invocations = append(plan.Invocations, winnerMultipassQualificationInvocation{Position: len(plan.Invocations) + 1, CasePosition: casePosition, CallId: call.Id, Operation: operation, Phase: phase, AllowModelLoad: load})
		if operation == "tokenize" {
			plan.TokenizerInvocations++
		}
	}
	appendInvocation(1, plan.Cases[0].Plan.Calls[0], "qualification_cancel_before_provider", "pre_input_cancellation", false)
	lastModel := ""
	// Admit every exact frozen leaf for both members before any generation claim.
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		for _, item := range plan.Cases[1:] {
			for _, call := range item.Plan.Calls {
				if call.Member == member && call.Stage == "scan" {
					appendInvocation(item.Position, call, "tokenize", "frozen_leaf_preflight", lastModel != member.Name)
					lastModel = member.Name
				}
			}
		}
	}
	// One complete member batch then the other; never per-case alternating loads.
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		for _, item := range plan.Cases[1:] {
			for _, call := range item.Plan.Calls {
				if call.Member != member {
					continue
				}
				appendInvocation(item.Position, call, "tokenize", "call_admission", lastModel != member.Name)
				lastModel = member.Name
				appendInvocation(item.Position, call, "generate", "capture", false)
			}
		}
	}
	return plan, nil
}

// Rebuild all labels, byte coverage, clocks and operations, rather than trust a
// hand-edited plan with a valid-looking aggregate checksum or qualified flag.
func validateWinnerMultipassQualificationPlan(plan *winnerMultipassQualificationPlan) error {
	if plan == nil || len(plan.Cases) != 19 {
		return errors.New("multi-pass qualification plan is absent or incomplete")
	}
	requests, groups := []winnerReviewBrokerRequest{}, [][]winnerMultipassScanGroup{}
	for index, item := range plan.Cases {
		if item.Plan == nil {
			return errors.New("qualification control has no complete coverage plan")
		}
		request := item.Bundle.Request
		if index == 0 {
			request.Operation = "qualification_cancel_before_provider"
		}
		requests = append(requests, request)
		groups = append(groups, item.Plan.ScanGroups)
	}
	want, err := prepareWinnerMultipassQualification(requests, plan.Owner, plan.Settings, plan.Release, groups)
	if err != nil {
		return err
	}
	actualBytes, _ := json.Marshal(plan)
	wantBytes, _ := json.Marshal(want)
	if !bytes.Equal(actualBytes, wantBytes) {
		return errors.New("qualification group differs from the complete immutable precommit")
	}
	return nil
}

// Every control requires both original final judgments, all twelve attributed
// findings and unchanged traffic/production expectations. Nothing is majority-voted.
func validateWinnerMultipassQualificationOutcomes(plan *winnerMultipassQualificationPlan, evidence map[int]winnerMultipassEvidence) error {
	if err := validateWinnerMultipassQualificationPlan(plan); err != nil {
		return err
	}
	if len(evidence) != 18 {
		return errors.New("qualification evidence does not cover exactly eighteen generated controls")
	}
	responseIds := map[string]bool{}
	for _, item := range plan.Cases[1:] {
		value, found := evidence[item.Position]
		if !found {
			return errors.New("qualification control evidence is absent")
		}
		combined, err := validateWinnerMultipassEvidence(item.Plan, item.Bundle, value)
		if err != nil {
			return err
		}
		if combined.Decision != item.Expectation.Decision || len(combined.Findings) != 12 {
			return errors.New("qualification combined outcome differs from its private fixed expectation")
		}
		finals := 0
		for index, call := range item.Plan.Calls {
			var response winnerModelResponse
			if json.Unmarshal(value.Calls[index].VisibleResponse, &response) != nil || responseIds[response.Id] {
				return errors.New("one provider response was replayed across qualification calls or cases")
			}
			responseIds[response.Id] = true
			if call.Stage != "final" {
				continue
			}
			finals++
			judgment := value.Calls[index].Observation.FinalJudgment
			if judgment == nil || judgment.Decision != item.Expectation.Decision || judgment.WorkloadPreservation == nil || judgment.ProductionBenefit == nil || judgment.WorkloadPreservation.Disposition != item.Expectation.WorkloadDisposition || judgment.ProductionBenefit.Disposition != item.Expectation.ProductionDisposition {
				return errors.New("qualification original member differs from its fixed decision or mandatory subchecks")
			}
		}
		if finals != 2 {
			return errors.New("qualification omitted one native member final")
		}
	}
	return nil
}

// Group storage uses the same per-case original/custody formulas and a single
// shared native process overhead. Actual filesystem reservation stays a host duty.
type winnerMultipassQualificationCapacity struct {
	PlanSha256           string                           `json:"plan_sha256"`
	Cases                map[int]*winnerMultipassCapacity `json:"cases"`
	RequiredDurableBytes int64                            `json:"required_durable_bytes"`
	RequiredMemoryBytes  int64                            `json:"required_memory_bytes"`
}

// No first draw is allowed merely because each individual case would fit.
func estimateWinnerMultipassQualificationCapacity(plan *winnerMultipassQualificationPlan, settings winnerMultipassCapacitySettings) (*winnerMultipassQualificationCapacity, error) {
	if err := validateWinnerMultipassQualificationPlan(plan); err != nil {
		return nil, err
	}
	content, _ := json.Marshal(plan)
	result := &winnerMultipassQualificationCapacity{PlanSha256: winnerMultipassSha256(content), Cases: map[int]*winnerMultipassCapacity{}, RequiredDurableBytes: 64 * 1024 * 1024, RequiredMemoryBytes: 384*1024*1024 + plan.ArtifactBytes + 2*int64(len(content))}
	for _, item := range plan.Cases[1:] {
		tokenizers := 0
		for _, invocation := range plan.Invocations {
			if invocation.CasePosition == item.Position && invocation.Operation == "tokenize" {
				tokenizers++
			}
		}
		capacity, err := estimateWinnerMultipassCapacity(item.Plan, item.Bundle, tokenizers, settings)
		if err != nil {
			return result, err
		}
		result.Cases[item.Position] = capacity
		result.RequiredDurableBytes += capacity.RequiredDurableBytes - 64*1024*1024
		result.RequiredMemoryBytes += capacity.RequiredMemoryBytes - 384*1024*1024 - int64(item.Plan.Settings.MaximumBundleBytes)
	}
	// One pre-input cancellation has no model transport/state, but retains its
	// full bounded input/stdout/metadata in both original and protected custody.
	limits := defaultWinnerMultipassProviderLimits()
	result.RequiredDurableBytes += int64(1+settings.ProtectedCopies) * (int64(limits.MaximumEnvelopeBytes+limits.MaximumResultBytes) + 32*int64(limits.MaximumMetadataBytes))
	if result.RequiredDurableBytes > settings.MaximumDurableBytes || result.RequiredMemoryBytes > settings.MaximumMemoryBytes {
		return result, errors.New("whole qualification group exceeds its declared durable or native-memory reservation")
	}
	return result, nil
}
