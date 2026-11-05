// Current QG execution reuses bounded native transport/custody primitives.
// No archived Mini/three-model case constructor or real command is enabled.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Bounds qualification independently of candidate text. Real operations use
// these defaults only; deterministic unit tests inject a no-network broker.
type currentQgQualificationSettings struct {
	CaseTimeout, TotalTimeout, CleanupGrace time.Duration
	MaximumBytes                            int64
	MaximumProviderSettingsBytes            int64
	MaximumSnowEvidenceBytes                int64
	MaximumExecutableBytes                  int64
	MaximumMemberJournalBytes               int64
	StateRoot, ScoringLock, ManagementCpus  string
}

// No model/effort, endpoint, candidate tool or retry option exists.
func defaultCurrentQgQualificationSettings() currentQgQualificationSettings {
	return currentQgQualificationSettings{CaseTimeout: 30 * time.Minute, TotalTimeout: 3 * time.Hour, CleanupGrace: 30 * time.Second,
		MaximumBytes: 16 * 1024 * 1024, MaximumProviderSettingsBytes: 4 * 1024 * 1024, MaximumSnowEvidenceBytes: 512 * 1024,
		MaximumExecutableBytes: 1024 * 1024 * 1024, MaximumMemberJournalBytes: 1024 * 1024, StateRoot: "/var/lib/urnetwork/competition-review/attempts",
		ScoringLock: "/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock", ManagementCpus: "20,22"}
}

// Independent publication digests identify operator inputs and original output.
type currentQgQualificationPin struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// The immutable image was built only from the trusted binary and public CA
// bundle. This externally reviewed receipt is not manufactured by the probe.
type currentQgQualificationImage struct {
	Schema        int                         `json:"schema"`
	Kind          string                      `json:"kind"`
	Accepted      bool                        `json:"accepted"`
	ImageId       string                      `json:"image_id"`
	BinarySha256  string                      `json:"binary_sha256"`
	SourceCommit  string                      `json:"source_commit"`
	GoVersion     string                      `json:"go_version"`
	PolicyModel   string                      `json:"policy_model"`
	ProviderModel string                      `json:"provider_model"`
	Evidence      []currentQgQualificationPin `json:"evidence"`
}

// Expected decisions are retained separately and never included in model input.
type currentQgQualificationCase struct {
	Position         int                       `json:"position"`
	Name             string                    `json:"name"`
	ExpectedDecision string                    `json:"expected_decision"`
	Request          winnerReviewBrokerRequest `json:"request"`
	RequestSha256    string                    `json:"semantic_request_sha256"`
}

// Cancellation is the first case and releases no provider input. All other
// cases must receive one genuine completed judgment; no case may be resampled.
type currentQgQualificationPlan struct {
	Schema                 int                                `json:"schema"`
	Kind                   string                             `json:"kind"`
	PolicyModel            string                             `json:"policy_model"`
	ProviderModel          string                             `json:"provider_model"`
	Reasoning              string                             `json:"reasoning"`
	BrokerSha256           string                             `json:"broker_sha256"`
	ActualImageId          string                             `json:"actual_image_id"`
	ImageReceiptSha256     string                             `json:"image_receipt_sha256"`
	Cases                  []currentQgQualificationCase       `json:"cases"`
	ProviderSettingsSha256 string                             `json:"provider_settings_sha256,omitempty"`
	Members                []currentQgSnowQualificationMember `json:"members,omitempty"`
	CaseTimeoutSeconds     int                                `json:"case_timeout_seconds"`
	TotalTimeoutSeconds    int                                `json:"total_timeout_seconds"`
	PreparationSha256      string                             `json:"preparation_sha256"`
	FixtureEvidenceSha256  string                             `json:"fixture_evidence_sha256"`
	UnitReceiptSha256      string                             `json:"unit_receipt_sha256"`
}

// This is private operational evidence, never a persisted integrity decision.
type currentQgQualificationObservation struct {
	Name                  string                       `json:"name"`
	Expected              string                       `json:"expected"`
	Observed              string                       `json:"observed"`
	Passed                bool                         `json:"passed"`
	ProviderResponseId    string                       `json:"provider_response_id"`
	ProviderResponseIds   []string                     `json:"provider_response_ids,omitempty"`
	PrivateEvidenceSha256 string                       `json:"private_evidence_sha256"`
	RequestSha256         string                       `json:"request_sha256"`
	ResponseSha256        string                       `json:"response_sha256"`
	WrapperReceiptSha256  string                       `json:"wrapper_receipt_sha256"`
	WouldAuthorize        bool                         `json:"would_authorize"`
	Refusal               *winnerReviewProviderRefusal `json:"provider_refusal,omitempty"`
}

// Offline unit success cannot claim provider qualification. Every unavailable,
// malformed, mismatched or uncertain-where-approval-required result fails closed.
type currentQgQualificationReport struct {
	Schema       int                                 `json:"schema"`
	Kind         string                              `json:"kind"`
	Mode         string                              `json:"mode"`
	Complete     bool                                `json:"complete"`
	Qualified    bool                                `json:"qualified"`
	NoMainWrites bool                                `json:"no_main_api_or_database_writes"`
	Failure      string                              `json:"failure"`
	PlanSha256   string                              `json:"plan_sha256"`
	Observations []currentQgQualificationObservation `json:"observations"`
}

// Production supplies only the fixed installed wrapper invocation. Unit tests
// provide pure HTTP/fixture hooks, explicitly marked offline_unit_mock.
type currentQgQualificationHooks struct {
	Mode                    string
	Now                     func() time.Time
	Invoke                  func(context.Context, []byte) ([]byte, []byte, error)
	AuthenticateContainment func([]byte, []byte, []byte, bool) (string, error)
	ValidatePlan            func(currentQgQualificationPlan) error
	ValidateResponse        func([]byte, winnerReviewBrokerRequest, string) (currentQgQualificationObservation, error)
}

// Byte identities never substitute for independent receipt provenance.
func currentQgQualificationHash(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// The semantic request excludes only clocks finalized later by the executor.
// A changed directory or clock cannot reroll the same authenticated input.
func currentQgQualificationSnowClaimKey(plan currentQgQualificationPlan, item currentQgQualificationCase) (string, error) {
	content, err := json.Marshal(item.Request)
	if err != nil || currentQgQualificationHash(content) != item.RequestSha256 {
		return "", errors.New("Snow claim semantic request is not independently pinned")
	}
	provider, err := json.Marshal(defaultWinnerSnowProviderSettings())
	if err != nil {
		return "", err
	}
	release, err := json.Marshal(struct {
		BrokerSha256           string `json:"broker_sha256"`
		ActualImageId          string `json:"actual_image_id"`
		ProviderSettingsSha256 string `json:"canonical_provider_settings_sha256"`
		SemanticRequestSha256  string `json:"semantic_request_sha256"`
	}{BrokerSha256: plan.BrokerSha256, ActualImageId: plan.ActualImageId, ProviderSettingsSha256: currentQgQualificationHash(provider), SemanticRequestSha256: item.RequestSha256})
	if err != nil {
		return "", err
	}
	return item.Request.Template.IdempotencyKey + ":" + currentQgQualificationHash(release), nil
}

// Canonical regular-file reads refuse links, special files, writable public
// inputs and private owner/mode drift before bounded allocation.
func readCurrentQgQualificationPin(pin currentQgQualificationPin, maximum int64, private bool) ([]byte, error) {
	if !validSha256(pin.Sha256) || !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || maximum <= 0 {
		return nil, errors.New("review qualification pin invalid")
	}
	resolved, err := filepath.EvalSymlinks(pin.Path)
	info, statErr := os.Lstat(pin.Path)
	if err != nil || statErr != nil || resolved != pin.Path || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > maximum {
		return nil, errors.New("review qualification input metadata invalid")
	}
	if private && (info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid())) {
		return nil, errors.New("review qualification input is not private")
	}
	fd, err := syscall.Open(pin.Path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), pin.Path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("review qualification input changed during open")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) != info.Size() || currentQgQualificationHash(content) != pin.Sha256 {
		return nil, errors.New("review qualification input bytes changed")
	}
	return content, nil
}

// Uses the existing exclusive private writer and retains every partial attempt.
func writeCurrentQgQualificationJson(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateReviewFile(path, content)
}

// Captures stdout even on nonzero exit so the private typed refusal envelope is
// retained. It never invokes a model directly or inherits host credentials.
func invokeCurrentQgQualificationBroker(ctx context.Context, broker currentQgQualificationPin, input []byte) ([]byte, []byte, error) {
	if err := verifySourceSupervisorProgram(broker.Path, broker.Sha256); err != nil {
		return nil, nil, err
	}
	command := exec.CommandContext(ctx, broker.Path)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin = bytes.NewReader(input)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = defaultCurrentQgQualificationSettings().CleanupGrace
	output, diagnostics := &sourceSupervisorOutput{limit: 1024 * 1024}, &sourceSupervisorOutput{limit: 64 * 1024}
	command.Stdout, command.Stderr = output, diagnostics
	err := command.Run()
	if output.exceeded || diagnostics.exceeded {
		err = errors.Join(err, errors.New("qualification wrapper output exceeded independent bound"))
	}
	return output.content, diagnostics.content, err
}

// Runs each fixed case once, retaining all private input/output before checking
// decisions. A quota/refusal/transport/schema/mismatch stops without another API
// call. Offline mocks can exercise guards but can never mark a real provider qualified.
func executeCurrentQgQualification(ctx context.Context, directory string, plan currentQgQualificationPlan, settings currentQgQualificationSettings, hooks currentQgQualificationHooks) (report *currentQgQualificationReport, resultErr error) {
	if hooks.Now == nil || hooks.Invoke == nil || hooks.AuthenticateContainment == nil || settings.CaseTimeout <= 0 || settings.TotalTimeout <= 0 || settings.CleanupGrace <= 0 || (hooks.Mode != "genuine_provider" && hooks.Mode != "offline_unit_mock") {
		return nil, errors.New("qualification requires an explicit bounded transport mode")
	}
	validatePlan, validateResponse := hooks.ValidatePlan, hooks.ValidateResponse
	if validatePlan == nil || validateResponse == nil {
		return nil, errors.New("current QG execution requires explicit current typed plan and response validators")
	}
	if err := validatePlan(plan); err != nil {
		return nil, err
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if err := writeCurrentQgQualificationJson(filepath.Join(directory, "run.started.json"), map[string]any{"schema": 1, "mode": hooks.Mode, "plan_sha256": currentQgQualificationHash(planBytes), "started_at": hooks.Now().UTC(), "automatic_retries": false}); err != nil {
		return nil, err
	}
	report = &currentQgQualificationReport{Schema: 1, Kind: "winner-review-provider-qualification", Mode: hooks.Mode, NoMainWrites: true, PlanSha256: currentQgQualificationHash(planBytes), Observations: []currentQgQualificationObservation{}}
	defer func() {
		if resultErr != nil {
			report.Qualified = false
			report.Failure = "qualification_failed_closed; inspect private originals; no automatic provider retry"
		}
		resultErr = errors.Join(resultErr, writeCurrentQgQualificationJson(filepath.Join(directory, "qualification-report.json"), report))
	}()
	ctx, cancel := context.WithTimeout(ctx, settings.TotalTimeout)
	defer cancel()
	seenProviderIdBools := map[string]bool{}
	for _, item := range plan.Cases {
		root := filepath.Join(directory, fmt.Sprintf("case-%02d", item.Position))
		if err := os.Mkdir(root, 0700); err != nil {
			return report, err
		}
		request := item.Request
		if plan.Kind == "winner-review-current-qg-qualification-plan/v1" {
			// Changed authenticated source evidence is a different input, not a
			// time/directory-based retry of the same model request.
			claimKey, err := currentQgQualificationSnowClaimKey(plan, item)
			if err != nil {
				return report, err
			}
			request.Template.IdempotencyKey = claimKey
		}
		now := hooks.Now().UTC()
		request.Request.StartedAt, request.Request.Deadline = now, now.Add(settings.CaseTimeout)
		input, err := json.Marshal(request)
		if err != nil {
			return report, err
		}
		if err := writePrivateReviewFile(filepath.Join(root, "broker-request.json"), input); err != nil {
			return report, err
		}
		caseCtx, caseCancel := context.WithDeadline(ctx, request.Request.Deadline)
		output, diagnostics, invokeErr := hooks.Invoke(caseCtx, input)
		if !hooks.Now().Before(request.Request.Deadline) {
			invokeErr = errors.Join(invokeErr, errors.New("original review case deadline expired"))
		}
		caseCancel()
		if err := writePrivateReviewFile(filepath.Join(root, "broker.stdout"), output); err != nil {
			return report, err
		}
		if err := writePrivateReviewFile(filepath.Join(root, "broker.stderr"), diagnostics); err != nil {
			return report, err
		}
		wrapperDigest, containmentErr := hooks.AuthenticateContainment(input, output, diagnostics, item.ExpectedDecision == "canceled")
		observation := currentQgQualificationObservation{Name: item.Name, Expected: item.ExpectedDecision, RequestSha256: currentQgQualificationHash(input), ResponseSha256: currentQgQualificationHash(output), WrapperReceiptSha256: wrapperDigest}
		var envelope struct {
			Schema  int                          `json:"schema"`
			Kind    string                       `json:"kind"`
			Refusal *winnerReviewProviderRefusal `json:"provider_refusal"`
			Body    json.RawMessage              `json:"private_redacted_body,omitempty"`
		}
		if json.Unmarshal(output, &envelope) == nil && envelope.Kind == "review_provider_refusal" {
			if envelope.Schema != 1 || envelope.Refusal == nil || invokeErr == nil || envelope.Refusal.StatusCode < 300 || envelope.Refusal.CapturedBodyBytes < 0 || int64(envelope.Refusal.CapturedBodyBytes) > defaultWinnerModelSettings().MaximumRefusalBytes || !validSha256(envelope.Refusal.CapturedBodySha256) {
				return report, errors.New("provider refusal envelope malformed or authorizing")
			}
			observation.Observed, observation.Refusal = "provider_refused", envelope.Refusal
			report.Observations = append(report.Observations, observation)
			return report, errors.Join(containmentErr, errors.New("provider refused qualification; retain quota/code/status; user action required, no automatic retry"))
		}
		if item.ExpectedDecision == "canceled" {
			if invokeErr == nil || len(output) != 0 || containmentErr != nil {
				observation.Observed = "cancellation_failed"
				report.Observations = append(report.Observations, observation)
				return report, errors.Join(containmentErr, errors.New("cancellation did not cleanly stop before provider input"))
			}
			observation.Observed, observation.Passed = "canceled", true
		} else {
			if invokeErr != nil || containmentErr != nil {
				observation.Observed = "broker_or_transport_failed"
				report.Observations = append(report.Observations, observation)
				return report, errors.Join(invokeErr, containmentErr)
			}
			verdict, err := validateResponse(output, request, item.ExpectedDecision)
			if err != nil {
				observation.Observed = "completed_judgment_not_qualified"
				report.Observations = append(report.Observations, observation)
				return report, err
			}
			verdict.Name, verdict.RequestSha256, verdict.WrapperReceiptSha256 = item.Name, observation.RequestSha256, wrapperDigest
			observation = verdict
			providerIds := verdict.ProviderResponseIds
			if len(providerIds) == 0 {
				providerIds = []string{verdict.ProviderResponseId}
			}
			for _, providerId := range providerIds {
				if hooks.Mode == "genuine_provider" && (providerId == "" || seenProviderIdBools[providerId]) {
					observation.Passed = false
					observation.Observed = "reused_provider_response"
					report.Observations = append(report.Observations, observation)
					return report, errors.New("provider response identity reused across qualification cases/members")
				}
				seenProviderIdBools[providerId] = true
			}
		}
		report.Observations = append(report.Observations, observation)
		if err := writeCurrentQgQualificationJson(filepath.Join(root, "observation.json"), observation); err != nil {
			return report, err
		}
	}
	report.Complete = true
	report.Qualified = hooks.Mode == "genuine_provider"
	return report, nil
}

// The actual command cannot be enabled on the wrong compiler/principal/CPU set.
func validateCurrentQgQualificationRuntime() error {
	if os.Geteuid() != 0 || runtime.Version() != "go1.27.3" {
		return errors.New("review qualification requires root and actual Go 1.27.3")
	}
	content, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "Cpus_allowed_list:") && strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:")) == defaultCurrentQgQualificationSettings().ManagementCpus {
			return nil
		}
	}
	return errors.New("review qualification requires management CPUs 20,22")
}
