package competition

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

type Service struct {
	settings    *Settings
	settingsErr error
	store       Store
}

func NewService(settings *Settings, store Store) *Service {
	service := &Service{settings: settings, store: store}
	if settings == nil {
		service.settingsErr = errors.New("competition settings unavailable")
	} else if err := settings.Validate(); err != nil {
		service.settingsErr = err
	}
	if store == nil {
		service.settingsErr = errors.New("competition store unavailable")
	}
	return service
}

var defaultService = sync.OnceValue(func() *Service {
	settings, err := LoadSettings()
	if err != nil {
		return &Service{settingsErr: err, store: PostgresStore{}}
	}
	return NewService(settings, PostgresStore{})
})

func DefaultService() *Service { return defaultService() }

func (s *Service) Settings() (*Settings, error) {
	if s == nil || s.settingsErr != nil {
		if s == nil {
			return nil, errors.New("competition service unavailable")
		}
		return nil, s.settingsErr
	}
	return s.settings, nil
}

func (s *Service) Health() HealthResult {
	version, err := server.Version()
	if err != nil {
		version = "unknown"
	}
	return HealthResult{Status: "alive", Version: version, Time: server.NowUtc()}
}

func (s *Service) Ready(ctx context.Context) (ReadinessResult, *CompetitionError) {
	result := ReadinessResult{Ready: false, Checks: map[string]bool{}, CheckedAt: server.NowUtc()}
	settings, err := s.Settings()
	if err != nil {
		result.Checks["configuration"] = false
		return result, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	checks, err := s.readinessChecks(ctx, settings)
	if err != nil {
		result.Checks["database"] = false
		return result, infrastructureError("readiness_failed", "competition dependencies did not pass readiness")
	}
	result.Checks = checks
	result.Ready = allChecks(checks)
	if !result.Ready {
		return result, infrastructureError("not_ready", "competition evaluator is not ready")
	}
	return result, nil
}

func (s *Service) readinessChecks(ctx context.Context, settings *Settings) (map[string]bool, error) {
	checks, err := s.store.Readiness(ctx, settings)
	if err != nil {
		return checks, err
	}
	if checks == nil {
		checks = map[string]bool{}
	}
	checks["artifact_archive"] = settings.artifactArchive != nil && settings.artifactArchive.Check(ctx) == nil
	return checks, nil
}

func allChecks(checks map[string]bool) bool {
	if len(checks) == 0 {
		return false
	}
	for _, passed := range checks {
		if !passed {
			return false
		}
	}
	return true
}

func secureEvaluatorChecksPass(checks map[string]bool) bool {
	if len(checks) == 0 {
		return false
	}
	for name, passed := range checks {
		// Queue capacity has its own stable 429 response. Every containment,
		// identity, reset, storage, and host check remains mandatory.
		if name != "queue_admission" && !passed {
			return false
		}
	}
	return true
}

func roundGenerationChecksPass(checks map[string]bool) bool {
	if len(checks) == 0 {
		return false
	}
	for name, passed := range checks {
		// A new round must exist before either host can attest its same-round
		// baseline. Queue admission and that round-scoped check are therefore
		// enforced for submissions, not for the atomic generation step.
		if name != "queue_admission" && name != "host_rebaseline" && !passed {
			return false
		}
	}
	return true
}

func (s *Service) requireSecureEvaluator(ctx context.Context, settings *Settings) *CompetitionError {
	checks, err := s.readinessChecks(ctx, settings)
	if err != nil || !secureEvaluatorChecksPass(checks) {
		return infrastructureError("not_ready", "competition evaluator containment is not ready")
	}
	return nil
}

func (s *Service) requireRoundGenerationInfrastructure(ctx context.Context, settings *Settings) *CompetitionError {
	checks, err := s.readinessChecks(ctx, settings)
	if err != nil || !roundGenerationChecksPass(checks) {
		return infrastructureError("not_ready", "competition evaluator infrastructure is not ready for round generation")
	}
	return nil
}

func (s *Service) Info(ctx context.Context) (*InfoResult, *CompetitionError) {
	settings, err := s.Settings()
	if err != nil {
		return nil, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	result := settings.PublicInfo()
	round, err := s.store.CurrentRound(ctx, settings)
	if err != nil {
		return nil, infrastructureError("storage_unavailable", "competition round storage is unavailable")
	}
	if round != nil {
		view, revealErr := s.roundView(round)
		if revealErr != nil {
			return nil, revealErr
		}
		result.ActiveRound = view
	}
	return &result, nil
}

func (s *Service) GenerateRound(ctx context.Context, args GenerateRoundArgs) (*RoundResult, *CompetitionError) {
	settings, err := s.Settings()
	if err != nil {
		return nil, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	args.OpensAt, args.ClosesAt, args.RevealAt = args.OpensAt.UTC(), args.ClosesAt.UTC(), args.RevealAt.UTC()
	if args.OpensAt.IsZero() || args.ClosesAt.IsZero() || args.RevealAt.IsZero() ||
		!args.OpensAt.Before(args.ClosesAt) || !args.RevealAt.Equal(args.ClosesAt) ||
		args.ClosesAt.Sub(args.OpensAt) != time.Duration(settings.SeasonPolicy.SubmissionWindowSeconds)*time.Second {
		return nil, submissionError("invalid_round_times", "round must use the frozen seven-day window with reveal_at equal to closes_at")
	}
	if args.OpensAt.Before(server.NowUtc().Add(-time.Minute)) {
		return nil, submissionError("invalid_round_times", "opens_at may not be in the past")
	}
	if settings.SeasonEndsAt.Before(args.ClosesAt) || settings.RetainUntil.Before(args.RevealAt) {
		return nil, submissionError("invalid_round_times", "round close/reveal exceeds the frozen season retention window")
	}
	if readyErr := s.requireRoundGenerationInfrastructure(ctx, settings); readyErr != nil {
		return nil, readyErr
	}
	round, err := s.store.CreateRound(ctx, settings, args)
	if errors.Is(err, ErrConflict) {
		return nil, &CompetitionError{Kind: "submission", Code: "round_overlap", Message: "round overlaps an existing round", Retriable: false}
	}
	if errors.Is(err, ErrPreviousEpochOpen) {
		return nil, &CompetitionError{Kind: "submission", Code: "previous_epoch_open", Message: "the previous epoch has not finished grading", Retriable: false}
	}
	if errors.Is(err, ErrSeasonComplete) {
		return nil, &CompetitionError{Kind: "submission", Code: "season_complete", Message: "all six competition epochs already exist", Retriable: false}
	}
	if err != nil {
		return nil, infrastructureError("round_create_failed", "round could not be committed")
	}
	return &round.RoundResult, nil
}

func (s *Service) Leaderboards(ctx context.Context) (*SeasonLeaderboardResult, *CompetitionError) {
	settings, err := s.Settings()
	if err != nil {
		return nil, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	result, err := s.store.Leaderboards(ctx, settings)
	if err != nil {
		return nil, infrastructureError("storage_unavailable", "competition leaderboard storage is unavailable")
	}
	return result, nil
}

func (s *Service) Submit(ctx context.Context, args ScoreArgs, principal *Principal) (*ScoreAcceptedResult, int, *CompetitionError) {
	metricOutcome := "infrastructure_error"
	defer func() { competitionSubmissions.WithLabelValues(metricOutcome).Inc() }()
	settings, err := s.Settings()
	if err != nil {
		return nil, 503, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	patch, patchErr := ValidateAndCanonicalizePatch(args.Patch, settings.PatchPolicy)
	if patchErr != nil {
		metricOutcome = "rejected"
		status := 422
		if patchErr.Code == "patch_too_large" {
			status = 413
		}
		return nil, status, patchErr
	}
	if readyErr := s.requireSecureEvaluator(ctx, settings); readyErr != nil {
		return nil, 503, readyErr
	}
	job, hit, err := s.store.Enqueue(ctx, settings, args.RoundId, patch, principal.Id)
	switch {
	case errors.Is(err, ErrNotFound):
		metricOutcome = "rejected"
		return nil, 404, submissionError("round_not_found", "round does not exist")
	case errors.Is(err, ErrRoundClosed):
		metricOutcome = "rejected"
		return nil, 409, submissionError("round_not_open", "round is not open for submissions")
	case errors.Is(err, ErrQueueFull):
		metricOutcome = "queue_full"
		return nil, 429, &CompetitionError{Kind: "infrastructure", Code: "queue_full", Message: "evaluation queue admission limit reached", Retriable: true}
	case err != nil:
		return nil, 503, infrastructureError("enqueue_failed", "submission could not be durably enqueued")
	}
	result := &ScoreAcceptedResult{
		JobId: job.JobId, RoundId: job.RoundId, PatchSha256: job.PatchSha256,
		State: job.State, CacheHit: hit, StatusUrl: "/competition/score/" + job.JobId.String(),
	}
	if hit {
		metricOutcome = "cache_hit"
	} else {
		metricOutcome = "accepted"
	}
	return result, 202, nil
}

func (s *Service) GetScore(ctx context.Context, jobId server.Id, principal *Principal) (*ScoreJobResult, int, *CompetitionError) {
	settings, err := s.Settings()
	if err != nil {
		return nil, 503, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	job, err := s.store.GetJob(ctx, settings, jobId, principal)
	if errors.Is(err, ErrNotFound) {
		return nil, 404, submissionError("job_not_found", "score job does not exist")
	}
	if err != nil {
		return nil, 503, infrastructureError("storage_unavailable", "score job storage is unavailable")
	}
	result := job.ScoreJobResult
	if server.NowUtc().Before(job.Round.RevealAt) && principal.Role != "operator" {
		result.Score = activeRoundScoreView(result.Score)
		result.EvalError = activeRoundErrorView(result.EvalError)
	}
	return &result, 200, nil
}

func (s *Service) GetRoundWorkload(ctx context.Context, roundId server.Id) ([]byte, string, int, *CompetitionError) {
	settings, err := s.Settings()
	if err != nil {
		return nil, "", 503, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	round, err := s.store.GetRound(ctx, settings, roundId)
	if errors.Is(err, ErrNotFound) {
		return nil, "", 404, submissionError("round_not_found", "round does not exist")
	}
	if err != nil {
		return nil, "", 503, infrastructureError("storage_unavailable", "competition round storage is unavailable")
	}
	if round.Canceled || server.NowUtc().Before(round.RevealAt) {
		return nil, "", 409, submissionError("round_not_revealed", "round workload is unavailable until reveal_at")
	}
	providers, err := readRoundWorkload(ctx, settings, round)
	if err != nil {
		return nil, "", 503, infrastructureError("round_workload_unavailable", "committed round workload failed authentication")
	}
	return providers, round.ProvidersSha256, 200, nil
}

func (s *Service) roundView(round *roundRecord) (*RoundResult, *CompetitionError) {
	view := round.RoundResult
	if !server.NowUtc().Before(round.RevealAt) {
		seed, err := revealRoundSecret(s.settings, round)
		if err != nil {
			return nil, infrastructureError("round_reveal_failed", "round commitment could not be revealed")
		}
		view.RevealedSeed = &seed
		view.ProvidersUrl = "/competition/round/" + round.RoundId.String() + "/providers.yml"
	}
	return &view, nil
}

func activeRoundScoreView(score *ScoreResult) *ScoreResult {
	if score == nil {
		return nil
	}
	view := *score
	view.RawScore = nil
	view.Diagnostics = nil
	view.Gates = make(map[string]Gate, len(score.Gates))
	for name, gate := range score.Gates {
		view.Gates[name] = Gate{Passed: gate.Passed, Details: map[string]any{}}
	}
	return &view
}

func activeRoundErrorView(evalError *CompetitionError) *CompetitionError {
	if evalError == nil {
		return nil
	}
	return &CompetitionError{
		Kind: evalError.Kind, Code: evalError.Code,
		Message: safeActiveErrorMessage(evalError), Retriable: evalError.Retriable,
	}
}

func safeActiveErrorMessage(evalError *CompetitionError) string {
	if evalError.Kind == "infrastructure" {
		return "evaluation infrastructure error"
	}
	return "submission did not pass evaluation"
}

func validateScore(score *ScoreResult) error {
	if score == nil || score.ScoreSchema != ScoreSchema || score.RawScore == nil || score.NormalizedScore == nil {
		return errors.New("score result is missing required fields")
	}
	if math.IsNaN(*score.RawScore) || math.IsInf(*score.RawScore, 0) || *score.RawScore <= 0 {
		return errors.New("raw score must be finite and positive")
	}
	if math.IsNaN(*score.NormalizedScore) || math.IsInf(*score.NormalizedScore, 0) || *score.NormalizedScore < 1 || 200 < *score.NormalizedScore {
		return errors.New("normalized score must be finite and in [1, 200]")
	}
	if score.Gates == nil {
		return errors.New("score gates are missing")
	}
	for name, gate := range score.Gates {
		if strings.TrimSpace(name) == "" || gate.Details == nil {
			return fmt.Errorf("score gate %q is malformed", name)
		}
	}
	return nil
}

func infrastructureError(code, message string) *CompetitionError {
	return &CompetitionError{Kind: "infrastructure", Code: code, Message: message, Retriable: true}
}
