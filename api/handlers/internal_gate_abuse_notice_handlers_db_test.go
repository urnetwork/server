//go:build sidecar_pending

package handlers

// The role-mail notice route over the controller on MG5's tables (QD-1070, owner decision 612; FS-49's paging half),
// the platform listener's bearer posting through server.AbuseNoticeGateClient, as mxedge-platform does: a listed
// registrar's verified mail opens one notice case and pages both rota members once under the case's key; a pager
// that refuses leaves the case standing and answers paged false, and taskworker's clock pass pages it under the same
// key, which is the crash between a notice's commit and its page costing a minute and not the page; a listed
// sender's key that did not answer is a mail to send again, opening nothing; and mail that is no notice, and a body
// holding a field beyond the header facts, open nothing and page no one.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/pager"
	"github.com/urnetwork/server/router"
)

// The route on api's own filer, served to the platform listener's bearer, and that listener's client.
type abuseNoticeDbTestWorld struct {
	server *httptest.Server
	client *server.AbuseNoticeGateClient
	bearer string
}

func newAbuseNoticeDbTestWorld(t testing.TB) *abuseNoticeDbTestWorld {
	raw := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(bearer))
	verifier, err := server.NewInternalGateVerifier([]byte("mxedge-platform: {class: mxedge-platform, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	handlers := NewInternalGateAbuseNoticeHandlers(&InternalGateAbuseNoticeDeps{Verifier: verifier})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	routeServer := httptest.NewServer(router.NewRouter(ctx, handlers.Routes()))
	t.Cleanup(routeServer.Close)
	client, err := server.NewAbuseNoticeGateClient(&server.AbuseNoticeGateClientArgs{ApiUrl: routeServer.URL, Bearer: bearer})
	if err != nil {
		t.Fatal(err)
	}
	return &abuseNoticeDbTestWorld{server: routeServer, client: client, bearer: bearer}
}

// A role mail's facts with its signatures, d= then result.
func testAbuseNoticeArgs(localPart string, signatures ...string) *server.AbuseNoticeGateArgs {
	args := &server.AbuseNoticeGateArgs{RecipientLocalPart: localPart, Root: "root.example", Signatures: []*server.AbuseNoticeGateSignature{}}
	for index := 0; index+1 < len(signatures); index += 2 {
		args.Signatures = append(args.Signatures, &server.AbuseNoticeGateSignature{Domain: signatures[index], Result: signatures[index+1]})
	}
	return args
}

// The notice cases on the test's database.
func testAbuseNoticeCaseIds(t testing.TB, ctx context.Context) []server.Id {
	t.Helper()
	caseIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT case_id FROM abuse_case WHERE join_kind = $1 ORDER BY case_id`, string(model.AbuseJoinKindNotice))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var caseId server.Id
				server.Raise(result.Scan(&caseId))
				caseIds = append(caseIds, caseId)
			}
		})
	})
	return caseIds
}

// A listed registrar's verified mail opens one notice case about the root and pages both people at once under the
// case's key, which a second clock pass repeats while the page is open and so wakes no one again.
func TestInternalGateAbuseNoticePagesOnceUnderTheCasesKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig("notice_senders:\n  registrar.example: registrar\n")()
		sink := pager.NewFakeSink(pager.FakeSinkSettings{})
		defer controller.SetAbuseNoticePager(sink)()
		world := newAbuseNoticeDbTestWorld(t)

		result, err := world.client.FileNotice(ctx, testAbuseNoticeArgs("abuse",
			"stranger.example", server.AbuseNoticeDkimResultPass,
			"registrar.example", server.AbuseNoticeDkimResultPass,
		))
		if err != nil || !result.Notice || !result.Paged {
			t.Fatalf("a listed registrar's mail: %+v, %v", result, err)
		}
		caseIds := testAbuseNoticeCaseIds(t, ctx)
		if len(caseIds) != 1 {
			t.Fatalf("%d notice cases, want 1", len(caseIds))
		}
		abuseCase := model.GetAbuseCase(ctx, caseIds[0])
		if model.AbuseNoticeClassOfCase(abuseCase) != model.AbuseNoticeClassRegistrar || abuseCase.Target != "root.example" {
			t.Fatalf("the mail's case is %+v", abuseCase)
		}
		notifications := sink.Notifications()
		if len(notifications) != 2 || notifications[0].Person == notifications[1].Person {
			t.Fatalf("paged %+v, want both people at once", notifications)
		}
		for _, notification := range notifications {
			if notification.Page.DedupKey != pager.NoticeDedupKey(caseIds[0]) {
				t.Fatalf("paged under %s, want the case's key", notification.Page.DedupKey)
			}
		}
		// the pass repeats the open page's key and wakes no one
		pass := controller.RunAbuseClockPass(ctx, server.NowUtc().Add(time.Minute), model.DefaultAbuseSettings())
		if pass.NoticePagedCount != 1 || len(sink.Notifications()) != 2 {
			t.Fatalf("a pass over an open page: %+v, %d notifications", pass, len(sink.Notifications()))
		}
	})
}

// A pager that refuses the page leaves the case standing: the route answers notice with paged false, and the clock
// pass of a process whose pager takes it pages the case under the key the refused page carried. So a crash between
// the case's commit and its page costs a minute and not the page, and the listener's sender is not asked again.
func TestInternalGateAbuseNoticeClockPassPagesAPageTheFilingLost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig("notice_senders:\n  registry.example: registry\n")()
		sink := pager.NewFakeSink(pager.FakeSinkSettings{})
		defer controller.SetAbuseNoticePager(sink)()
		world := newAbuseNoticeDbTestWorld(t)

		sink.Fail(errors.New("the pager service is down"))
		result, err := world.client.FileNotice(ctx, testAbuseNoticeArgs("postmaster", "registry.example", server.AbuseNoticeDkimResultPass))
		if err != nil || !result.Notice || result.Paged {
			t.Fatalf("a notice the pager refused: %+v, %v", result, err)
		}
		caseIds := testAbuseNoticeCaseIds(t, ctx)
		attempts := sink.Attempts()
		if len(caseIds) != 1 || len(attempts) != 1 || !attempts[0].Refused || attempts[0].DedupKey != pager.NoticeDedupKey(caseIds[0]) {
			t.Fatalf("cases %v, attempts %+v", caseIds, attempts)
		}
		if count := len(sink.Notifications()); count != 0 {
			t.Fatalf("%d notifications while the pager refused", count)
		}

		sink.Fail(nil)
		pass := controller.RunAbuseClockPass(ctx, server.NowUtc().Add(time.Minute), model.DefaultAbuseSettings())
		if pass.NoticePagedCount != 1 || pass.NoticeOwedCount != 0 {
			t.Fatalf("the pass after the outage: %+v", pass)
		}
		notifications := sink.Notifications()
		if len(notifications) != 2 || notifications[0].Page.DedupKey != attempts[0].DedupKey || !notifications[0].Page.BothAtOnce {
			t.Fatalf("the pass paged %+v", notifications)
		}
		if count := len(testAbuseNoticeCaseIds(t, ctx)); count != 1 {
			t.Fatalf("%d notice cases after the pass, want 1", count)
		}
	})
}

// A listed sender's key that did not answer, with no signature passing under a listed domain, is a mail to send
// again: 503, so the listener answers 451, with nothing opened or paged. Mail that is no notice (an unlisted
// sender, a listed sender's failed signature, mail to another local part, a temperror under an unlisted domain)
// answers notice false and opens nothing, and a body with a field beyond the header facts is refused with nothing
// filed.
func TestInternalGateAbuseNoticeOpensNothingForMailThatIsNoNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig("notice_senders:\n  registrar.example: registrar\n  police.example: law_enforcement\n")()
		sink := pager.NewFakeSink(pager.FakeSinkSettings{})
		defer controller.SetAbuseNoticePager(sink)()
		world := newAbuseNoticeDbTestWorld(t)

		_, err := world.client.FileNotice(ctx, testAbuseNoticeArgs("abuse",
			"police.example", server.AbuseNoticeDkimResultTempError,
			"stranger.example", server.AbuseNoticeDkimResultPass,
		))
		if !errors.Is(err, server.ErrAbuseNoticeGate) || !strings.Contains(err.Error(), "503") {
			t.Fatalf("a listed sender's key that did not answer: %v", err)
		}
		for _, args := range []*server.AbuseNoticeGateArgs{
			testAbuseNoticeArgs("abuse", "stranger.example", server.AbuseNoticeDkimResultPass),
			testAbuseNoticeArgs("abuse", "registrar.example", server.AbuseNoticeDkimResultFail),
			testAbuseNoticeArgs("abuse", "registrar.example", server.AbuseNoticeDkimResultPermError),
			testAbuseNoticeArgs("sales", "registrar.example", server.AbuseNoticeDkimResultPass),
			testAbuseNoticeArgs("abuse", "mail.registrar.example", server.AbuseNoticeDkimResultPass),
			testAbuseNoticeArgs("abuse", "stranger.example", server.AbuseNoticeDkimResultTempError),
			testAbuseNoticeArgs("postmaster"),
		} {
			result, err := world.client.FileNotice(ctx, args)
			if err != nil || result.Notice || result.Paged {
				t.Errorf("%+v: %+v, %v, want no notice", args, result, err)
			}
		}
		request, err := http.NewRequest(http.MethodPost, world.server.URL+server.AbuseNoticeGatePath, strings.NewReader(
			`{"recipient_local_part":"abuse","root":"root.example","signatures":[{"domain":"registrar.example","result":"pass"}],"body":"Please remove it."}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+world.bearer)
		response, err := world.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("a body field answered %d", response.StatusCode)
		}
		if caseIds := testAbuseNoticeCaseIds(t, ctx); len(caseIds) != 0 {
			t.Fatalf("%d notice cases from mail that is no notice", len(caseIds))
		}
		if attempts := sink.Attempts(); len(attempts) != 0 {
			t.Fatalf("the pager saw %+v from mail that is no notice", attempts)
		}
	})
}
