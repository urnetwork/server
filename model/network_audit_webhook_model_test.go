//go:build sidecar_pending

package model

// §7.11's audit reads and SIEM webhooks (TEAM-M6-09), on Postgres with MG1 and
// MG6, the sender, the clock and the vault key injected.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// One delivery a fake sender was handed.
type testWebhookSend struct {
	url    string
	header http.Header
	body   []byte
}

// A sender that records each delivery and answers err.
type testWebhookSender struct {
	stateLock sync.Mutex
	sends     []*testWebhookSend
	err       error
}

func (self *testWebhookSender) Send(ctx context.Context, targetUrl string, header http.Header, body []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sends = append(self.sends, &testWebhookSend{url: targetUrl, header: header.Clone(), body: bytes.Clone(body)})
	return self.err
}

func (self *testWebhookSender) taken() []*testWebhookSend {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sends := self.sends
	self.sends = nil
	return sends
}

// The audit routes over a fake clock, the tier check passed.
type testAudits struct {
	*networkAudits
	clock time.Time
}

func newTestAudits() *testAudits {
	self := &testAudits{clock: time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)}
	self.networkAudits = &networkAudits{
		settings:        DefaultNetworkAuditWebhookSettings(),
		eventSettings:   DefaultNetworkAdminEventSettings(),
		now:             func() time.Time { return self.clock },
		requireTeamTier: func(ctx context.Context, networkId server.Id) error { return nil },
	}
	return self
}

// Appends count admin events to the network's log.
func addTestAdminEvents(ctx context.Context, networkId server.Id, count int) {
	for i := 0; i < count; i += 1 {
		server.Tx(ctx, func(tx server.PgTx) {
			targetId := server.NewId()
			AddAdminEventInTx(ctx, tx, &AdminEvent{
				NetworkId:  networkId,
				ActorKind:  AdminEventActorSystem,
				Action:     AdminEventActionAuthKeyCreate,
				TargetKind: AdminEventTargetAuthKey,
				TargetId:   &targetId,
				Result:     AdminEventResultOk,
			})
		})
	}
}

func decodeTestDelivery(t testing.TB, body []byte) *NetworkAuditWebhookDelivery {
	t.Helper()
	delivery := &NetworkAuditWebhookDelivery{}
	if err := json.Unmarshal(body, delivery); err != nil {
		t.Fatal(err)
	}
	return delivery
}

// Auditors and above read the log, paged by seq, and the chain verifies from
// what a reader is answered; a Member, a pending Auditor, a client token and a
// personal network's owner are refused 403, and a network outside the tier
// gets the QuotaRefusal.
func TestAuditorReadsAdminEventsMemberRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, _ := newTestSignInTeam(t, ctx, false)
		audits := newTestAudits()
		addTestAdminEvents(ctx, networkId, 5)

		for _, role := range []NetworkMemberRole{
			NetworkMemberRoleOwner,
			NetworkMemberRoleAdmin,
			NetworkMemberRoleNetworkAdmin,
			NetworkMemberRoleItAdmin,
			NetworkMemberRoleBillingAdmin,
			NetworkMemberRoleAuditor,
		} {
			result, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, newTestTeamMemberSession(t, ctx, networkId, role, false))
			if err != nil || len(result.Events) != 5 || result.More {
				t.Fatalf("%s read %+v %v", role, result, err)
			}
		}
		auditorSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAuditor, false)

		// paged by seq
		firstPage, err := audits.getEvents(&GetNetworkAdminEventsArgs{Limit: 2}, auditorSession)
		if err != nil || len(firstPage.Events) != 2 || !firstPage.More || firstPage.Events[0].Seq != 1 || firstPage.Events[1].Seq != 2 {
			t.Fatalf("the first page: %+v %v", firstPage, err)
		}
		rest, err := audits.getEvents(&GetNetworkAdminEventsArgs{After: 2, Limit: 3}, auditorSession)
		if err != nil || len(rest.Events) != 3 || rest.More || rest.Events[0].Seq != 3 {
			t.Fatalf("the rest: %+v %v", rest, err)
		}
		past, err := audits.getEvents(&GetNetworkAdminEventsArgs{After: 5}, auditorSession)
		if err != nil || len(past.Events) != 0 || past.More {
			t.Fatalf("past the head: %+v %v", past, err)
		}
		// the log's read limit bounds a page whatever is asked
		audits.eventSettings.ReadLimit = 4
		bounded, err := audits.getEvents(&GetNetworkAdminEventsArgs{Limit: 1000}, auditorSession)
		if err != nil || len(bounded.Events) != 4 || !bounded.More {
			t.Fatalf("the bounded page: %+v %v", bounded, err)
		}
		audits.eventSettings.ReadLimit = 1000

		// the chain verifies from the answer alone, as JSON carried it, a page
		// anchored on the page before it
		all, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, auditorSession)
		connect.AssertEqual(t, err, nil)
		wire, err := json.Marshal(all)
		connect.AssertEqual(t, err, nil)
		received := &GetNetworkAdminEventsResult{}
		connect.AssertEqual(t, json.Unmarshal(wire, received), nil)
		connect.AssertEqual(t, VerifyNetworkAdminEventResults(networkId, received.Events), nil)
		connect.AssertEqual(t, received.Events[0].PrevSha256, strings.Repeat("0", 64))
		connect.AssertEqual(t, VerifyNetworkAdminEventResults(networkId, append([]*NetworkAdminEventResult{firstPage.Events[1]}, rest.Events...)), nil)
		// an event changed, dropped or moved to another network's log breaks it
		tampered := *received.Events[2]
		tampered.Action = AdminEventActionAuthKeyRevoke
		for label, run := range map[string][]*NetworkAdminEventResult{
			"changed": {received.Events[0], received.Events[1], &tampered, received.Events[3]},
			"dropped": {received.Events[0], received.Events[2]},
			"swapped": {received.Events[1], received.Events[0]},
		} {
			if err := VerifyNetworkAdminEventResults(networkId, run); !errors.Is(err, errAdminEventChainBroken) {
				t.Errorf("a %s event verified: %v", label, err)
			}
		}
		if err := VerifyNetworkAdminEventResults(server.NewId(), received.Events); !errors.Is(err, errAdminEventChainBroken) {
			t.Errorf("another network's log verified: %v", err)
		}
		// no event carries an address or a name: ids and fixed words only
		if strings.Contains(string(wire), "192.0.2.") || strings.Contains(string(wire), "team-") {
			t.Fatalf("an event names an address or a network: %s", wire)
		}

		clientSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAuditor, false)
		clientId := server.NewId()
		clientSession.ByJwt.ClientId = &clientId
		personalUserId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)
		personalSession := session.NewLocalClientSession(ctx, "192.0.2.41:1", session.NewByJwt(personalNetworkId, personalUserId, "personal", false, false))
		defer personalSession.Cancel()
		for label, refusedSession := range map[string]*session.ClientSession{
			"member":          newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleMember, false),
			"pending auditor": newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAuditor, true),
			"client token":    clientSession,
			"personal":        personalSession,
		} {
			result, err := audits.getEvents(&GetNetworkAdminEventsArgs{}, refusedSession)
			if result != nil || err == nil || !strings.HasPrefix(err.Error(), "403 ") {
				t.Errorf("%s read %+v %v, want 403", label, result, err)
			}
		}
		outside := newTestAudits()
		outside.requireTeamTier = RequireTeamTier
		result, err := outside.getEvents(&GetNetworkAdminEventsArgs{}, auditorSession)
		refusal := &QuotaRefusalError{}
		if result != nil || !errors.As(err, &refusal) || !refusal.Refusal.Upgrade {
			t.Fatalf("outside the tier answered %+v %v", result, err)
		}
	})
}

// The signature and the key derivation against values computed apart from
// this code: HMAC-SHA256 under the key of "<t>.<body>", and under the root key
// of the derivation's domain and the webhook id.
func TestAuditWebhookSignatureVector(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	body := []byte(`{"webhook_id":"018f0000-0000-7000-8000-0000000000aa","network_id":"018f0000-0000-7000-8000-0000000000bb","events":[]}`)
	signTime := time.Unix(1700000000, 0)
	signature := SignNetworkAuditWebhook(key, signTime, body)
	connect.AssertEqual(t, signature, "t=1700000000,v1=2c5e07aee2574f8d1e0261e77f613caec644d50e8cb8b77ce9500bc56d04c4c0")

	connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(key, signature, body, signTime.Add(time.Minute), 5*time.Minute), true)
	// another body, another key, a stale t and a t rewritten without its MAC
	connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(key, signature, append(bytes.Clone(body), ' '), signTime, 5*time.Minute), false)
	otherKey := bytes.Repeat([]byte{9}, 32)
	connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(otherKey, signature, body, signTime, 5*time.Minute), false)
	connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(key, signature, body, signTime.Add(6*time.Minute), 5*time.Minute), false)
	replayed := strings.Replace(signature, "t=1700000000", "t=1700000300", 1)
	connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(key, replayed, body, signTime.Add(5*time.Minute), 5*time.Minute), false)
	connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(key, "", body, signTime, 5*time.Minute), false)

	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	webhookId := server.RequireParseId("018f0000-0000-7000-8000-0000000000aa")
	connect.AssertEqual(
		t,
		hex.EncodeToString(deriveNetworkAuditWebhookKey(rootKey, webhookId)),
		"b956cb7dfdbfa2296a29f33d235c36ba68e5d1d5c7d3e74b4d8dc813c81127e5",
	)
	// each webhook's key is its own
	if bytes.Equal(deriveNetworkAuditWebhookKey(rootKey, webhookId), deriveNetworkAuditWebhookKey(rootKey, server.NewId())) {
		t.Fatal("two webhooks share a key")
	}
}

// A private-address target is refused when a webhook is added: only an https
// URL whose host is public passes.
func TestAuditWebhookUrlMustBePublicHttps(t *testing.T) {
	for _, webhookUrl := range []string{
		"https://siem.example/hooks/urnetwork",
		"https://siem.example:8443/hook?tenant=7",
		"https://203.0.113.9/hook",
		"https://[2001:db8::9]/hook",
	} {
		if err := ValidNetworkAuditWebhookUrl(webhookUrl); err != nil {
			t.Errorf("%q refused: %v", webhookUrl, err)
		}
	}
	for _, refused := range []string{
		"",
		"http://siem.example/hook",
		"ftp://siem.example/hook",
		"https://user:pass@siem.example/hook",
		"https://siem.example/hook#frag",
		"https://127.0.0.1/hook",
		"https://127.0.0.1:8443/hook",
		"https://10.0.0.8/hook",
		"https://172.16.3.4/hook",
		"https://192.168.1.1/hook",
		"https://169.254.169.254/latest/meta-data",
		"https://100.64.0.1/hook",
		"https://0.0.0.0/hook",
		"https://[::1]/hook",
		"https://[fe80::1]/hook",
		"https://[::ffff:10.0.0.1]/hook",
		"https://localhost/hook",
		"https://LOCALHOST./hook",
		"https://api.localhost/hook",
		"https://intranet/hook",
		"https:///hook",
		"https://siem.example/" + strings.Repeat("a", 2048),
	} {
		if ValidNetworkAuditWebhookUrl(refused) == nil {
			t.Errorf("%q passed", refused)
		}
	}
}

// api's HTTP sender posts the body and headers and takes only a 2xx; it
// follows no redirect; and with its own guard it neither accepts a
// private-address target nor dials one a name would resolve to.
func TestAuditWebhookSenderGuardsItsTarget(t *testing.T) {
	type received struct {
		path   string
		header http.Header
		body   []byte
	}
	var stateLock sync.Mutex
	requests := []*received{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stateLock.Lock()
		defer stateLock.Unlock()
		requests = append(requests, &received{path: r.URL.Path, header: r.Header.Clone(), body: body})
	}
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		http.Error(w, "no", http.StatusInternalServerError)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
	})
	receiver := httptest.NewTLSServer(mux)
	defer receiver.Close()
	taken := func() []*received {
		stateLock.Lock()
		defer stateLock.Unlock()
		taken := requests
		requests = nil
		return taken
	}
	ctx := t.Context()
	header := http.Header{}
	header.Set(NetworkAuditWebhookSignatureHeader, "t=1,v1=00")
	body := []byte(`{"events":[]}`)

	// a receiver on loopback, reachable only with the guard lifted
	open := DefaultHttpNetworkAuditWebhookSenderSettings()
	open.TargetCheck = func(targetUrl string) error { return nil }
	open.DialControl = nil
	open.Transport = receiver.Client().Transport.(*http.Transport)
	openSender := NewHttpNetworkAuditWebhookSender(open)
	connect.AssertEqual(t, openSender.Send(ctx, receiver.URL+"/ok", header, body), nil)
	got := taken()
	if len(got) != 1 || !bytes.Equal(got[0].body, body) || got[0].header.Get(NetworkAuditWebhookSignatureHeader) != "t=1,v1=00" {
		t.Fatalf("the receiver got %+v", got)
	}
	if openSender.Send(ctx, receiver.URL+"/fail", header, body) == nil {
		t.Fatal("a 500 was taken as delivered")
	}
	taken()
	// a redirect is an answer that is not 2xx, and its target is never asked
	if openSender.Send(ctx, receiver.URL+"/redirect", header, body) == nil {
		t.Fatal("a redirect was taken as delivered")
	}
	got = taken()
	if len(got) != 1 || got[0].path != "/redirect" {
		t.Fatalf("the sender followed a redirect: %+v", got)
	}

	// the default sender refuses the loopback target outright
	guarded := DefaultHttpNetworkAuditWebhookSenderSettings()
	guarded.Transport = receiver.Client().Transport.(*http.Transport)
	if NewHttpNetworkAuditWebhookSender(guarded).Send(ctx, receiver.URL+"/ok", header, body) == nil {
		t.Fatal("the guarded sender posted to a loopback URL")
	}
	// and, were a public-looking name to resolve there, refuses the dial
	rebound := DefaultHttpNetworkAuditWebhookSenderSettings()
	rebound.TargetCheck = func(targetUrl string) error { return nil }
	rebound.Transport = receiver.Client().Transport.(*http.Transport)
	if NewHttpNetworkAuditWebhookSender(rebound).Send(ctx, receiver.URL+"/ok", header, body) == nil {
		t.Fatal("the guarded sender dialed a loopback address")
	}
	if got := taken(); len(got) != 0 {
		t.Fatalf("a guarded delivery reached the receiver: %+v", got)
	}
	// plain http is refused whatever the host
	if NewHttpNetworkAuditWebhookSender(DefaultHttpNetworkAuditWebhookSenderSettings()).Send(ctx, "http://siem.example/hook", header, body) == nil {
		t.Fatal("the sender posted over http")
	}
}

// The Owner adds a webhook and is answered its key once; deliveries carry the
// events from its creation on, oldest first, signed under that key; a second
// pass sends nothing; a backlog goes in batches; and the Owner lists and
// removes it. Adding owes Owners and Admins the notice, which names no target.
func TestAuditWebhookDelivers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		defer Testing_SetNetworkAuditWebhookRootKey(bytes.Repeat([]byte{5}, 32))()
		audits := newTestAudits()
		sender := &testWebhookSender{}
		// events before the webhook exists are not its to send
		addTestAdminEvents(ctx, networkId, 3)

		added, err := audits.setWebhook(&NetworkAuditWebhookArgs{Url: "https://siem.example/hooks/urnetwork"}, ownerSession)
		if err != nil || added.Error != nil || added.WebhookId == nil || added.SigningKey == "" || len(added.Webhooks) != 1 {
			t.Fatalf("add: %v %+v", err, added.Error)
		}
		webhookId := *added.WebhookId
		signingKey, err := base64.RawURLEncoding.DecodeString(added.SigningKey)
		if err != nil || len(signingKey) != 32 {
			t.Fatalf("the signing key: %v", err)
		}
		connect.AssertEqual(t, added.Webhooks[0].DeliveredSeq, int64(3))
		// neither the key nor the URL prints with the result
		if printed := added.String() + added.GoString(); strings.Contains(printed, added.SigningKey) || strings.Contains(printed, "siem.example") {
			t.Fatalf("the result prints %s", printed)
		}
		notices := unsentTestNotices(t, ctx, networkId, SecurityNetworkAuthorityTemplateName)
		if len(notices) == 0 {
			t.Fatal("no notice is owed")
		}
		for _, notice := range notices {
			changes := notice.changes(t)
			if len(changes) != 1 || changes[0]["action"] != SecurityNetworkAuthorityAuditWebhook || strings.Contains(notice.templateJson, "siem.example") {
				t.Fatalf("the notice: %s", notice.templateJson)
			}
		}

		// the first delivery is the webhook's own creation
		result := audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 1)
		sends := sender.taken()
		if len(sends) != 1 || sends[0].url != "https://siem.example/hooks/urnetwork" {
			t.Fatalf("the first pass sent %+v", sends)
		}
		connect.AssertEqual(t, sends[0].header.Get(NetworkAuditWebhookIdHeader), webhookId.String())
		connect.AssertEqual(t, sends[0].header.Get("Content-Type"), "application/json")
		connect.AssertEqual(t, VerifyNetworkAuditWebhookSignature(signingKey, sends[0].header.Get(NetworkAuditWebhookSignatureHeader), sends[0].body, audits.clock, time.Minute), true)
		delivery := decodeTestDelivery(t, sends[0].body)
		if delivery.WebhookId != webhookId || delivery.NetworkId != networkId || len(delivery.Events) != 1 ||
			delivery.Events[0].Seq != 4 || delivery.Events[0].Action != AdminEventActionAuditWebhookAdd ||
			delivery.Events[0].TargetId == nil || *delivery.Events[0].TargetId != webhookId {
			t.Fatalf("the first delivery: %s", sends[0].body)
		}
		connect.AssertEqual(t, VerifyNetworkAdminEventResults(networkId, delivery.Events), nil)
		connect.AssertEqual(t, GetNetworkAuditWebhook(ctx, webhookId).DeliveredSeq, int64(4))

		// caught up: nothing to send
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 0)
		connect.AssertEqual(t, len(sender.taken()), 0)

		// a backlog goes oldest first, a batch at a time, within the pass's bound
		addTestAdminEvents(ctx, networkId, 5)
		audits.settings.BatchEventCount = 2
		audits.settings.PassBatchCount = 2
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 2)
		seqs := []int64{}
		for _, send := range sender.taken() {
			for _, event := range decodeTestDelivery(t, send.body).Events {
				seqs = append(seqs, event.Seq)
			}
		}
		connect.AssertEqual(t, seqs, []int64{5, 6, 7, 8})
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 1)
		sends = sender.taken()
		if len(sends) != 1 || decodeTestDelivery(t, sends[0].body).Events[0].Seq != 9 {
			t.Fatalf("the rest of the backlog: %+v", sends)
		}

		// the list, then the removal, after which nothing is sent
		listed, err := audits.setWebhook(&NetworkAuditWebhookArgs{}, ownerSession)
		if err != nil || listed.Error != nil || len(listed.Webhooks) != 1 || listed.Webhooks[0].DeliveredSeq != 9 || listed.SigningKey != "" || listed.WebhookId != nil {
			t.Fatalf("list: %v %+v", err, listed.Webhooks)
		}
		removed, err := audits.setWebhook(&NetworkAuditWebhookArgs{RemoveWebhookId: &webhookId}, ownerSession)
		if err != nil || !removed.Removed || len(removed.Webhooks) != 0 {
			t.Fatalf("remove: %v %+v", err, removed.Webhooks)
		}
		again, err := audits.setWebhook(&NetworkAuditWebhookArgs{RemoveWebhookId: &webhookId}, ownerSession)
		if err != nil || again.Removed {
			t.Fatalf("remove again: %v", err)
		}
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 0)
		connect.AssertEqual(t, len(sender.taken()), 0)
		actions := testAdminEventActions(ctx, networkId)
		connect.AssertEqual(t, actions[len(actions)-1], AdminEventActionAuditWebhookRemove)
		connect.AssertEqual(t, VerifyNetworkAdminEventChain(ctx, networkId), nil)
	})
}

// Retry to 24 hours on a fake clock: a failing receiver is tried again with a
// growing wait, never before its next time, and its last try lands at 24
// hours; then the run's events are skipped, the skip is an admin event, and
// that event is what a receiver back in service is sent.
func TestAuditWebhookRetriesFor24Hours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		defer Testing_SetNetworkAuditWebhookRootKey(bytes.Repeat([]byte{5}, 32))()
		audits := newTestAudits()
		sender := &testWebhookSender{err: errors.New("synthetic receiver failure")}
		added, err := audits.setWebhook(&NetworkAuditWebhookArgs{Url: "https://siem.example/hook"}, ownerSession)
		if err != nil || added.Error != nil {
			t.Fatalf("add: %v %+v", err, added.Error)
		}
		webhookId := *added.WebhookId
		addTestAdminEvents(ctx, networkId, 2)
		runStart := audits.clock

		// the first failure starts the run and waits the base delay
		result := audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.FailedCount, 1)
		connect.AssertEqual(t, len(sender.taken()), 1)
		webhook := GetNetworkAuditWebhook(ctx, webhookId)
		if webhook.FailTime == nil || !webhook.FailTime.Equal(runStart) || webhook.NextTime == nil || !webhook.NextTime.Equal(runStart.Add(time.Minute)) {
			t.Fatalf("after the first failure: %+v", webhook)
		}
		connect.AssertEqual(t, webhook.DeliveredSeq, int64(0))

		// a pass before the next time tries nothing
		audits.clock = runStart.Add(30 * time.Second)
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.FailedCount, 0)
		connect.AssertEqual(t, len(sender.taken()), 0)

		// each try at its next time: the waits grow to the cap and the last
		// try lands at the window's end
		tries := 1
		waits := []time.Duration{}
		for {
			webhook = GetNetworkAuditWebhook(ctx, webhookId)
			if webhook.NextTime == nil {
				break
			}
			waits = append(waits, webhook.NextTime.Sub(audits.clock))
			audits.clock = *webhook.NextTime
			if runStart.Add(24 * time.Hour).Before(audits.clock) {
				t.Fatalf("a try is due at %s, past the window", audits.clock.Sub(runStart))
			}
			result = audits.deliver(ctx, audits.clock, nil, sender)
			connect.AssertEqual(t, len(sender.taken()), 1)
			tries += 1
			if result.SkippedCount == 1 {
				break
			}
			connect.AssertEqual(t, result.FailedCount, 1)
			if 100 < tries {
				t.Fatal("the run does not end")
			}
		}
		connect.AssertEqual(t, audits.clock, runStart.Add(24*time.Hour))
		// 1 min doubling to the 1 h cap, then hourly
		connect.AssertEqual(t, waits[1:8], []time.Duration{
			time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour,
		})
		for _, wait := range waits {
			if wait <= 0 || time.Hour < wait {
				t.Fatalf("a wait of %s", wait)
			}
		}

		// skipped: the cursor is past the run's events, the run is over and
		// the skip is the log's newest event
		webhook = GetNetworkAuditWebhook(ctx, webhookId)
		if webhook == nil || webhook.FailTime != nil || webhook.NextTime != nil || webhook.DeliveredSeq != 3 {
			t.Fatalf("after the skip: %+v", webhook)
		}
		events := GetNetworkAdminEvents(ctx, networkId, 0, 0)
		skip := events[len(events)-1]
		if skip.Seq != 4 || skip.Action != AdminEventActionAuditWebhookSkip || skip.ActorKind != AdminEventActorSystem || skip.TargetId == nil || *skip.TargetId != webhookId {
			t.Fatalf("the skip event: %+v", skip)
		}

		// the receiver is back: it is sent the skip, not what was skipped
		sender.err = nil
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 1)
		sends := sender.taken()
		delivery := decodeTestDelivery(t, sends[0].body)
		if len(sends) != 1 || len(delivery.Events) != 1 || delivery.Events[0].Seq != 4 {
			t.Fatalf("after the receiver came back: %s", sends[0].body)
		}
		// the skipped events are still the log's, for a reader
		connect.AssertEqual(t, VerifyNetworkAdminEventChain(ctx, networkId), nil)

		// a success mid-run ends the run
		sender.err = errors.New("synthetic receiver failure")
		addTestAdminEvents(ctx, networkId, 1)
		audits.deliver(ctx, audits.clock, nil, sender)
		if webhook = GetNetworkAuditWebhook(ctx, webhookId); webhook.FailTime == nil {
			t.Fatal("a failure started no run")
		}
		sender.err = nil
		audits.clock = *webhook.NextTime
		result = audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, result.DeliveredCount, 1)
		if webhook = GetNetworkAuditWebhook(ctx, webhookId); webhook.FailTime != nil || webhook.NextTime != nil || webhook.DeliveredSeq != 5 {
			t.Fatalf("after a success mid-run: %+v", webhook)
		}
	})
}

// The webhook route is the Owner's; a private-address target, both an add and
// a removal, a webhook past the limit, another network's webhook and an add
// while vault holds no key are each refused with nothing stored; and with no
// key nothing is delivered.
func TestAuditWebhookRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, ownerSession := newTestDomainTeam(t, ctx)
		restoreKey := Testing_SetNetworkAuditWebhookRootKey(bytes.Repeat([]byte{5}, 32))
		defer restoreKey()
		audits := newTestAudits()
		webhookCount := func(countNetworkId server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_audit_webhook WHERE network_id = $1`, countNetworkId).Scan(&count))
			})
			return
		}

		clientSession := newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleOwner, false)
		clientId := server.NewId()
		clientSession.ByJwt.ClientId = &clientId
		for label, refusedSession := range map[string]*session.ClientSession{
			"admin":         newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAdmin, false),
			"auditor":       newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleAuditor, false),
			"member":        newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleMember, false),
			"pending owner": newTestTeamMemberSession(t, ctx, networkId, NetworkMemberRoleOwner, true),
			"client token":  clientSession,
		} {
			for _, args := range []*NetworkAuditWebhookArgs{{Url: "https://siem.example/hook"}, {}} {
				result, err := audits.setWebhook(args, refusedSession)
				if result != nil || err == nil || !strings.HasPrefix(err.Error(), "403 ") {
					t.Errorf("%s was answered %+v %v, want 403", label, result, err)
				}
			}
		}
		connect.AssertEqual(t, webhookCount(networkId), 0)

		someId := server.NewId()
		for label, args := range map[string]*NetworkAuditWebhookArgs{
			"loopback":       {Url: "https://127.0.0.1/hook"},
			"private":        {Url: "https://10.1.2.3/hook"},
			"metadata":       {Url: "https://169.254.169.254/hook"},
			"localhost":      {Url: "https://localhost/hook"},
			"http":           {Url: "http://siem.example/hook"},
			"add and remove": {Url: "https://siem.example/hook", RemoveWebhookId: &someId},
		} {
			result, err := audits.setWebhook(args, ownerSession)
			if err != nil || result.Error == nil || result.SigningKey != "" || result.WebhookId != nil {
				t.Errorf("%s was answered %+v %v", label, result.Error, err)
			}
		}
		connect.AssertEqual(t, webhookCount(networkId), 0)
		connect.AssertEqual(t, len(testAdminEventActions(ctx, networkId)), 0)

		// no vault key: no webhook, and nothing delivered for one that exists
		Testing_SetNetworkAuditWebhookRootKey(nil)
		result, err := audits.setWebhook(&NetworkAuditWebhookArgs{Url: "https://siem.example/hook"}, ownerSession)
		if err != nil || result.Error == nil || result.SigningKey != "" {
			t.Fatalf("a webhook was added with no key: %+v %v", result.Error, err)
		}
		connect.AssertEqual(t, webhookCount(networkId), 0)
		Testing_SetNetworkAuditWebhookRootKey(bytes.Repeat([]byte{5}, 32))

		// the limit
		audits.settings.WebhookLimit = 2
		webhookIds := []server.Id{}
		for i := 0; i < 2; i += 1 {
			result, err := audits.setWebhook(&NetworkAuditWebhookArgs{Url: "https://siem.example/hook"}, ownerSession)
			if err != nil || result.Error != nil {
				t.Fatalf("add %d: %+v %v", i, result.Error, err)
			}
			webhookIds = append(webhookIds, *result.WebhookId)
		}
		over, err := audits.setWebhook(&NetworkAuditWebhookArgs{Url: "https://siem.example/hook"}, ownerSession)
		if err != nil || over.Error == nil || len(over.Webhooks) != 2 {
			t.Fatalf("add past the limit: %+v %v", over.Error, err)
		}
		connect.AssertEqual(t, webhookCount(networkId), 2)

		// another team's Owner cannot remove it
		_, otherOwnerSession := newTestDomainTeam(t, ctx)
		other, err := audits.setWebhook(&NetworkAuditWebhookArgs{RemoveWebhookId: &webhookIds[0]}, otherOwnerSession)
		if err != nil || other.Removed || len(other.Webhooks) != 0 {
			t.Fatalf("another network's removal: %+v %v", other, err)
		}
		connect.AssertEqual(t, webhookCount(networkId), 2)

		// with the key gone, a due webhook is sent nothing
		sender := &testWebhookSender{}
		Testing_SetNetworkAuditWebhookRootKey(nil)
		delivered := audits.deliver(ctx, audits.clock, nil, sender)
		connect.AssertEqual(t, delivered.DeliveredCount, 0)
		connect.AssertEqual(t, len(sender.taken()), 0)
		// with it back, a pass over a bound of one webhook hands on its cursor
		Testing_SetNetworkAuditWebhookRootKey(bytes.Repeat([]byte{5}, 32))
		audits.settings.PassWebhookCount = 1
		first := audits.deliver(ctx, audits.clock, nil, sender)
		if first.DeliveredCount != 1 || first.AfterWebhookId == nil {
			t.Fatalf("the bounded pass: %+v", first)
		}
		second := audits.deliver(ctx, audits.clock, first.AfterWebhookId, sender)
		connect.AssertEqual(t, second.DeliveredCount, 1)
		connect.AssertEqual(t, len(sender.taken()), 2)

		// outside the team tier
		outside := newTestAudits()
		outside.requireTeamTier = RequireTeamTier
		refused, err := outside.setWebhook(&NetworkAuditWebhookArgs{}, ownerSession)
		refusal := &QuotaRefusalError{}
		if refused != nil || !errors.As(err, &refusal) || !refusal.Refusal.Upgrade {
			t.Fatalf("outside the tier answered %+v %v", refused, err)
		}
	})
}

// The sweep of the Team routes for admin actions that wrote no event: a first
// IdP sign-in's pending membership, which the server makes, is in the team's
// log, once, by the user that signed in; a later sign-in adds nothing.
func TestJitMembershipWritesAnAdminEvent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		idp := &NetworkIdp{IdpId: server.NewId(), NetworkId: teamId, Issuer: "https://idp.example", ClientId: "c", ClientAuth: NetworkIdpClientAuthClientSecret}
		userId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "password-account-"+userId.String(), userId)
		identity := &NetworkOidcIdentity{Sub: "employee-7", Email: "employee@team.example", EmailVerified: true}
		now := time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)
		for i := 0; i < 2; i += 1 {
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := ResolveNetworkSsoLoginInTx(ctx, tx, idp, identity, &userId, now, DefaultNetworkIdpSettings())
				server.Raise(err)
			})
		}
		member := GetNetworkMemberOfUser(ctx, teamId, userId)
		events := GetNetworkAdminEvents(ctx, teamId, 0, 0)
		if member == nil || len(events) != 1 {
			t.Fatalf("the JIT member %+v wrote %d events", member, len(events))
		}
		event := events[0]
		if event.Action != AdminEventActionMemberAdd || event.ActorKind != AdminEventActorUser || event.ActorId == nil || *event.ActorId != userId ||
			event.TargetKind != AdminEventTargetMember || event.TargetId == nil || *event.TargetId != member.MemberId {
			t.Fatalf("the JIT member's event: %+v", event)
		}
	})
}
