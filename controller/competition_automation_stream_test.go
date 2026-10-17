// Full-round transport tests use an in-memory authenticated origin, not live network services.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// Captures origin authentication, deadlines, and response closure without opening a socket.
type apexRoundTestTransport struct {
	body        string
	status      int
	authorized  bool
	hasDeadline bool
	closed      bool
	requests    int
}

// Each retry receives its own immutable response stream.
func (self *apexRoundTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	self.requests++
	self.authorized = request.Header.Get("Authorization") == "Bearer synthetic-settlement-token"
	_, self.hasDeadline = request.Context().Deadline()
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	self.closed = false
	return &http.Response{StatusCode: self.status, Header: http.Header{}, Request: request, Body: &apexRoundTestBody{Reader: strings.NewReader(self.body), closed: &self.closed}}, nil
}

// Closing the response must release transport resources on every decode path.
type apexRoundTestBody struct {
	io.Reader
	closed *bool
}

// Records closure deterministically rather than relying on connection reuse timing.
func (self *apexRoundTestBody) Close() error {
	*self.closed = true
	return nil
}

// Large authenticated score evidence must not inherit the small control-message limit.
func TestApexV2StreamsCompleteRoundBeyondControlMessageLimit(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	entry := result.Seal.Entries[0]
	entry.Score.Gates["G1_success"].Details["synthetic_evidence"] = strings.Repeat("x", apexResponseLimit+1024)
	archivedScore, err := json.Marshal(entry.Score)
	if err != nil {
		t.Fatal(err)
	}
	archivedDigest := sha256.Sum256(archivedScore)
	entry.ScoreSha256 = hex.EncodeToString(archivedDigest[:])
	entry.Rank, entry.EligibleCandidateRank, entry.Eligible, entry.IneligibilityReason = 0, 0, false, ""
	result.Seal.Entries = []model.CompetitionSealedEntry{entry}
	result.Seal.InventorySha256, _ = model.CompetitionAutomationDigest(result.Seal.Entries)
	result.Seal.Entries, err = model.RankCompetitionSealedEntries(result.Seal.Entries, result.Seal.BaselineRawScore, result.Seal.TakeoverMargin, nil)
	if err != nil {
		t.Fatal(err)
	}
	result.Seal.RankingSha256, _ = model.CompetitionAutomationDigest(result.Seal.Entries)
	result.Seal.SealSha256 = ""
	result.Seal.SealSha256, _ = model.CompetitionAutomationDigest(result.Seal)
	decision := first.Decision
	decision.SealSha256, decision.DecisionSha256 = result.Seal.SealSha256, ""
	decision.DecisionSha256, _ = model.CompetitionAutomationDigest(decision)
	result.Decisions, result.RewardRevision, result.ProvisionalRewardWinner = []model.CompetitionRewardDecision{decision}, 1, decision.SelectedJobId
	result.IntegrityReviews, result.IntegrityReview, result.SourceApproval = nil, "not_reviewed", "pending"
	body, err := json.Marshal(result)
	if err != nil || len(body) <= apexResponseLimit {
		t.Fatalf("large round fixture failed: bytes=%d error=%v", len(body), err)
	}
	transport := &apexRoundTestTransport{body: string(body), status: http.StatusOK}
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewApexRewardAdapter("https://competition.test", "synthetic-settlement-token", store, ApexRewardAdapterOptions{Enabled: true, HttpClient: &http.Client{Transport: transport}, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := adapter.roundResult(context.Background(), result.Seal.RoundId)
	if err != nil {
		t.Fatalf("unlimited-round result hit the control-message cap: %v", err)
	}
	if !transport.authorized || !transport.hasDeadline || !transport.closed || transport.requests != 1 {
		t.Fatalf("unsafe streamed response lifecycle: authenticated=%t deadline=%t closed=%t requests=%d", transport.authorized, transport.hasDeadline, transport.closed, transport.requests)
	}
	if err := ValidateApexV2RoundResult(actual, scope, now); err != nil {
		t.Fatalf("large transported round lost authenticated evidence: %v", err)
	}
}

// Removing the small-message byte cap does not accept partial JSON, extra values, or wrong HTTP status.
func TestApexV2RoundStreamRequiresExactCompleteSuccess(t *testing.T) {
	scope, _, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	transport := &apexRoundTestTransport{}
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewApexRewardAdapter("https://competition.test", "synthetic-settlement-token", store, ApexRewardAdapterOptions{Enabled: true, HttpClient: &http.Client{Transport: transport}, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		body   string
		status int
	}{
		{body: string(body) + "\n{}", status: http.StatusOK},
		{body: string(body[:len(body)-1]), status: http.StatusOK},
		{body: string(body[:len(body)-1]) + `,"unknown_field":true}`, status: http.StatusOK},
		{body: string(body), status: http.StatusAccepted},
		{body: `{"code":"scope_denied"}`, status: http.StatusForbidden},
	}
	for index, fixture := range fixtures {
		transport.body, transport.status = fixture.body, fixture.status
		if _, err := adapter.roundResult(context.Background(), result.Seal.RoundId); err == nil || !transport.closed {
			t.Fatalf("malformed stream %d escaped or leaked its body: %v", index, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.roundResult(ctx, result.Seal.RoundId); err == nil {
		t.Fatal("canceled round fetch continued")
	}
}

// An oversized page is refused before any unbounded round fetch is started.
func TestApexV2RejectsOversizedEventPageBeforeFetchingRounds(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	page := model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{first, second}, NextCursor: second.Cursor}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	transport := &apexRoundTestTransport{body: string(body), status: http.StatusOK}
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewApexRewardAdapter("https://competition.test", "synthetic-settlement-token", store, ApexRewardAdapterOptions{Enabled: true, HttpClient: &http.Client{Transport: transport}, MaxAttempts: 1, PageLimit: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Reconcile(context.Background()); err == nil || transport.requests != 1 {
		t.Fatalf("oversized feed triggered round work: requests=%d error=%v", transport.requests, err)
	}
}
