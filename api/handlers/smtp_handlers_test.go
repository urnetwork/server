//go:build sidecar_pending

package handlers

// §14.14's four routes over the router (smtp_handlers.go; SMTP-M9-19) on a private database: POST /smtp/dkim-keys
// dark with no dns root, refused behind a closed smtpedge gate, with no §8 check installed and without mail_send,
// then publishing for a sidecar's own client and refusing a client that is no sidecar; GET /smtp/status answering
// the network's halts; GET /smtp/reports refused without §8 read and paging with it; and POST /smtp/appeal refusing
// a client token, lifting on the ladder's recheck, and answering 404 for a halt it does not hold.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// One call of the smtp routes, its status and body.
func testSmtpCall(t testing.TB, ctx context.Context, method string, path string, authorization string, body any) (int, []byte) {
	t.Helper()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/smtp/dkim-keys", SmtpDkimKeys),
		router.NewRoute("GET", "/smtp/status", GetSmtpStatus),
		router.NewRoute("GET", "/smtp/reports", GetSmtpReports),
		router.NewRoute("POST", "/smtp/appeal", SmtpAppeal),
	})
	payload := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = string(data)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(payload))
	request.RemoteAddr = "127.0.0.1:54321"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	return w.Code, w.Body.Bytes()
}

// The code a refusal's body carries.
func testSmtpCode(body []byte) string {
	refusal := struct {
		Code string `json:"code"`
	}{}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Code
}

// §8's checks, answering as set.
type testSmtpApiGate struct {
	mailSend    bool
	readReports bool
	err         error
}

func (self *testSmtpApiGate) MaySendMail(ctx context.Context, networkId server.Id, sidecarId server.Id, now time.Time) (bool, error) {
	return self.mailSend, self.err
}

func (self *testSmtpApiGate) MayReadReports(ctx context.Context, networkId server.Id, sidecarId server.Id, now time.Time) (bool, error) {
	return self.readReports, self.err
}

// A network with a client that speaks for a sidecar, and the bearers its client and its network hold.
type testSmtpWorld struct {
	root          string
	networkId     server.Id
	clientId      server.Id
	sidecarId     server.Id
	byJwt         *session.ByJwt
	clientBearer  string
	networkBearer string
}

func newTestSmtpWorld(t testing.TB, ctx context.Context) *testSmtpWorld {
	t.Helper()
	root := fmt.Sprintf("s%s.example.", strings.ReplaceAll(server.NewId().String()[24:], "-", ""))
	if err := model.EnsureDnsRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "smtp-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "sender", "smtp")
	byJwt := session.NewByJwt(networkId, userId, "smtp", false, false)
	world := &testSmtpWorld{
		root:          root,
		networkId:     networkId,
		clientId:      clientId,
		sidecarId:     server.NewId(),
		byJwt:         byJwt,
		clientBearer:  "Bearer " + byJwt.Client(deviceId, clientId).Testing_Sign(),
		networkBearer: "Bearer " + byJwt.Testing_Sign(),
	}
	previous := smtpClientSidecar
	smtpClientSidecar = func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool) {
		if networkId == world.networkId && clientId == world.clientId {
			return world.sidecarId, true
		}
		return server.Id{}, false
	}
	t.Cleanup(func() {
		smtpClientSidecar = previous
	})
	return world
}

// One selector to publish at domain, its key RSA-2048.
func testSmtpPublishArgs(t testing.TB, domain string) (*sidecar.SmtpDkimKeysArgs, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	selector := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(random)
	return &sidecar.SmtpDkimKeysArgs{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: domain, PublicKey: der}}}, selector
}

func TestSmtpDkimKeysRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestSmtpWorld(t, ctx)

		// no dns root in this process: dark, before the caller is looked at
		restoreRoot := model.Testing_SetDnsUserRoot("")
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, &sidecar.SmtpDkimKeysArgs{}); status != http.StatusServiceUnavailable {
			t.Errorf("with no root answered %d", status)
		}
		restoreRoot()
		t.Cleanup(model.Testing_SetDnsUserRoot(world.root))
		var label string
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			label, err = model.EnsureNetworkLabelInTx(ctx, tx, world.networkId, world.root)
			if err != nil {
				t.Fatal(err)
			}
		})
		domain := label + "." + strings.TrimSuffix(world.root, ".")
		args, selector := testSmtpPublishArgs(t, domain)

		// the smtpedge gate is closed by default
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, args); status != http.StatusServiceUnavailable {
			t.Errorf("behind a closed gate answered %d", status)
		}
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  smtpedge: {enabled: true, networks: [%q]}\n", world.networkId)))
		// no §8 check installed
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, args); status != http.StatusServiceUnavailable {
			t.Errorf("with no check answered %d", status)
		}
		gate := &testSmtpApiGate{}
		t.Cleanup(model.SetSmtpApiGate(gate))
		if status, body := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, args); status != http.StatusForbidden || testSmtpCode(body) != model.SmtpApiCodeMailSend {
			t.Errorf("without mail_send answered %d %s", status, body)
		}
		gate.err = errors.New("the gate cache has not loaded the network")
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, args); status != http.StatusServiceUnavailable {
			t.Errorf("a check that did not answer answered %d", status)
		}
		gate.err = nil
		gate.mailSend = true
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.networkBearer, args); status == http.StatusOK {
			t.Errorf("a bearer with no client answered %d", status)
		}
		if key, _ := model.FindSmtpDkimKey(ctx, selector, domain); key != nil {
			t.Fatal("a refused call published")
		}
		status, body := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, args)
		result := &sidecar.SmtpDkimKeysResult{}
		if status != http.StatusOK || json.Unmarshal(body, result) != nil || len(result.Published) != 1 || result.Published[0] != selector || result.Retired == nil {
			t.Fatalf("publish %d %s", status, body)
		}
		if key, err := model.FindSmtpDkimKey(ctx, selector, domain); err != nil || key == nil || key.SidecarId != sidecar.Id(world.sidecarId) {
			t.Fatalf("the row %+v, %v", key, err)
		}

		// a client that speaks for no sidecar
		otherDeviceId, otherClientId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, world.networkId, otherDeviceId, otherClientId, "other", "smtp")
		otherBearer := "Bearer " + world.byJwt.Client(otherDeviceId, otherClientId).Testing_Sign()
		if status, body := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", otherBearer, args); status != http.StatusForbidden || testSmtpCode(body) != model.SmtpApiCodeNoSidecar {
			t.Errorf("a client of no sidecar answered %d %s", status, body)
		}
		// a retirement needs neither the gate nor mail_send
		gate.mailSend = false
		restoreClosed := server.Testing_PushSidecarConfig("sidecar:\n  smtpedge: {enabled: false}\n")
		retire := &sidecar.SmtpDkimKeysArgs{Retire: []*sidecar.SmtpDkimKeyRetire{{Selector: selector, Domain: domain}}}
		if status, body := testSmtpCall(t, ctx, "POST", "/smtp/dkim-keys", world.clientBearer, retire); status != http.StatusOK {
			t.Fatalf("a retirement answered %d %s", status, body)
		}
		restoreClosed()
		if key, _ := model.FindSmtpDkimKey(ctx, selector, domain); key == nil || key.RetireTime.IsZero() {
			t.Fatal("the retirement was not kept")
		}
	})
}

func TestSmtpStatusRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestSmtpWorld(t, ctx)
		halt := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: sidecar.Id(world.networkId), Label: "maple-1234", Trigger: smtpedgeservice.SmtpHaltTriggerLabelComplaint, StartTime: server.NowUtc().Add(-time.Hour)}
		if err := model.SetSmtpHalt(ctx, halt); err != nil {
			t.Fatal(err)
		}
		for _, bearer := range []string{world.clientBearer, world.networkBearer} {
			status, body := testSmtpCall(t, ctx, "GET", "/smtp/status", bearer, nil)
			result := &model.SmtpStatusResult{}
			if status != http.StatusOK || json.Unmarshal(body, result) != nil || len(result.Halts) != 1 || result.Halts[0].Label != "maple-1234" || result.Halts[0].HaltId != server.Id(halt.HaltId) {
				t.Fatalf("status %d %s", status, body)
			}
		}
		if status, _ := testSmtpCall(t, ctx, "GET", "/smtp/status", "", nil); status == http.StatusOK {
			t.Fatal("no bearer was answered")
		}
	})
}

func TestSmtpReportsRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestSmtpWorld(t, ctx)
		arrival := time.Date(2032, time.March, 1, 12, 0, 0, 0, time.UTC)
		complaint := &smtpedgeservice.SmtpComplaint{
			ComplaintId:  sidecar.NewId(),
			NetworkId:    sidecar.Id(world.networkId),
			Reporter:     "loop.example",
			Type:         smtpedgeservice.SmtpComplaintTypeAbuse,
			ArrivalTime:  arrival,
			Domain:       "quietnet.root.example",
			JoinResult:   "high",
			SealedReport: []byte("sealed under the network mail key"),
		}
		if err := model.InsertSmtpComplaint(ctx, complaint); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/smtp/reports?since=%d", arrival.Add(-time.Hour).UnixMilli())
		if status, _ := testSmtpCall(t, ctx, "GET", path, world.clientBearer, nil); status != http.StatusServiceUnavailable {
			t.Errorf("with no check answered %d", status)
		}
		gate := &testSmtpApiGate{}
		t.Cleanup(model.SetSmtpApiGate(gate))
		if status, body := testSmtpCall(t, ctx, "GET", path, world.clientBearer, nil); status != http.StatusForbidden || testSmtpCode(body) != model.SmtpApiCodeReportsRead {
			t.Errorf("without read answered %d %s", status, body)
		}
		gate.readReports = true
		if status, _ := testSmtpCall(t, ctx, "GET", path, world.networkBearer, nil); status == http.StatusOK {
			t.Errorf("a bearer with no client answered %d", status)
		}
		if status, _ := testSmtpCall(t, ctx, "GET", "/smtp/reports?since=yesterday", world.clientBearer, nil); status != http.StatusBadRequest {
			t.Errorf("a since that is no number answered %d", status)
		}
		status, body := testSmtpCall(t, ctx, "GET", path, world.clientBearer, nil)
		result := &model.SmtpReportsResult{}
		if status != http.StatusOK || json.Unmarshal(body, result) != nil || len(result.Complaints) != 1 || result.Cursor != "" ||
			result.Complaints[0].ComplaintId != server.Id(complaint.ComplaintId) || string(result.Complaints[0].SealedReport) != string(complaint.SealedReport) {
			t.Fatalf("reports %d %s", status, body)
		}
		later := fmt.Sprintf("/smtp/reports?since=%d", arrival.Add(time.Hour).UnixMilli())
		if status, body := testSmtpCall(t, ctx, "GET", later, world.clientBearer, nil); status != http.StatusOK || json.Unmarshal(body, result) != nil || len(result.Complaints) != 0 {
			t.Fatalf("later reports %d %s", status, body)
		}
	})
}

// A ladder that lifts the halts it is told to.
type testSmtpLadder struct {
	lift    map[sidecar.Id]bool
	checked []sidecar.Id
}

func (self *testSmtpLadder) Lift(ctx context.Context, networkId sidecar.Id, haltId sidecar.Id) (bool, error) {
	self.checked = append(self.checked, haltId)
	if !self.lift[haltId] {
		return false, nil
	}
	return true, model.DeleteSmtpHalt(ctx, server.Id(haltId))
}

func TestSmtpAppealRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestSmtpWorld(t, ctx)
		now := server.NowUtc()
		duplicate := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: sidecar.Id(world.networkId), Trigger: smtpedgeservice.SmtpHaltTriggerCommunityComplaint, StartTime: now.Add(-time.Hour)}
		holding := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: sidecar.Id(world.networkId), Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: now.Add(-2 * time.Hour)}
		ended := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: sidecar.Id(world.networkId), Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: now.Add(-30 * time.Hour)}
		for _, halt := range []*smtpedgeservice.SmtpHalt{duplicate, holding, ended} {
			if err := model.SetSmtpHalt(ctx, halt); err != nil {
				t.Fatal(err)
			}
		}
		ladder := &testSmtpLadder{lift: map[sidecar.Id]bool{duplicate.HaltId: true}}
		previous := smtpAppealLadderOf
		smtpAppealLadderOf = func() (smtpAppealLadder, error) {
			return ladder, nil
		}
		// §21's record of each halt the recheck lifted, and of none it kept (ABUSE-M9-01)
		abuseLifts := []server.Id{}
		previousAbuseLift := smtpAppealAbuseLift
		smtpAppealAbuseLift = func(ctx context.Context, networkId server.Id, haltId server.Id) (*controller.AbuseActionResult, error) {
			if networkId != world.networkId {
				t.Errorf("§21's lift for another network")
			}
			abuseLifts = append(abuseLifts, haltId)
			return nil, nil
		}
		t.Cleanup(func() {
			smtpAppealLadderOf = previous
			smtpAppealAbuseLift = previousAbuseLift
		})

		// a client token, a child of the network, never appeals
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/appeal", world.clientBearer, &SmtpAppealArgs{}); status != http.StatusForbidden || len(ladder.checked) != 0 {
			t.Fatalf("a client token answered %d", status)
		}
		status, body := testSmtpCall(t, ctx, "POST", "/smtp/appeal", world.networkBearer, &SmtpAppealArgs{})
		result := &SmtpAppealResult{}
		if status != http.StatusOK || json.Unmarshal(body, result) != nil || len(result.Halts) != 2 {
			t.Fatalf("appeal %d %s", status, body)
		}
		lifted := map[server.Id]bool{}
		for _, halt := range result.Halts {
			lifted[halt.HaltId] = halt.Lifted
		}
		if !lifted[server.Id(duplicate.HaltId)] || lifted[server.Id(holding.HaltId)] {
			t.Fatalf("lifted %v", lifted)
		}
		if len(abuseLifts) != 1 || abuseLifts[0] != server.Id(duplicate.HaltId) {
			t.Fatalf("§21 lifted %v", abuseLifts)
		}
		halts, err := model.FindSmtpHalts(ctx, world.networkId, now.Add(-time.Hour*48))
		if err != nil || len(halts) != 2 {
			t.Fatalf("halts left %v, %v", halts, err)
		}
		unknown := server.NewId()
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/appeal", world.networkBearer, &SmtpAppealArgs{HaltId: &unknown}); status != http.StatusNotFound {
			t.Fatalf("an unknown halt answered %d", status)
		}
		endedId := server.Id(ended.HaltId)
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/appeal", world.networkBearer, &SmtpAppealArgs{HaltId: &endedId}); status != http.StatusNotFound {
			t.Fatalf("an ended halt answered %d", status)
		}
		smtpAppealLadderOf = func() (smtpAppealLadder, error) {
			return nil, errors.New("no smtpedge.yml")
		}
		if status, _ := testSmtpCall(t, ctx, "POST", "/smtp/appeal", world.networkBearer, &SmtpAppealArgs{}); status != http.StatusServiceUnavailable {
			t.Fatalf("with no ladder answered %d", status)
		}
	})
}
