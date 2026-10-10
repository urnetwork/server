// Exercise subscription acknowledgements against a private HTTP fixture.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
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
