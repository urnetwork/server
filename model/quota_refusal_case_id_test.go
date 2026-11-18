package model

// QuotaRefusal's case_id through QuotaRefusalError and the router (QD-235, QD-1014): a screen review's refusal
// carries its case to the body, and no other refusal's body names one. Pure: no database.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

func TestQuotaRefusalErrorCarriesAScreenReviewsCase(t *testing.T) {
	caseId := server.NewId()
	review := &AbuseScreenReviewError{NetworkId: server.NewId(), Subject: AbuseScreenSubjectName, Name: "held-name", CaseId: &caseId}
	quotaError := NewQuotaRefusalError(review.Refusal())
	if quotaError.Refusal.CaseId != caseId.String() || quotaError.HttpStatus() != http.StatusTooManyRequests || quotaError.RetryAfterSeconds() != 0 {
		t.Fatalf("the error: %+v", quotaError)
	}
	body, ok := quotaError.HttpErrorResultBody().(*sidecar.QuotaRefusal)
	if !ok || *body != *review.Refusal() {
		t.Fatalf("the body: %+v", quotaError.HttpErrorResultBody())
	}
	w := httptest.NewRecorder()
	if !router.RaiseHttpError(quotaError, w) {
		t.Fatal("the refusal declares no status")
	}
	want := `{"limit_key":"screen_review","limit":0,"used":0,"retry_after_ms":0,"upgrade":false,"case_id":"` + caseId.String() + `"}` + "\n"
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" || w.Body.String() != want {
		t.Fatalf("status %d, Retry-After %q, body %q, want %q", w.Code, w.Header().Get("Retry-After"), w.Body.String(), want)
	}

	// a review whose case has not opened names none, and neither does any other limit's refusal
	unopened := &AbuseScreenReviewError{NetworkId: server.NewId(), Subject: AbuseScreenSubjectName, Name: "held-name"}
	for _, refusal := range []*sidecar.QuotaRefusal{
		unopened.Refusal(),
		{LimitKey: "evs_drops_per_day", Limit: 20, Used: 20, RetryAfterMs: 1500},
		{LimitKey: "sidecar_clients", Limit: 20, Used: 20, Upgrade: true},
		{LimitKey: "directory_target_pending", Limit: 100, Used: 100},
	} {
		w := httptest.NewRecorder()
		if !router.RaiseHttpError(NewQuotaRefusalError(refusal), w) {
			t.Fatalf("%s declares no status", refusal.LimitKey)
		}
		if w.Code/100 != 4 || !strings.Contains(w.Body.String(), `"limit_key":"`+refusal.LimitKey+`"`) || strings.Contains(w.Body.String(), "case_id") {
			t.Errorf("%s: status %d, body %q", refusal.LimitKey, w.Code, w.Body.String())
		}
	}
}
