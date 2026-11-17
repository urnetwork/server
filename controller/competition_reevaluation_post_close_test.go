// Fixed clocks exercise recovery approval boundaries without waiting or
// changing an actual submission, failure, score or finalization record.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A private approval for one close cannot become indefinite or silently
// authorize recovery before that close or after its own exclusive deadline.
func TestCompetitionReevaluationPostCloseWindowHasExactExclusiveBounds(t *testing.T) {
	closeAt := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	round := &roundRecord{RoundResult: RoundResult{OpensAt: closeAt.Add(-time.Hour), ClosesAt: closeAt}}
	a := model.CompetitionJobReevaluationAuthorization{
		AuthorizedAt: closeAt.Add(time.Minute), ExpiresAt: closeAt.Add(time.Hour),
		PostCloseApproval: &model.CompetitionJobReevaluationPostCloseApproval{
			Schema: 1, ClosesAt: closeAt, ExpiresAt: closeAt.Add(2 * time.Hour), EvidenceSha256: strings.Repeat("a", 64),
		},
	}
	for _, c := range []struct {
		name    string
		at      time.Time
		allowed bool
	}{
		{name: "before-close", at: closeAt.Add(-time.Nanosecond)},
		{name: "before-approval", at: a.AuthorizedAt.Add(-time.Nanosecond)},
		{name: "at-approval", at: a.AuthorizedAt, allowed: true},
		{name: "last-approved-instant", at: a.ExpiresAt.Add(-time.Nanosecond), allowed: true},
		{name: "at-expiry", at: a.ExpiresAt},
		{name: "after-expiry", at: a.ExpiresAt.Add(time.Nanosecond)},
	} {
		if actual := competitionReevaluationWithinApprovedWindow(a, round, c.at); actual != c.allowed {
			t.Fatalf("%s: allowed=%t, want %t", c.name, actual, c.allowed)
		}
	}
	other := *round
	other.ClosesAt = round.ClosesAt.Add(time.Second)
	if competitionReevaluationWithinApprovedWindow(a, &other, a.AuthorizedAt) {
		t.Fatal("approval from another close widened recovery authority")
	}
	if competitionReevaluationWithinApprovedWindow(a, nil, a.AuthorizedAt) {
		t.Fatal("an absent round gained post-close recovery authority")
	}
}

// Old serialized authorizations stay byte-compatible and retain the original
// rule: the close itself is outside an ordinary recovery grant's window.
func TestCompetitionReevaluationPostCloseOmissionRetainsLegacyAdmission(t *testing.T) {
	closeAt := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	round := &roundRecord{RoundResult: RoundResult{OpensAt: closeAt.Add(-time.Hour), ClosesAt: closeAt}}
	a := model.CompetitionJobReevaluationAuthorization{AuthorizedAt: round.OpensAt, ExpiresAt: closeAt.Add(time.Hour)}
	if !competitionReevaluationWithinApprovedWindow(a, round, closeAt.Add(-time.Nanosecond)) ||
		competitionReevaluationWithinApprovedWindow(a, round, closeAt) {
		t.Fatal("omitting approval changed the original open-round recovery window")
	}
	content, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["post_close_approval"]; present {
		t.Fatal("legacy authorization gained a serialized extension")
	}
}

// Even a freshly recomputed outer digest cannot grant malformed or wider
// close-bound authority through the public request validator.
func TestCompetitionReevaluationPostCloseRequestRejectsMalformedApproval(t *testing.T) {
	settings, principal, request := newCompetitionLegacyArchiveRequestFixture(t)
	closeAt := request.Authorization.AuthorizedAt
	request.Authorization.PostCloseApproval = &model.CompetitionJobReevaluationPostCloseApproval{
		Schema: 1, ClosesAt: closeAt, ExpiresAt: request.Authorization.ExpiresAt, EvidenceSha256: strings.Repeat("a", 64),
	}
	for _, c := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationAuthorization)
	}{
		{name: "schema", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.PostCloseApproval.Schema = 2 }},
		{name: "missing-close", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.PostCloseApproval.ClosesAt = time.Time{} }},
		{name: "missing-expiry", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.PostCloseApproval.ExpiresAt = time.Time{} }},
		{name: "no-close-window", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.PostCloseApproval.ExpiresAt = a.PostCloseApproval.ClosesAt
		}},
		{name: "missing-evidence", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.PostCloseApproval.EvidenceSha256 = "" }},
		{name: "before-close", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.AuthorizedAt = a.PostCloseApproval.ClosesAt.Add(-time.Nanosecond)
		}},
		{name: "beyond-approval", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.ExpiresAt = a.PostCloseApproval.ExpiresAt.Add(time.Nanosecond)
		}},
	} {
		changed := request
		approval := *request.Authorization.PostCloseApproval
		changed.Authorization.PostCloseApproval = &approval
		c.mutate(&changed.Authorization)
		var err error
		changed.AuthorizationSha256, err = model.CompetitionAutomationDigest(changed.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		if validateCompetitionReevaluationRequest(settings, principal, changed) == nil {
			t.Fatalf("%s granted malformed post-close authority", c.name)
		}
	}
	var err error
	request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err != nil {
		t.Fatalf("bound synthetic approval refused: %v", err)
	}
}
