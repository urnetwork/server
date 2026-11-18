//go:build sidecar_pending

package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// A screen review's refusal over HTTP (QD-235, QD-1014): a record write and an alias claim held for review answer
// 429 with QuotaRefusal{limit_key: screen_review, case_id}, the id of the case the refusal opened or joined, which
// is the id GET /abuse/cases lists once a reviewer has acted on the case; every other refusal of the same routes
// names no case. On the DNS tests' worlds, which the sidecar_pending build holds.

// The router's answer to a route's error (router.RaiseHttpError), as api's handlers write it.
func testScreenHttpAnswer(t testing.TB, err error) *httptest.ResponseRecorder {
	t.Helper()
	if err == nil {
		t.Fatal("no refusal")
	}
	w := httptest.NewRecorder()
	if !router.RaiseHttpError(err, w) {
		t.Fatalf("%v declares no status", err)
	}
	return w
}

// The QuotaRefusal an answer's body is, which holds no field the type does not.
func testScreenHttpRefusal(t testing.TB, w *httptest.ResponseRecorder) *sidecar.QuotaRefusal {
	t.Helper()
	refusal := &sidecar.QuotaRefusal{}
	decoder := json.NewDecoder(strings.NewReader(w.Body.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(refusal); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return refusal
}

// The case a held name's answer names: 429 with no window to wait, the screen's limit key and a case id in its
// canonical uuid form.
func testScreenHttpReviewCase(t testing.TB, w *httptest.ResponseRecorder) server.Id {
	t.Helper()
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" {
		t.Fatalf("status %d, Retry-After %q, body %q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	refusal := testScreenHttpRefusal(t, w)
	if refusal.LimitKey != AbuseScreenReviewLimitKey || refusal.Limit != 0 || refusal.Used != 0 || refusal.RetryAfterMs != 0 || refusal.Upgrade {
		t.Fatalf("the refusal: %+v", refusal)
	}
	caseId, err := server.ParseId(refusal.CaseId)
	if err != nil || caseId == (server.Id{}) || caseId.String() != refusal.CaseId {
		t.Fatalf("case_id %q: %v", refusal.CaseId, err)
	}
	if !strings.Contains(w.Body.String(), `"case_id":"`+caseId.String()+`"`) {
		t.Fatalf("body %q", w.Body.String())
	}
	return caseId
}

// The cases GET /abuse/cases lists the network (ListNetworkAbuseCases).
func testScreenListedCases(ctx context.Context, networkId server.Id) []*AbuseNetworkCase {
	return ListNetworkAbuseCases(ctx, networkId, DefaultAbuseCustomerSettings()).Cases
}

// A record owner's host held for review: the answer names the case it opened, a retry the same case, and once a
// reviewer allows the name the network's case list shows that id. A limit's refusal of the same write names none.
func TestDnsRecordsScreenReviewAnswersItsCase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		owner := "paypal-login." + world.apex

		_, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`))
		caseId := testScreenHttpReviewCase(t, testScreenHttpAnswer(t, err))
		cases := testScreenCases(t, ctx, world.networkId)["paypal-login"]
		if len(cases) != 1 || cases[0].CaseId != caseId {
			t.Fatalf("the answer names %s, the cases: %+v", caseId, cases)
		}
		// a retry joins the case and names it again
		_, err = world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`))
		if retryCaseId := testScreenHttpReviewCase(t, testScreenHttpAnswer(t, err)); retryCaseId != caseId {
			t.Fatalf("the retry names %s, want %s", retryCaseId, caseId)
		}
		// another held host of the network is another case
		_, err = world.write(ctx, testDnsUpsert("paypal-support."+world.apex, "TXT", `"x"`))
		if otherCaseId := testScreenHttpReviewCase(t, testScreenHttpAnswer(t, err)); otherCaseId == caseId {
			t.Fatalf("another name answered the first name's case %s", otherCaseId)
		}

		// the reviewer acts, and GET /abuse/cases lists the case under the id the refusal named
		testScreenAllow(t, ctx, caseId, "paypal-login")
		listed := testScreenListedCases(ctx, world.networkId)
		if len(listed) != 1 || listed[0].CaseId != caseId || listed[0].Target != "paypal-login" ||
			len(listed[0].Actions) != 1 || listed[0].Actions[0].Kind != AbuseActionKindScreenAllow {
			t.Fatalf("the listed cases: %+v", listed)
		}
		body, err := json.Marshal(listed[0])
		if err != nil || !strings.Contains(string(body), `"case_id":"`+caseId.String()+`"`) {
			t.Fatalf("the listed case %s: %v", body, err)
		}
		if _, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`)); err != nil {
			t.Fatalf("the allowed host: %v", err)
		}

		// a limit's refusal of a write is a QuotaRefusal too, and names no case
		world.limits.set(DnsEntriesLimitKey, 1)
		_, err = world.write(ctx, testDnsUpsert("shop."+world.apex, "TXT", `"x"`))
		w := testScreenHttpAnswer(t, err)
		if w.Code != http.StatusPaymentRequired {
			t.Fatalf("status %d, body %q", w.Code, w.Body.String())
		}
		if refusal := testScreenHttpRefusal(t, w); refusal.LimitKey != DnsEntriesLimitKey || refusal.CaseId != "" || strings.Contains(w.Body.String(), "case_id") {
			t.Fatalf("a limit's refusal: %q", w.Body.String())
		}
	})
}

// An alias claim held for review: the same answer, its case a label's, and the claim landing once a reviewer
// allows the name. The verification gate's refusal of a claim names no case.
func TestDnsAliasScreenReviewAnswersItsCase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, _ := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		// a brand and a keyword written together pass §12.5's name rule, whose brand check reads whole tokens,
		// and go to §21.6's review
		name := "paypalsupport"
		if err := sidecar.ValidateDnsName(name); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, world.networkId, name))
		})

		serial := world.serial(t, ctx)
		_, err := world.claimAlias(ctx, name)
		caseId := testScreenHttpReviewCase(t, testScreenHttpAnswer(t, err))
		cases := testScreenCases(t, ctx, world.networkId)[name]
		if len(cases) != 1 || cases[0].CaseId != caseId || cases[0].TargetKind != AbuseTargetKindLabel || cases[0].Category != AbuseCategoryBrand {
			t.Fatalf("the answer names %s, the cases: %+v", caseId, cases)
		}
		_, err = world.claimAlias(ctx, name)
		if retryCaseId := testScreenHttpReviewCase(t, testScreenHttpAnswer(t, err)); retryCaseId != caseId {
			t.Fatalf("the retry names %s, want %s", retryCaseId, caseId)
		}
		if world.serial(t, ctx) != serial {
			t.Fatal("a held alias wrote")
		}

		testScreenAllow(t, ctx, caseId, name)
		listed := testScreenListedCases(ctx, world.networkId)
		if len(listed) != 1 || listed[0].CaseId != caseId || listed[0].Target != name {
			t.Fatalf("the listed cases: %+v", listed)
		}
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatalf("the allowed alias: %v", err)
		}

		// an unverified network's claim is refused by the verification gate, a QuotaRefusal that names no case
		unverified, unverifiedName := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, false)
		_, err = unverified.claimAlias(ctx, unverifiedName)
		w := testScreenHttpAnswer(t, err)
		if w.Code != http.StatusPaymentRequired {
			t.Fatalf("status %d, body %q", w.Code, w.Body.String())
		}
		if refusal := testScreenHttpRefusal(t, w); refusal.LimitKey != DnsVerificationLimitKey || refusal.CaseId != "" || strings.Contains(w.Body.String(), "case_id") {
			t.Fatalf("the verification gate's refusal: %q", w.Body.String())
		}
	})
}
