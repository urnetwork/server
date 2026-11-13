// Real SDK mint callbacks distinguish child creation, restored reuse and pool
// admission failure. Every identity and endpoint in these controls is synthetic.
package localclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Multiple independent owners share one fixture database, so their synthetic
// network names must be unique as well as their generated identifiers.
func authorityMintTestOwner(t testing.TB, ctx context.Context) (*Authority, *session.ByJwt) {
	t.Helper()
	networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	name := "synthetic-mint-" + networkId.String()
	model.Testing_CreateNetwork(ctx, networkId, name, userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
	claims := session.NewByJwt(networkId, userId, name, false, false).Client(deviceId, clientId)
	owner, err := New(ctx, claims.Testing_Sign(), "https://control.example")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	return owner, claims
}

// Constructs the same typed credential hook used by a hosted device, without
// opening platform transports or running discovery. The returned close joins
// both the generator and its private strategy before the fixture disappears.
func authorityMintGenerator(t testing.TB, ctx context.Context, owner *Authority) (*connect.ApiMultiClientGenerator, func()) {
	t.Helper()
	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.RequestTimeout = 10 * time.Second
	strategy := connect.NewClientStrategy(ctx, settings)
	generatorSettings := connect.DefaultApiMultiClientGeneratorSettings()
	generatorSettings.ClientCredentials = owner
	parent := connect.Id(owner.clientId)
	generator := connect.NewApiMultiClientGenerator(ctx, nil, strategy, nil,
		owner.apiUrl, owner.token(), "https://platform.example", "test", "test", "test",
		&parent, connect.DefaultClientSettings, generatorSettings)
	return generator, func() {
		joinCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := generator.CloseAndWait(joinCtx); err != nil {
			t.Error("generator join failed", err)
		}
		strategy.Close()
	}
}

// A finite snapshot with no labels derived from identities or request data.
func authorityMintPoolGauge(t testing.TB, state string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_pg_pool_connections" {
			continue
		}
		for _, metric := range family.Metric {
			poolMatches, stateMatches := false, false
			for _, label := range metric.Label {
				poolMatches = poolMatches || label.GetName() == "pool" && label.GetValue() == "default"
				stateMatches = stateMatches || label.GetName() == "state" && label.GetValue() == state
			}
			if poolMatches && stateMatches {
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatal("default pool gauge missing")
	return 0
}

// Counts durable children only after the measured operation bracket has ended.
func authorityMintChildCount(ctx context.Context, owner *Authority) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE network_id=$1 AND source_client_id=$2`, owner.networkId, owner.clientId).Scan(&count))
	})
	return
}

// Optional local PGSS evidence uses the fixture database only and never resets
// shared counters. The fixture owner enables it on a preloaded native cluster.
type authorityMintQueryWork struct {
	calls, rows, hits, reads, walBytes int64
	executionMs                        float64
}

func authorityMintQuerySnapshot(ctx context.Context) (work authorityMintQueryWork) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT COALESCE(SUM(calls),0)::bigint,
				COALESCE(SUM(rows),0)::bigint,
				COALESCE(SUM(shared_blks_hit),0)::bigint,
				COALESCE(SUM(shared_blks_read),0)::bigint,
				COALESCE(SUM(wal_bytes),0)::bigint,
				COALESCE(SUM(total_exec_time),0)::double precision
			FROM pg_stat_statements
			WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
				AND query LIKE $1 AND query LIKE $2
		`, "%SELECT network_user.credential_change_time%", "%INNER JOIN network_client%").Scan(
			&work.calls, &work.rows, &work.hits, &work.reads, &work.walBytes, &work.executionMs))
	})
	return
}

// Eight independent owners mint four actual SDK identities each. This is a
// bounded concurrency control, not an estimate of production owner counts.
func TestAuthorityMintFanoutMeasuresActualSdkWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		var httpCalls atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			httpCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		const ownerCount, requestsPerOwner = 8, 4
		const requests = ownerCount * requestsPerOwner
		owners := make([]*Authority, ownerCount)
		generators := make([]*connect.ApiMultiClientGenerator, ownerCount)
		for i := range owners {
			owners[i], _ = authorityMintTestOwner(t, ctx)
			owners[i].apiUrl = trap.URL
			var closeGenerator func()
			generators[i], closeGenerator = authorityMintGenerator(t, ctx, owners[i])
			defer closeGenerator()
		}
		pgss := os.Getenv("WARP_TEST_HOSTED_MINT_PGSS") == "1"
		var queryBefore authorityMintQueryWork
		if pgss {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"))
			})
			queryBefore = authorityMintQuerySnapshot(ctx)
		}
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		acquiresBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		queriesBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		validBefore := authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid")
		type mintResult struct {
			owner int
			args  *connect.MultiClientGeneratorClientArgs
			err   error
		}
		results := make(chan mintResult, requests)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i, generator := range generators {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for range requestsPerOwner {
					destination, err := connect.NewMultiHopId(connect.NewId())
					var args *connect.MultiClientGeneratorClientArgs
					if err == nil {
						args, err = generator.NewClientArgsForDestinationContext(ctx, destination)
					}
					results <- mintResult{owner: i, args: args, err: err}
				}
			}()
		}
		started := time.Now()
		close(start)
		workers.Wait()
		elapsed := time.Since(started)
		close(results)
		acquires := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) - acquiresBefore
		queries := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) - queriesBefore
		valid := authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid") - validBefore
		if acquires != requests || queries != requests || valid != requests || httpCalls.Load() != 0 {
			t.Fatalf("mint work changed: acquires=%v queries=%v valid=%v http=%d", acquires, queries, valid, httpCalls.Load())
		}
		if pgss {
			after := authorityMintQuerySnapshot(ctx)
			if after.calls-queryBefore.calls != requests || after.rows-queryBefore.rows != requests {
				t.Fatalf("local JWT statement work changed: calls=%d rows=%d", after.calls-queryBefore.calls, after.rows-queryBefore.rows)
			}
			t.Logf("synthetic JWT SQL: calls=%d hits=%d reads=%d wal_bytes=%d execution_ms=%.3f",
				after.calls-queryBefore.calls, after.hits-queryBefore.hits, after.reads-queryBefore.reads,
				after.walBytes-queryBefore.walBytes, after.executionMs-queryBefore.executionMs)
		}
		clientIds := map[connect.Id]bool{}
		for result := range results {
			if result.err != nil || result.args == nil || result.args.ClientAuth == nil {
				t.Fatal("actual SDK mint failed", result.err)
			}
			if clientIds[result.args.ClientId] {
				t.Fatal("independent mints collapsed onto one client")
			}
			clientIds[result.args.ClientId] = true
			claims, err := session.ParseByJwtForAudience(ctx, result.args.ClientAuth.ByJwt, session.ByJwtAudienceApi)
			owner := owners[result.owner]
			if err != nil || claims.ClientId == nil || claims.DeviceId == nil || claims.NetworkId != owner.networkId ||
				*claims.DeviceId != owner.deviceId || connect.Id(*claims.ClientId) != result.args.ClientId {
				t.Fatal("mint lost its parent device or distinct child identity")
			}
		}
		for _, owner := range owners {
			if authorityMintChildCount(ctx, owner) != requestsPerOwner {
				t.Fatal("committed child count differs from successful SDK requests")
			}
		}
		t.Logf("synthetic mint bracket: owners=%d requests=%d acquires=%.0f jwt_queries=%.0f elapsed=%s operations_per_second=%.3f",
			ownerCount, requests, acquires, queries, elapsed, float64(requests)/elapsed.Seconds())
	})
}

// Durable JWT entitlement bypasses both cache tiers. A committed upgrade and
// later lapse must each be reflected even though the parent's claim is older.
func TestAuthorityMintKeepsFreshProAcrossCachedTransitions(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, _ := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		if model.IsProNetwork(ctx, owner.networkId) {
			t.Fatal("synthetic free network was unexpectedly Pro")
		}
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(model.AddProTransferBalanceInTx(tx, ctx, owner.networkId, model.ByteCount(1024*1024), now, now.Add(time.Hour)))
		})
		local, localOk, cached, cachedOk := model.Testing_ProNetworkCacheEntries(ctx, owner.networkId)
		if !localOk || !cachedOk || local || cached {
			t.Fatal("upgrade fixture did not retain both stale free cache entries")
		}
		upgraded, err := generator.NewClientArgsContext(ctx)
		if err != nil || upgraded == nil || upgraded.ClientAuth == nil {
			t.Fatal("upgraded mint failed", err)
		}
		upgradedClaims, err := session.ParseByJwtForAudience(ctx, upgraded.ClientAuth.ByJwt, session.ByJwtAudienceApi)
		if err != nil || !upgradedClaims.Pro {
			t.Fatal("fresh child inherited stale free entitlement")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET pro=false WHERE network_id=$1`, owner.networkId))
		})
		local, localOk, cached, cachedOk = model.Testing_ProNetworkCacheEntries(ctx, owner.networkId)
		if !localOk || !cachedOk || !local || !cached {
			t.Fatal("lapse fixture did not retain both stale Pro cache entries")
		}
		lapsed, err := generator.NewClientArgsContext(ctx)
		if err != nil || lapsed == nil || lapsed.ClientAuth == nil {
			t.Fatal("lapsed mint failed", err)
		}
		lapsedClaims, err := session.ParseByJwtForAudience(ctx, lapsed.ClientAuth.ByJwt, session.ByJwtAudienceApi)
		if err != nil || lapsedClaims.Pro {
			t.Fatal("fresh child inherited stale Pro entitlement")
		}
	})
}

// Every local credential boundary still runs before acquisition: signature,
// audience, owning account/client/device, cancellation and closed ownership.
func TestAuthorityMintPreflightRejectsBeforePool(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, parent := authorityMintTestOwner(t, ctx)
		original := owner.token()
		source := connect.Id(owner.clientId)
		args := &connect.AuthNetworkClientArgs{SourceClientId: &source, Description: "test", DeviceSpec: "test"}
		tokens := []string{"invalid.signature.token"}
		for _, field := range []string{"network", "user", "client", "device", "audience"} {
			claims := *parent
			other := server.NewId()
			switch field {
			case "network":
				claims.NetworkId = other
			case "user":
				claims.UserId = other
				claims.Subject = other.String()
			case "client":
				claims.ClientId = &other
			case "device":
				claims.DeviceId = &other
			case "audience":
				claims.Audience = []string{session.ByJwtAudienceConnect}
			}
			tokens = append(tokens, claims.Testing_Sign())
		}
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		acquiresBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		queriesBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		for _, token := range tokens {
			owner.stateLock.Lock()
			owner.parentJwt = token
			owner.stateLock.Unlock()
			result, err := owner.AuthNetworkClient(ctx, args)
			authorityTestUnauthorized(t, err)
			if result != nil {
				t.Fatal("invalid parent credential returned a mint")
			}
		}
		owner.stateLock.Lock()
		owner.parentJwt = original
		owner.stateLock.Unlock()
		foreign := connect.NewId()
		for _, badArgs := range []*connect.AuthNetworkClientArgs{nil, {}, {SourceClientId: &foreign}, {ClientId: &source, SourceClientId: &source}} {
			if result, err := owner.AuthNetworkClient(ctx, badArgs); result != nil || err == nil {
				t.Fatal("invalid mint shape reached the model")
			}
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		if result, err := owner.AuthNetworkClient(canceled, args); result != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled mint passed preflight")
		}
		owner.Close()
		if result, err := owner.AuthNetworkClient(ctx, args); result != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("closed owner admitted a mint")
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore ||
			authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queriesBefore {
			t.Fatal("local preflight refusal acquired a connection or entered JWT SQL")
		}
		if authorityMintChildCount(ctx, owner) != 0 {
			t.Fatal("local preflight refusal created a child")
		}
	})
}

// A real serialization error rolls back the first child INSERT. Retrying the
// transaction repeats live parent validation and emits exactly one identity.
func TestAuthorityMintTransactionRetryRepeatsLiveValidation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, _ := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE synthetic_parent_mint_attempt`))
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION synthetic_parent_mint_retry() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.source_client_id IS NOT NULL AND nextval('synthetic_parent_mint_attempt') = 1 THEN
						RAISE EXCEPTION 'synthetic first mint serialization failure' USING ERRCODE='40001';
					END IF;
					RETURN NEW;
				END $$
			`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER synthetic_parent_mint_retry BEFORE INSERT ON network_client FOR EACH ROW EXECUTE FUNCTION synthetic_parent_mint_retry()`))
		})
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		acquiresBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		validBefore := authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid")
		args, err := generator.NewClientArgsContext(ctx)
		if err != nil || args == nil {
			t.Fatal("retry did not return a child identity", err)
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore+2 ||
			authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid") != validBefore+2 {
			t.Fatal("transaction retry reused stale parent authority or acquired a separate validation connection")
		}
		if authorityMintChildCount(ctx, owner) != 1 {
			t.Fatal("rolled-back mint leaked or duplicated its child")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM synthetic_parent_mint_attempt`).Scan(&attempts))
			if attempts != 2 {
				t.Fatal("serialization control did not make exactly two insertion attempts")
			}
		})
	})
}

// An exhausted real pool cannot enter the JWT callback. The holder barrier
// fixes admission order; each request owns its deadline and all workers join.
func TestAuthorityMintPoolDeadlinePrecedesJwtQuery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 4\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		owner, _ := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		if authorityMintPoolGauge(t, "maximum") != 4 {
			t.Fatal("synthetic pool maximum was not applied")
		}
		ready := make(chan struct{}, 4)
		release := make(chan struct{})
		holderErrors := make(chan error, 4)
		var holders sync.WaitGroup
		var releaseOnce sync.Once
		joinHolders := func() {
			releaseOnce.Do(func() {
				close(release)
				holders.Wait()
				close(holderErrors)
				for err := range holderErrors {
					if err != nil {
						t.Error("pool holder failed", err)
					}
				}
			})
		}
		defer joinHolders()
		for range 4 {
			holders.Add(1)
			go func() {
				defer holders.Done()
				holderErrors <- server.HandleError1(func() error {
					server.Db(ctx, func(server.PgConn) {
						ready <- struct{}{}
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
					return nil
				}, func(err error) error { return err })
			}()
		}
		for range 4 {
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("pool holder barrier did not complete")
			}
		}
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		canceledLabels := map[string]string{"pool": "default", "outcome": "canceled"}
		acquiresBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		canceledBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", canceledLabels)
		queriesBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		const requests = 8
		results := make(chan error, requests)
		var workers sync.WaitGroup
		for range requests {
			workers.Add(1)
			go func() {
				defer workers.Done()
				requestCtx, requestCancel := context.WithTimeout(ctx, 2*time.Second)
				defer requestCancel()
				args, err := generator.NewClientArgsContext(requestCtx)
				if args != nil || !errors.Is(err, context.DeadlineExceeded) {
					err = errors.New("held pool did not return deadline without an identity")
				} else {
					err = nil
				}
				results <- err
			}()
		}
		workers.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore ||
			authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", canceledLabels) != canceledBefore+requests ||
			authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queriesBefore ||
			authorityMintPoolGauge(t, "acquired") != 4 {
			t.Fatal("Acquire deadline crossed into JWT SQL or lost its held-pool boundary")
		}
		joinHolders()
		if authorityMintChildCount(ctx, owner) != 0 {
			t.Fatal("pool-refused requests created durable children")
		}
		before := authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid")
		args, err := generator.NewClientArgsContext(ctx)
		if err != nil || args == nil || authorityMintChildCount(ctx, owner) != 1 ||
			authorityStateQueryMetric(t, "hosted", "mint", "client", "state_valid") != before+1 {
			t.Fatal("released pool did not restore the same owner's healthy mint", err)
		}
	})
}

// Bootstrap is not an authorization lease: revoking the parent's active row
// after construction must refuse every later mint before durable child writes.
func TestAuthorityMintRejectsParentRevocationAfterBootstrap(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, _ := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, owner.clientId))
		})
		before := authorityStateQueryMetric(t, "hosted", "mint", "client", "no_active_row")
		for range 4 {
			args, err := generator.NewClientArgsContext(ctx)
			authorityTestUnauthorized(t, err)
			if args != nil {
				t.Fatal("revoked parent minted a child")
			}
		}
		if authorityMintChildCount(ctx, owner) != 0 ||
			authorityStateQueryMetric(t, "hosted", "mint", "client", "no_active_row") != before+4 {
			t.Fatal("revoked parent work or child count changed")
		}
	})
}

// A returned validation row can still reject credential rotation. Neither
// bootstrap success nor an earlier successful mint authorizes the next mint.
func TestAuthorityMintRejectsCredentialRotationAfterSuccess(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, parent := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		if args, err := generator.NewClientArgsContext(ctx); err != nil || args == nil {
			t.Fatal("initial mint failed", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, parent.UserId, parent.CreateTime.Add(time.Minute)))
		})
		before := authorityStateQueryMetric(t, "hosted", "mint", "client", "credential_rotated")
		for range 4 {
			args, err := generator.NewClientArgsContext(ctx)
			authorityTestUnauthorized(t, err)
			if args != nil {
				t.Fatal("rotated parent minted another child")
			}
		}
		if authorityMintChildCount(ctx, owner) != 1 ||
			authorityStateQueryMetric(t, "hosted", "mint", "client", "credential_rotated") != before+4 {
			t.Fatal("rotation refusal reused earlier authorization or counted its row as success")
		}
	})
}

// A fixed synthetic restored record avoids waiting for an asynchronous store
// publication. Store callbacks are no-ops; the generator still owns and joins
// its actual load and persistence workers.
type authorityMintIdentityStore struct {
	identity connect.WindowClientIdentity
}

func (self *authorityMintIdentityStore) LoadWindowClientIdentities() []*connect.WindowClientIdentity {
	identity := self.identity
	return []*connect.WindowClientIdentity{&identity}
}

func (self *authorityMintIdentityStore) StoreWindowClientIdentities([]*connect.WindowClientIdentity) {
}

// Continuity reuses one exact persisted child without minting. The store is
// consumed once: a second independent request remains a distinct fresh child.
func TestAuthorityMintRestoredIdentityAvoidsFreshQueries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, _ := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		destination, err := connect.NewMultiHopId(connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		original, err := generator.NewClientArgsForDestinationContext(ctx, destination)
		if err != nil || original == nil || original.ClientAuth == nil {
			t.Fatal("initial identity creation failed", err)
		}
		restored, closeRestored := authorityMintGenerator(t, ctx, owner)
		defer closeRestored()
		restored.SetIdentityStore(&authorityMintIdentityStore{identity: connect.WindowClientIdentity{
			ClientId: original.ClientId, ByJwt: original.ClientAuth.ByJwt,
			InstanceId: original.ClientAuth.InstanceId, Destination: destination,
		}})
		poolLabels := map[string]string{"pool": "default", "outcome": "acquired"}
		acquiresBefore := authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels)
		queriesBefore := authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
		reused, err := restored.NewClientArgsForDestinationContext(ctx, destination)
		if err != nil || reused == nil || reused.ClientAuth == nil || reused.ClientId != original.ClientId ||
			reused.ClientAuth.ByJwt != original.ClientAuth.ByJwt || reused.ClientAuth.InstanceId != original.ClientAuth.InstanceId {
			t.Fatal("persisted destination identity was not reused", err)
		}
		if authorityObservedCounter(t, "urnetwork_pg_pool_acquires_total", poolLabels) != acquiresBefore ||
			authorityObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queriesBefore {
			t.Fatal("restored identity performed a fresh auth query")
		}
		fresh, err := restored.NewClientArgsForDestinationContext(ctx, destination)
		if err != nil || fresh == nil || fresh.ClientId == original.ClientId || authorityMintChildCount(ctx, owner) != 2 {
			t.Fatal("restored record was consumed more than once", err)
		}
	})
}
