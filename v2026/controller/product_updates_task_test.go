// Exercise subscription acknowledgements against a private HTTP fixture.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/reengagement"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Serialized user identity selects the account's preference. A provider
// failure remains retryable; opting out removes the user from both lists.
func TestUserProductUpdatesTaskHonorsPreferenceAndProviderFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId, userId := server.NewId(), server.NewId()
		email := model.Testing_CreateNetwork(ctx, networkId, "synthetic-user-updates", userId)
		preference := func(enabled bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_preferences(network_id,product_updates)
					VALUES($1,$2) ON CONFLICT(network_id) DO UPDATE SET product_updates=$2`, networkId, enabled))
			})
		}
		preference(true)
		previousNetworks, previousUpdates := newNetworksListId, productUpdatesListId
		newNetworksListId, productUpdatesListId = func() int { return 101 }, func() int { return 102 }
		defer func() { newNetworksListId, productUpdatesListId = previousNetworks, previousUpdates }()
		var fail atomic.Bool
		var adds, removals atomic.Int64
		fail.Store(true)
		newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodPost {
				t.Error("unexpected list method", r.Method)
				w.WriteHeader(400)
				return
			}
			if r.URL.Path == "/v3/contacts" {
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":1}`))
				return
			}
			var body BrevoListArgs
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Emails) != 1 || body.Emails[0] != email {
				t.Error("task lost its serialized user identity")
				w.WriteHeader(400)
				return
			}
			switch r.URL.Path {
			case "/v3/contacts/lists/102/contacts/add":
				adds.Add(1)
				if fail.Load() {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"code":"synthetic_unavailable"}`))
					return
				}
			case "/v3/contacts/lists/101/contacts/remove", "/v3/contacts/lists/102/contacts/remove":
				removals.Add(1)
			default:
				t.Error("unexpected list path", r.URL.Path)
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(BrevoListResult{Contacts: &BrevoListResultContacts{Success: body.Emails}})
		}))
		owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		owner.ClientAddress = ""
		defer owner.Cancel()
		target := task.NewTaskTargetWithPost(SyncProductUpdatesForUser, SyncProductUpdatesForUserPost)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		queue := func() server.Id {
			return task.ScheduleTask(SyncProductUpdatesForUser, &SyncProductUpdatesForUserArgs{}, owner,
				task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
		}
		id := queue()
		if _, _, err := target.RunSpecific(ctx, task.GetTasks(ctx, id)[id]); err == nil {
			tb.Fatal("provider failure acknowledged")
		}
		fail.Store(false)
		for _, optOut := range []bool{false, true} {
			if optOut {
				preference(false)
				id = queue()
			}
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
				tb.Fatal("user preference task did not finish", finished, retried, posts, err)
			}
			if !task.GetFinishedTasks(ctx, id)[id].PostCompleted {
				tb.Fatal("one-shot preference task lost Post completion")
			}
		}
		if adds.Load() != 2 || removals.Load() != 2 {
			tb.Fatal("wrong preference effects", adds.Load(), removals.Load())
		}
	})
}

// The old goroutines logged provider errors and returned nil. Each successful
// marker must survive that partial failure, while the failed identity retries.
func TestInitialProductUpdatesRetainsPartialFailureAndReplays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		healthyNetwork, failedNetwork := server.NewId(), server.NewId()
		healthyUser, failedUser := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, healthyNetwork, "synthetic-updates-healthy", healthyUser)
		failedEmail := model.Testing_CreateNetwork(ctx, failedNetwork, "synthetic-updates-retry", failedUser)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_preferences(network_id,product_updates)
				VALUES($1,true),($2,true) ON CONFLICT(network_id) DO UPDATE SET product_updates=true`, healthyNetwork, failedNetwork))
		})
		previousNetworks, previousUpdates := newNetworksListId, productUpdatesListId
		newNetworksListId, productUpdatesListId = func() int { return 101 }, func() int { return 102 }
		defer func() { newNetworksListId, productUpdatesListId = previousNetworks, previousUpdates }()
		var fail atomic.Bool
		fail.Store(true)
		var requests atomic.Int64
		newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost && r.URL.Path == "/v3/contacts" {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]int{"id": 1})
				return
			}
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/contacts/add") {
				var body BrevoListArgs
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Emails) != 1 {
					t.Error("invalid fixture list request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body.Emails[0] == failedEmail && fail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": "synthetic_unavailable"})
					return
				}
				_ = json.NewEncoder(w).Encode(BrevoListResult{Contacts: &BrevoListResultContacts{Success: body.Emails}})
				return
			}
			t.Error("unexpected fixture request", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}))
		verify := func(all bool) {
			server.Db(ctx, func(conn server.PgConn) {
				var correct bool
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT count(*)=2 AND bool_and(product_updates_sync=(network_id=$1 OR $5)) FROM network WHERE network_id IN ($1,$2)) AND
					(SELECT count(*)=2 AND bool_and(product_updates_sync=(user_id=$3 OR $5)) FROM network_user_auth_password WHERE user_id IN ($3,$4))`,
					healthyNetwork, failedNetwork, healthyUser, failedUser, all).Scan(&correct))
				if !correct {
					tb.Fatal("provider acknowledgement and committed markers disagree", all)
				}
			})
		}
		if err := SyncInitialProductUpdates(ctx); err == nil {
			tb.Fatal("partial provider failure was acknowledged")
		}
		verify(false)
		fail.Store(false)
		if err := SyncInitialProductUpdates(ctx); err != nil {
			tb.Fatal("failed identity did not recover", err)
		}
		verify(true)
		before := requests.Load()
		if err := SyncInitialProductUpdates(ctx); err != nil || requests.Load() != before {
			tb.Fatal("replay re-sent acknowledged subscriptions", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := SyncInitialProductUpdates(canceled); !errors.Is(err, context.Canceled) {
			tb.Fatal("canceled child goroutines were acknowledged", err)
		}
	})
}

// Retryable provider refusal stays an error; an absent contact is already
// removed, and a non-email identity must not reach the provider at all.
func TestRemoveProductUpdatesTaskPropagatesProviderOutcome(t *testing.T) {
	var status atomic.Int64
	var requests atomic.Int64
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodDelete || r.URL.Path != "/v3/contacts/synthetic@example.invalid" {
			t.Error("unexpected removal request", r.Method, r.URL.Path)
		}
		code := int(status.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code != http.StatusNoContent {
			providerCode := "synthetic_unavailable"
			if code == http.StatusNotFound {
				providerCode = "document_not_found"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"code": providerCode})
		}
	}))
	owner := session.NewLocalClientSession(t.Context(), "", nil)
	defer owner.Cancel()
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusNoContent, http.StatusNotFound} {
		status.Store(int64(code))
		result, err := RemoveProductUpdates(&RemoveProductUpdatesArgs{UserAuth: "synthetic@example.invalid"}, owner)
		if (err == nil) != (code != http.StatusServiceUnavailable) || err == nil && result == nil {
			t.Fatal("task changed provider outcome", code, result, err)
		}
	}
	before := requests.Load()
	if _, err := RemoveProductUpdates(&RemoveProductUpdatesArgs{UserAuth: "+12025550100"}, owner); err != nil || requests.Load() != before {
		t.Fatal("non-email removal contacted provider", err)
	}
}

// brevoListFixture is a fake Brevo that keeps one address's contact and list
// memberships the way the provider answers the sync's calls: a duplicate
// contact, an add to a list the address is on, and a removal from a list it is
// not on are the provider's refusals the sync treats as done.
type brevoListFixture struct {
	t            *testing.T
	email        string
	mutex        sync.Mutex
	contact      bool
	lists        map[string]bool
	failRemovals atomic.Bool
	requests     atomic.Int64
}

func newBrevoListFixture(t *testing.T, email string) *brevoListFixture {
	fixture := &brevoListFixture{t: t, email: email, lists: map[string]bool{}}
	writeJson := func(w http.ResponseWriter, statusCode int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(value)
	}
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		fixture.mutex.Lock()
		defer fixture.mutex.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v3/contacts" {
			var args BrevoContactArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args.Email != email {
				t.Error("contact request lost the synced identity")
				writeJson(w, http.StatusBadRequest, BrevoContactResult{Code: "invalid_json"})
				return
			}
			if fixture.contact {
				writeJson(w, http.StatusBadRequest, BrevoContactResult{Code: "duplicate_parameter"})
				return
			}
			fixture.contact = true
			writeJson(w, http.StatusCreated, BrevoContactResult{Id: 1})
			return
		}
		listId, operation, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v3/contacts/lists/"), "/contacts/")
		if r.Method != http.MethodPost || !ok || !strings.HasPrefix(r.URL.Path, "/v3/contacts/lists/") {
			t.Error("unexpected Brevo request", r.Method, r.URL.Path)
			writeJson(w, http.StatusBadRequest, BrevoContactResult{Code: "not_found"})
			return
		}
		var args BrevoListArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil || len(args.Emails) != 1 || args.Emails[0] != email {
			t.Error("list request lost the synced identity")
			writeJson(w, http.StatusBadRequest, BrevoListResult{Code: "invalid_json"})
			return
		}
		switch operation {
		case "add":
			if fixture.lists[listId] || !fixture.contact {
				writeJson(w, http.StatusBadRequest, BrevoListResult{Code: "invalid_parameter"})
				return
			}
			fixture.lists[listId] = true
		case "remove":
			if fixture.failRemovals.Load() {
				writeJson(w, http.StatusServiceUnavailable, BrevoListResult{Code: "synthetic_unavailable"})
				return
			}
			if !fixture.lists[listId] {
				writeJson(w, http.StatusBadRequest, BrevoListResult{Code: "invalid_parameter"})
				return
			}
			delete(fixture.lists, listId)
		default:
			t.Error("unexpected list operation", operation)
			writeJson(w, http.StatusNotFound, BrevoListResult{Code: "not_found"})
			return
		}
		writeJson(w, http.StatusOK, BrevoListResult{Contacts: &BrevoListResultContacts{Success: args.Emails}})
	}))
	return fixture
}

// on reports whether the address is on the list.
func (self *brevoListFixture) on(listId int) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.lists[strconv.Itoa(listId)]
}

func (self *brevoListFixture) put(listId int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.lists[strconv.Itoa(listId)] = true
}

// pendingProductUpdatesSyncs are the queued per-user syncs, by run-once key.
func pendingProductUpdatesSyncs(ctx context.Context) map[string]*task.Task {
	syncs := map[string]*task.Task{}
	for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if strings.HasPrefix(pending.RunOnceKey, `["sync_product_updates_for_user",`) {
			syncs[pending.RunOnceKey] = pending
		}
	}
	return syncs
}

// requireQueuedMirror returns the one sync queued for the user and checks the
// shape the mirror queues it in: the user in the args and no session user.
func requireQueuedMirror(tb testing.TB, ctx context.Context, userId server.Id) *task.Task {
	tb.Helper()
	syncs := pendingProductUpdatesSyncs(ctx)
	queued := syncs[task.RunOnce("sync_product_updates_for_user", userId).String()]
	if queued == nil {
		tb.Fatal("no product-updates sync queued for the user", len(syncs))
	}
	var args SyncProductUpdatesForUserArgs
	if err := json.Unmarshal([]byte(queued.ArgsJson), &args); err != nil || args.UserId != userId {
		tb.Fatal("the queued sync does not name the user in its args", err)
	}
	if queued.ClientByJwtJson != "" {
		tb.Fatal("the mirror queued its sync with a session user")
	}
	return queued
}

// A suppression takes the admin's address off both lists through the
// product-updates sync, which retries a provider failure; a replay converges;
// a lift puts the address back on network_users only while product updates
// are on; a bounce or complaint is never lifted, and the settings switch does
// not override it.
func TestProductUpdatesMirrorMarketingSuppression(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId, userId := server.NewId(), server.NewId()
		email := model.Testing_CreateNetwork(ctx, networkId, "synthetic-suppression-mirror", userId)
		model.AccountPreferencesSetForNetwork(ctx, networkId, true)
		previousNetworks, previousUpdates := newNetworksListId, productUpdatesListId
		newNetworksListId, productUpdatesListId = func() int { return 101 }, func() int { return 102 }
		defer func() { newNetworksListId, productUpdatesListId = previousNetworks, previousUpdates }()
		brevo := newBrevoListFixture(t, email)

		target := task.NewTaskTargetWithPost(SyncProductUpdatesForUser, SyncProductUpdatesForUserPost)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		run := func(label string) {
			tb.Helper()
			syncs := pendingProductUpdatesSyncs(ctx)
			queued := syncs[task.RunOnce("sync_product_updates_for_user", userId).String()]
			if len(syncs) != 1 || queued == nil {
				tb.Fatal(label, "expected one queued sync for the user", len(syncs))
			}
			// a task queued now is claimable from the next second
			deadline := time.Now().Add(time.Minute)
			for {
				finished, retried, posts, err := worker.EvalTasks(1)
				if err != nil || 1 < len(finished) || len(finished) == 1 && finished[0] != queued.TaskId || len(retried)+len(posts) != 0 {
					tb.Fatal(label, "sync did not finish", finished, retried, posts, err)
				}
				if len(finished) == 1 {
					return
				}
				if time.Now().After(deadline) {
					tb.Fatal(label, "the queued sync was never claimed")
				}
				select {
				case <-ctx.Done():
					tb.Fatal(label, ctx.Err())
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		expectLists := func(label string, newNetworks bool, networkUsers bool) {
			tb.Helper()
			if brevo.on(101) != newNetworks || brevo.on(102) != networkUsers {
				tb.Fatal(label, "lists", brevo.on(101), brevo.on(102), "want", newNetworks, networkUsers)
			}
		}
		owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		owner.ClientAddress = ""
		defer owner.Cancel()
		setProductUpdates := func(productUpdates bool) {
			if _, err := AccountPreferencesSet(&model.AccountPreferencesSetArgs{ProductUpdates: productUpdates}, owner); err != nil {
				tb.Fatal(err)
			}
		}
		liftable := []string{reengagement.SuppressSunset, reengagement.SuppressSwitched}
		now := server.NowUtc()

		// subscribed through settings; the initial sync put it on new_networks
		setProductUpdates(true)
		run("subscribe")
		brevo.put(101)
		expectLists("subscribed", true, true)

		SuppressNetworkMarketing(ctx, networkId, reengagement.SuppressSunset, now)
		queued := requireQueuedMirror(tb, ctx, userId)
		brevo.failRemovals.Store(true)
		if _, _, err := target.RunSpecific(ctx, queued); err == nil {
			tb.Fatal("a provider failure was acknowledged")
		}
		expectLists("provider failure", true, true)
		brevo.failRemovals.Store(false)
		run("suppress")
		expectLists("suppressed", false, false)

		// a replayed suppression keeps the first reason and converges (a
		// reason no connection lifts would replace the sunset:
		// model.SuppressNetworkMarketingInTx)
		SuppressNetworkMarketing(ctx, networkId, reengagement.SuppressSunset, now.Add(time.Minute))
		requireQueuedMirror(tb, ctx, userId)
		run("replayed suppression")
		expectLists("replayed suppression", false, false)
		if state := model.GetNetworkMailState(ctx, networkId); state.SuppressedReason != reengagement.SuppressSunset || !state.SuppressedAt.Equal(now) {
			tb.Fatal("a replay replaced the first suppression", state.SuppressedReason, state.SuppressedAt)
		}

		// a lift puts the address back on network_users; a replayed lift does nothing
		if !LiftNetworkMarketingSuppression(ctx, networkId, liftable) {
			tb.Fatal("a sunset was not lifted")
		}
		requireQueuedMirror(tb, ctx, userId)
		run("lift")
		expectLists("lifted", false, true)
		if LiftNetworkMarketingSuppression(ctx, networkId, liftable) || len(pendingProductUpdatesSyncs(ctx)) != 0 {
			tb.Fatal("a replayed lift lifted or queued a sync")
		}

		// a lift never re-adds an address whose product updates are off
		SuppressNetworkMarketing(ctx, networkId, reengagement.SuppressSwitched, now)
		run("switched")
		setProductUpdates(false)
		run("opt out")
		if !LiftNetworkMarketingSuppression(ctx, networkId, liftable) {
			tb.Fatal("a switched answer was not lifted")
		}
		run("lift while opted out")
		expectLists("lifted while opted out", false, false)

		// a complaint is never lifted, and the settings switch does not put the
		// address back while it stands
		setProductUpdates(true)
		run("subscribe again")
		expectLists("subscribed again", false, true)
		SuppressNetworkMarketing(ctx, networkId, onboarding.ExitComplained, now)
		run("complaint")
		expectLists("complained", false, false)
		if LiftNetworkMarketingSuppression(ctx, networkId, liftable) || len(pendingProductUpdatesSyncs(ctx)) != 0 {
			tb.Fatal("a complaint was lifted")
		}
		setProductUpdates(true)
		run("settings switch while suppressed")
		expectLists("settings switch while suppressed", false, false)

		// the args name the user even when a session carries another one
		other := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
		defer other.Cancel()
		before := brevo.requests.Load()
		if _, err := SyncProductUpdatesForUser(&SyncProductUpdatesForUserArgs{UserId: userId}, other); err != nil || brevo.requests.Load() != before+2 {
			tb.Fatal("the sync did not follow its args", err, brevo.requests.Load()-before)
		}

		// a network that no longer exists queues nothing
		SuppressNetworkMarketing(ctx, server.NewId(), onboarding.ExitBounced, now)
		if len(pendingProductUpdatesSyncs(ctx)) != 0 {
			tb.Fatal("a missing network queued a sync")
		}
	})
}

// The campaigns' bounce and complaint webhooks and a re-engagement exit that
// suppresses queue the mirror for the network's admin, one per user, and call
// Brevo for nothing themselves.
func TestCampaignSuppressionsQueueBrevoMirror(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("a suppression called Brevo instead of queueing the sync", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}))
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-webhook-mirror", userId)
		exitNetworkId, exitUserId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, exitNetworkId, "synthetic-exit-mirror", exitUserId)
		now := server.NowUtc()
		onboardingMessageId := "synthetic-onboarding-" + server.NewId().String()
		reengagementMessageId := "synthetic-reengagement-" + server.NewId().String()
		server.Tx(ctx, func(tx server.PgTx) {
			model.AddNetworkOnboardingEmailInTx(tx, ctx, &model.NetworkOnboardingEmail{
				MessageId: onboardingMessageId, NetworkId: networkId, Step: onboarding.StepE1,
				Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, Locale: "en", SentAt: now,
			})
			model.AddNetworkReengagementEmailInTx(tx, ctx, &model.NetworkReengagementEmail{
				MessageId: reengagementMessageId, NetworkId: networkId, Step: reengagement.StepR1,
				Template: reengagement.TemplateR1News, Variant: reengagement.StreamLapsed, Locale: "en", SentAt: now,
			})
		})
		expectReason := func(networkId server.Id, reason string) {
			tb.Helper()
			if state := model.GetNetworkMailState(ctx, networkId); state.SuppressedAt == nil || state.SuppressedReason != reason {
				tb.Fatal("suppression", state.SuppressedAt, state.SuppressedReason, "want", reason)
			}
		}

		handleOnboardingBrevoWebhook(ctx, &BrevoWebhookArgs{Event: "hard_bounce", MessageId: "synthetic-unknown"})
		if len(pendingProductUpdatesSyncs(ctx)) != 0 {
			tb.Fatal("a bounce for an unknown message queued a sync")
		}

		handleOnboardingBrevoWebhook(ctx, &BrevoWebhookArgs{Event: "hard_bounce", MessageId: onboardingMessageId})
		requireQueuedMirror(tb, ctx, userId)
		expectReason(networkId, onboarding.ExitBounced)

		// a complaint on the other program keeps the bounce and joins the queued sync
		handleReengagementBrevoWebhook(ctx, &BrevoWebhookArgs{Event: "spam", MessageId: reengagementMessageId})
		requireQueuedMirror(tb, ctx, userId)
		expectReason(networkId, onboarding.ExitBounced)
		if syncs := pendingProductUpdatesSyncs(ctx); len(syncs) != 1 {
			tb.Fatal("the second suppression queued a second sync", len(syncs))
		}

		applyReengagementExit(ctx, &model.NetworkReengagement{NetworkId: exitNetworkId, Stream: reengagement.StreamLapsed},
			reengagement.ExitSunset, reengagement.SuppressSunset, "", false, now)
		requireQueuedMirror(tb, ctx, exitUserId)
		expectReason(exitNetworkId, reengagement.SuppressSunset)
		if syncs := pendingProductUpdatesSyncs(ctx); len(syncs) != 2 {
			tb.Fatal("expected one queued sync per suppressed network", len(syncs))
		}
	})
}

// A sync that names no user, in its args or its session, has nothing to sync
// and finishes without calling Brevo.
func TestSyncProductUpdatesForUserWithoutUser(t *testing.T) {
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a sync without a user called Brevo", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	clientSession := session.NewLocalClientSession(t.Context(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	if result, err := SyncProductUpdatesForUser(&SyncProductUpdatesForUserArgs{}, clientSession); err != nil || result == nil {
		t.Fatal("a sync without a user failed", result, err)
	}
}

// The backfill queues the mirror for every suppressed network that still
// exists, across pages, and calls Brevo for nothing itself; repeating it
// queues nothing new.
func TestMirrorNetworkMarketingSuppressionsQueuesEverySuppressedNetwork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("the backfill called Brevo instead of queueing the sync", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}))
		now := server.NowUtc()
		suppressedUserIds := []server.Id{}
		for i, reason := range []string{onboarding.ExitBounced, onboarding.ExitComplained, reengagement.SuppressSunset} {
			networkId, userId := server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-backfill-"+strconv.Itoa(i), userId)
			// recorded before the mirror existed: no sync queued
			server.Tx(ctx, func(tx server.PgTx) {
				model.SuppressNetworkMarketingInTx(tx, ctx, networkId, reason, now)
			})
			suppressedUserIds = append(suppressedUserIds, userId)
		}
		subscribedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, subscribedNetworkId, "synthetic-backfill-subscribed", server.NewId())
		model.AccountPreferencesSetForNetwork(ctx, subscribedNetworkId, true)
		// a suppression a removed network left behind
		server.Tx(ctx, func(tx server.PgTx) {
			model.SuppressNetworkMarketingInTx(tx, ctx, server.NewId(), onboarding.ExitBounced, now)
		})
		if len(pendingProductUpdatesSyncs(ctx)) != 0 {
			tb.Fatal("a sync was queued before the backfill")
		}

		for range 2 {
			pacer := newProductUpdatesSyncPacer(server.NowUtc(), DefaultProductUpdatesMirrorSettings())
			if queued := mirrorNetworkMarketingSuppressions(ctx, pacer, 2); queued != len(suppressedUserIds) {
				tb.Fatal("the backfill queued", queued, "want", len(suppressedUserIds))
			}
			for _, userId := range suppressedUserIds {
				requireQueuedMirror(tb, ctx, userId)
			}
			if syncs := pendingProductUpdatesSyncs(ctx); len(syncs) != len(suppressedUserIds) {
				tb.Fatal("expected one queued sync per suppressed network", len(syncs))
			}
		}
	})
}

// A network whose sign-up country gets no marketing (the mail policy's
// default no_marketing_countries, Iran among them since 2026-11-13) comes off
// both lists at its next sync, stays off on a repeat, and the initial sync
// never puts it back, though its product updates are on. Before, the sync
// added such an address to network_users like any other.
func TestProductUpdatesKeepNoMarketingCountriesOffTheLists(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		networkId, userId := server.NewId(), server.NewId()
		email := model.Testing_CreateNetwork(ctx, networkId, "synthetic-country-mirror", userId)
		model.AccountPreferencesSetForNetwork(ctx, networkId, true)
		model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: server.NowUtc(), Email: true, Country: "IR"})
		previousNetworks, previousUpdates := newNetworksListId, productUpdatesListId
		newNetworksListId, productUpdatesListId = func() int { return 101 }, func() int { return 102 }
		defer func() { newNetworksListId, productUpdatesListId = previousNetworks, previousUpdates }()
		brevo := newBrevoListFixture(t, email)
		// on both lists from before the country joined the policy
		brevo.put(101)
		brevo.put(102)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		sync := func(label string) {
			tb.Helper()
			if _, err := SyncProductUpdatesForUser(&SyncProductUpdatesForUserArgs{UserId: userId}, owner); err != nil {
				tb.Fatal(label, err)
			}
			if brevo.on(101) || brevo.on(102) {
				tb.Fatal(label, "a no-marketing country's address is on the lists", brevo.on(101), brevo.on(102))
			}
		}
		sync("sync")
		sync("repeated sync")
		if err := SyncInitialProductUpdates(ctx); err != nil {
			tb.Fatal(err)
		}
		if brevo.on(101) || brevo.on(102) {
			tb.Fatal("the initial sync put a no-marketing country's address on the lists")
		}
	})
}

// The backfill queues the sync of every network of a no-marketing country,
// across pages, one paced slot each and never at once, and calls Brevo for
// nothing itself; a repeat keeps each sync's slot.
func TestMirrorNoMarketingCountryNetworksQueuesPacedSyncs(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("the backfill called Brevo instead of queueing the sync", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}))
		now := server.NowUtc()
		userIds := map[string]server.Id{}
		networkIds := map[string]server.Id{}
		for _, country := range []string{"IR", "SY", "US"} {
			networkId, userId := server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-backfill-"+strings.ToLower(country), userId)
			model.AccountPreferencesSetForNetwork(ctx, networkId, true)
			model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: now, Email: true, Country: country})
			userIds[country], networkIds[country] = userId, networkId
		}
		// the slots in network id order
		order := []string{"IR", "SY"}
		if networkIds["SY"].Less(networkIds["IR"]) {
			order = []string{"SY", "IR"}
		}
		start := now.Add(time.Hour)
		for range 2 {
			pacer := newProductUpdatesSyncPacer(start, &ProductUpdatesMirrorSettings{SyncsPerSecond: 4})
			if queued := mirrorNoMarketingCountryNetworks(ctx, []string{"IR", "SY", "CU", "KP"}, pacer, 1); queued != 2 {
				tb.Fatal("the backfill queued", queued)
			}
			if syncs := pendingProductUpdatesSyncs(ctx); len(syncs) != 2 {
				tb.Fatal("expected one queued sync per no-marketing network", len(syncs))
			}
			for i, country := range order {
				queued := requireQueuedMirror(tb, ctx, userIds[country])
				if want := start.Add(time.Duration(i) * 250 * time.Millisecond); !queued.RunAt.Equal(want) {
					tb.Fatal("slot of", country, queued.RunAt, "want", want)
				}
			}
		}
	})
}
