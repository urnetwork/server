// Protected-spool qualification has a distinct report and resolver. Historical
// full-local capture pins are never reinterpreted as compact archive commits.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"time"
)

type winnerMultipassQualificationSpoolReport struct {
	Schema               int                                     `json:"schema"`
	Kind                 string                                  `json:"kind"`
	Protocol             string                                  `json:"protocol"`
	Mode                 string                                  `json:"mode"`
	Plan                 currentQgQualificationPin               `json:"plan"`
	PlanSha256           string                                  `json:"plan_sha256"`
	Release              winnerMultipassQualificationRelease     `json:"release"`
	Owner                winnerMultipassOwner                    `json:"owner"`
	Capacity             *winnerMultipassQualificationCapacity   `json:"full_local_aggregate_capacity"`
	SpoolCapacity        *winnerMultipassSpoolCapacity           `json:"protected_spool_capacity"`
	CaptureCommits       []currentQgQualificationPin             `json:"compact_protected_capture_commits"`
	FailureCaptureCommit *currentQgQualificationPin              `json:"failure_capture_commit,omitempty"`
	FailureInvocation    *winnerMultipassQualificationInvocation `json:"failure_invocation,omitempty"`
	CaseEvidence         map[int]currentQgQualificationPin       `json:"case_evidence"`
	Complete             bool                                    `json:"complete"`
	CompletedAt          time.Time                               `json:"completed_at"`
	Qualified            bool                                    `json:"qualified"`
	Error                string                                  `json:"error"`
}

// Failed authentication has no portable success proof. Native custody may still
// protect the original bounded host capture and return its compact audit pin.
// The executor records this terminal failure, never deletes originals or resumes.
type winnerMultipassQualificationSpoolRetentionError struct {
	Failure currentQgQualificationPin
	Cause   error
}

func (self *winnerMultipassQualificationSpoolRetentionError) Error() string {
	if self.Cause == nil {
		return "qualification failed after original failure custody"
	}
	return "qualification failed after original failure custody: " + self.Cause.Error()
}

func (self *winnerMultipassQualificationSpoolRetentionError) Unwrap() error {
	return self.Cause
}

// This is transient native replay state, NOT a serialized capture record. All
// raw leaves are separately protected objects, never embedded in the 32MiB
// descriptor-based Capture. The resolver verifies their exact archive versions.
type winnerMultipassQualificationSpoolResolvedCapture struct {
	Commit                     currentQgQualificationPin
	Capture                    *winnerMultipassQualificationCapture
	CaptureReference           winnerMultipassProtectedReference
	Originals                  map[string][]byte
	IndependentGenerationClaim []byte
	PhysicalAuthentication     *winnerMultipassAuthenticatedPhysicalProof
}

// The caller reads a <=1MiB private compact commit; this callback must validate
// its native schema, exact protected versions and COMPLIANCE readback, then load
// the opaque physical proof through loadWinnerMultipassPhysicalAuthentication.
type winnerMultipassQualificationSpoolResolver func(context.Context, currentQgQualificationPin, []byte) (*winnerMultipassQualificationSpoolResolvedCapture, error)

type winnerMultipassQualificationSpoolHooks struct {
	Mode            string
	Now             func() time.Time
	ValidateRelease func(winnerMultipassQualificationRelease) error
	Reserve         func(*winnerMultipassSpoolCapacity, *winnerMultipassQualificationCapacity) error
	Load            func(winnerMultipassQualificationInvocation) (*winnerMultipassQualificationSpoolResolvedCapture, error)
	Claim           func(winnerMultipassQualificationInvocation, []byte) (currentQgQualificationPin, error)
	Invoke          func(context.Context, []byte) ([]byte, []byte, error)
	Authenticate    func([]byte, []byte, []byte, bool) (*winnerMultipassContainmentReceipt, error)
	Retain          func(context.Context, winnerMultipassQualificationCapture) (currentQgQualificationPin, error)               // Exact durable commit only after original protection, retirement and storage restoration.
	Resolve         func(context.Context, currentQgQualificationPin) (*winnerMultipassQualificationSpoolResolvedCapture, error) // Fresh bounded exact-version replay; never borrows the protection deadline.
	RetainCase      func(int, winnerMultipassEvidence) (currentQgQualificationPin, error)
	CompletedReport *currentQgQualificationPin // Optional original terminal: only fully authenticated read-only replay, no new draw or retention.
}

// A compact commit must bind the whole canonical capture, not just a desired
// result or a local path. Its native resolver separately verifies actual custody.
func validateWinnerMultipassResolvedSpoolCapture(value *winnerMultipassQualificationSpoolResolvedCapture, owner winnerMultipassOwner) error {
	if value == nil || value.Capture == nil || !filepath.IsAbs(value.Commit.Path) || filepath.Clean(value.Commit.Path) != value.Commit.Path || !validSha256(value.Commit.Sha256) {
		return errors.New("spool capture lacks its exact compact custody commit")
	}
	content, err := json.Marshal(value.Capture)
	ref := value.CaptureReference
	if err != nil || len(content) > winnerMultipassSpoolCaptureBytes || ref.Key == "" || ref.VersionId == "" || ref.Bytes != int64(len(content)) || ref.Sha256 != winnerMultipassSha256(content) || ref.RetainUntil.Before(owner.Deadline) {
		return errors.New("spool capture lacks its complete bounded protected original record")
	}
	return nil
}

func applyWinnerMultipassResolvedSpoolCapture(plan *winnerMultipassQualificationPlan, invocation winnerMultipassQualificationInvocation, value *winnerMultipassQualificationSpoolResolvedCapture, state *winnerMultipassQualificationState) error {
	if err := validateWinnerMultipassResolvedSpoolCapture(value, plan.Owner); err != nil {
		return err
	}
	profile := winnerMultipassContainmentProfile{ImageId: plan.Release.ImageId, ProviderSettings: plan.Release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	return applyWinnerMultipassQualificationCapture(plan, invocation, *value.Capture, state, func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
		lines := bytes.Split(bytes.TrimSpace(diagnostics), []byte("\n"))
		if len(lines) == 0 || !bytes.Equal(input, value.Originals["input.json"]) || !bytes.Equal(output, value.Originals["broker.stdout"]) || !bytes.Equal(lines[len(lines)-1], value.Originals["wrapper-receipt.json"]) || value.PhysicalAuthentication == nil || canceled != value.PhysicalAuthentication.proof.Canceled {
			return nil, errors.New("spool capture differs from independently protected original leaves")
		}
		return replayWinnerMultipassPortableContainment(value.Originals, value.IndependentGenerationClaim, value.PhysicalAuthentication, profile)
	})
}

// Same dispatcher/one-shot clock as full-local execution, but physical checks
// precede retention and every subsequent replay uses protected immutable bytes.
// A failed retention/cleanup/resolve stops the next invocation; nothing rerolls.
func executeWinnerMultipassSpoolQualification(ctx context.Context, plan *winnerMultipassQualificationPlan, planPin currentQgQualificationPin, capacitySettings winnerMultipassCapacitySettings, spoolSettings winnerMultipassSpoolSettings, hooks winnerMultipassQualificationSpoolHooks) (report *winnerMultipassQualificationSpoolReport, resultErr error) {
	if err := validateWinnerMultipassQualificationPlan(plan); err != nil {
		return nil, err
	}
	if hooks.Mode != "offline" && hooks.Mode != "genuine_provider" || hooks.Now == nil || hooks.ValidateRelease == nil || hooks.Reserve == nil || hooks.Load == nil || hooks.Claim == nil || hooks.Invoke == nil || hooks.Authenticate == nil || hooks.Retain == nil || hooks.Resolve == nil || hooks.RetainCase == nil {
		return nil, errors.New("protected-spool qualification requires complete explicit native lifecycle hooks")
	}
	planBytes, _ := json.Marshal(plan)
	if planPin.Sha256 != winnerMultipassSha256(planBytes) {
		return nil, errors.New("spool qualification lacks the exact fixed owner plan")
	}
	report = &winnerMultipassQualificationSpoolReport{Schema: 1, Kind: "winner-review-multipass-qualification-spool-report/v1", Protocol: winnerMultipassVersion, Mode: hooks.Mode, Plan: planPin, PlanSha256: planPin.Sha256, Release: plan.Release, Owner: plan.Owner, CaptureCommits: []currentQgQualificationPin{}, CaseEvidence: map[int]currentQgQualificationPin{}}
	defer func() {
		if resultErr != nil {
			report.Error = resultErr.Error()
			report.Qualified = false
			report.Complete = false
		}
	}()
	if hooks.Mode == "genuine_provider" {
		if err := validateCurrentQgQualificationRuntime(); err != nil {
			return report, err
		}
	}
	if err := hooks.ValidateRelease(plan.Release); err != nil {
		return report, err
	}
	spool, aggregate, err := estimateWinnerMultipassQualificationSpoolCapacity(plan, capacitySettings, spoolSettings)
	report.Capacity, report.SpoolCapacity = aggregate, spool
	if err != nil {
		return report, err
	}
	var originalReport *winnerMultipassQualificationSpoolReport
	if hooks.CompletedReport != nil {
		originalReport = &winnerMultipassQualificationSpoolReport{}
		if err := readWinnerMultipassCompletedReport(*hooks.CompletedReport, originalReport); err != nil {
			return report, err
		}
	} else if err := hooks.Reserve(spool, aggregate); err != nil {
		return report, err
	}
	var latestProvider time.Time
	dispatch := winnerMultipassQualificationHooks{Now: hooks.Now, Claim: hooks.Claim, Invoke: hooks.Invoke, Authenticate: hooks.Authenticate}
	state := newWinnerMultipassQualificationState(plan)
	for _, invocation := range plan.Invocations {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		value, err := hooks.Load(invocation)
		if err != nil {
			return report, err
		}
		committed := false
		if value == nil {
			if originalReport != nil {
				return report, errors.New("completed spool terminal cannot authorize an unstarted invocation")
			}
			capture, err := dispatchWinnerMultipassQualification(ctx, plan, invocation, state, dispatch)
			if err != nil {
				return report, err
			}
			retentionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plan.Settings.Custody.ProtectionTimeout)
			commit, err := hooks.Retain(retentionCtx, *capture)
			cancel()
			if err != nil {
				var failure *winnerMultipassQualificationSpoolRetentionError
				if errors.As(err, &failure) && failure != nil {
					if _, auditErr := readCurrentQgQualificationPin(failure.Failure, winnerMultipassSpoolManifestBytes, true); auditErr != nil {
						return report, errors.Join(err, errors.New("failed capture custody audit pin cannot be authenticated"), auditErr)
					}
					report.FailureCaptureCommit = &failure.Failure
					originalInvocation := invocation
					report.FailureInvocation = &originalInvocation
				}
				return report, err
			}
			if _, err := readCurrentQgQualificationPin(commit, winnerMultipassSpoolManifestBytes, true); err != nil {
				return report, errors.Join(errors.New("protected spool returned no authentic durable compact commit"), err)
			}
			// The durable reference remains in a failed terminal even if a later
			// exact-version read times out after all local payloads were retired.
			report.CaptureCommits = append(report.CaptureCommits, commit)
			committed = true
			if err := ctx.Err(); err != nil {
				return report, err
			}
			replayCtx, replayCancel := context.WithTimeout(ctx, plan.Settings.Custody.PostCommitReplayTimeout)
			value, err = hooks.Resolve(replayCtx, commit)
			replayCancel()
			if err != nil {
				return report, err
			}
			if value == nil || value.Commit != commit || !reflect.DeepEqual(value.Capture, capture) {
				return report, errors.New("protected custody changed original qualification capture")
			}
		}
		if err := validateWinnerMultipassResolvedSpoolCapture(value, plan.Owner); err != nil {
			return report, err
		}
		if !committed {
			report.CaptureCommits = append(report.CaptureCommits, value.Commit)
		}
		if err := applyWinnerMultipassResolvedSpoolCapture(plan, invocation, value, state); err != nil {
			return report, err
		}
		if value.Capture.Containment.Result != nil && value.Capture.Containment.Result.CompletedAt.After(latestProvider) {
			latestProvider = value.Capture.Containment.Result.CompletedAt
		}
		if invocation.Operation == "generate" {
			item := plan.Cases[invocation.CasePosition-1]
			evidence := state.Evidence[item.Position]
			if len(evidence.Calls) == len(item.Plan.Calls) {
				var pin currentQgQualificationPin
				var err error
				if originalReport != nil {
					pin = originalReport.CaseEvidence[item.Position]
					err = replayWinnerMultipassCompletedCase(pin, evidence)
				} else {
					pin, err = hooks.RetainCase(item.Position, evidence)
				}
				if err != nil {
					return report, err
				}
				content, _ := json.Marshal(evidence)
				if pin.Sha256 != winnerMultipassSha256(content) {
					return report, errors.New("spool case completion lost exact core evidence custody")
				}
				report.CaseEvidence[item.Position] = pin
			}
		}
	}
	delete(state.Evidence, 1)
	if err := validateWinnerMultipassQualificationOutcomes(plan, state.Evidence); err != nil {
		return report, err
	}
	if originalReport != nil {
		report.CompletedAt = originalReport.CompletedAt
	} else {
		report.CompletedAt = hooks.Now().UTC()
	}
	if err := validateWinnerMultipassCompletionClock(plan.Owner, report.CompletedAt, latestProvider); err != nil {
		return report, err
	}
	report.Complete = true
	report.Qualified = hooks.Mode == "genuine_provider"
	if originalReport != nil && !reflect.DeepEqual(report, originalReport) {
		return report, errors.New("completed spool replay changed original terminal")
	}
	return report, nil
}

// This gate accepts only the new typed spool report and a non-nil exact-version
// resolver. It never falls back to original paths, missing files or a flat report.
func authenticateWinnerMultipassSpoolQualification(reportPin currentQgQualificationPin, release winnerMultipassQualificationRelease, resolve winnerMultipassQualificationSpoolResolver) (*winnerMultipassQualificationSpoolReport, error) {
	if validateWinnerMultipassQualificationReleaseShape(release) != nil || resolve == nil {
		return nil, errors.New("spool qualification requires pinned release and protected resolver")
	}
	content, err := readCurrentQgQualificationPin(reportPin, int64(defaultWinnerMultipassProviderLimits().MaximumResultBytes), true)
	if err != nil {
		return nil, err
	}
	var report winnerMultipassQualificationSpoolReport
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &report, "original protected-spool qualification report") != nil || report.Schema != 1 || report.Kind != "winner-review-multipass-qualification-spool-report/v1" || report.Protocol != winnerMultipassVersion || report.Mode != "genuine_provider" || !report.Complete || !report.Qualified || report.Error != "" || report.FailureCaptureCommit != nil || report.FailureInvocation != nil || !reflect.DeepEqual(report.Release, release) || report.Capacity == nil || report.Capacity.Cases[2] == nil || report.SpoolCapacity == nil {
		return nil, errors.New("spool qualification report is partial, offline, historical or from another release")
	}
	if err := validateWinnerMultipassCompletionClock(report.Owner, report.CompletedAt, time.Time{}); err != nil {
		return nil, err
	}
	planBytes, err := readCurrentQgQualificationPin(report.Plan, 8*int64(defaultWinnerMultipassSettings().MaximumBundleBytes), true)
	if err != nil {
		return nil, err
	}
	var plan winnerMultipassQualificationPlan
	if decodeStrictJSONBytes(planBytes, &plan, "protected-spool qualification plan") != nil || validateWinnerMultipassQualificationPlan(&plan) != nil || report.Plan.Sha256 != report.PlanSha256 || plan.Owner != report.Owner || !reflect.DeepEqual(plan.Release, release) || len(report.CaptureCommits) != len(plan.Invocations) || len(report.CaseEvidence) != 18 {
		return nil, errors.New("spool qualification lost exact whole-group owner/plan/census")
	}
	canonical, _ := json.Marshal(plan)
	if !bytes.Equal(canonical, planBytes) {
		return nil, errors.New("spool qualification plan is not its canonical precommit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWinnerMultipassSettings().MaximumOwnerDuration)
	defer cancel()
	inputs, err := authenticateWinnerMultipassQualificationInputs(ctx, release)
	if err != nil {
		return nil, err
	}
	if err := validateWinnerMultipassQualificationOriginalInputs(&plan, inputs); err != nil {
		return nil, err
	}
	spool, aggregate, err := estimateWinnerMultipassQualificationSpoolCapacity(&plan, report.Capacity.Cases[2].Settings, report.SpoolCapacity.Settings)
	if err != nil || !reflect.DeepEqual(spool, report.SpoolCapacity) || !reflect.DeepEqual(aggregate, report.Capacity) {
		return nil, errors.New("spool qualification storage/memory census differs from original plan")
	}
	state := newWinnerMultipassQualificationState(&plan)
	for index, invocation := range plan.Invocations {
		pin := report.CaptureCommits[index]
		commitBytes, err := readCurrentQgQualificationPin(pin, winnerMultipassSpoolManifestBytes, true)
		if err != nil {
			return nil, err
		}
		value, err := resolve(ctx, pin, commitBytes)
		if err != nil {
			return nil, err
		}
		if value == nil || value.Commit != pin {
			return nil, errors.New("protected resolver substituted original compact custody commit")
		}
		if err := validateWinnerMultipassResolvedSpoolCapture(value, plan.Owner); err != nil {
			return nil, err
		}
		claim, err := readCurrentQgQualificationPin(value.Capture.Claim, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
		if err != nil || !bytes.Equal(claim, value.Capture.ClaimContent) {
			return nil, errors.New("spool capture differs from independent retained root intent")
		}
		if err := applyWinnerMultipassResolvedSpoolCapture(&plan, invocation, value, state); err != nil {
			return nil, err
		}
		if value.Capture.Containment.Result != nil && report.CompletedAt.Before(value.Capture.Containment.Result.CompletedAt) {
			return nil, errors.New("spool qualification terminal predates an original provider completion")
		}
	}
	delete(state.Evidence, 1)
	if err := validateWinnerMultipassQualificationOutcomes(&plan, state.Evidence); err != nil {
		return nil, err
	}
	for _, item := range plan.Cases[1:] {
		actual, err := readCurrentQgQualificationPin(report.CaseEvidence[item.Position], aggregate.Cases[item.Position].SerializedEvidenceBytes+1024*1024, true)
		if err != nil {
			return nil, err
		}
		original, _ := json.Marshal(state.Evidence[item.Position])
		if !bytes.Equal(actual, original) {
			return nil, errors.New("spool case evidence differs from complete original protected invocation replay")
		}
	}
	return &report, nil
}
