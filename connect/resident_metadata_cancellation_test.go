package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The database lock is a real blocked profile lookup. The disposable fixture
// owns its PostgreSQL database and Redis namespace; no dependency is mocked.
func holdResidentProfileTable(t testing.TB, ctx context.Context) func() {
	t.Helper()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		server.HandleError(func() {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, "BEGIN"))
				defer conn.Exec(context.WithoutCancel(ctx), "ROLLBACK")
				server.RaisePgResult(conn.Exec(ctx, "LOCK TABLE network_client IN ACCESS EXCLUSIVE MODE"))
				close(locked)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}, server.OptReadWrite())
		})
	}()
	select {
	case <-locked:
	case <-done:
		t.Fatal("profile lock owner exited before acquiring its lock")
	case <-ctx.Done():
		t.Fatal("profile lock admission exceeded fixture deadline")
	}
	return func() {
		once.Do(func() { close(release) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("profile lock owner did not join")
		}
	}
}

func waitForBlockedResidentProfiles(t testing.TB, ctx context.Context, count int) {
	t.Helper()
	if err := TestingWaitForConnectCondition(ctx, 10*time.Second, time.Millisecond, func(ctx context.Context) (bool, string) {
		blocked := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM pg_locks
				WHERE database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND relation = 'network_client'::regclass AND NOT granted
			`).Scan(&blocked))
		})
		return blocked == count, "waiting for the exact blocked profile cohort"
	}); err != nil {
		t.Fatal(err)
	}
}

func joinResidentMetadataWorkers(done <-chan struct{}, count int, timeout time.Duration) int {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	joined := 0
	for joined < count {
		select {
		case <-done:
			joined++
		case <-timer.C:
			return joined
		}
	}
	return joined
}

// Repeatedly replaced residents must stop queued metadata work while the
// exchange and the dependency lock both remain live. Releasing the dependency
// first would hide the original exchange-context ownership bug.
func TestResidentRefreshCancellationReleasesBlockedProfileCohort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		exchange.settings.EnableNetworkPeers = true
		defer exchange.Close()
		const count = 8
		residents := make([]*Resident, 0, count)
		defer func() {
			for _, resident := range residents {
				if err := resident.CloseAndWait(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
		for range count {
			resident := NewResident(exchange.ctx, exchange, server.NewId(), server.NewId(), server.NewId())
			residents = append(residents, resident)
			networkId := server.NewId()
			resident.peerNetworkId = &networkId
			resident.transports[&clientTransport{}] = true
			if !model.NominateResident(ctx, nil, &model.NetworkClientResident{
				ClientId: resident.clientId, InstanceId: resident.instanceId, ResidentId: resident.residentId,
			}, time.Minute) {
				t.Fatal("fixture could not reserve a resident")
			}
		}
		release := holdResidentProfileTable(t, ctx)
		defer release()
		done := make(chan struct{}, count)
		for _, resident := range residents {
			go func() {
				defer func() { done <- struct{}{} }()
				exchange.refreshResidentRegistration(resident)
			}()
		}
		waitForBlockedResidentProfiles(t, ctx, count)
		for _, resident := range residents {
			resident.Cancel()
		}
		joined := joinResidentMetadataWorkers(done, count, 3*time.Second)
		parentAlive := exchange.ctx.Err() == nil
		release()
		if joined < count {
			joinResidentMetadataWorkers(done, count-joined, 10*time.Second)
		}
		t.Logf("blocked_profile_workers=%d canceled_workers_joined_before_dependency_release=%d exchange_alive=%t", count, joined, parentAlive)
		if joined != count || !parentAlive {
			t.Fatalf("retired resident metadata outlived its owner: joined=%d/%d exchange_alive=%t", joined, count, parentAlive)
		}
	})
}

// Real transport admission performs Redis nomination and constructs the
// internal SDK client before the peer-profile query blocks. Canceling all
// callers must join those constructors without waiting for the database lock.
func TestResidentTransportCancellationAbortsBlockedNominationCohort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		defer exchange.Close()
		var ledger clientconnect.TransferMemoryOwnerLedger
		exchange.memoryOwnerLedger = &ledger
		const count = 8
		prepared := make(chan *Resident, count)
		exchange.beforeResidentProfileForTest = func(resident *Resident) { prepared <- resident }
		release := holdResidentProfileTable(t, ctx)
		defer release()
		transports := make([]*ResidentTransport, 0, count)
		residents := make([]*Resident, 0, count)
		done := make(chan struct{}, count)
		defer func() {
			for _, transport := range transports {
				transport.Close()
			}
			release()
			exchange.Close()
			if !exchange.WaitForIdle(ctx) {
				t.Error("nomination fixture did not join exchange ownership")
			}
			for _, resident := range residents {
				if err := resident.CloseAndWait(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
		for range count {
			transport := NewResidentTransport(ctx, exchange, server.NewId(), server.NewId())
			transports = append(transports, transport)
			go func() {
				defer func() { done <- struct{}{} }()
				server.HandleError(transport.Run)
			}()
		}
		for range count {
			select {
			case resident := <-prepared:
				residents = append(residents, resident)
			case <-ctx.Done():
				t.Fatal("nomination did not reach the real constructor")
			}
		}
		waitForBlockedResidentProfiles(t, ctx, count)
		for _, transport := range transports {
			transport.Cancel()
		}
		joined := joinResidentMetadataWorkers(done, count, 3*time.Second)
		stillLive := 0
		for _, resident := range residents {
			if !resident.client.IsDone() || resident.residentController.ctx.Err() == nil {
				stillLive++
			}
		}
		exchange.stateLock.Lock()
		published := len(exchange.residents)
		exchange.stateLock.Unlock()
		parentAlive := exchange.ctx.Err() == nil
		release()
		if joined < count {
			joinResidentMetadataWorkers(done, count-joined, 10*time.Second)
		}
		t.Logf("blocked_constructors=%d canceled_callers_joined_before_dependency_release=%d retained_constructor_clients=%d published_residents=%d exchange_alive=%t", count, joined, stillLive, published, parentAlive)
		if joined != count || stillLive != 0 || published != 0 || !parentAlive {
			t.Fatalf("abandoned nomination retained work: joined=%d/%d clients=%d published=%d exchange_alive=%t", joined, count, stillLive, published, parentAlive)
		}
		for _, transport := range transports {
			if resident := model.GetResidentForClient(ctx, transport.clientId, 0); resident != nil {
				t.Error("abandoned nomination kept its Redis reservation")
			}
		}
	})
}

func TestResidentNominationCanceledBeforeAdmission(t *testing.T) {
	exchange := newResidentConstructionExchange(t.Context())
	defer exchange.Close()
	callerCtx, cancelCaller := context.WithCancel(t.Context())
	cancelCaller()
	exchange.beforeResidentProfileForTest = func(*Resident) {
		t.Error("canceled caller reached resident construction")
	}
	if exchange.NominateLocalResidentWithContext(callerCtx, server.NewId(), server.NewId(), nil) {
		t.Fatal("already-canceled caller installed a resident")
	}
	if exchange.ctx.Err() != nil || len(exchange.residents) != 0 {
		t.Fatal("refusing canceled admission changed the live exchange")
	}
}

// Admission cancellation is not the lifetime of an already published resident:
// another carrier can share that resident after the nominating carrier leaves.
func TestResidentNominationTransfersLifetimeToExchange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		defer func() {
			exchange.Close()
			if !exchange.WaitForIdle(ctx) {
				t.Error("successful nomination fixture did not join")
			}
		}()
		callerCtx, cancelCaller := context.WithCancel(ctx)
		defer cancelCaller()
		clientId, instanceId := server.NewId(), server.NewId()
		if !exchange.NominateLocalResidentWithContext(callerCtx, clientId, instanceId, nil) {
			t.Fatal("healthy caller could not nominate a resident")
		}
		exchange.stateLock.Lock()
		resident := exchange.residents[clientId]
		exchange.stateLock.Unlock()
		if resident == nil {
			t.Fatal("successful nomination did not publish its resident")
		}
		_, _, closeTransport, err := resident.AddTransport()
		if err != nil {
			t.Fatal(err)
		}
		defer closeTransport()
		cancelCaller()
		if resident.ctx.Err() != nil || resident.client.IsDone() || resident.residentController.ctx.Err() != nil {
			t.Fatal("nominating carrier cancellation killed an installed shared resident")
		}
		current := model.GetResidentForClient(ctx, clientId, 0)
		if current == nil || current.ResidentId != resident.residentId {
			t.Fatal("nominating carrier cancellation removed an installed reservation")
		}
	})
}
