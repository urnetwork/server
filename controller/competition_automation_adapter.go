// The reference v2 inbox acknowledges durable receipt only; it performs no money movement.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Every persisted identity and request belongs to exactly one explicitly enabled competition.
type ApexRewardScope struct {
	Staging       bool   `json:"staging,omitempty"`
	Environment   string `json:"environment"`
	CompetitionId string `json:"competition_id"`
	SeasonId      string `json:"season_id"`
}

// Received events and server acknowledgments are separate crash-recoverable records.
type ApexRewardEventRecord struct {
	Event      model.CompetitionRewardEvent        `json:"event"`
	ReceivedAt time.Time                           `json:"received_at"`
	Receipt    *model.CompetitionSettlementReceipt `json:"receipt,omitempty"`
}

// A bounded poll may advance only through the exact events committed to this inbox.
type apexRewardState struct {
	Schema  int                     `json:"schema"`
	Scope   ApexRewardScope         `json:"scope"`
	Cursor  int64                   `json:"cursor"`
	Records []ApexRewardEventRecord `json:"records"`
}

// Safe for concurrent goroutines and cooperating processes; file locks cover one durable transaction.
type ApexRewardFileStore struct {
	directory string
	scope     ApexRewardScope
	stateLock sync.Mutex
}

// Creates a private inbox; it never reads or rewrites legacy adapter history.
func NewApexRewardFileStore(directory string, scope ApexRewardScope) (*ApexRewardFileStore, error) {
	if !tokenNamePattern.MatchString(scope.Environment) || !tokenNamePattern.MatchString(scope.CompetitionId) ||
		!tokenNamePattern.MatchString(scope.SeasonId) || strings.TrimSpace(directory) == "" {
		return nil, errors.New("v2 reward inbox requires an explicit scope and private directory")
	}
	absDirectory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absDirectory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absDirectory)
	if err != nil || !info.IsDir() || info.Mode()&0077 != 0 {
		return nil, errors.New("v2 reward inbox directory must be private and not a symlink")
	}
	store := &ApexRewardFileStore{directory: absDirectory, scope: scope}
	if err := store.withState(false, func(*apexRewardState) error { return nil }); err != nil {
		return nil, err
	}
	return store, nil
}

// Reads, verifies, then atomically replaces one state image. Unique temporary files
// make a crash before rename harmless; committed state is always fsynced first.
func (self *ApexRewardFileStore) withState(write bool, operation func(*apexRewardState) error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lockFile, err := os.OpenFile(filepath.Join(self.directory, "reward-inbox.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if info, err := lockFile.Stat(); err != nil || !info.Mode().IsRegular() || info.Mode()&0077 != 0 {
		return errors.New("reward inbox lock must be a private regular file")
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	state := apexRewardState{Schema: 2, Scope: self.scope, Records: []ApexRewardEventRecord{}}
	statePath := filepath.Join(self.directory, "reward-inbox.json")
	file, err := os.OpenFile(statePath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		info, infoErr := file.Stat()
		if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&0077 != 0 {
			file.Close()
			return errors.New("reward inbox state must be a private regular file")
		}
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&state)
		if decodeErr == nil {
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				decodeErr = errors.New("reward inbox contains trailing json")
			}
		}
		closeErr := file.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := validateApexRewardState(&state, self.scope); err != nil {
		return err
	}
	if err := operation(&state); err != nil || !write {
		return err
	}
	if err := validateApexRewardState(&state, self.scope); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(&state, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(self.directory, ".reward-inbox-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { temporary.Close(); os.Remove(temporaryPath) }()
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, statePath); err != nil {
		return err
	}
	directory, err := os.Open(self.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Validates content identity independently of transport authentication. No digest is
// mistaken for a signature: only the configured authenticated feed can deliver events.
func validateApexRewardEvent(event model.CompetitionRewardEvent, scope ApexRewardScope, now time.Time) error {
	decision := event.Decision
	if event.Cursor <= 0 || event.EventId == (server.Id{}) || event.PublicationAt.IsZero() || now.Before(event.PublicationAt) ||
		decision.Staging != scope.Staging || (decision.Staging && decision.SettlementMode != "shadow") ||
		decision.Environment != scope.Environment || decision.CompetitionId != scope.CompetitionId || decision.SeasonId != scope.SeasonId ||
		decision.RoundId == (server.Id{}) || decision.Revision < 1 || decision.DecidedAt.IsZero() ||
		!sha256Pattern.MatchString(decision.SealSha256) || !sha256Pattern.MatchString(decision.DecisionSha256) ||
		decision.ReasonCode == "" || decision.Action == "" || decision.SettlementPolicyVersion == "" || decision.BurnPolicyVersion == "" ||
		(decision.SettlementMode != "shadow" && decision.SettlementMode != "live") {
		return errors.New("v2 reward event has an invalid scope, publication gate, or decision identity")
	}
	if decision.SelectedJobId != nil && *decision.SelectedJobId == (server.Id{}) {
		return errors.New("v2 reward event has an empty selected job")
	}
	if decision.Action != "elect" && decision.Action != "replace" && decision.Action != "no_winner" && decision.Action != "invalidated" {
		return errors.New("v2 reward event has an unsupported action")
	}
	if decision.Revision == 1 && (decision.Action == "replace" || decision.Action == "invalidated" || decision.ReviewId != nil) ||
		decision.Revision > 1 && decision.Action == "elect" ||
		decision.Action == "invalidated" && (decision.ReviewId != nil || decision.ReasonCode != "EVALUATION_INTEGRITY_INCIDENT") {
		return errors.New("v2 reward action has invalid election or incident provenance")
	}
	if (decision.Action == "no_winner" || decision.Action == "invalidated") != (decision.SelectedJobId == nil) {
		return errors.New("v2 reward action disagrees with its recipient")
	}
	if decision.SelectedJobId == nil && decision.EffectiveSettlementAnchor != nil {
		return errors.New("no-winner decision cannot invent a settlement anchor")
	}
	if decision.SelectedJobId != nil && (decision.EffectiveSettlementAnchor == nil || decision.EffectiveSettlementAnchor.IsZero()) {
		return errors.New("reward decision lacks its authenticated original completion anchor")
	}
	if decision.Revision == 1 && (decision.PreviousDecisionSha256 != "" || decision.SupersededRevision != 0 || decision.SupersededJobId != nil) ||
		decision.Revision > 1 && (!sha256Pattern.MatchString(decision.PreviousDecisionSha256) || decision.SupersededRevision != decision.Revision-1) {
		return errors.New("v2 reward event has an invalid predecessor")
	}
	storedDigest := decision.DecisionSha256
	decision.DecisionSha256 = ""
	actualDigest, err := model.CompetitionAutomationDigest(decision)
	if err != nil || actualDigest != storedDigest {
		return errors.New("v2 reward decision digest mismatch")
	}
	payloadDigest, err := model.CompetitionAutomationDigest(event.Decision)
	if err != nil || payloadDigest != event.PayloadSha256 {
		return errors.New("v2 reward event payload digest mismatch")
	}
	return nil
}

// An invalidated evaluation is terminal, and its economic scope cannot change in a later revision.
func validateApexRewardSuccessor(previous, next model.CompetitionRewardDecision) error {
	if previous.Action == "invalidated" || next.Revision != previous.Revision+1 || next.PreviousDecisionSha256 != previous.DecisionSha256 || next.SealSha256 != previous.SealSha256 ||
		next.Staging != previous.Staging || next.SettlementMode != previous.SettlementMode || next.SettlementPolicyVersion != previous.SettlementPolicyVersion || next.BurnPolicyVersion != previous.BurnPolicyVersion ||
		(next.SupersededJobId == nil) != (previous.SelectedJobId == nil) || next.SupersededJobId != nil && *next.SupersededJobId != *previous.SelectedJobId {
		return errors.New("reward history contains a revision gap, changed predecessor, or a decision after invalidation")
	}
	return nil
}

// A contiguous per-round digest chain prevents stale, skipped, or conflicting replacement effects.
func validateApexRewardState(state *apexRewardState, scope ApexRewardScope) error {
	if state.Schema != 2 || state.Scope != scope || state.Cursor < 0 {
		return errors.New("reward inbox changed schema or immutable scope")
	}
	lastCursor := int64(0)
	seenEventIds := map[server.Id]bool{}
	lastRoundDecisions := map[server.Id]model.CompetitionRewardDecision{}
	for _, record := range state.Records {
		event, decision := record.Event, record.Event.Decision
		if record.ReceivedAt.IsZero() || event.Cursor <= lastCursor || seenEventIds[event.EventId] {
			return errors.New("reward inbox contains duplicate or unordered event identities")
		}
		if err := validateApexRewardEvent(event, scope, record.ReceivedAt); err != nil {
			return err
		}
		previous, exists := lastRoundDecisions[decision.RoundId]
		if !exists && decision.Revision != 1 {
			return errors.New("reward inbox contains a revision gap or conflicting predecessor")
		}
		if exists {
			if err := validateApexRewardSuccessor(previous, decision); err != nil {
				return err
			}
		}
		if record.Receipt != nil {
			if err := validateApexRewardReceipt(event, *record.Receipt); err != nil {
				return err
			}
		}
		lastRoundDecisions[decision.RoundId] = decision
		seenEventIds[event.EventId], lastCursor = true, event.Cursor
	}
	if state.Cursor != lastCursor {
		return errors.New("reward inbox cursor skipped an unpersisted event")
	}
	return nil
}

// Accepts only received acknowledgments here. Applying emissions or confirming funds
// requires a separately approved external integration, absent from this reference adapter.
func validateApexRewardReceipt(event model.CompetitionRewardEvent, receipt model.CompetitionSettlementReceipt) error {
	decision := event.Decision
	if receipt.EventId != event.EventId || receipt.RoundId != decision.RoundId || receipt.Revision != decision.Revision ||
		receipt.Environment != decision.Environment || receipt.CompetitionId != decision.CompetitionId || receipt.SeasonId != decision.SeasonId ||
		receipt.PayloadSha256 != event.PayloadSha256 || receipt.DecisionSha256 != decision.DecisionSha256 || receipt.Status != "received" ||
		receipt.ReceivedAt.IsZero() || receipt.AppliedAt != nil || receipt.ChainTransaction != "" || receipt.ActorId == "" {
		return errors.New("reward receipt does not acknowledge the exact received event")
	}
	return nil
}

// Records a complete bounded page before any acknowledgment can leave the process.
// Replaying the identical page is safe; reusing an event/revision/cursor differently conflicts.
func (self *ApexRewardFileStore) RecordPage(page model.CompetitionRewardEventPage, now time.Time) error {
	if len(page.Events) > 200 {
		return errors.New("reward event page exceeds the bounded contract")
	}
	return self.withState(true, func(state *apexRewardState) error {
		seenEventRecords := map[server.Id]ApexRewardEventRecord{}
		for _, record := range state.Records {
			seenEventRecords[record.Event.EventId] = record
		}
		pageCursor := int64(0)
		for _, event := range page.Events {
			if event.Cursor <= pageCursor {
				return errors.New("reward event page is not strictly ordered")
			}
			pageCursor = event.Cursor
			if err := validateApexRewardEvent(event, self.scope, now); err != nil {
				return err
			}
			if previous, exists := seenEventRecords[event.EventId]; exists {
				previousDigest, _ := model.CompetitionAutomationDigest(previous.Event)
				digest, _ := model.CompetitionAutomationDigest(event)
				if previousDigest != digest {
					return errors.New("reward event id was reused with different content")
				}
				continue
			}
			if event.Cursor <= state.Cursor {
				return errors.New("reward event cursor was reused or delivered out of order")
			}
			record := ApexRewardEventRecord{Event: event, ReceivedAt: now.UTC()}
			state.Records = append(state.Records, record)
			state.Cursor = event.Cursor
			seenEventRecords[event.EventId] = record
		}
		if len(page.Events) == 0 && page.NextCursor != state.Cursor || len(page.Events) != 0 && page.NextCursor != pageCursor {
			return errors.New("reward feed cursor skipped an unpersisted event")
		}
		return nil
	})
}

// Returns copies from the persisted state, including pending receipts after restart.
func (self *ApexRewardFileStore) Snapshot() (int64, []ApexRewardEventRecord, error) {
	var cursor int64
	var records []ApexRewardEventRecord
	err := self.withState(false, func(state *apexRewardState) error { cursor, records = state.Cursor, state.Records; return nil })
	return cursor, records, err
}

// A lost response is replayed with the same request until the exact server receipt is durable.
func (self *ApexRewardFileStore) RecordReceipt(receipt model.CompetitionSettlementReceipt) error {
	return self.withState(true, func(state *apexRewardState) error {
		for i := range state.Records {
			record := &state.Records[i]
			if record.Event.EventId != receipt.EventId {
				continue
			}
			if err := validateApexRewardReceipt(record.Event, receipt); err != nil {
				return err
			}
			if record.Receipt != nil {
				previousDigest, _ := model.CompetitionAutomationDigest(record.Receipt)
				digest, _ := model.CompetitionAutomationDigest(receipt)
				if previousDigest != digest {
					return errors.New("reward receipt id was reused with different content")
				}
				return nil
			}
			record.Receipt = &receipt
			return nil
		}
		return errors.New("cannot acknowledge an event absent from the durable inbox")
	})
}

// Explicit enablement is separate from configuration discovery and never inferred from a response.
type ApexRewardAdapterOptions struct {
	Enabled     bool
	HttpClient  *http.Client
	MaxAttempts int
	Wait        func(context.Context, time.Duration) error
	Now         func() time.Time
	PageLimit   int
}

// Does not start a goroutine. The owner invokes bounded reconciliation with a cancellable context.
type ApexRewardAdapter struct {
	store     *ApexRewardFileStore
	transport *ApexAdapter
	pageLimit int
}

// Settlement credentials are sent only to one authenticated origin; redirects cannot forward them.
func NewApexRewardAdapter(baseUrl string, settlementToken string, store *ApexRewardFileStore, options ApexRewardAdapterOptions) (*ApexRewardAdapter, error) {
	parsed, err := url.Parse(baseUrl)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && (parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost")) {
		return nil, errors.New("reward adapter requires an https origin except loopback conformance tests")
	}
	if !options.Enabled || store == nil || strings.TrimSpace(settlementToken) == "" {
		return nil, errors.New("reward adapter requires explicit enablement, scope, durable inbox, and settlement credential")
	}
	if options.HttpClient == nil {
		options.HttpClient = server.NewHttpClient(30 * time.Second)
	}
	client := *options.HttpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 5
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 20 {
		return nil, errors.New("reward adapter max attempts must be between 1 and 20")
	}
	if options.PageLimit == 0 {
		options.PageLimit = 100
	}
	if options.PageLimit < 1 || options.PageLimit > 200 {
		return nil, errors.New("reward adapter page limit must be between 1 and 200")
	}
	if options.Now == nil {
		options.Now = server.NowUtc
	}
	if options.Wait == nil {
		options.Wait = func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}
	}
	transport := &ApexAdapter{baseUrl: parsed, submitterJwt: settlementToken, httpClient: &client, maxAttempts: options.MaxAttempts, wait: options.Wait, now: options.Now}
	return &ApexRewardAdapter{store: store, transport: transport, pageLimit: options.PageLimit}, nil
}

// One bounded pass retries durable receipts, fetches every missed event after the
// stored cursor, commits them atomically, then acknowledges receipt. No payment API is called.
func (self *ApexRewardAdapter) Reconcile(ctx context.Context) (int, error) {
	if err := self.acknowledgePending(ctx); err != nil {
		return 0, err
	}
	cursor, _, err := self.store.Snapshot()
	if err != nil {
		return 0, err
	}
	var page model.CompetitionRewardEventPage
	endpoint := "/competition/v2/events?after_cursor=" + strconv.FormatInt(cursor, 10) + "&limit=" + strconv.Itoa(self.pageLimit)
	if err := self.transport.requestWithRetry(ctx, http.MethodGet, endpoint, nil, true, &page); err != nil {
		return 0, err
	}
	if len(page.Events) > self.pageLimit || len(page.Events) > 200 {
		return 0, errors.New("reward feed exceeded the requested bounded page")
	}
	verifiedRoundResults := map[server.Id]model.CompetitionV2RoundResult{}
	for _, event := range page.Events {
		if err := validateApexRewardEvent(event, self.store.scope, self.transport.now()); err != nil {
			return 0, err
		}
		result, exists := verifiedRoundResults[event.Decision.RoundId]
		if !exists {
			result, err = self.roundResult(ctx, event.Decision.RoundId)
			if err != nil {
				return 0, err
			}
			if err := ValidateApexV2RoundResult(result, self.store.scope, self.transport.now()); err != nil {
				return 0, err
			}
			verifiedRoundResults[event.Decision.RoundId] = result
		}
		if event.Decision.Revision < 1 || event.Decision.Revision > len(result.Decisions) || result.Decisions[event.Decision.Revision-1].DecisionSha256 != event.Decision.DecisionSha256 {
			return 0, errors.New("reward feed event disagrees with the authoritative sealed decision history")
		}
	}
	if err := self.store.RecordPage(page, self.transport.now()); err != nil {
		return 0, err
	}
	if err := self.acknowledgePending(ctx); err != nil {
		return len(page.Events), err
	}
	return len(page.Events), nil
}

// Complete round evidence grows with unlimited admissions. Decode directly from
// the authenticated origin under a deadline; only error/control messages retain the small-message cap.
func (self *ApexRewardAdapter) roundResult(ctx context.Context, roundId server.Id) (model.CompetitionV2RoundResult, error) {
	requestUrl, err := self.transport.resolvePath("/competition/v2/rounds/" + roundId.String() + "/result")
	if err != nil {
		return model.CompetitionV2RoundResult{}, err
	}
	for attempt := 1; attempt <= self.transport.maxAttempts; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestUrl.String(), nil)
		if err != nil {
			cancel()
			return model.CompetitionV2RoundResult{}, err
		}
		request.Header.Set("Authorization", "Bearer "+self.transport.submitterJwt)
		request.Header.Set("Accept", "application/json")
		response, requestErr := self.transport.httpClient.Do(request)
		if requestErr != nil {
			cancel()
			if attempt == self.transport.maxAttempts {
				return model.CompetitionV2RoundResult{}, requestErr
			}
			if err := self.transport.wait(ctx, apexRetryDelay(nil, attempt)); err != nil {
				return model.CompetitionV2RoundResult{}, err
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			var result model.CompetitionV2RoundResult
			decoder := json.NewDecoder(response.Body)
			decoder.DisallowUnknownFields()
			decodeErr := decoder.Decode(&result)
			if decodeErr == nil {
				if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
					decodeErr = errors.New("competition round response contains trailing JSON")
				}
			}
			closeErr := response.Body.Close()
			cancel()
			if decodeErr != nil {
				return model.CompetitionV2RoundResult{}, fmt.Errorf("decode competition round response: %w", decodeErr)
			}
			if closeErr != nil {
				return model.CompetitionV2RoundResult{}, closeErr
			}
			return result, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, apexResponseLimit+1))
		closeErr := response.Body.Close()
		cancel()
		if readErr != nil {
			return model.CompetitionV2RoundResult{}, readErr
		}
		if closeErr != nil {
			return model.CompetitionV2RoundResult{}, closeErr
		}
		if len(body) > apexResponseLimit {
			return model.CompetitionV2RoundResult{}, errors.New("competition round error response exceeded the control-message limit")
		}
		var apiError CompetitionError
		_ = json.Unmarshal(body, &apiError)
		retriable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 && response.StatusCode <= 599
		if !retriable || attempt == self.transport.maxAttempts {
			if apiError.Code != "" {
				return model.CompetitionV2RoundResult{}, &apiError
			}
			return model.CompetitionV2RoundResult{}, fmt.Errorf("competition round returned HTTP %d", response.StatusCode)
		}
		if err := self.transport.wait(ctx, apexRetryDelay(response, attempt)); err != nil {
			return model.CompetitionV2RoundResult{}, err
		}
	}
	return model.CompetitionV2RoundResult{}, errors.New("competition round retry loop exhausted")
}

// External delivery is outside file locks; only acknowledgment persistence reacquires the inbox.
func (self *ApexRewardAdapter) acknowledgePending(ctx context.Context) error {
	_, records, err := self.store.Snapshot()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Receipt != nil {
			continue
		}
		event, decision := record.Event, record.Event.Decision
		request := model.CompetitionSettlementReceipt{
			EventId: event.EventId, Environment: decision.Environment, CompetitionId: decision.CompetitionId,
			SeasonId: decision.SeasonId, RoundId: decision.RoundId, Revision: decision.Revision,
			PayloadSha256: event.PayloadSha256, DecisionSha256: decision.DecisionSha256, Status: "received",
			AccountingReference: "reference-inbox/" + event.EventId.String(), ReceivedAt: record.ReceivedAt,
		}
		var receipt model.CompetitionSettlementReceipt
		if err := self.transport.requestWithRetry(ctx, http.MethodPost, "/competition/v2/receipts", request, true, &receipt); err != nil {
			return err
		}
		if receipt.AccountingReference != request.AccountingReference {
			return errors.New("reward receipt changed its acknowledgment reference")
		}
		if err := self.store.RecordReceipt(receipt); err != nil {
			return err
		}
	}
	return nil
}

// Public findings retain bounded criterion outcomes, never the private reproduction details.
func validateApexPublicReview(review model.CompetitionIntegrityReviewResult) error {
	request := review.Request
	if request.CriteriaVersion == "" || strings.TrimSpace(request.PublicExplanation) == "" || strings.TrimSpace(request.WithheldEvidenceExplanation) == "" ||
		len(request.PublicExplanation) > 4096 || len(request.ReasonCodes) < 1 || len(request.ReasonCodes) > 16 ||
		request.RewardDisposition != "approved" && request.RewardDisposition != "disqualified" && request.RewardDisposition != "restored" && request.RewardDisposition != "unchanged" ||
		request.SourceDisposition != "approved" && request.SourceDisposition != "rejected" && request.SourceDisposition != "deferred" && request.SourceDisposition != "unchanged" ||
		request.SourceDisposition == "approved" && request.RewardDisposition != "approved" && request.RewardDisposition != "restored" {
		return errors.New("v2 public review lacks its bounded criteria or disposition")
	}
	criteriaKVs := map[string]bool{"measurement_integrity": false, "workload_generalization": false, "containment": false, "patch_scope": false, "reproducibility": false, "source_safety": false}
	for _, finding := range request.Findings {
		seen, exists := criteriaKVs[finding.Criterion]
		if !exists || seen || finding.ReasonCode == "" ||
			finding.Disposition != "pass" && finding.Disposition != "fail" && finding.Disposition != "not_applicable" ||
			request.SourceDisposition == "approved" && finding.Disposition == "fail" {
			return errors.New("v2 public review has invalid or contradictory criterion findings")
		}
		criteriaKVs[finding.Criterion] = true
	}
	for _, seen := range criteriaKVs {
		if !seen {
			return errors.New("v2 public review is missing a required criterion")
		}
	}
	return nil
}

// A plain approval cannot undo a disqualification; only an exact append-only appeal can.
func applyApexRewardReview(review model.CompetitionIntegrityReviewResult, disqualifiedJobIds map[server.Id]bool, latestReviewIds map[server.Id]server.Id) error {
	jobId := review.Request.JobId
	switch review.Request.RewardDisposition {
	case "disqualified":
		disqualifiedJobIds[jobId] = true
	case "approved":
		if disqualifiedJobIds[jobId] {
			return errors.New("v2 approval attempted to erase a disqualification without an appeal")
		}
	case "restored":
		if !disqualifiedJobIds[jobId] || review.Request.SupersedesReviewId == nil || *review.Request.SupersedesReviewId != latestReviewIds[jobId] || review.Request.AppealId == "" {
			return errors.New("v2 restoration lacks the exact superseded disqualification")
		}
		delete(disqualifiedJobIds, jobId)
	}
	if review.Request.RewardDisposition != "unchanged" {
		latestReviewIds[jobId] = review.ReviewId
	}
	return nil
}

// Verifies the immutable ranking using the same production eligibility specification.
// Integrity findings can change the current reward revision, never the sealed measurements.
func ValidateApexV2RoundResult(result model.CompetitionV2RoundResult, scope ApexRewardScope, now time.Time) error {
	if result.DecisionMode != model.CompetitionDecisionModeV2 || result.RequiredApiCapability != model.CompetitionAutomationCapability ||
		result.Staging != scope.Staging || result.Seal.Staging != scope.Staging ||
		result.Seal.Environment != scope.Environment || result.Seal.CompetitionId != scope.CompetitionId || result.Seal.SeasonId != scope.SeasonId ||
		result.Seal.PublicationAt.IsZero() || now.Before(result.Seal.PublicationAt) || len(result.Decisions) == 0 || result.RewardRevision != len(result.Decisions) {
		return errors.New("invalid or unpublished v2 round result")
	}
	seal := result.Seal
	sealDigest := seal.SealSha256
	seal.SealSha256 = ""
	actualDigest, err := model.CompetitionAutomationDigest(seal)
	if err != nil || actualDigest != sealDigest {
		return errors.New("v2 evaluation seal digest mismatch")
	}
	baselineRaw, margin := seal.BaselineRawScore, seal.TakeoverMargin
	baselineMean := float64(0)
	excludedPatchKVs := map[string]bool{}
	for _, entry := range seal.Entries {
		if !sha256Pattern.MatchString(entry.PatchSha256) || !sha256Pattern.MatchString(entry.BaseSourceLockSha256) || entry.State != "succeeded" && entry.Score != nil {
			return errors.New("v2 seal contains invalid terminal evidence")
		}
		if entry.Score != nil {
			if err := model.ValidateCompetitionScore(entry.Score); err != nil {
				return err
			}
			if baselineMean == 0 {
				baselineMean = entry.Score.Significance.BaselineMeanRawScore
			}
			if entry.Score.Significance.BaselineMeanRawScore != baselineMean || math.Abs(entry.Score.Significance.TakeoverMarginPercent-margin*100) > 1e-9 {
				return errors.New("v2 seal mixed statistical controls")
			}
			// ScoreSha256 identifies the exact archived score.json bytes, not a re-encoding of this projection.
			if !sha256Pattern.MatchString(entry.ScoreSha256) {
				return errors.New("v2 sealed score lacks its archived artifact identity")
			}
		}
		if entry.IneligibilityReason == "reward_duplicate" {
			excludedPatchKVs[entry.PatchSha256] = true
		}
	}
	ranked, err := model.RankCompetitionSealedEntriesForMode(seal.Entries, baselineRaw, margin, excludedPatchKVs, seal.Staging)
	if err != nil {
		return err
	}
	rankedBytes, _ := json.Marshal(ranked)
	storedBytes, _ := json.Marshal(seal.Entries)
	if !bytes.Equal(rankedBytes, storedBytes) {
		return errors.New("v2 seal disagrees with deterministic ranking and eligibility")
	}
	rankingDigest, _ := model.CompetitionAutomationDigest(seal.Entries)
	if rankingDigest != seal.RankingSha256 {
		return errors.New("v2 ranking manifest digest mismatch")
	}
	inventory := append([]model.CompetitionSealedEntry{}, seal.Entries...)
	for i := range inventory {
		inventory[i].Rank, inventory[i].EligibleCandidateRank, inventory[i].Eligible, inventory[i].IneligibilityReason = 0, 0, false, ""
	}
	sort.Slice(inventory, func(i, j int) bool {
		if inventory[i].SubmittedAt.Equal(inventory[j].SubmittedAt) {
			return inventory[i].JobId.String() < inventory[j].JobId.String()
		}
		return inventory[i].SubmittedAt.Before(inventory[j].SubmittedAt)
	})
	inventoryDigest, _ := model.CompetitionAutomationDigest(inventory)
	if inventoryDigest != seal.InventorySha256 {
		return errors.New("v2 terminal inventory digest mismatch")
	}
	previous := ""
	disqualifiedJobIds := map[server.Id]bool{}
	latestReviewIds := map[server.Id]server.Id{}
	seenReviewIds := map[server.Id]bool{}
	for _, review := range result.IntegrityReviews {
		if err := validateApexPublicReview(review); err != nil {
			return err
		}
		if review.RoundId != seal.RoundId || review.ReviewId == (server.Id{}) || seenReviewIds[review.ReviewId] || review.CreatedAt.IsZero() ||
			review.Request.ExpectedRevision < 1 || review.Request.ExpectedRevision > len(result.Decisions) || review.RewardDecision.Revision < 1 || review.RewardDecision.Revision > len(result.Decisions) ||
			review.RewardDecision.DecisionSha256 != result.Decisions[review.RewardDecision.Revision-1].DecisionSha256 {
			return errors.New("v2 review has a conflicting round or decision identity")
		}
		if review.RewardDecision.Revision != review.Request.ExpectedRevision && review.RewardDecision.Revision != review.Request.ExpectedRevision+1 ||
			result.Decisions[review.Request.ExpectedRevision-1].Action == "invalidated" ||
			review.RewardDecision.Revision == review.Request.ExpectedRevision+1 && (review.RewardDecision.ReviewId == nil || *review.RewardDecision.ReviewId != review.ReviewId) {
			return errors.New("v2 review does not match its expected reward transition")
		}
		seenReviewIds[review.ReviewId] = true
		matched := false
		for _, entry := range seal.Entries {
			if entry.JobId == review.Request.JobId && entry.Score != nil && entry.PatchSha256 == review.Request.PatchSha256 &&
				entry.BaseSourceLockSha256 == review.Request.BaseSourceLockSha256 && entry.ScoreSha256 == review.Request.ScoreSha256 &&
				entry.ArtifactManifestSha256 == review.Request.ArtifactManifestSha256 && seal.BaselineSha256 == review.Request.BaselineSha256 {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("v2 review does not identify the exact sealed candidate evidence")
		}
	}
	invalidated := false
	for _, incident := range result.Incidents {
		if incident.RoundId != seal.RoundId || incident.Environment != scope.Environment || incident.CompetitionId != scope.CompetitionId || incident.SeasonId != scope.SeasonId ||
			incident.IncidentId == (server.Id{}) || incident.CreatedAt.IsZero() || !sha256Pattern.MatchString(incident.RequestSha256) {
			return errors.New("v2 incident has an invalid scope or identity")
		}
		invalidated = invalidated || incident.Request.InvalidatesEvaluation
	}
	if result.Invalidated != invalidated {
		return errors.New("v2 invalidation projection disagrees with incident history")
	}
	reviewIndex := 0
	for i, decision := range result.Decisions {
		if decision.Revision != i+1 || decision.RoundId != seal.RoundId || decision.SealSha256 != sealDigest || decision.PreviousDecisionSha256 != previous {
			return errors.New("v2 decision history has a gap or conflicting identity")
		}
		payloadDigest, _ := model.CompetitionAutomationDigest(decision)
		event := model.CompetitionRewardEvent{Cursor: int64(i + 1), EventId: seal.RoundId, PayloadSha256: payloadDigest, PublicationAt: seal.PublicationAt, Decision: decision}
		if err := validateApexRewardEvent(event, scope, now); err != nil {
			return err
		}
		if i > 0 {
			if err := validateApexRewardSuccessor(result.Decisions[i-1], decision); err != nil {
				return err
			}
		}
		if decision.Action == "invalidated" {
			matched := false
			for _, incident := range result.Incidents {
				if incident.Request.InvalidatesEvaluation && incident.Request.ExpectedRewardRevision == decision.Revision-1 &&
					incident.Request.ReasonCode == "EVALUATION_INTEGRITY_INCIDENT" && incident.CreatedAt.Equal(decision.DecidedAt) {
					matched = true
					break
				}
			}
			if !matched {
				return errors.New("v2 invalidation lacks its exact incident provenance")
			}
		}
		if i > 0 && decision.Action != "invalidated" {
			if decision.ReviewId == nil {
				return errors.New("replacement revision has no authenticated review provenance")
			}
			found := false
			for reviewIndex < len(result.IntegrityReviews) {
				review := result.IntegrityReviews[reviewIndex]
				reviewIndex++
				if err := applyApexRewardReview(review, disqualifiedJobIds, latestReviewIds); err != nil {
					return err
				}
				if review.ReviewId == *decision.ReviewId {
					found = true
					break
				}
			}
			if !found {
				return errors.New("replacement review is missing or out of order")
			}
		}
		if decision.Action != "invalidated" {
			selected, _ := model.SelectCompetitionReward(seal.Entries, disqualifiedJobIds)
			if (selected == nil) != (decision.SelectedJobId == nil) || selected != nil && selected.JobId != *decision.SelectedJobId {
				return errors.New("reward decision disagrees with the first eligible sealed candidate")
			}
		}
		if decision.SelectedJobId != nil {
			found := false
			for _, entry := range seal.Entries {
				if entry.JobId == *decision.SelectedJobId && entry.Eligible && decision.EffectiveSettlementAnchor != nil && entry.CompletedAt.Equal(*decision.EffectiveSettlementAnchor) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("revision %d names an ineligible or differently timed sealed candidate", decision.Revision)
			}
		}
		previous = decision.DecisionSha256
	}
	current := result.Decisions[len(result.Decisions)-1].SelectedJobId
	if (current == nil) != (result.ProvisionalRewardWinner == nil) || current != nil && *current != *result.ProvisionalRewardWinner {
		return errors.New("v2 current winner disagrees with append-only history")
	}
	if result.Invalidated != (result.Decisions[len(result.Decisions)-1].Action == "invalidated") {
		return errors.New("v2 invalidation is not the terminal reward decision")
	}
	for ; reviewIndex < len(result.IntegrityReviews); reviewIndex++ {
		review := result.IntegrityReviews[reviewIndex]
		if err := applyApexRewardReview(review, disqualifiedJobIds, latestReviewIds); err != nil {
			return err
		}
	}
	if !result.Invalidated {
		selected, _ := model.SelectCompetitionReward(seal.Entries, disqualifiedJobIds)
		if (selected == nil) != (current == nil) || selected != nil && selected.JobId != *current {
			return errors.New("v2 final review state requires an absent reward revision")
		}
	}
	integrity, source := competitionV2CurrentReviewState(current, result.IntegrityReviews)
	if result.IntegrityReview != integrity || result.SourceApproval != source {
		return errors.New("v2 approval projection disagrees with genuine current-candidate reviews")
	}
	return nil
}
