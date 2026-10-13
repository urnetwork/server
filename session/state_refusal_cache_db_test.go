// Real SQL proves which verdicts repeat the state query. The counters are the
// process's own query observations, so each assertion reads exact work done.
package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// One network whose clients form the chain top -> middle -> leaf.
type testingStateRefusalNetwork struct {
	networkId server.Id
	userId    server.Id
	deviceId  server.Id
}

func newTestingStateRefusalNetwork(ctx context.Context) *testingStateRefusalNetwork {
	network := &testingStateRefusalNetwork{networkId: server.NewId(), userId: server.NewId(), deviceId: server.NewId()}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'state-refusal','password',true)`, network.userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,'state-refusal',$2)`, network.networkId, network.userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, network.deviceId, network.networkId))
	})
	return network
}

// Adds a client under parentClientId (nil for top level) and returns its
// legacy credential, which carries neither a root nor a session claim.
func (self *testingStateRefusalNetwork) client(ctx context.Context, parentClientId *server.Id, active bool) *ByJwt {
	return self.clientWithId(ctx, server.NewId(), parentClientId, active)
}

// A client whose refusals the attribution samples, or skips.
func (self *testingStateRefusalNetwork) classifiedClient(ctx context.Context, parentClientId *server.Id, active bool, classified bool) *ByJwt {
	for {
		clientId := server.NewId()
		if stateRefusalClassified(clientId) == classified {
			return self.clientWithId(ctx, clientId, parentClientId, active)
		}
	}
}

func (self *testingStateRefusalNetwork) clientWithId(ctx context.Context, clientId server.Id, parentClientId *server.Id, active bool) *ByJwt {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,active,source_client_id) VALUES($1,$2,$3,'synthetic',$4,$5)`, clientId, self.networkId, self.deviceId, active, parentClientId))
	})
	byJwt := NewByJwt(self.networkId, self.userId, "state-refusal", false, false).Client(self.deviceId, clientId)
	byJwt.RootClientId = nil
	return byJwt
}

func testingNoActiveRowRejections() float64 {
	return testutil.ToFloat64(authRejectionCounter.WithLabelValues(string(AuthRejectionNoActiveRow)))
}

// The storm: a credential whose client is gone redials every few seconds, and
// each handshake repeated the recursive state query for an answer that cannot
// change. Every retry is still refused and counted, but only the first one in
// the process reaches the database.
func TestRefusedClientCredentialRunsOneStateQuery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		inactive := network.client(ctx, nil, true)
		deleted := network.client(ctx, nil, true)
		top := network.client(ctx, nil, true)
		derived := network.client(ctx, top.ClientId, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, *inactive.ClientId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=$1`, *deleted.ClientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, *top.ClientId))
		})
		const retries = 5
		for _, control := range []struct {
			name  string
			byJwt *ByJwt
		}{
			{name: "inactive client", byJwt: inactive},
			{name: "deleted client", byJwt: deleted},
			{name: "derived client of an inactive parent", byJwt: derived},
		} {
			queriesBefore := stateQueryCount(StateQueryConnectH1, true, stateQueryNoActiveRow)
			rejectionsBefore := testingNoActiveRowRejections()
			for range retries {
				err := ValidateByJwtState(WithStateQuerySource(ctx, StateQueryConnectH1), control.byJwt, true)
				if !errors.Is(err, ErrByJwtInactive) {
					t.Fatalf("%s: a retry was not refused as inactive: %v", control.name, err)
				}
			}
			if queries := stateQueryCount(StateQueryConnectH1, true, stateQueryNoActiveRow) - queriesBefore; queries != 1 {
				t.Errorf("%s: %d retries ran %.0f state queries, want 1", control.name, retries, queries)
			}
			if rejections := testingNoActiveRowRejections() - rejectionsBefore; rejections != retries {
				t.Errorf("%s: %.0f of %d refusals were counted", control.name, rejections, retries)
			}
		}

		// a parent ownership check is remembered for its own parent only
		queriesBefore := stateQueryCount(StateQueryHostedControl, true, stateQueryNoActiveRow)
		for range retries {
			if err := ValidateByJwtStateForParent(WithStateQuerySource(ctx, StateQueryHostedControl), derived, *top.ClientId); !errors.Is(err, ErrByJwtInactive) {
				t.Fatalf("a parent check of a refused child was not refused: %v", err)
			}
		}
		if queries := stateQueryCount(StateQueryHostedControl, true, stateQueryNoActiveRow) - queriesBefore; queries != 1 {
			t.Errorf("%d parent checks ran %.0f state queries, want 1", retries, queries)
		}
	})
}

// Only a completed no-active-row verdict is remembered. A valid or rotated
// credential, a canceled check, and another credential of a refused client all
// reach the database every time.
func TestStateRefusalRemembersOnlyDefinitiveNoActiveRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		validate := func(byJwt *ByJwt) error {
			return ValidateByJwtState(WithStateQuerySource(ctx, StateQueryApiControl), byJwt, true)
		}
		const repeats = 3

		valid := network.client(ctx, nil, true)
		validBefore := stateQueryCount(StateQueryApiControl, true, stateQueryValid)
		for range repeats {
			if err := validate(valid); err != nil {
				t.Fatalf("a valid credential was refused: %v", err)
			}
		}
		if queries := stateQueryCount(StateQueryApiControl, true, stateQueryValid) - validBefore; queries != repeats {
			t.Fatalf("%d valid checks ran %.0f queries; a valid answer must never be remembered", repeats, queries)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, network.userId, valid.CreateTime.Add(time.Minute)))
		})
		rotatedBefore := stateQueryCount(StateQueryApiControl, true, stateQueryCredentialRotated)
		for range repeats {
			if err := validate(valid); !errors.Is(err, ErrByJwtInactive) {
				t.Fatalf("a rotated credential was not refused: %v", err)
			}
		}
		if queries := stateQueryCount(StateQueryApiControl, true, stateQueryCredentialRotated) - rotatedBefore; queries != repeats {
			t.Fatalf("%d rotated checks ran %.0f queries; rotation must not be remembered", repeats, queries)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, network.userId, time.Unix(0, 0).UTC()))
		})
		if err := validate(valid); err != nil {
			t.Fatalf("the credential stayed refused after its rotation was undone: %v", err)
		}

		// a check that never completed is no answer
		inactive := network.client(ctx, nil, false)
		canceledCtx, cancel := context.WithCancel(WithStateQuerySource(ctx, StateQueryApiControl))
		cancel()
		var canceledErr error
		server.HandleError(func() {
			canceledErr = ValidateByJwtState(canceledCtx, inactive, true)
		}, func(err error) {
			canceledErr = err
		})
		if canceledErr == nil || errors.Is(canceledErr, ErrByJwtInactive) {
			t.Fatalf("a canceled check answered: %v", canceledErr)
		}
		noRowBefore := stateQueryCount(StateQueryApiControl, true, stateQueryNoActiveRow)
		if err := validate(inactive); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("an inactive credential was not refused: %v", err)
		}
		if queries := stateQueryCount(StateQueryApiControl, true, stateQueryNoActiveRow) - noRowBefore; queries != 1 {
			t.Fatalf("the check after a canceled one ran %.0f queries, want 1: the canceled check was remembered", queries)
		}

		// the legacy credential walks every ancestor; a root-claimed renewal of
		// the same client checks only its root, which is still active
		top := network.client(ctx, nil, true)
		middle := network.client(ctx, top.ClientId, false)
		leaf := network.client(ctx, middle.ClientId, true)
		if err := validate(leaf); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("the legacy credential ignored its inactive ancestor: %v", err)
		}
		renewed := *leaf
		renewed.RootClientId = top.ClientId
		validBefore = stateQueryCount(StateQueryApiControl, true, stateQueryValid)
		if err := validate(&renewed); err != nil {
			t.Fatalf("a remembered legacy refusal refused the root-claimed credential: %v", err)
		}
		if queries := stateQueryCount(StateQueryApiControl, true, stateQueryValid) - validBefore; queries != 1 {
			t.Fatalf("the root-claimed credential ran %.0f queries, want its own 1", queries)
		}
	})
}

// The first stored refusal of a sampled credential attributes the refused
// client by its own row, without ids: missing, inactive, or active under a
// refused ancestor; top level or derived; and whether it provides publicly.
func TestStateRefusalClassifiesTheRefusedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		provider := network.classifiedClient(ctx, nil, true, true)
		missing := network.classifiedClient(ctx, nil, true, true)
		parent := network.client(ctx, nil, true)
		child := network.classifiedClient(ctx, parent.ClientId, true, true)
		unsampled := network.classifiedClient(ctx, nil, false, false)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key(client_id,provide_mode,secret_key) VALUES($1,$2,decode('01','hex'))`, *provider.ClientId, stateRefusalPublicProvideMode))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=ANY($1)`, []server.Id{*provider.ClientId, *parent.ClientId}))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=$1`, *missing.ClientId))
		})
		for _, control := range []struct {
			byJwt   *ByJwt
			row     string
			level   string
			provide string
		}{
			{byJwt: provider, row: "inactive", level: "top_level", provide: "public"},
			{byJwt: missing, row: "missing", level: "unknown", provide: "unknown"},
			{byJwt: child, row: "active", level: "derived", provide: "none"},
		} {
			class := stateRefusalClassCounter.WithLabelValues("connect_h1", "handshake", control.row, control.level, control.provide)
			before := testutil.ToFloat64(class)
			for range 3 {
				if err := ValidateByJwtState(WithStateQuerySource(ctx, StateQueryConnectH1), control.byJwt, true); !errors.Is(err, ErrByJwtInactive) {
					t.Fatalf("%s/%s was not refused: %v", control.row, control.level, err)
				}
			}
			if counted := testutil.ToFloat64(class) - before; counted != 1 {
				t.Errorf("%s/%s/%s was attributed %.0f times, want once per stored refusal", control.row, control.level, control.provide, counted)
			}
		}
		skipped := stateRefusalClassCounter.WithLabelValues("connect_h1", "handshake", "inactive", "top_level", "none")
		before := testutil.ToFloat64(skipped)
		if err := ValidateByJwtState(WithStateQuerySource(ctx, StateQueryConnectH1), unsampled, true); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("the unsampled credential was not refused: %v", err)
		}
		if testutil.ToFloat64(skipped) != before {
			t.Error("a credential outside the sample was attributed")
		}
	})
}
