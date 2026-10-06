package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type privateLoadProcessConfig struct {
	PG       []byte
	Redis    []byte
	Owner    *model.ProberShardOwner
	Peer     server.Id
	Tokens   []string
	Clients  []server.Id
	PoolSize int
	Peers    []server.Id
}

type privateLoadProcessReport struct {
	Ready     bool
	Failure   string
	Completed int
	Failed    int
	Elapsed   time.Duration
	Counters  map[string]float64
	Contracts []*protocol.StoredContract
	Errors    map[string]int
	Started   time.Time
}

func privateLoadCreateWave(ctx context.Context, t testing.TB, f privateLoadFixture) privateLoadProcessReport {
	return privateLoadCreateWaveBarrier(ctx, t, f, nil, nil)
}

func privateLoadCreateWaveBarrier(ctx context.Context, t testing.TB, f privateLoadFixture, ready chan<- struct{}, release <-chan struct{}) privateLoadProcessReport {
	t.Helper()
	before := privateLoadStats(t)
	start := make(chan struct{})
	contracts := make([]*protocol.StoredContract, len(f.tokens))
	errs := make([]error, len(f.tokens))
	var wg sync.WaitGroup
	for index := range f.tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			destination := f.peer
			if len(f.peers) > 0 {
				destination = f.peers[index%len(f.peers)]
			}
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			contracts[index], errs[index] = privateLoadCall(requestCtx, f, f.tokens[index], &protocol.CreateContract{
				DestinationId: destination.Bytes(), TransferByteCount: uint64(connect.DefaultContractManagerSettings().StandardContractTransferByteCount),
			})
		}()
	}
	if ready != nil {
		ready <- struct{}{}
		<-release
	}
	began := time.Now()
	close(start)
	wg.Wait()
	report := privateLoadProcessReport{Elapsed: time.Since(began), Started: began, Contracts: contracts, Counters: privateLoadStats(t), Errors: map[string]int{}}
	for index, err := range errs {
		if err == nil && contracts[index] != nil {
			report.Completed++
		} else {
			report.Failed++
			kind := "other"
			var refusal *privateLoadProtocolRefusal
			switch {
			case errors.As(err, &refusal):
				kind = "protocol_" + refusal.reason.String()
				if errors.Is(refusal.requestError, context.DeadlineExceeded) {
					kind += "_request_deadline"
				}
				if errors.Is(refusal.requestError, context.Canceled) {
					kind += "_request_canceled"
				}
			case errors.Is(err, context.DeadlineExceeded):
				kind = "deadline"
			case errors.Is(err, context.Canceled):
				kind = "canceled"
			case err == nil:
				kind = "missing_result"
			}
			report.Errors[kind]++
		}
	}
	for key := range report.Counters {
		report.Counters[key] -= before[key]
	}
	return report
}

// One test-owned observer connection samples only the disposable database.
// Observations are finite source shapes; query text and local identities are
// discarded. Query age is never described as a lock's residence duration.
func privateLoadStartObserver(ctx context.Context, t testing.TB) func() map[string]float64 {
	t.Helper()
	conn, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan map[string]float64, 1)
	go func() {
		defer conn.Release()
		values := map[string]float64{}
		for {
			rows, err := conn.Query(ctx, `SELECT query,state,COALESCE(wait_event_type,''),
				EXTRACT(epoch FROM clock_timestamp()-query_start)::double precision
				FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state<>'idle'`)
			if err != nil {
				values["sampling_error"]++
				break
			}
			counts := map[string]float64{}
			for rows.Next() {
				var query, state, wait string
				var age float64
				if err := rows.Scan(&query, &state, &wait, &age); err != nil {
					values["sampling_error"]++
					continue
				}
				query = strings.ToLower(strings.Join(strings.Fields(query), " "))
				kind := "other"
				switch {
				case strings.Contains(query, "update network_client as top"):
					kind = "usage_stamp"
				case strings.Contains(query, "coalesce(sum(selected_escrow.balance_byte_count)"):
					kind = "census"
				case strings.Contains(query, "snapshot") && strings.Contains(query, "transfer_balance_net_escrow"):
					kind = "cache"
				case strings.Contains(query, "transfer_balance") && strings.Contains(query, "for update"):
					kind = "balance"
				case strings.Contains(query, "update transfer_escrow"):
					kind = "metadata"
				case strings.Contains(query, "transfer_contract") && strings.Contains(query, "for update"):
					kind = "contract"
				}
				if wait == "Lock" {
					kind += "_lock"
				} else if state == "idle in transaction" {
					kind += "_idle_tx"
				} else if wait != "" {
					kind += "_other_wait"
				} else {
					kind += "_active"
				}
				counts[kind]++
				values[kind+"_max_query_age_seconds"] = max(values[kind+"_max_query_age_seconds"], age)
			}
			rows.Close()
			for kind, count := range counts {
				values[kind+"_peak"] = max(values[kind+"_peak"], count)
			}
			values["samples"]++
			select {
			case <-stop:
				done <- values
				return
			case <-ctx.Done():
				done <- values
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		done <- values
	}()
	return func() map[string]float64 { close(stop); return <-done }
}

// Invoked only by the parent test's own executable. It does not create, reset,
// migrate or flush any database. Ephemeral test resources and synthetic minted
// credentials travel over anonymous stdin, never arguments or output artifacts.
func TestPrivateProviderLoadedPeerProcess(t *testing.T) {
	if os.Getenv("URNETWORK_PRIVATE_LOAD_CHILD") != "1" {
		t.Skip("owned subprocess only")
	}
	reports, err := privateLoadChildReportPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(reports)
	var config privateLoadProcessConfig
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	if config.PoolSize != 16 {
		_ = encoder.Encode(privateLoadProcessReport{Failure: "child pool size is not the independent workload size"})
		t.Fatal("child pool size is not the independent workload size")
	}
	if err := server.ValidateTestEnvironmentChildResources(t.Context(), config.PG, config.Redis); err != nil {
		_ = encoder.Encode(privateLoadProcessReport{Failure: err.Error()})
		t.Fatal(err)
	}
	popPG := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, config.PG)
	popMaintenance := server.Vault.PushSimpleResource(server.MaintenancePgVaultResourceName, config.PG)
	popRedis := server.Vault.PushSimpleResource("redis.yml", config.Redis)
	popConfig := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 16\n"))
	defer popPG()
	defer popMaintenance()
	defer popRedis()
	defer popConfig()
	server.PgReset()
	server.RedisReset()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	identity, err := model.ProberShardIdentity(ctx, config.Owner)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := newProviderEgressCredentials(identity)
	if err != nil {
		t.Fatal(err)
	}
	// The parent minted these exact children. This second process starts with
	// a different local queue and pool; every request still validates current
	// signed credentials against their durable private network ownership.
	for _, client := range config.Clients {
		credentials.children.Store(client, struct{}{})
	}
	notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
	defer notifications.Close()
	control, err := newProviderEgressControl(credentials, notifications)
	if err != nil {
		t.Fatal(err)
	}
	f := privateLoadFixture{control: control, owner: config.Owner, peer: config.Peer, tokens: config.Tokens, clients: config.Clients, peers: config.Peers}
	if err := encoder.Encode(privateLoadProcessReport{Ready: true}); err != nil {
		t.Fatal(err)
	}
	var previous []*protocol.StoredContract
	for {
		var command string
		if err := decoder.Decode(&command); err != nil {
			t.Fatal(err)
		}
		if command == "stop" {
			return
		}
		var report privateLoadProcessReport
		switch command {
		case "prepare_create", "prepare_create_shared":
			waveFixture := f
			if command == "prepare_create_shared" {
				waveFixture.peers = nil
			}
			ready, release := make(chan struct{}), make(chan struct{})
			result := make(chan privateLoadProcessReport, 1)
			go func() { result <- privateLoadCreateWaveBarrier(ctx, t, waveFixture, ready, release) }()
			<-ready
			if err := encoder.Encode(privateLoadProcessReport{Ready: true}); err != nil {
				t.Fatal(err)
			}
			var next string
			if err := decoder.Decode(&next); err != nil || next != "go" {
				t.Fatal("cross-process barrier not released")
			}
			close(release)
			report = <-result
			previous = report.Contracts
		case "prepare_settle":
			ready, release := make(chan struct{}), make(chan struct{})
			result := make(chan privateLoadProcessReport, 1)
			go func() { result <- privateLoadSettleWave(ctx, t, f, previous, ready, release) }()
			<-ready
			if err := encoder.Encode(privateLoadProcessReport{Ready: true}); err != nil {
				t.Fatal(err)
			}
			var next string
			if err := decoder.Decode(&next); err != nil || next != "go" {
				t.Fatal("settlement barrier not released")
			}
			close(release)
			report = <-result
		case "create":
			report = privateLoadCreateWave(ctx, t, f)
			previous = report.Contracts
		case "settle":
			before := privateLoadStats(t)
			start := make(chan struct{})
			results := make(chan error, len(previous))
			for index, stored := range previous {
				go func() {
					<-start
					requestCtx, cancelRequest := context.WithTimeout(ctx, 10*time.Second)
					defer cancelRequest()
					if stored == nil {
						results <- errors.New("prior contract absent")
						return
					}
					if _, err := privateLoadCall(requestCtx, f, f.tokens[index], &protocol.CloseContract{ContractId: stored.ContractId}); err != nil {
						results <- err
						return
					}
					results <- server.HandleError1(func() error {
						id, err := server.IdFromBytes(stored.ContractId)
						if err != nil {
							return err
						}
						// The independent provider acknowledgment invokes the same
						// current model authority as its API/Connect controller.
						return model.CloseContract(requestCtx, id, f.peer, 0, false)
					}, func(err error) error { return err })
				}()
			}
			began := time.Now()
			close(start)
			for range previous {
				if <-results == nil {
					report.Completed++
				} else {
					report.Failed++
				}
			}
			report.Elapsed, report.Counters = time.Since(began), privateLoadStats(t)
			for key := range report.Counters {
				report.Counters[key] -= before[key]
			}
		default:
			t.Fatal("invalid child command")
		}
		if err := encoder.Encode(report); err != nil {
			t.Fatal(err)
		}
	}
}

// Observe accounting before the finite success assertion, including RED arms.
// Returned success is deliberately not used as a committed-contract oracle.
func privateLoadObserveAccounting(t testing.TB, ctx context.Context, f privateLoadFixture) {
	t.Helper()
	var exact, promised, remaining, start, cached model.ByteCount
	var current bool
	var open, terminal int64
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
   (SELECT COALESCE(sum(e.balance_byte_count),0) FROM transfer_escrow e JOIN transfer_contract c USING(contract_id)
    WHERE e.balance_id=$1 AND NOT e.settled AND c.outcome IS NULL),
   (SELECT COALESCE(sum(transfer_byte_count),0) FROM transfer_contract WHERE payer_network_id=$2 AND outcome IS NULL),
   b.balance_byte_count,b.start_balance_byte_count,s.reserved_byte_count,s.revision=r.revision,
   (SELECT count(*) FROM transfer_contract WHERE payer_network_id=$2 AND outcome IS NULL AND transfer_byte_count>1),
   (SELECT count(*) FROM transfer_contract WHERE payer_network_id=$2 AND outcome IS NOT NULL)
   FROM transfer_balance b JOIN transfer_balance_net_escrow_snapshot s USING(balance_id)
   JOIN transfer_balance_net_escrow_revision r USING(balance_id)
   WHERE balance_id=$1`, f.owner.BalanceId, f.owner.NetworkId).Scan(&exact, &promised, &remaining, &start, &cached, &current, &open, &terminal))
	})
	mirror := model.Testing_NetEscrowByteCount(ctx, f.owner.BalanceId)
	t.Logf("accounting_before_completion_gate exact_reserved=%d committed_open_promises=%d remaining=%d start=%d cache_current=%t cache_reserved=%d mirror_reserved=%d committed_open_standard_contracts=%d terminal_contracts=%d", exact, promised, remaining, start, current, cached, mirror, open, terminal)
	if exact != promised || exact < 0 || exact > remaining || remaining != start || (current && cached != exact) {
		t.Fatal("authoritative accounting invariant failed before completion gate")
	}
}

func TestPrivateProviderLoadedIndependentProcesses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
		defer cancel()
		f := newPrivateLoadFixture(t, ctx, 0, 10001)
		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		child := privateLoadStartPeerProcess(t, ctx, 50*time.Second)
		defer child.close(t)
		encoder, decoder := child.encoder, child.decoder
		config := privateLoadProcessConfig{
			PG:    server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName).Bytes(),
			Redis: server.Vault.RequireSimpleResource("redis.yml").Bytes(), Owner: f.owner,
			Peer: f.peer, Tokens: f.tokens, Clients: f.clients, PoolSize: 16,
		}
		if err := encoder.Encode(config); err != nil {
			t.Fatal(err)
		}
		child.requireReady(t)
		want := model.ByteCount(10001)
		var childCreated []*protocol.StoredContract
		for _, command := range []string{"create", "settle"} {
			observe := privateLoadStartObserver(ctx, t)
			if err := encoder.Encode(command); err != nil {
				t.Fatal(err)
			}
			local := privateLoadCreateWave(ctx, t, f)
			var remote privateLoadProcessReport
			if err := decoder.Decode(&remote); err != nil {
				t.Fatalf("independent process did not return: decode=%v private_diagnostics=%s", err, child.diagnostics)
			}
			t.Logf("independent_processes history=10001 operation=%s local_pool=1 remote_pool=16 local=%d/%d elapsed=%s errors=%v remote=%d/%d elapsed=%s local_counters=%v remote_counters=%v activity=%v", command, local.Completed, local.Failed, local.Elapsed, local.Errors, remote.Completed, remote.Failed, remote.Elapsed, local.Counters, remote.Counters, observe())
			privateLoadObserveAccounting(t, ctx, f)
			if local.Completed != 64 || local.Failed != 0 || remote.Completed != 64 || remote.Failed != 0 {
				t.Fatalf("independent process finite operation failed: local=%d/%d remote=%d/%d", local.Completed, local.Failed, remote.Completed, remote.Failed)
			}
			for _, stored := range local.Contracts {
				want += model.ByteCount(stored.TransferByteCount)
			}
			if command == "create" {
				childCreated = remote.Contracts
				for _, stored := range childCreated {
					want += model.ByteCount(stored.TransferByteCount)
				}
				if local.Counters["selected_first"] <= 0 || remote.Counters["selected_first"] <= 0 {
					t.Fatal("independent processes did not exercise selected-first allocation")
				}
			} else {
				for _, stored := range childCreated {
					want -= model.ByteCount(stored.TransferByteCount)
				}
			}
			privateLoadAssertAccounting(t, ctx, f, want)
			t.Logf("independent_processes operation=%s exact_reserved=%d", command, want)
		}
		if err := encoder.Encode("stop"); err != nil {
			t.Fatal(err)
		}
		if err := child.wait(); err != nil {
			t.Fatalf("independent process did not join: %v private_diagnostics=%s", err, child.diagnostics)
		}
	})
}

func TestPrivateProviderCreationDoesNotQueueOnSharedFinancialRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
		defer cancel()
		f := newPrivateLoadFixture(t, ctx, 0, 0)
		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		child := privateLoadStartPeerProcess(t, ctx, 90*time.Second)
		defer child.close(t)
		encoder, decoder := child.encoder, child.decoder
		config := privateLoadProcessConfig{
			PG:    server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName).Bytes(),
			Redis: server.Vault.RequireSimpleResource("redis.yml").Bytes(), Owner: f.owner,
			Peer: f.peer, Tokens: f.tokens, Clients: f.clients, PoolSize: 16,
		}
		if err := encoder.Encode(config); err != nil {
			t.Fatal(err)
		}
		child.requireReady(t)
		warm := privateLoadCreateWave(ctx, t, f)
		if warm.Completed != 64 || warm.Failed != 0 {
			t.Fatal("healthy warm control failed")
		}
		server.Db(ctx, func(conn server.PgConn) {
			// New default admission never creates legacy revision/cache rows.
			// Seed those historical rows explicitly so every barrier holds a
			// real shared row while the independent creator must still finish.
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_revision(balance_id,revision)
                VALUES($1,1) ON CONFLICT(balance_id) DO NOTHING`, f.owner.BalanceId))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_snapshot(balance_id,revision,reserved_byte_count)
                SELECT balance_id,revision,0 FROM transfer_balance_net_escrow_revision WHERE balance_id=$1
                ON CONFLICT(balance_id) DO NOTHING`, f.owner.BalanceId))
			var foreignKeys int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype='f' AND conrelid='transfer_escrow'::regclass AND confrelid='transfer_balance'::regclass`).Scan(&foreignKeys))
			t.Logf("actual escrow-to-balance foreign keys=%d", foreignKeys)
		})
		for _, arm := range []struct{ name, query string }{
			{"balance", `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`},
			{"revision", `SELECT 1 FROM transfer_balance_net_escrow_revision WHERE balance_id=$1 FOR UPDATE`},
			{"snapshot", `SELECT 1 FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1 FOR UPDATE`},
		} {
			conn, err := server.AcquireMaintenanceDbConn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held, err := conn.Begin(ctx)
			if err != nil {
				conn.Release()
				t.Fatal(err)
			}
			tag, err := held.Exec(ctx, arm.query, f.owner.BalanceId)
			if err != nil || tag.RowsAffected() != 1 {
				held.Rollback(context.WithoutCancel(ctx))
				conn.Release()
				t.Fatal("barrier row not held", err)
			}
			observe := privateLoadStartObserver(ctx, t)
			if err := encoder.Encode("create"); err != nil {
				t.Fatal(err)
			}
			var remote privateLoadProcessReport
			decodeErr := decoder.Decode(&remote)
			activity := observe()
			// Do not release the blocker until the actual child returns.
			rollbackErr := held.Rollback(context.WithoutCancel(ctx))
			conn.Release()
			if decodeErr != nil || rollbackErr != nil {
				t.Fatal("child/barrier did not join", decodeErr, rollbackErr)
			}
			t.Logf("contention barrier=%s completed=%d failed=%d elapsed=%s error_classes=%v activity=%v", arm.name, remote.Completed, remote.Failed, remote.Elapsed, remote.Errors, activity)
			if remote.Completed != 64 || remote.Failed != 0 {
				t.Errorf("contract creation queued on shared %s row", arm.name)
			}
			if err := encoder.Encode("create"); err != nil {
				t.Fatal(err)
			}
			var healthy privateLoadProcessReport
			if err := decoder.Decode(&healthy); err != nil || healthy.Completed != 64 || healthy.Failed != 0 {
				t.Fatal("released barrier healthy control failed", err)
			}
		}
		if err := encoder.Encode("stop"); err != nil {
			t.Fatal(err)
		}
		if err := child.wait(); err != nil {
			t.Fatal("child did not join", err)
		}
	})
}
