// The native group runner owns one fixed clock, one sequential reserved spool
// window and permanent intent journals. It never calls the winner approval API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Only archive scope is used; this does not fabricate a candidate DB owner.
func winnerHandoffGroupArchiveArgs(config winnerHandoffGroupConfig) model.CompetitionReviewHandoffArgs {
	return model.CompetitionReviewHandoffArgs{RoundId: config.ArchiveRoundId, JobId: config.ArchiveJobId}
}

// Every root-owned original is authenticated again before the first invocation.
func loadWinnerHandoffFrozenGroup(ctx context.Context, config winnerHandoffGroupConfig) (*winnerHandoffGroupFrozen, *winnerMultipassQualificationPlan, error) {
	if err := validateWinnerHandoffGroupConfig(config); err != nil {
		return nil, nil, err
	}
	if err := authenticateWinnerMultipassReleaseArtifacts(config.Release); err != nil {
		return nil, nil, err
	}
	content, err := readCurrentQgQualificationPin(config.Frozen, 8*int64(defaultWinnerMultipassSettings().MaximumBundleBytes), true)
	if err != nil {
		return nil, nil, err
	}
	var frozen winnerHandoffGroupFrozen
	if decodeStrictJSONBytes(content, &frozen, "native frozen qualification group") != nil || frozen.Schema != 1 || frozen.Kind != "winner-review-frozen-qualification-owner/v1" || frozen.ConfigurationSha256 != winnerHandoffGroupConfigurationSha256(config) || frozen.FreezePermissionSha256 != config.FreezePermission.Sha256 || frozen.Owner.Id != winnerHandoffGroupOwnerId(config) ||
		frozen.Owner.Deadline.Sub(frozen.Owner.StartedAt) != time.Duration(config.TimeoutSeconds)*time.Second || frozen.OwnerClaim.Path != filepath.Join("/var/lib/urnetwork/competition-review-multipass/qualification-owners", frozen.Owner.Id+".json") || frozen.Plan.Path != filepath.Join(config.OutputDirectory, "group-plan.json") {
		return nil, nil, errors.New("qualification frozen identity or clock changed")
	}
	claim, err := readCurrentQgQualificationPin(frozen.OwnerClaim, winnerMultipassSpoolManifestBytes, true)
	if err != nil {
		return nil, nil, err
	}
	var ownerClaim struct {
		Schema              int                  `json:"schema"`
		Kind                string               `json:"kind"`
		ConfigurationSha256 string               `json:"configuration_sha256"`
		PermissionSha256    string               `json:"permission_sha256"`
		Owner               winnerMultipassOwner `json:"owner"`
		PlanSha256          string               `json:"plan_sha256"`
	}
	if decodeStrictJSONBytes(claim, &ownerClaim, "independent qualification owner claim") != nil || ownerClaim.Schema != 1 || ownerClaim.Kind != "winner-review-qualification-owner/v1" || ownerClaim.ConfigurationSha256 != frozen.ConfigurationSha256 || ownerClaim.PermissionSha256 != frozen.FreezePermissionSha256 || ownerClaim.Owner != frozen.Owner || ownerClaim.PlanSha256 != frozen.Plan.Sha256 {
		return nil, nil, errors.New("qualification independent root owner does not match frozen plan")
	}
	planBytes, err := readCurrentQgQualificationPin(frozen.Plan, 8*int64(defaultWinnerMultipassSettings().MaximumBundleBytes), true)
	if err != nil {
		return nil, nil, err
	}
	var plan winnerMultipassQualificationPlan
	if decodeStrictJSONBytes(planBytes, &plan, "exact frozen qualification plan") != nil || validateWinnerMultipassQualificationPlan(&plan) != nil || plan.Owner != frozen.Owner || !reflect.DeepEqual(plan.Release, config.Release) || !reflect.DeepEqual(plan.Settings, config.Settings) {
		return nil, nil, errors.New("qualification frozen complete plan changed")
	}
	canonical, _ := json.Marshal(plan)
	if !bytes.Equal(canonical, planBytes) {
		return nil, nil, errors.New("qualification plan is not its canonical original")
	}
	for index, item := range plan.Cases {
		if !reflect.DeepEqual(item.Plan.ScanGroups, config.ScanGroups[index]) {
			return nil, nil, errors.New("qualification source coverage grouping changed")
		}
	}
	inputs, err := authenticateWinnerMultipassQualificationInputs(ctx, config.Release)
	if err != nil {
		return nil, nil, err
	}
	if err := validateWinnerMultipassQualificationOriginalInputs(&plan, inputs); err != nil {
		return nil, nil, err
	}
	spool, aggregate, err := estimateWinnerMultipassQualificationSpoolCapacity(&plan, config.Capacity, config.Spool)
	if err != nil || !reflect.DeepEqual(spool, frozen.Spool) || !reflect.DeepEqual(aggregate, frozen.Capacity) {
		return nil, nil, errors.New("qualification frozen aggregate/local/memory census changed")
	}
	return &frozen, &plan, nil
}

// A repeated capture process is refused even under another output directory.
// Per-call and wrapper generation claims are permanent separate fences as well.
func captureWinnerHandoffGroup(ctx context.Context, config winnerHandoffGroupConfig) (resultErr error) {
	frozen, plan, err := loadWinnerHandoffFrozenGroup(ctx, config)
	if err != nil {
		return err
	}
	permission, err := readCurrentQgQualificationPin(config.CapturePermission, winnerMultipassSpoolManifestBytes, true)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !now.Before(frozen.Owner.Deadline) || now.Before(frozen.Owner.StartedAt) {
		return errors.New("qualification owner is expired or not yet active; no renewal permitted")
	}
	if err := validateWinnerHandoffGroupPermission(permission, "capture", config, frozen, now); err != nil {
		return err
	}
	control, _, err := loadCompetitionControl()
	if err != nil {
		return err
	}
	read := winnerHandoffProtectedReader(control, &model.CompetitionReviewHandoff{Args: winnerHandoffGroupArchiveArgs(config)})
	retain := func(ctx context.Context, content []byte, digest string) (*controller.CompetitionWinnerReviewEvidence, error) {
		return controller.RetainCompetitionReviewHandoffEvidence(ctx, control, config.ArchiveRoundId, config.ArchiveJobId, content, digest)
	}
	profile := winnerMultipassContainmentProfile{ImageId: config.Release.ImageId, ProviderSettings: config.Release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	for _, required := range []string{config.OutputDirectory, os.TempDir(), profile.StateRoot, filepath.Dir(profile.StateRoot)} {
		if !slices.Contains(config.StoragePaths, required) {
			return errors.New("qualification storage roots omit an actual capture/wrapper/archive staging filesystem")
		}
	}
	if err := currentQgQualificationPrivateRoot(config.OutputDirectory); err != nil {
		return err
	}
	startBytes, _ := json.Marshal(struct {
		Schema           int                  `json:"schema"`
		Kind             string               `json:"kind"`
		Owner            winnerMultipassOwner `json:"owner"`
		PlanSha256       string               `json:"plan_sha256"`
		PermissionSha256 string               `json:"permission_sha256"`
		StartedAt        time.Time            `json:"started_at"`
	}{Schema: 1, Kind: "winner-review-qualification-capture-start/v1", Owner: plan.Owner, PlanSha256: frozen.Plan.Sha256, PermissionSha256: config.CapturePermission.Sha256, StartedAt: now})
	startPath := filepath.Join(filepath.Dir(frozen.OwnerClaim.Path), plan.Owner.Id+".capture.json")
	if err := writePrivateReviewFile(startPath, startBytes); err != nil {
		return errors.New("qualification capture already started or cannot persist its one-shot fence; do not reroll")
	}
	if err := syncWinnerHandoffSpoolDirectory(filepath.Dir(startPath)); err != nil {
		return err
	}
	claimsDirectory, spoolDirectory := filepath.Join(config.OutputDirectory, "claims"), filepath.Join(config.OutputDirectory, "protected-spool")
	for _, directory := range []string{claimsDirectory, spoolDirectory} {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
	}
	spoolBytes, _ := json.Marshal(frozen.Spool)
	capacitySha := winnerMultipassSha256(spoolBytes)
	resolve := winnerHandoffQualificationSpoolResolver(config.Release, frozen.Plan.Sha256, plan.Owner.Id, capacitySha, read)
	var storage *winnerHandoffStorage
	var report *winnerMultipassQualificationSpoolReport
	var protectedReport *winnerHandoffCustodyOriginal
	failures := []winnerHandoffCustodyOriginal{}
	defer func() {
		if storage != nil {
			resultErr = errors.Join(resultErr, storage.Close())
		}
		if report != nil {
			if resultErr != nil {
				report.Qualified = false
				report.Error = resultErr.Error()
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultWinnerHandoffCaptureSettings().ReceiptTimeout)
			var err error
			protectedReport, err = publishWinnerHandoffProtectedReport(cleanup, filepath.Join(config.OutputDirectory, "group-report.json"), defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes, plan.Owner.Deadline, func(publicationErr error) ([]byte, error) {
				if publicationErr != nil {
					report.Complete, report.Qualified, report.Error = false, false, errors.Join(resultErr, publicationErr).Error()
				}
				return json.Marshal(report)
			}, retain)
			cancel()
			resultErr = errors.Join(resultErr, err)
		}
		outcome, _ := json.Marshal(struct {
			Schema           int                            `json:"schema"`
			Kind             string                         `json:"kind"`
			FailureOriginals []winnerHandoffCustodyOriginal `json:"failed_originals"`
			ProtectedReport  *winnerHandoffCustodyOriginal  `json:"protected_report,omitempty"`
			Error            string                         `json:"error"`
			WinnerApproved   bool                           `json:"winner_approved"`
		}{Schema: 1, Kind: "winner-review-qualification-native-outcome/v1", FailureOriginals: failures, ProtectedReport: protectedReport, Error: fmt.Sprint(resultErr)})
		resultErr = errors.Join(resultErr, writePrivateReviewFile(filepath.Join(config.OutputDirectory, "native-outcome.json"), outcome), syncWinnerHandoffSpoolDirectory(config.OutputDirectory))
	}()
	var physical *winnerMultipassPhysicalAuthentication
	var independentClaim []byte
	hooks := winnerMultipassQualificationSpoolHooks{Mode: "genuine_provider", Now: func() time.Time { return time.Now().UTC() }, ValidateRelease: authenticateWinnerMultipassReleaseArtifacts,
		Reserve: func(spool *winnerMultipassSpoolCapacity, aggregate *winnerMultipassQualificationCapacity) error {
			if !reflect.DeepEqual(spool, frozen.Spool) || !reflect.DeepEqual(aggregate, frozen.Capacity) {
				return errors.New("qualification reservation census changed")
			}
			cgroup, limit, current, available, err := readWinnerHandoffMemory()
			if err != nil {
				return err
			}
			if limit > config.Capacity.MaximumMemoryBytes || current < 0 || limit-current < spool.RequiredMemoryBytes || available < spool.RequiredMemoryBytes {
				return errors.New("actual qualification native memory owner lacks complete-review headroom")
			}
			storage, err = allocateWinnerHandoffStorage(config.OutputDirectory, config.StoragePaths, spool.RequiredLocalPeakBytes)
			if err != nil {
				return err
			}
			aggregateBytes, _ := json.Marshal(aggregate)
			proof := winnerHandoffCapacityReservation{CapacitySha256: winnerMultipassSha256(aggregateBytes), SpoolCapacitySha256: capacitySha, ReservationPath: storage.path, ReservationInode: storage.inode, Device: storage.device, ReservedBytes: storage.allocated, MemoryLimitBytes: limit, MemoryCurrentBytes: current, HostAvailableBytes: available, MemoryCgroup: cgroup, StoragePaths: config.StoragePaths}
			content, _ := json.Marshal(proof)
			if err := writePrivateReviewFile(filepath.Join(config.OutputDirectory, "actual-reservation.json"), content); err != nil {
				return err
			}
			if err := storage.Consume(spool.PermanentDocumentsBytes + spool.CompactManifestBytes); err != nil {
				return err
			}
			for _, document := range []struct {
				name  string
				value any
			}{{name: "frozen-group.json", value: frozen}, {name: "actual-reservation.json", value: proof}, {name: "group-plan.json", value: plan}, {name: "capture-permission.json", value: json.RawMessage(permission)}} {
				content, _ := json.Marshal(document.value)
				if _, err := retainWinnerHandoffDocument(ctx, document.name, content, defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes, plan.Owner.Deadline, retain); err != nil {
					return err
				}
			}
			return nil
		},
		Load: func(invocation winnerMultipassQualificationInvocation) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
			path := filepath.Join(claimsDirectory, fmt.Sprintf("invocation-%06d.json", invocation.Position))
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("qualification invocation has an existing or ambiguous claim; no repeated capture")
			}
			return nil, nil
		},
		Claim: func(invocation winnerMultipassQualificationInvocation, content []byte) (currentQgQualificationPin, error) {
			path := filepath.Join(claimsDirectory, fmt.Sprintf("invocation-%06d.json", invocation.Position))
			if err := writePrivateReviewFile(path, content); err != nil {
				return currentQgQualificationPin{}, err
			}
			if err := syncWinnerHandoffSpoolDirectory(claimsDirectory); err != nil {
				return currentQgQualificationPin{}, err
			}
			if _, err := retainWinnerHandoffDocument(ctx, "native-qualification-claim.json", content, int(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), plan.Owner.Deadline, retain); err != nil {
				return currentQgQualificationPin{}, err
			}
			return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, nil
		},
		Invoke: func(callCtx context.Context, input []byte) ([]byte, []byte, error) {
			physical, independentClaim = nil, nil
			var envelope winnerMultipassBrokerEnvelope
			if storage == nil || decodeStrictJSONBytes(input, &envelope, "exact contained qualification call") != nil {
				return nil, nil, errors.New("qualification invocation lacks reserved capacity or exact envelope")
			}
			if err := storage.Borrow(frozen.Spool.ActiveInvocationBytes); err != nil {
				return nil, nil, err
			}
			return invokeWinnerMultipassBroker(callCtx, config.Release.Wrapper, input)
		},
		Authenticate: func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
			var receipt *winnerMultipassContainmentReceipt
			var err error
			physical, receipt, independentClaim, err = createWinnerMultipassPhysicalAuthentication(input, output, diagnostics, canceled, profile, config.Release)
			return receipt, err
		},
		Retain: func(retentionCtx context.Context, capture winnerMultipassQualificationCapture) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
			content, err := json.Marshal(capture)
			if err != nil || len(content) > winnerMultipassSpoolCaptureBytes {
				return nil, errors.New("original qualification capture exceeds separately admitted descriptor-record bound")
			}
			if capture.AuthenticationError != "" || capture.Containment == nil || physical == nil {
				original, failure := retainWinnerHandoffGroupFailure(retentionCtx, config.OutputDirectory, frozen.Plan.Sha256, capture, plan.Owner.Deadline, retain, read)
				if original.Retention.VersionId != "" {
					failures = append(failures, original)
				}
				return nil, failure
			}
			var envelope winnerMultipassBrokerEnvelope
			if decodeStrictJSONBytes(capture.Input, &envelope, "original qualification input") != nil {
				return nil, errors.New("qualification original envelope cannot be decoded")
			}
			releaseBytes, _ := json.Marshal(config.Release)
			binding := winnerHandoffSpoolBinding{RunId: plan.Owner.Id, InvocationPosition: capture.Invocation.Position, PlanSha256: frozen.Plan.Sha256, BundleSha256: envelope.BundleSha256, ReleaseSha256: winnerMultipassSha256(releaseBytes), NativeDriverSha256: config.Release.NativeDriver.Sha256, CapacitySha256: capacitySha}
			custody, err := protectWinnerHandoffSpoolInvocation(retentionCtx, spoolDirectory, capture.Invocation.Phase, binding, envelope, capture.Containment, physical, independentClaim, content, config.Release, retain, read)
			if err != nil {
				return nil, err
			}
			if err := storage.Restore(); err != nil {
				return nil, err
			}
			pin := currentQgQualificationPin{Path: custody.SpoolReferencePath, Sha256: custody.SpoolReferenceSha256}
			compact, err := readCurrentQgQualificationPin(pin, winnerMultipassSpoolManifestBytes, true)
			if err != nil {
				return nil, err
			}
			return resolve(retentionCtx, pin, compact)
		},
		RetainCase: func(position int, evidence winnerMultipassEvidence) (currentQgQualificationPin, error) {
			content, err := json.Marshal(evidence)
			if err != nil {
				return currentQgQualificationPin{}, err
			}
			if int64(len(content)) > frozen.Capacity.Cases[position].SerializedEvidenceBytes+winnerMultipassSpoolManifestBytes {
				return currentQgQualificationPin{}, errors.New("original case evidence exceeds fixed document census")
			}
			path := filepath.Join(config.OutputDirectory, fmt.Sprintf("case-%02d-evidence.json", position))
			if err := writePrivateReviewFile(path, content); err != nil {
				return currentQgQualificationPin{}, err
			}
			if _, err := retainWinnerHandoffDocument(ctx, "case-evidence.json", content, defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes, plan.Owner.Deadline, retain); err != nil {
				return currentQgQualificationPin{}, err
			}
			return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, nil
		},
	}
	bounded, cancel := context.WithDeadline(ctx, plan.Owner.Deadline)
	defer cancel()
	report, resultErr = executeWinnerMultipassSpoolQualification(bounded, plan, frozen.Plan, config.Capacity, config.Spool, hooks)
	return resultErr
}
