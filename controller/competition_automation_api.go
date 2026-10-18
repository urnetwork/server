// Explicit v2 transports preserve legacy winner semantics and the publication embargo.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Optional storage capabilities permit old readers to fail explicitly during rollout.
type competitionAutomationStore interface {
	SealAndElect(context.Context, *Settings, server.Id) (*model.CompetitionV2RoundResult, error)
	AutomationResult(context.Context, *Settings, server.Id) (*model.CompetitionV2RoundResult, error)
	RecordIntegrityReview(context.Context, *Settings, server.Id, string, model.CompetitionIntegrityReviewRequest) (*model.CompetitionIntegrityReviewResult, error)
	RewardEvents(context.Context, *Settings, int64, int) (*model.CompetitionRewardEventPage, error)
	RecordSettlementReceipt(context.Context, *Settings, string, model.CompetitionSettlementReceipt) (*model.CompetitionSettlementReceipt, error)
}

// Reads only the frozen mode; current settings cannot reclassify an existing round.
func competitionRoundUsesV2(round *roundRecord) bool {
	if round == nil || len(round.PolicyJson) == 0 {
		return false
	}
	var policy struct {
		AutomationPolicy *model.CompetitionAutomationPolicy `json:"automation_policy"`
	}
	return json.Unmarshal(round.PolicyJson, &policy) == nil && policy.AutomationPolicy != nil && policy.AutomationPolicy.Enabled
}

// Malformed explicit modes fail closed rather than falling back to legacy admission.
func requireRoundDecisionContract(round *roundRecord, v2 bool) (int, *CompetitionError) {
	if round == nil {
		return http.StatusNotFound, submissionError("round_not_found", "round does not exist")
	}
	if len(round.PolicyJson) != 0 {
		if _, err := decodeRoundPolicySnapshot(round.PolicyJson); err != nil {
			return http.StatusServiceUnavailable, infrastructureError("round_policy_mismatch", "round policy could not be authenticated")
		}
	}
	if competitionRoundUsesV2(round) != v2 {
		return http.StatusConflict, submissionError("unsupported_decision_mode", "use the immutable API contract advertised for this round")
	}
	if v2 {
		policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
		if err != nil || policy.AutomationPolicy == nil || model.ValidateCompetitionAutomationPolicy(*policy.AutomationPolicy) != nil || policy.AutomationPolicy.Staging != round.Staging {
			return http.StatusServiceUnavailable, infrastructureError("round_policy_mismatch", "round automation policy could not be authenticated")
		}
	}
	return http.StatusOK, nil
}

// Capability errors intentionally reveal no private evidence or expected candidate.
func competitionScopeDenied() *CompetitionError {
	return &CompetitionError{Kind: "auth", Code: "scope_denied", Message: "this token does not authorize the requested competition capability", Retriable: false}
}

// New replicas advertise optional capabilities without rewriting old round snapshots.
func applyCompetitionDecisionDiscovery(result *InfoResult, round *roundRecord) {
	if competitionRoundUsesV2(round) {
		result.DecisionMode, result.RequiredApiCapability = model.CompetitionDecisionModeV2, model.CompetitionAutomationCapability
	}
}

// The public projection contains sanctioned explanations, never private review notes or token names.
func publicCompetitionV2Result(result *model.CompetitionV2RoundResult, now time.Time) (*model.CompetitionV2RoundResult, error) {
	if result == nil || result.Seal.PublicationAt.IsZero() || now.Before(result.Seal.PublicationAt) {
		return nil, ErrAutomationNotPublished
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var public model.CompetitionV2RoundResult
	if err := json.Unmarshal(encoded, &public); err != nil {
		return nil, err
	}
	public.IntegrityReview, public.SourceApproval = competitionV2CurrentReviewState(public.ProvisionalRewardWinner, public.IntegrityReviews)
	for i := range public.IntegrityReviews {
		review := &public.IntegrityReviews[i]
		review.ActorId = ""
		review.Request.IdempotencyKey = ""
		review.Request.PrivateEvidenceSha256 = ""
		review.Request.ExpectedReplacementJobId = nil
		for j := range review.Request.Findings {
			review.Request.Findings[j].EvidenceSha256 = ""
			review.Request.Findings[j].Explanation = ""
		}
	}
	for i := range public.SettlementReceipts {
		public.SettlementReceipts[i].ActorId = ""
		public.SettlementReceipts[i].AccountingReference = ""
	}
	for i := range public.Incidents {
		public.Incidents[i].ActorId = ""
		public.Incidents[i].Request.IdempotencyKey = ""
	}
	if public.SourceDecision != nil {
		public.SourceDecision.GitRefs = nil
		public.SourceDecision.Manifest = nil
	}
	return &public, nil
}

// Only actual reviews of the selected candidate affect its public approvals.
// Restoring reward eligibility alone cannot resurrect a disqualified source approval.
func competitionV2CurrentReviewState(current *server.Id, reviews []model.CompetitionIntegrityReviewResult) (string, string) {
	if current == nil {
		return "not_applicable", "not_applicable"
	}
	integrity, source := "not_reviewed", "pending"
	for _, review := range reviews {
		if review.Request.JobId != *current {
			continue
		}
		if review.Request.SourceDisposition != "unchanged" {
			source = review.Request.SourceDisposition
		}
		switch review.Request.RewardDisposition {
		case "approved", "restored":
			integrity = "approved"
		case "disqualified":
			integrity, source = "disqualified", "rejected"
		}
	}
	return integrity, source
}

// Maps durable transition refusals to bounded, non-sensitive protocol errors.
func competitionAutomationError(err error) (int, *CompetitionError) {
	switch {
	case errors.Is(err, ErrAutomationHeld):
		return http.StatusConflict, submissionError("automation_held", "this capability is paused by an authorized hold")
	case errors.Is(err, ErrEvaluationInvalidated):
		return http.StatusConflict, submissionError("evaluation_invalidated", "an integrity incident invalidated the affected evaluation")
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, submissionError("round_not_found", "the requested competition record does not exist")
	case errors.Is(err, ErrAutomationDisabled), errors.Is(err, ErrUnsupportedDecisionMode):
		return http.StatusConflict, submissionError("unsupported_decision_mode", "this operation requires the explicitly enabled v2 contract")
	case errors.Is(err, ErrAutomationNotPublished):
		return http.StatusConflict, submissionError("round_not_published", "results are unavailable until close, complete drain, and reveal")
	case errors.Is(err, ErrConflict), errors.Is(err, ErrPreviousEpochOpen), errors.Is(err, ErrRoundNotOpen), errors.Is(err, ErrRoundClosed), errors.Is(err, ErrReviewNotReady):
		return http.StatusConflict, submissionError("decision_conflict", "the transition conflicts with the current immutable decision; reconcile the published history")
	default:
		return http.StatusServiceUnavailable, infrastructureError("automation_unavailable", "competition automation storage is unavailable")
	}
}

// Resolves the store without acquiring another connection or falling back to v1 methods.
func (self *Service) automationStore() (*Settings, competitionAutomationStore, int, *CompetitionError) {
	settings, err := self.Settings()
	if err != nil {
		return nil, nil, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	store, ok := self.store.(competitionAutomationStore)
	if !ok {
		return nil, nil, http.StatusServiceUnavailable, infrastructureError("capability_unavailable", "this replica does not support the v2 decision contract")
	}
	return settings, store, http.StatusOK, nil
}

// V2 writes cannot be accidentally reached by the legacy score endpoint.
func (self *Service) SubmitV2(ctx context.Context, args ScoreArgs, principal *Principal) (*ScoreAcceptedResult, int, *CompetitionError) {
	return self.submitWithDecisionContract(ctx, args, principal, true)
}

// V2 polling still uses the authoritative job-owner check and outcome-neutral projection.
func (self *Service) GetScoreV2(ctx context.Context, jobId server.Id, principal *Principal) (*ScoreJobResult, int, *CompetitionError) {
	return self.getScoreWithDecisionContract(ctx, jobId, principal, true)
}

// Reads a public projection only after the durable store and local embargo check agree.
func (self *Service) AutomationResult(ctx context.Context, roundId server.Id) (*model.CompetitionV2RoundResult, int, *CompetitionError) {
	settings, store, status, evalError := self.automationStore()
	if evalError != nil {
		return nil, status, evalError
	}
	result, err := store.AutomationResult(ctx, settings, roundId)
	if err == nil {
		result, err = publicCompetitionV2Result(result, server.NowUtc())
	}
	if err != nil {
		status, evalError = competitionAutomationError(err)
		return nil, status, evalError
	}
	return result, http.StatusOK, nil
}

// Strict path identity parsing is shared by all round controls.
func competitionV2PathId(w http.ResponseWriter, r *http.Request) (server.Id, bool) {
	values := router.GetPathValues(r)
	if len(values) == 1 {
		if id, err := server.ParseId(values[0]); err == nil {
			return id, true
		}
	}
	writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_id", "the requested competition identity is malformed"))
	return server.Id{}, false
}

// Restricts v2 submission to the submitter role before decoding a candidate patch.
func CompetitionV2SubmitScoreHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	if principal.Role != "submitter" {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	var args ScoreArgs
	if evalError := decodeCompetitionBody(w, r, &args, int64(hardRequestBodyLimit)); evalError != nil {
		status := http.StatusBadRequest
		if evalError.Code == "request_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	result, status, evalError := service.SubmitV2(r.Context(), args, principal)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, status, result)
}

// Never upgrades a legacy operator to v2 job-owner authority.
func CompetitionV2GetScoreHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	if principal.Role != "submitter" {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	jobId, ok := competitionV2PathId(w, r)
	if !ok {
		return
	}
	result, status, evalError := service.GetScoreV2(r.Context(), jobId, principal)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, status, result)
}

// Public results have no privileged bypass for scores, candidates, or findings.
func CompetitionV2RoundResultHandler(w http.ResponseWriter, r *http.Request) {
	roundId, ok := competitionV2PathId(w, r)
	if !ok {
		return
	}
	result, status, evalError := DefaultService().AutomationResult(r.Context(), roundId)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, status, result)
}

// History uses the same complete result shape and embargo, with immutable revisions.
func CompetitionV2DecisionsHandler(w http.ResponseWriter, r *http.Request) {
	CompetitionV2RoundResultHandler(w, r)
}

// The scheduler can request close-and-drain election, but cannot select its winner.
func CompetitionV2SealHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	if _, ok := requireCompetitionV2Principal(w, r, service, competitionSchedulerRole); !ok {
		return
	}
	roundId, ok := competitionV2PathId(w, r)
	if !ok {
		return
	}
	settings, store, status, evalError := service.automationStore()
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	result, err := store.SealAndElect(r.Context(), settings, roundId)
	if err == nil {
		result, err = publicCompetitionV2Result(result, server.NowUtc())
	}
	if err != nil {
		status, evalError = competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

// Findings and optimistic revision checks are bounded and attributed by authentication.
func CompetitionV2ReviewHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requireCompetitionV2Principal(w, r, service, competitionReviewerRole)
	if !ok {
		return
	}
	roundId, ok := competitionV2PathId(w, r)
	if !ok {
		return
	}
	var request model.CompetitionIntegrityReviewRequest
	if evalError := decodeCompetitionBody(w, r, &request, 64*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	settings, store, status, evalError := service.automationStore()
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	if status, evalError := service.validateV2IntegrityReview(r.Context(), settings, roundId, request); evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	result, err := store.RecordIntegrityReview(r.Context(), settings, roundId, principal.Id, request)
	if err != nil {
		status, evalError = competitionAutomationError(err)
		if status == http.StatusConflict {
			if current, readErr := store.AutomationResult(r.Context(), settings, roundId); readErr == nil && current != nil && len(current.Decisions) > 0 && !server.NowUtc().Before(current.Seal.PublicationAt) {
				evalError.CurrentRevision = current.RewardRevision
				evalError.CurrentDecisionSha256 = current.Decisions[len(current.Decisions)-1].DecisionSha256
			}
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, result)
}

// Historical reviews use their own immutable criteria, not the next round's live policy.
// The store repeats these checks inside its serialized transition.
func (self *Service) validateV2IntegrityReview(ctx context.Context, settings *Settings, roundId server.Id, request model.CompetitionIntegrityReviewRequest) (int, *CompetitionError) {
	round, err := self.store.GetRound(ctx, settings, roundId)
	if err != nil {
		return competitionAutomationError(err)
	}
	if status, evalError := requireRoundDecisionContract(round, true); evalError != nil {
		return status, evalError
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil || policy.AutomationPolicy == nil {
		return http.StatusServiceUnavailable, infrastructureError("round_policy_mismatch", "round review policy could not be authenticated")
	}
	if err := model.ValidateCompetitionIntegrityReview(request, *policy.AutomationPolicy); err != nil {
		return http.StatusBadRequest, submissionError("invalid_review", err.Error())
	}
	return http.StatusOK, nil
}

// Cursor/limit spelling is strict so an invalid request can never reset reconciliation.
func competitionRewardEventQuery(r *http.Request) (int64, int, *CompetitionError) {
	afterCursor, limit := int64(0), 100
	for key, values := range r.URL.Query() {
		if len(values) != 1 || (key != "after_cursor" && key != "limit") {
			return 0, 0, submissionError("invalid_event_query", "expected a single after_cursor and limit")
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || strconv.FormatInt(value, 10) != values[0] {
			return 0, 0, submissionError("invalid_event_query", "cursor and limit must be canonical decimal integers")
		}
		if key == "after_cursor" {
			afterCursor = value
		} else if value >= 1 && value <= 200 {
			limit = int(value)
		} else {
			return 0, 0, submissionError("invalid_event_query", "limit must be between 1 and 200")
		}
	}
	if afterCursor < 0 {
		return 0, 0, submissionError("invalid_event_query", "after_cursor must not be negative")
	}
	return afterCursor, limit, nil
}

// Durable polling is the authority; dropped notifications do not lose revisions.
func CompetitionV2EventsHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	if _, ok := requireCompetitionV2Principal(w, r, service, competitionSettlementRole); !ok {
		return
	}
	afterCursor, limit, evalError := competitionRewardEventQuery(r)
	if evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	settings, store, status, evalError := service.automationStore()
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	page, err := store.RewardEvents(r.Context(), settings, afterCursor, limit)
	if err == nil {
		for _, event := range page.Events {
			if event.PublicationAt.IsZero() || server.NowUtc().Before(event.PublicationAt) {
				err = ErrAutomationNotPublished
				break
			}
		}
	}
	if err != nil {
		status, evalError = competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeCompetitionJson(w, http.StatusOK, page)
}

// Receipts bind the exact event, revision and scope; the actor cannot be supplied by the caller.
func CompetitionV2ReceiptHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requireCompetitionV2Principal(w, r, service, competitionSettlementRole)
	if !ok {
		return
	}
	var receipt model.CompetitionSettlementReceipt
	if evalError := decodeCompetitionBody(w, r, &receipt, 16*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	settings, store, status, evalError := service.automationStore()
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	if receipt.ActorId != "" && receipt.ActorId != principal.Id || receipt.Environment != principal.Environment || receipt.CompetitionId != principal.CompetitionId || receipt.SeasonId != principal.SeasonId {
		writeCompetitionJson(w, http.StatusForbidden, competitionScopeDenied())
		return
	}
	if evalError := validateCompetitionReceiptRequest(receipt); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	receipt.ActorId = principal.Id
	result, err := store.RecordSettlementReceipt(r.Context(), settings, principal.Id, receipt)
	if err != nil {
		status, evalError = competitionAutomationError(err)
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

// Invalid receipt syntax is a non-retriable client error, not a storage outage.
func validateCompetitionReceiptRequest(receipt model.CompetitionSettlementReceipt) *CompetitionError {
	if receipt.EventId == (server.Id{}) || receipt.RoundId == (server.Id{}) || receipt.Revision < 1 ||
		!sha256Pattern.MatchString(receipt.PayloadSha256) || !sha256Pattern.MatchString(receipt.DecisionSha256) ||
		len(receipt.AccountingReference) > 1024 || len(receipt.ChainTransaction) > 256 {
		return submissionError("invalid_receipt", "receipt identities and accounting references must satisfy the bounded v2 contract")
	}
	switch receipt.Status {
	case "received", "rejected":
		if receipt.AppliedAt != nil || receipt.ChainTransaction != "" {
			return submissionError("invalid_receipt", "receipt-only status cannot assert an accounting application")
		}
	case "applied", "confirmed":
		if receipt.AppliedAt == nil || receipt.AppliedAt.IsZero() || receipt.AccountingReference == "" {
			return submissionError("invalid_receipt", "accounting application requires an explicit reference and effective time")
		}
	default:
		return submissionError("invalid_receipt", "unknown receipt status")
	}
	return nil
}
