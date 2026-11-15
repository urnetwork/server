package handlers

// POST /abuse/action over the router (abuse_action_handlers.go; ABUSE-M5-12, QD-126): with no staff file every
// caller is refused; the credential's table answers one 403 for every refusal; a removed credential stops at the
// next read of the file; a lookup through the route answers its case and logs its read, which runs on MG5's tables
// in a build with sidecar_pending.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A clock a test moves.
type testAbuseRouteClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testAbuseRouteClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testAbuseRouteClock) Advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// What a route answered.
type testAbuseRouteAnswer struct {
	status int
	body   string
	header http.Header
}

// One call of the staff route on handlers with authorization.
func testAbuseActionCall(t testing.TB, handlers *AbuseActionHandlers, authorization string, body string) *testAbuseRouteAnswer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/abuse/action", handlers.Action),
	})
	request := httptest.NewRequest("POST", "/abuse/action", strings.NewReader(body))
	request.RemoteAddr = "192.0.2.60:41000"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	return &testAbuseRouteAnswer{status: w.Code, body: w.Body.String(), header: w.Header()}
}

// A verify file holding each bearer with its reviewer, roles and expiry.
func testAbuseActionStaffFile(t testing.TB, entries map[string]string) *controller.AbuseStaffVerifier {
	t.Helper()
	data := "staff:\n"
	for bearer, entry := range entries {
		digest := sha256.Sum256([]byte(bearer))
		data += fmt.Sprintf("  - sha256: %s\n%s", hex.EncodeToString(digest[:]), entry)
	}
	verifier, err := controller.NewAbuseStaffVerifier([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

// An entry's fields after its digest.
func testAbuseActionStaffEntry(reviewerId server.Id, roles string, expires time.Time) string {
	return fmt.Sprintf("    reviewer_id: %s\n    roles: [%s]\n    expires: %s\n", reviewerId, roles, expires.UTC().Format(time.RFC3339))
}

// With no staff file, or one that does not read, every caller is refused alike.
func TestAbuseActionRefusesEveryCallerWithoutAStaffFile(t *testing.T) {
	handlers := NewAbuseActionHandlers(&AbuseActionHandlersDeps{
		LoadVerifier: func() (*controller.AbuseStaffVerifier, error) {
			return nil, controller.ErrAbuseStaffFileRefused
		},
	})
	networkBearer := "Bearer " + session.NewByJwt(server.NewId(), server.NewId(), "staff", false, false).Testing_Sign()
	for _, authorization := range []string{"", "Bearer anything", networkBearer} {
		answer := testAbuseActionCall(t, handlers, authorization, `{"kind":"lookup_case"}`)
		if answer.status != http.StatusForbidden || strings.TrimSpace(answer.body) != "Forbidden." {
			t.Errorf("%q: %d %q", authorization, answer.status, answer.body)
		}
	}
}

// Every refusal of the credential answers one 403; a member past it reaches the verbs, whose own refusals differ.
func TestAbuseActionCredentialTable(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	verifier := testAbuseActionStaffFile(t, map[string]string{
		"reviewer-bearer": testAbuseActionStaffEntry(server.NewId(), "reviewer", now.Add(24*time.Hour)),
		"engineer-bearer": testAbuseActionStaffEntry(server.NewId(), "engineering", now.Add(24*time.Hour)),
		"expired-bearer":  testAbuseActionStaffEntry(server.NewId(), "reviewer", now.Add(-time.Hour)),
		"long-bearer":     testAbuseActionStaffEntry(server.NewId(), "reviewer", now.Add(91*24*time.Hour)),
	})
	handlers := NewAbuseActionHandlers(&AbuseActionHandlersDeps{
		LoadVerifier: func() (*controller.AbuseStaffVerifier, error) { return verifier, nil },
		Now:          func() time.Time { return now },
	})
	networkBearer := "Bearer " + session.NewByJwt(server.NewId(), server.NewId(), "staff", false, false).Testing_Sign()
	for _, authorization := range []string{"", "reviewer-bearer", "Bearer ", "Bearer wrong", "Bearer expired-bearer", "Bearer long-bearer", networkBearer} {
		answer := testAbuseActionCall(t, handlers, authorization, `{"kind":"lookup_case"}`)
		if answer.status != http.StatusForbidden || strings.TrimSpace(answer.body) != "Forbidden." {
			t.Errorf("%q: %d %q", authorization, answer.status, answer.body)
		}
	}

	if answer := testAbuseActionCall(t, handlers, "Bearer reviewer-bearer", `{"kind":"everything"}`); answer.status != http.StatusBadRequest {
		t.Errorf("an unknown kind: %d %q", answer.status, answer.body)
	}
	if answer := testAbuseActionCall(t, handlers, "Bearer engineer-bearer", `{"kind":"lookup_case"}`); answer.status != http.StatusForbidden || strings.TrimSpace(answer.body) == "Forbidden." {
		t.Errorf("a role the kind does not take: %d %q", answer.status, answer.body)
	}
	if answer := testAbuseActionCall(t, handlers, "Bearer reviewer-bearer", `not json`); answer.status != http.StatusBadRequest {
		t.Errorf("a body that is no JSON: %d", answer.status)
	}
	huge := `{"kind":"lookup_case","pad":"` + strings.Repeat("p", int(DefaultAbuseActionRouteSettings().RequestMaxByteCount)) + `"}`
	if answer := testAbuseActionCall(t, handlers, "Bearer reviewer-bearer", huge); answer.status != http.StatusRequestEntityTooLarge {
		t.Errorf("a body past its bound: %d", answer.status)
	}
}

// The staff file is read again once its interval has passed, so a removed credential stops then and not before,
// and a read that fails refuses every caller.
func TestAbuseActionRereadsTheStaffFile(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	clock := &testAbuseRouteClock{now: now}
	reviewerId := server.NewId()
	withBearer := testAbuseActionStaffFile(t, map[string]string{
		"reviewer-bearer": testAbuseActionStaffEntry(reviewerId, "reviewer", now.Add(24*time.Hour)),
	})
	withoutBearer := testAbuseActionStaffFile(t, map[string]string{
		"other-bearer": testAbuseActionStaffEntry(server.NewId(), "reviewer", now.Add(24*time.Hour)),
	})
	var stateLock sync.Mutex
	current := withBearer
	var loadErr error
	reads := 0
	settings := DefaultAbuseActionRouteSettings()
	handlers := NewAbuseActionHandlers(&AbuseActionHandlersDeps{
		LoadVerifier: func() (*controller.AbuseStaffVerifier, error) {
			stateLock.Lock()
			defer stateLock.Unlock()
			reads += 1
			return current, loadErr
		},
		Now:      clock.Now,
		Settings: settings,
	})
	// an unknown kind answers 400 past the credential, and 403 before it
	call := func() int {
		return testAbuseActionCall(t, handlers, "Bearer reviewer-bearer", `{"kind":"everything"}`).status
	}
	if status := call(); status != http.StatusBadRequest {
		t.Fatalf("with the credential: %d", status)
	}
	stateLock.Lock()
	current = withoutBearer
	stateLock.Unlock()
	clock.Advance(settings.VerifierRereadInterval - time.Second)
	if status := call(); status != http.StatusBadRequest {
		t.Fatalf("inside the interval, before the reread: %d", status)
	}
	clock.Advance(time.Second)
	if status := call(); status != http.StatusForbidden {
		t.Fatalf("after the reread: %d", status)
	}
	stateLock.Lock()
	current = nil
	loadErr = errors.New("vault is down")
	stateLock.Unlock()
	clock.Advance(settings.VerifierRereadInterval)
	if status := testAbuseActionCall(t, handlers, "Bearer other-bearer", `{"kind":"everything"}`).status; status != http.StatusForbidden {
		t.Fatalf("after a read that failed: %d", status)
	}
	if reads != 3 {
		t.Fatalf("%d reads of the file", reads)
	}
}
