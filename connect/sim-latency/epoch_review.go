package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

const maximumHonestyEvidenceBytes = 1024 * 1024

type epochReviewResult struct {
	Schema             int                              `json:"schema"`
	Action             string                           `json:"action"`
	State              *controller.CandidateReviewState `json:"state"`
	CandidateDirectory string                           `json:"candidate_directory,omitempty"`
}

func readHonestyEvidence(path string) ([]byte, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || maximumHonestyEvidenceBytes < info.Size() {
		return nil, "", errors.New("honesty evidence must be a nonempty regular JSON file of at most 1 MiB")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if !json.Valid(content) {
		return nil, "", errors.New("honesty evidence is not valid JSON")
	}
	digest := sha256.Sum256(content)
	return content, hex.EncodeToString(digest[:]), nil
}

func writePrivateReviewFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func materializeReviewCandidate(
	state *controller.CandidateReviewState,
	requestedDirectory string,
) (string, error) {
	if state == nil || state.Status != "pending_review" || state.Candidate == nil {
		return "", errors.New("candidate review state has no pending candidate")
	}
	directory := ""
	var err error
	if requestedDirectory == "" {
		directory, err = os.MkdirTemp("", fmt.Sprintf("sim-latency-review-epoch-%d-", state.Epoch))
	} else {
		directory, err = filepath.Abs(requestedDirectory)
		if err == nil {
			err = os.Mkdir(directory, 0o700)
		}
	}
	if err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", err
	}
	candidateBytes, err := json.MarshalIndent(state.Candidate, "", "  ")
	if err != nil {
		return "", err
	}
	scoreBytes, err := json.MarshalIndent(state.Candidate.Score, "", "  ")
	if err != nil {
		return "", err
	}
	for name, content := range map[string][]byte{
		"candidate.json":  append(candidateBytes, '\n'),
		"score.json":      append(scoreBytes, '\n'),
		"canonical.patch": state.Candidate.Patch,
	} {
		if err := writePrivateReviewFile(filepath.Join(directory, name), content); err != nil {
			return "", err
		}
	}
	cleanup = false
	return directory, nil
}

func loadCompetitionControl() (*controller.Settings, controller.PostgresStore, error) {
	settings, err := controller.LoadSettings()
	if err != nil {
		return nil, controller.PostgresStore{}, err
	}
	if !settings.Enabled {
		return nil, controller.PostgresStore{}, errors.New("competition is disabled")
	}
	return settings, controller.PostgresStore{}, nil
}

// Keeps rejection as a state-only transition so a later explicit enumeration
// owns the next private directory and its cleanup.
func reviewActionMaterializesCandidate(action string, state *controller.CandidateReviewState) bool {
	return action == "next" && state != nil && state.Status == "pending_review" && state.Candidate != nil
}

func runEpochReview(opts docopt.Opts) {
	epoch, err := configuredEpoch(opts)
	if err != nil || epoch < 1 || maximumCompetitionEpoch < epoch {
		fatalf("honesty-review epoch must be an integer in 1..%d", maximumCompetitionEpoch)
	}
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("load competition control plane: %s", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	action := "next"
	var state *controller.CandidateReviewState
	materializedState := (*controller.CandidateReviewState)(nil)
	if optBool(opts, "export-winner") {
		action = "export-winner"
		jobId, parseErr := server.ParseId(optString(opts, "--job-id", ""))
		if parseErr != nil {
			fatalf("winner job id: %s", parseErr)
		}
		state, err = store.PrepareCandidateReview(ctx, settings, epoch)
		if err == nil && (state.Status != "finalized" || state.WinnerJobId == nil || *state.WinnerJobId != jobId) {
			err = errors.New("epoch does not have this finalized approved winner")
		}
		var candidate *controller.CandidateReviewCandidate
		if err == nil {
			candidate, err = store.RequirePromotionDecision(ctx, settings, epoch, &jobId)
		}
		if err == nil {
			copy := *state
			copy.Status = "pending_review"
			copy.Candidate = candidate
			materializedState = &copy
		}
	} else if optBool(opts, "reject") || optBool(opts, "approve") {
		decision := "approved"
		if optBool(opts, "reject") {
			action = "reject"
			decision = "rejected"
		} else {
			action = "approve"
		}
		jobId, parseErr := server.ParseId(optString(opts, "--job-id", ""))
		if parseErr != nil {
			fatalf("candidate job id: %s", parseErr)
		}
		evidence, evidenceSha256, evidenceErr := readHonestyEvidence(optString(opts, "--evidence", ""))
		if evidenceErr != nil {
			fatalf("honesty evidence: %s", evidenceErr)
		}
		state, err = store.RecordCandidateReview(ctx, settings, epoch, controller.CandidateReviewDecision{
			JobId:          jobId,
			Decision:       decision,
			ReviewerId:     optString(opts, "--reviewer", ""),
			Reason:         optString(opts, "--reason", ""),
			Evidence:       evidence,
			EvidenceSha256: evidenceSha256,
		})
	} else {
		state, err = store.PrepareCandidateReview(ctx, settings, epoch)
	}
	if err != nil {
		fatalf("epoch honesty review: %s", err)
	}
	result := epochReviewResult{Schema: 1, Action: action, State: state}
	if materializedState == nil && reviewActionMaterializesCandidate(action, state) {
		materializedState = state
	}
	if materializedState != nil {
		result.CandidateDirectory, err = materializeReviewCandidate(
			materializedState,
			optString(opts, "--out-dir", ""),
		)
		if err != nil {
			fatalf("materialize review candidate: %s", err)
		}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("encode honesty-review result: %s", err)
	}
	fmt.Printf("%s\n", encoded)
}

func requireReviewedPromotion(
	epoch int,
	winnerJobId string,
	noWinner bool,
) (*controller.CandidateReviewCandidate, error) {
	settings, store, err := loadCompetitionControl()
	if err != nil {
		return nil, err
	}
	var winner *server.Id
	if !noWinner {
		parsed, err := server.ParseId(winnerJobId)
		if err != nil {
			return nil, fmt.Errorf("winner job id: %w", err)
		}
		winner = &parsed
	}
	return store.RequirePromotionDecision(context.Background(), settings, epoch, winner)
}

func approvedSignificanceMatches(
	approved *controller.ScoreSignificance,
	promotion *sourceSignificance,
) bool {
	if approved == nil || promotion == nil || approved.BaselineSampleVariance == nil ||
		approved.CandidateSampleVariance == nil ||
		approved.MinimumSignificantImprovementPercent == nil ||
		approved.RequiredImprovementPercent == nil || approved.OneSidedPValue == nil ||
		approved.NextEpochMinimumImprovementPercent == nil ||
		approved.RecommendedNextEpochTakeoverMarginPercent == nil {
		return false
	}
	return approved.Method == promotion.Method &&
		approved.Alpha == promotion.Alpha &&
		approved.ReplicateCount == promotion.ReplicateCount &&
		approved.BaselineMeanRawScore == promotion.BaselineMeanRawScore &&
		approved.CandidateMeanRawScore == promotion.CandidateMeanRawScore &&
		*approved.BaselineSampleVariance == promotion.BaselineSampleVariance &&
		*approved.CandidateSampleVariance == promotion.CandidateSampleVariance &&
		approved.ObservedImprovementPercent == promotion.ObservedImprovementPercent &&
		approved.TakeoverMarginPercent == promotion.TakeoverMarginPercent &&
		*approved.MinimumSignificantImprovementPercent == promotion.MinimumSignificantImprovementPercent &&
		*approved.RequiredImprovementPercent == promotion.RequiredImprovementPercent &&
		*approved.OneSidedPValue == promotion.OneSidedPValue &&
		*approved.NextEpochMinimumImprovementPercent == promotion.NextEpochMinimumImprovementPercent &&
		*approved.RecommendedNextEpochTakeoverMarginPercent == promotion.RecommendedNextEpochTakeoverMarginPercent
}

func authenticateApprovedWinnerBundle(
	winnerRoot string,
	approved *controller.CandidateReviewCandidate,
	winnerScore *controller.ScoreResult,
	promotionSignificance *sourceSignificance,
) error {
	if approved == nil || approved.JobId == (server.Id{}) {
		return errors.New("approved promotion candidate is missing")
	}
	for _, repository := range sourceRepositoryNames() {
		if repository == "server" {
			continue
		}
		_, found, err := promotionPatchPath(winnerRoot, repository)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("winner contains unevaluated %s repository patch", repository)
		}
	}
	patchPath, found, err := promotionPatchPath(winnerRoot, "server")
	if err != nil {
		return err
	}
	if !found {
		return errors.New("winner bundle is missing the evaluated canonical patch")
	}
	patchBytes, err := readBoundedFile(patchPath, "winner patch")
	if err != nil {
		return err
	}
	digest := sha256.Sum256(patchBytes)
	if hex.EncodeToString(digest[:]) != approved.PatchSha256 {
		return errors.New("winner patch does not match the approved candidate")
	}
	if !exactApprovedScoreMatches(approved.Score, winnerScore) {
		return errors.New("winner score document does not match the approved candidate")
	}
	if !approvedSignificanceMatches(approved.Score.Significance, promotionSignificance) {
		return errors.New("winner score significance does not match the approved candidate")
	}
	return nil
}
