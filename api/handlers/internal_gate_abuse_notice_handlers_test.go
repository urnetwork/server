package handlers

// api's role-mail notice route (QD-1070, owner decision 612): only the platform listener's bearer reaches it, and no
// edge block's, other service's, client's or staff member's token does; a body holding anything beyond the header
// facts, a body or a subject among them, is refused with nothing filed; the filer's outcome reaches the listener as
// notice, paged, 503 for a mail to send again or 400 for a root that is no host name; and the listener's client,
// server.AbuseNoticeGateClient, sends the format alone and refuses what the format does not take before it sends.
// internal_gate_abuse_notice_handlers_db_test.go has the route over the controller on MG5's tables.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// One call the route's filer received.
type abuseNoticeTestFiling struct {
	recipientLocalPart string
	root               string
	verdicts           []controller.AbuseNoticeMailVerdict
}

// The route on a verify file of the platform listener's, the edge class's and mxedge's router's bearers, with a
// filer that records each call and answers what the test sets.
type abuseNoticeTestWorld struct {
	t        testing.TB
	server   *httptest.Server
	platform string
	edge     string
	mxedge   string
	// the platform listener's digest, which is no bearer
	platformDigest string

	stateLock sync.Mutex
	filings   []*abuseNoticeTestFiling
	result    *controller.AbuseNoticeResult
	err       error
	// what the route received, raw, at each call that reached the filer or not
	bodies [][]byte
}

func newAbuseNoticeTestWorld(t *testing.T) *abuseNoticeTestWorld {
	platform, platformDigest := newInternalGateTestBearer(t)
	edge, edgeDigest := newInternalGateTestBearer(t)
	mxedge, mxedgeDigest := newInternalGateTestBearer(t)
	verifier, err := server.NewInternalGateVerifier([]byte(
		"mxedge-platform: {class: mxedge-platform, sha256: [" + platformDigest + "]}\n" +
			"edge: {class: edge, sha256: [" + edgeDigest + "]}\n" +
			"mxedge: {class: mxedge, sha256: [" + mxedgeDigest + "]}\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	self := &abuseNoticeTestWorld{t: t, platform: platform, edge: edge, mxedge: mxedge, platformDigest: platformDigest}
	handlers := NewInternalGateAbuseNoticeHandlers(&InternalGateAbuseNoticeDeps{
		Verifier: verifier,
		File: func(ctx context.Context, recipientLocalPart string, root string, verdicts []controller.AbuseNoticeMailVerdict) (*controller.AbuseNoticeResult, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.filings = append(self.filings, &abuseNoticeTestFiling{recipientLocalPart: recipientLocalPart, root: root, verdicts: verdicts})
			return self.result, self.err
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	routes := handlers.Routes()
	// the raw body, kept for the test, then handed to the route unchanged
	recording := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.bodies = append(self.bodies, body)
		}()
		r.Body = io.NopCloser(bytes.NewReader(body))
		router.NewRouter(ctx, routes).ServeHTTP(w, r)
	})
	self.server = httptest.NewServer(recording)
	t.Cleanup(self.server.Close)
	return self
}

// Sets what the filer answers from the next call on.
func (self *abuseNoticeTestWorld) answer(result *controller.AbuseNoticeResult, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.result = result
	self.err = err
}

// The filer's calls, in order.
func (self *abuseNoticeTestWorld) filed() []*abuseNoticeTestFiling {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.filings)
}

// One post with an Authorization value and a body, and its status, answer and Retry-After.
func (self *abuseNoticeTestWorld) post(authorization string, body string) (int, *server.AbuseNoticeGateResult, string) {
	self.t.Helper()
	request, err := http.NewRequest(http.MethodPost, self.server.URL+server.AbuseNoticeGatePath, strings.NewReader(body))
	if err != nil {
		self.t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := self.server.Client().Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	defer response.Body.Close()
	result := &server.AbuseNoticeGateResult{}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			self.t.Fatal(err)
		}
	}
	return response.StatusCode, result, response.Header.Get("Retry-After")
}

// A registrar's notice as the listener sends it: one stranger's signature that failed and a listed one that passed.
const testAbuseNoticeBody = `{"recipient_local_part":"abuse","root":"root.example",` +
	`"signatures":[{"domain":"stranger.example","result":"fail"},{"domain":"registrar.example","result":"pass"}]}`

// Only the platform listener's bearer reaches the filer. No bearer, one api does not hold, the edge class's, the
// router's, the platform listener's digest in place of its bearer, a URnetwork client's token and a staff member's
// bearer are each refused 403 with nothing read.
func TestInternalGateAbuseNoticeTakesOnlyThePlatformListenersBearer(t *testing.T) {
	world := newAbuseNoticeTestWorld(t)
	world.answer(&controller.AbuseNoticeResult{CaseId: server.NewId(), Opened: true, Paged: true}, nil)
	stranger, _ := newInternalGateTestBearer(t)
	clientToken := session.NewByJwt(server.NewId(), server.NewId(), "notice-test", false, false).Testing_Sign()
	for _, authorization := range []string{
		"",
		"Bearer ",
		"Bearer " + stranger,
		"Bearer " + world.edge,
		"Bearer " + world.mxedge,
		"Bearer " + world.platformDigest,
		"Bearer " + clientToken,
		"Bearer synthetic-staff-bearer-not-a-gate-bearer",
		world.platform,
	} {
		if status, _, _ := world.post(authorization, testAbuseNoticeBody); status != http.StatusForbidden {
			t.Errorf("%.24q answered %d, want 403", authorization, status)
		}
	}
	if filings := world.filed(); len(filings) != 0 {
		t.Fatalf("a refused bearer reached the filer %d times", len(filings))
	}
	status, result, _ := world.post("Bearer "+world.platform, testAbuseNoticeBody)
	if status != http.StatusOK || !result.Notice || !result.Paged {
		t.Fatalf("the platform listener's notice answered %d %+v", status, result)
	}
	filings := world.filed()
	if len(filings) != 1 {
		t.Fatalf("%d filings, want 1", len(filings))
	}
	want := []controller.AbuseNoticeMailVerdict{
		{Domain: "stranger.example", Result: server.AbuseNoticeDkimResultFail},
		{Domain: "registrar.example", Result: server.AbuseNoticeDkimResultPass},
	}
	if filings[0].recipientLocalPart != "abuse" || filings[0].root != "root.example" || !slices.Equal(filings[0].verdicts, want) {
		t.Fatalf("the filer received %+v", filings[0])
	}
}

// A verify file api cannot read refuses every caller, the platform listener's included.
func TestInternalGateAbuseNoticeRefusesEveryCallerWithoutAVerifyFile(t *testing.T) {
	pop := server.Vault.PushSimpleResource(server.InternalGateVerifyFileName, []byte("not: [a verify file\n"))
	defer pop()
	filed := 0
	handlers := NewInternalGateAbuseNoticeHandlers(&InternalGateAbuseNoticeDeps{
		File: func(ctx context.Context, recipientLocalPart string, root string, verdicts []controller.AbuseNoticeMailVerdict) (*controller.AbuseNoticeResult, error) {
			filed += 1
			return nil, nil
		},
	})
	platform, _ := newInternalGateTestBearer(t)
	request := httptest.NewRequest(http.MethodPost, server.AbuseNoticeGatePath, strings.NewReader(testAbuseNoticeBody))
	request.Header.Set("Authorization", "Bearer "+platform)
	w := httptest.NewRecorder()
	handlers.PostAbuseNotice(w, request)
	if w.Code != http.StatusForbidden || filed != 0 {
		t.Fatalf("with no verify file: %d, %d filings", w.Code, filed)
	}
}

// The body is the format's header facts and nothing more: a body, a subject, a From, the message or any other field
// beside them, a second value after the request, a body past the bound, more signatures than the format's, a result
// that is no RFC 8601 word, and a local part, root or domain that is no name are each refused 400, with nothing
// filed.
func TestInternalGateAbuseNoticeRefusesAnyBodyField(t *testing.T) {
	world := newAbuseNoticeTestWorld(t)
	world.answer(nil, nil)
	signatures := func(count int) string {
		items := []string{}
		for index := range count {
			items = append(items, fmt.Sprintf(`{"domain":"s%d.example","result":"pass"}`, index))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	head := `{"recipient_local_part":"abuse","root":"root.example","signatures":[]`
	refused := []string{
		head + `,"body":"Please take down the page at https://phish.root.example/"}`,
		head + `,"subject":"Notice of abuse"}`,
		head + `,"from":"registrar@registrar.example"}`,
		head + `,"message":"RnJvbTogeA=="}`,
		head + `,"raw":"RnJvbTogeA=="}`,
		head + `,"sender":"bounce@registrar.example"}`,
		`{"recipient_local_part":"abuse","root":"root.example","signatures":[{"domain":"registrar.example","result":"pass","body":"text"}]}`,
		head + `}{"body":"a second value"}`,
		head + `}` + strings.Repeat(" ", 5*1024),
		`{"recipient_local_part":"abuse","root":"root.example","signatures":` + signatures(9) + `}`,
		`{"recipient_local_part":"abuse","root":"root.example","signatures":[{"domain":"registrar.example","result":"verified"}]}`,
		`{"recipient_local_part":"abuse","root":"root.example","signatures":[{"domain":"registrar example","result":"pass"}]}`,
		`{"recipient_local_part":"abuse","root":"root.example","signatures":[null]}`,
		`{"recipient_local_part":"","root":"root.example","signatures":[]}`,
		`{"recipient_local_part":"abuse","root":"","signatures":[]}`,
		`{"recipient_local_part":"abuse","root":"root .example","signatures":[]}`,
		`{"recipient_local_part":"` + strings.Repeat("a", 65) + `","root":"root.example","signatures":[]}`,
		`{"recipient_local_part":"abuse","root":"root.example","signatures":"pass"}`,
		`not json`,
		``,
	}
	for _, body := range refused {
		if status, _, _ := world.post("Bearer "+world.platform, body); status != http.StatusBadRequest {
			t.Errorf("%.80q answered %d, want 400", body, status)
		}
	}
	if filings := world.filed(); len(filings) != 0 {
		t.Fatalf("a refused body reached the filer %d times", len(filings))
	}
	// eight signatures, the format's bound, and a signature that named no domain, are taken
	for _, body := range []string{
		`{"recipient_local_part":"abuse","root":"root.example","signatures":` + signatures(8) + `}`,
		`{"recipient_local_part":"postmaster","root":"root.example","signatures":[{"domain":"","result":"permerror"}]}` + "\n",
	} {
		if status, result, _ := world.post("Bearer "+world.platform, body); status != http.StatusOK || result.Notice {
			t.Errorf("%.80q answered %d %+v, want no notice", body, status, result)
		}
	}
}

// What the filer answers reaches the listener: a notice paged, a notice whose page is owed, no notice, 503 with
// Retry-After for a mail to send again (a listed sender's key that did not answer, a binary without MG5, a store
// that did not answer), and 400 for a root that is no host name.
func TestInternalGateAbuseNoticeAnswersTheFilersOutcome(t *testing.T) {
	world := newAbuseNoticeTestWorld(t)
	caseId := server.NewId()
	cases := []struct {
		result     *controller.AbuseNoticeResult
		err        error
		status     int
		notice     bool
		paged      bool
		retryAfter string
	}{
		{result: &controller.AbuseNoticeResult{CaseId: caseId, Opened: true, Paged: true}, status: http.StatusOK, notice: true, paged: true},
		{result: &controller.AbuseNoticeResult{CaseId: caseId, Opened: true}, err: errors.New("the pager service is down"), status: http.StatusOK, notice: true},
		{status: http.StatusOK},
		{err: controller.ErrAbuseNoticeMailDeferred, status: http.StatusServiceUnavailable, retryAfter: "60"},
		{err: model.ErrAbuseUnlinked, status: http.StatusServiceUnavailable, retryAfter: "60"},
		{err: errors.New("abuse notice: the store did not answer"), status: http.StatusServiceUnavailable, retryAfter: "60"},
		{err: fmt.Errorf("%w: a notice about no host name", model.ErrAbuseCaseInvalid), status: http.StatusBadRequest},
	}
	for index, c := range cases {
		world.answer(c.result, c.err)
		status, result, retryAfter := world.post("Bearer "+world.platform, testAbuseNoticeBody)
		if status != c.status || retryAfter != c.retryAfter {
			t.Errorf("case %d answered %d with Retry-After %q, want %d and %q", index, status, retryAfter, c.status, c.retryAfter)
			continue
		}
		if status == http.StatusOK && (result.Notice != c.notice || result.Paged != c.paged) {
			t.Errorf("case %d answered %+v", index, result)
		}
	}
}

// The listener's client posts the format and nothing more, with its bearer, and reads each answer: a notice, none,
// and an error for a refusal and for a mail to send again. A request the format does not take is refused before it
// is sent, and a client is not made without a bearer or with an api url it cannot use. No error holds the bearer.
func TestAbuseNoticeGateClientSendsTheHeaderFactsAlone(t *testing.T) {
	world := newAbuseNoticeTestWorld(t)
	client, err := server.NewAbuseNoticeGateClient(&server.AbuseNoticeGateClientArgs{ApiUrl: world.server.URL, Bearer: world.platform})
	if err != nil {
		t.Fatal(err)
	}
	args := &server.AbuseNoticeGateArgs{
		RecipientLocalPart: "postmaster",
		Root:               "root.example",
		Signatures: []*server.AbuseNoticeGateSignature{
			{Domain: "registrar.example", Result: server.AbuseNoticeDkimResultPass},
			{Domain: "", Result: server.AbuseNoticeDkimResultPermError},
		},
	}
	ctx := context.Background()

	world.answer(&controller.AbuseNoticeResult{CaseId: server.NewId(), Opened: true, Paged: true}, nil)
	result, err := client.FileNotice(ctx, args)
	if err != nil || !result.Notice || !result.Paged {
		t.Fatalf("a filed notice: %+v, %v", result, err)
	}
	world.answer(nil, nil)
	if result, err := client.FileNotice(ctx, args); err != nil || result.Notice || result.Paged {
		t.Fatalf("no notice: %+v, %v", result, err)
	}
	// exactly the format's fields went, and the bearer went in the header alone
	world.stateLock.Lock()
	sent := map[string]any{}
	err = json.Unmarshal(world.bodies[0], &sent)
	world.stateLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fields := []string{}
	for field := range sent {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	if !slices.Equal(fields, []string{"recipient_local_part", "root", "signatures"}) {
		t.Fatalf("the client sent %v", fields)
	}

	world.answer(nil, controller.ErrAbuseNoticeMailDeferred)
	if _, err := client.FileNotice(ctx, args); !errors.Is(err, server.ErrAbuseNoticeGate) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a mail to send again: %v", err)
	}
	strange, err := server.NewAbuseNoticeGateClient(&server.AbuseNoticeGateClientArgs{ApiUrl: world.server.URL, Bearer: world.edge})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strange.FileNotice(ctx, args); !errors.Is(err, server.ErrAbuseNoticeGate) || strings.Contains(err.Error(), world.edge) {
		t.Fatalf("another class's bearer: %v", err)
	}

	// refused before a request goes out
	filingCount := len(world.filed())
	for _, invalid := range []*server.AbuseNoticeGateArgs{
		{RecipientLocalPart: "abuse", Root: "root.example", Signatures: []*server.AbuseNoticeGateSignature{{Domain: "registrar.example", Result: "verified"}}},
		{RecipientLocalPart: "abuse", Root: "", Signatures: []*server.AbuseNoticeGateSignature{}},
		{RecipientLocalPart: "abuse", Root: "root.example", Signatures: slices.Repeat([]*server.AbuseNoticeGateSignature{{Domain: "s.example", Result: "pass"}}, 9)},
	} {
		if _, err := client.FileNotice(ctx, invalid); !errors.Is(err, server.ErrAbuseNoticeGateArgsInvalid) {
			t.Errorf("%+v: %v", invalid, err)
		}
	}
	if count := len(world.filed()); count != filingCount {
		t.Fatalf("a refused request reached the route")
	}

	for _, clientArgs := range []*server.AbuseNoticeGateClientArgs{
		{ApiUrl: world.server.URL},
		{ApiUrl: world.server.URL + "/", Bearer: world.platform},
		{ApiUrl: "ftp://api.example", Bearer: world.platform},
		{ApiUrl: "", Bearer: world.platform},
	} {
		if _, err := server.NewAbuseNoticeGateClient(clientArgs); !errors.Is(err, server.ErrAbuseNoticeGate) || strings.Contains(err.Error(), world.platform) {
			t.Errorf("a client of %q: %v", clientArgs.ApiUrl, err)
		}
	}
}

// A redirect is answered as the refusal it is: the client follows none, so its bearer goes to api's own url alone.
func TestAbuseNoticeGateClientFollowsNoRedirect(t *testing.T) {
	elsewhereCalls := atomic.Int32{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereCalls.Add(1)
		w.Write([]byte(`{"notice":true,"paged":true}`))
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	platform, _ := newInternalGateTestBearer(t)
	client, err := server.NewAbuseNoticeGateClient(&server.AbuseNoticeGateClientArgs{ApiUrl: redirecting.URL, Bearer: platform})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FileNotice(context.Background(), &server.AbuseNoticeGateArgs{RecipientLocalPart: "abuse", Root: "root.example"})
	if !errors.Is(err, server.ErrAbuseNoticeGate) || elsewhereCalls.Load() != 0 {
		t.Fatalf("a redirect: %v, %d calls elsewhere", err, elsewhereCalls.Load())
	}
}
