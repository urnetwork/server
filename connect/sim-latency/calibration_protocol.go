// Explicit prospective calibration protocols retain their original source,
// sample positions and approval pins. The published five-cohort default stays
// unchanged; input cardinality never selects an amendment automatically.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

// This named protocol is the user's separately published October 26 amendment.
const calibrationProtocolThreeABridgeThreeB = "launch-3a-bridge-3b-2026-10-26-v1"

// Identities come from the original five-position publication, not successful
// result discovery. Positions four/five remain retained and unused.
type calibrationOriginalPosition struct {
	Position         int    `json:"position"`
	JobId            string `json:"job_id"`
	RoundId          string `json:"round_id"`
	Root             string `json:"root"`
	RequestSha256    string `json:"request_sha256"`
	ProvenanceSha256 string `json:"provenance_sha256"`
}

// Deliberately no generic cohort-count, bootstrap or threshold override.
func calibrationSettingsForProtocol(protocol string) (calibrationAnalysisSettings, error) {
	settings := defaultCalibrationAnalysisSettings()
	switch protocol {
	case "":
		return settings, nil
	case calibrationProtocolThreeABridgeThreeB:
		settings.Protocol = protocol
		settings.Cohorts = 3
		return settings, nil
	default:
		return settings, errors.New("unknown prospective calibration protocol")
	}
}

// Public approval/plan records may live outside private artifact directories;
// exact independently supplied byte pins still reject redirection and drift.
func readCalibrationProtocolPin(path, digest string) ([]byte, error) {
	if !validSha256(digest) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("calibration protocol publication pin invalid")
	}
	resolved, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	if err != nil || statErr != nil || resolved != path || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("calibration protocol publication redirected or writable")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() < 0 || opened.Size() > int64(maximumSourceConfigBytes) {
		return nil, errors.New("calibration protocol publication changed or oversized")
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(maximumSourceConfigBytes)+1))
	after, statErr := file.Stat()
	hash := sha256.Sum256(content)
	if err != nil || statErr != nil || len(content) > maximumSourceConfigBytes || int64(len(content)) != opened.Size() || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() || hex.EncodeToString(hash[:]) != digest {
		return nil, errors.New("calibration protocol publication bytes changed")
	}
	return content, nil
}

// Authenticates the approval publication and original full five-position plan
// before selecting the fixed first three, independently of result contents.
func bindCalibrationProtocolPublications(settings calibrationAnalysisSettings, approvalPath, approvalDigest, planPath, planDigest string) (calibrationAnalysisSettings, error) {
	if settings.Protocol == "" {
		if approvalPath != "" || approvalDigest != "" || planPath != "" || planDigest != "" {
			return settings, errors.New("approval/plan options require the named amended calibration protocol")
		}
		return settings, nil
	}
	if settings.Protocol != calibrationProtocolThreeABridgeThreeB {
		return settings, errors.New("unsupported calibration amendment")
	}
	if _, err := readCalibrationProtocolPin(approvalPath, approvalDigest); err != nil {
		return settings, err
	}
	content, err := readCalibrationProtocolPin(planPath, planDigest)
	if err != nil {
		return settings, err
	}
	var plan map[string]json.RawMessage
	if err := decodeStrictJSONBytes(content, &plan, "original five-position calibration plan"); err != nil {
		return settings, err
	}
	var schema int
	var kind string
	var positions []calibrationOriginalPosition
	if json.Unmarshal(plan["schema"], &schema) != nil || schema != 1 || json.Unmarshal(plan["kind"], &kind) != nil || kind != "prepared-corrected-release-five-cohort-aa-plan" {
		return settings, errors.New("amendment requires the original corrected-release five-position plan")
	}
	if err := decodeStrictJSONBytes(plan["ordered_cohorts"], &positions, "original ordered calibration positions"); err != nil {
		return settings, err
	}
	if len(positions) != defaultCalibrationAnalysisSettings().Cohorts {
		return settings, errors.New("original five-position inventory must be retained intact")
	}
	seenIdBools := map[string]bool{}
	seenRootBools := map[string]bool{}
	for index, position := range positions {
		if position.Position != index+1 || position.JobId == "" || position.RoundId == "" || seenIdBools[position.JobId] || seenIdBools[position.RoundId] || seenRootBools[position.Root] || !validSha256(position.RequestSha256) || !validSha256(position.ProvenanceSha256) || !filepath.IsAbs(position.Root) || filepath.Clean(position.Root) != position.Root {
			return settings, errors.New("original calibration position/order identity invalid")
		}
		seenIdBools[position.JobId], seenIdBools[position.RoundId] = true, true
		seenRootBools[position.Root] = true
		if _, err := readCalibrationArtifact(position.Root, "worker-request.json", position.RequestSha256); err != nil {
			return settings, errors.New("original five-position request inventory not retained")
		}
		if _, err := readCalibrationArtifact(position.Root, "fixture-provenance.json", position.ProvenanceSha256); err != nil {
			return settings, errors.New("original five-position provenance inventory not retained")
		}
		if index >= settings.Cohorts {
			for _, relative := range []string{"started", "started.json", "manager-terminal.log", "artifacts/worker-result.json"} {
				if _, err := os.Lstat(filepath.Join(position.Root, relative)); !errors.Is(err, os.ErrNotExist) {
					return settings, errors.New("unused original fourth/fifth calibration position has execution evidence")
				}
			}
		}
	}
	for key, destination := range map[string]*string{"base_sha": &settings.OriginalBaseSha, "evaluator_image_digest": &settings.OriginalImageDigest, "evaluation_policy_sha256": &settings.OriginalPolicySha256} {
		if json.Unmarshal(plan[key], destination) != nil || *destination == "" {
			return settings, errors.New("original calibration plan release identity missing")
		}
	}
	if !sourceGitShaPattern.MatchString(settings.OriginalBaseSha) || len(settings.OriginalImageDigest) != len("sha256:")+64 || settings.OriginalImageDigest[:len("sha256:")] != "sha256:" || !validSha256(settings.OriginalImageDigest[len("sha256:"):]) || !validSha256(settings.OriginalPolicySha256) {
		return settings, errors.New("original calibration release identity malformed")
	}
	settings.ProtocolApprovalSha256, settings.OriginalPlanSha256 = approvalDigest, planDigest
	settings.OriginalPositions = append([]calibrationOriginalPosition{}, positions[:settings.Cohorts]...)
	return settings, nil
}

// Each original first position must match exact precommitted identities, not a
// favorable three-result subset, reordered input or a relabeled new image.
func validateCalibrationProtocolManifest(manifest calibrationFileManifest, settings calibrationAnalysisSettings) error {
	if settings.Protocol == "" {
		if manifest.Protocol != "" || manifest.ProtocolApprovalSha256 != "" || manifest.OriginalPlanSha256 != "" || len(manifest.OriginalPositions) != 0 {
			return errors.New("amended inputs cannot use the historical default calibration contract")
		}
		return nil
	}
	if settings.Protocol != calibrationProtocolThreeABridgeThreeB || settings.Cohorts != 3 || !validSha256(settings.ProtocolApprovalSha256) || !validSha256(settings.OriginalPlanSha256) || len(settings.OriginalPositions) != 3 || manifest.Protocol != settings.Protocol || manifest.ProtocolApprovalSha256 != settings.ProtocolApprovalSha256 || manifest.OriginalPlanSha256 != settings.OriginalPlanSha256 || !reflect.DeepEqual(manifest.OriginalPositions, []int{1, 2, 3}) || len(manifest.Cohorts) != 3 {
		return errors.New("amended calibration protocol/approval/original-position binding missing")
	}
	if manifest.BaseSha != settings.OriginalBaseSha || manifest.EvaluatorImageDigest != settings.OriginalImageDigest || manifest.EvaluationPolicySha256 != settings.OriginalPolicySha256 {
		return errors.New("original three A observations cannot be relabeled as a new release")
	}
	for index, cohort := range manifest.Cohorts {
		position := settings.OriginalPositions[index]
		if position.Position != index+1 || cohort.JobId != position.JobId || cohort.RoundId != position.RoundId || cohort.RequestSha256 != position.RequestSha256 || cohort.ArtifactDirectory != filepath.Join(position.Root, "artifacts") {
			return errors.New("amended calibration changed an original first-three position")
		}
	}
	return nil
}
