// Deterministic membership and cancellation controls use actual current Rpc
// observations and original registration writes, never accepted read fixtures.
package controller

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// An unexpected real operation error is reported at its barrier instead of
// hiding the cause behind the package's outer timeout.
func stClientKeyRegistrationAwait(t testing.TB, reached <-chan struct{}, ended <-chan error) {
	t.Helper()
	select {
	case <-reached:
	case err := <-ended:
		t.Fatal("registration ended before its required barrier", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

// A query after all operation returns observes real durable state, not a
// transport ack or an in-memory success flag.
func stClientKeyRegistrationAssertNoRow(t testing.TB, clientId server.Id) {
	t.Helper()
	server.Db(t.Context(), func(conn server.PgConn) {
		var count int
		if err := conn.QueryRow(t.Context(), `SELECT COUNT(*) FROM st_client_key_history WHERE client_id = $1`, clientId).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("refused registration acquired a durable signed row", clientId)
		}
	})
}

// A later authenticated arrival cannot consume a boundary whose current read
// already started. Both actual finalized observations and signatures differ.
func TestStClientKeyRegistrationCohortLateArrivalUsesFreshBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, cfg, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		endpoint := stClientKeyRegistrationConnectEndpoint(tb)
		ctx, cancel := context.WithCancel(tb.Context())
		var workers sync.WaitGroup
		tb.Cleanup(func() { cancel(); workers.Wait() })
		clientId, deviceId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(tb.Context(), credential.NetworkId, deviceId, clientId, "late-registration", "test")
		later := credential.Client(deviceId, clientId)
		entered, continueRead := make(chan struct{}), make(chan struct{})
		admitted := make(chan struct{}, 2)
		cohorts.afterAdmissionForTest = func() { admitted <- struct{}{} }
		initial := fixture.snapshot().boundary
		next := protocol.ClientKeyEffectiveBoundary{Block: 200, Hash: [32]byte{6}}
		fixture.beforeFinalizedForTest = func(ctx context.Context, index int) {
			if index == 1 {
				close(entered)
				select {
				case <-ctx.Done():
				case <-continueRead:
				}
			}
			if index == 3 {
				fixture.stateLock.Lock()
				fixture.base.boundary = next
				fixture.boundaries[common.Hash(next.Hash)] = next
				fixture.stateLock.Unlock()
			}
		}
		firstResult, nextResult := make(chan error, 1), make(chan error, 1)
		key := bytes.Repeat([]byte{9}, 32)
		workers.Add(1)
		go func() {
			defer workers.Done()
			firstResult <- stClientKeyRegistrationRequest(ctx, endpoint.URL, credential.Sign(), key)
		}()
		stClientKeyRegistrationAwait(tb, entered, firstResult)
		stClientKeyRegistrationAwait(tb, admitted, firstResult)
		workers.Add(1)
		go func() {
			defer workers.Done()
			nextResult <- stClientKeyRegistrationRequest(ctx, endpoint.URL, later.Sign(), key)
		}()
		select {
		case <-admitted:
		case err := <-nextResult:
			tb.Fatal(err)
		case <-ctx.Done():
			tb.Fatal(ctx.Err())
		}
		cohorts.stateLock.Lock()
		separate := cohorts.active != nil && cohorts.active.sealed && cohorts.active.waiters == 1 && cohorts.pending != nil && !cohorts.pending.sealed && cohorts.pending.waiters == 1
		cohorts.stateLock.Unlock()
		if !separate {
			tb.Fatal("late client joined an already-reading cohort")
		}
		close(continueRead)
		for _, result := range []chan error{firstResult, nextResult} {
			select {
			case err := <-result:
				if err != nil {
					tb.Fatal(err)
				}
			case <-ctx.Done():
				tb.Fatal(ctx.Err())
			}
		}
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, credential, key, initial)
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, later, key, next)
		if requests, methods := fixture.counts(); requests != 9 || methods != 41 {
			tb.Fatal("two real current observations plus cold dial differ", requests, methods)
		}
	})
}

// Canceling the member that created the cohort cannot cancel a surviving
// member's real chain read, Sql generation or immutable publication.
func TestStClientKeyRegistrationCohortOneCancellationPreservesSurvivor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, cfg, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		firstCtx, cancelFirst := context.WithCancel(tb.Context())
		secondCtx, cancelSecond := context.WithCancel(tb.Context())
		var workers sync.WaitGroup
		tb.Cleanup(func() { cancelFirst(); cancelSecond(); workers.Wait() })
		clientId, deviceId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(tb.Context(), credential.NetworkId, deviceId, clientId, "surviving-registration", "test")
		survivor := credential.Client(deviceId, clientId)
		admitted, seal := make(chan struct{}, 2), make(chan struct{})
		entered, continueRead := make(chan struct{}), make(chan struct{})
		cohorts.afterAdmissionForTest = func() { admitted <- struct{}{} }
		cohorts.beforeSealForTest = func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-seal:
			}
		}
		fixture.beforeFinalizedForTest = func(ctx context.Context, index int) {
			if index == 1 {
				close(entered)
				select {
				case <-ctx.Done():
				case <-continueRead:
				}
			}
		}
		key := bytes.Repeat([]byte{9}, 32)
		firstResult, secondResult := make(chan error, 1), make(chan error, 1)
		workers.Add(1)
		go func() { defer workers.Done(); firstResult <- StRegisterClientKey(firstCtx, *credential.ClientId, key) }()
		stClientKeyRegistrationAwait(tb, admitted, firstResult)
		workers.Add(1)
		go func() { defer workers.Done(); secondResult <- StRegisterClientKey(secondCtx, clientId, key) }()
		stClientKeyRegistrationAwait(tb, admitted, secondResult)
		close(seal)
		stClientKeyRegistrationAwait(tb, entered, secondResult)
		cancelFirst()
		if err := <-firstResult; !errors.Is(err, context.Canceled) {
			tb.Fatal("first member cancellation lost its cause", err)
		}
		stClientKeyRegistrationAssertNoRow(tb, *credential.ClientId)
		close(continueRead)
		if err := <-secondResult; err != nil {
			tb.Fatal("first member canceled the surviving authority read", err)
		}
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, survivor, key, fixture.snapshot().boundary)
		if requests, methods := fixture.counts(); requests != 5 || methods != 21 {
			tb.Fatal("cancellation caused a second or partial authority read", requests, methods)
		}
	})
}

// The final departing member cancels and joins the actual paused transport;
// no queue or background owner survives the completed registration calls.
func TestStClientKeyRegistrationCohortAllCancellationJoinsActualRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, _, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		firstCtx, cancelFirst := context.WithCancel(tb.Context())
		secondCtx, cancelSecond := context.WithCancel(tb.Context())
		var workers sync.WaitGroup
		tb.Cleanup(func() { cancelFirst(); cancelSecond(); workers.Wait() })
		admitted, seal := make(chan struct{}, 2), make(chan struct{})
		entered, transportDone := make(chan struct{}), make(chan struct{})
		cohorts.afterAdmissionForTest = func() { admitted <- struct{}{} }
		cohorts.beforeSealForTest = func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-seal:
			}
		}
		fixture.beforeFinalizedForTest = func(ctx context.Context, index int) {
			if index == 1 {
				close(entered)
				<-ctx.Done()
				close(transportDone)
			}
		}
		key := bytes.Repeat([]byte{9}, 32)
		results := make(chan error, 2)
		workers.Add(1)
		go func() { defer workers.Done(); results <- StRegisterClientKey(firstCtx, *credential.ClientId, key) }()
		stClientKeyRegistrationAwait(tb, admitted, results)
		workers.Add(1)
		go func() { defer workers.Done(); results <- StRegisterClientKey(secondCtx, *credential.ClientId, key) }()
		stClientKeyRegistrationAwait(tb, admitted, results)
		close(seal)
		stClientKeyRegistrationAwait(tb, entered, results)
		cancelFirst()
		cancelSecond()
		for index := 0; index < 2; index++ {
			if err := <-results; !errors.Is(err, context.Canceled) {
				tb.Fatal("registration did not retain cancellation", err)
			}
		}
		select {
		case <-transportDone:
		case <-tb.Context().Done():
			tb.Fatal(tb.Context().Err())
		}
		cohorts.stateLock.Lock()
		closed := cohorts.slots == 0 && cohorts.pending == nil && cohorts.active == nil
		cohorts.stateLock.Unlock()
		if !closed {
			tb.Fatal("canceled cohort retained work after joining calls")
		}
		stClientKeyRegistrationAssertNoRow(tb, *credential.ClientId)
	})
}

// Capacity counts complete registration owners, even after the shared read
// finishes. The 1,025th admission cannot spend Rpc or release somebody's slot.
func TestStClientKeyRegistrationCohortExactCapacityAndOneOver(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, _, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		seal := make(chan struct{})
		cohorts.beforeSealForTest = func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-seal:
			}
		}
		owner, err := newStClientKeyAuthorityOwner()
		if err != nil {
			tb.Fatal(err)
		}
		members := make([]*stClientKeyRegistrationReservation, 0, maxStClientKeyRegistrationSlots)
		tb.Cleanup(func() {
			for _, member := range members {
				member.release()
			}
		})
		for index := 0; index < maxStClientKeyRegistrationSlots; index++ {
			member, err := cohorts.admit(tb.Context(), owner)
			if err != nil {
				tb.Fatal("exact capacity refused", index, err)
			}
			members = append(members, member)
		}
		if member, err := cohorts.admit(tb.Context(), owner); err == nil || member != nil {
			tb.Fatal("one-over registration escaped finite admission")
		}
		if requests, methods := fixture.counts(); requests != 0 || methods != 0 {
			tb.Fatal("one-over admission reached Rpc before sealing")
		}
		close(seal)
		if _, _, err := members[0].readBoundary(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		if member, err := cohorts.admit(tb.Context(), owner); err == nil || member != nil {
			tb.Fatal("finished authority read released still-owned registration slots")
		}
		for _, member := range members {
			member.release()
		}
		cohorts.stateLock.Lock()
		closed := cohorts.slots == 0 && cohorts.pending == nil && cohorts.active == nil
		cohorts.stateLock.Unlock()
		if !closed {
			tb.Fatal("exact capacity was not fully released")
		}
		if requests, methods := fixture.counts(); requests != 5 || methods != 21 {
			tb.Fatal("capacity refusal performed an extra read", requests, methods)
		}
		stClientKeyRegistrationAssertNoRow(tb, *credential.ClientId)
	})
}

// Exact domain, signer, Core and endpoint ownership cannot be merged even
// when two configurations happen to observe an equal height or block hash.
func TestStClientKeyRegistrationCohortRejectsForeignAuthorityOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, _, cfg, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		cfg.RpcUrls = []string{cfg.RpcUrls[0], cfg.RpcUrls[0] + "/fallback"}
		cohorts.beforeSealForTest = func(ctx context.Context) { <-ctx.Done() }
		owner, err := newStClientKeyAuthorityOwner()
		if err != nil {
			tb.Fatal(err)
		}
		member, err := cohorts.admit(tb.Context(), owner)
		if err != nil {
			tb.Fatal(err)
		}
		defer member.release()
		for index := 0; index < 8; index++ {
			foreign := *owner
			switch index {
			case 0:
				foreign.domain.NoID++
			case 1:
				foreign.domain.PolicyHash[1]++
			case 2:
				foreign.domain.GenesisHash[1]++
			case 3:
				foreign.deploymentID += "-other"
			case 4:
				foreign.rootKey, foreign.artifactKey = owner.artifactKey, owner.rootKey
			case 5:
				foreign.artifactKey = owner.rootKey
			case 6:
				foreign.rpcURLs = []string{owner.rpcURLs[1], owner.rpcURLs[0]}
			case 7:
				foreign.client = &CoreStClient{cfg: owner.client.cfg}
			}
			if other, err := cohorts.admit(tb.Context(), &foreign); err == nil || other != nil {
				tb.Fatal("foreign authority shared pending census", index)
			}
		}
		if requests, methods := fixture.counts(); requests != 0 || methods != 0 {
			tb.Fatal("foreign admission performed chain work")
		}
	})
}

// A shared observation cannot turn a foreign root or changed final canonical
// witness into any client's durable generation. Each signed row remains absent.
func TestStClientKeyRegistrationCohortRpcFaultCannotPartlyRegisterMembers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for _, fault := range []string{"root", "final-witness", "native-null"} {
			fixture, credential, _, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
			ctx, cancel := context.WithCancel(tb.Context())
			var workers sync.WaitGroup
			tb.Cleanup(func() { cancel(); workers.Wait() })
			admitted, seal := make(chan struct{}, 2), make(chan struct{})
			cohorts.afterAdmissionForTest = func() { admitted <- struct{}{} }
			cohorts.beforeSealForTest = func(ctx context.Context) {
				select {
				case <-ctx.Done():
				case <-seal:
				}
			}
			fixture.base.fault = fault
			results := make(chan error, 2)
			workers.Add(2)
			for index := 0; index < 2; index++ {
				go func() {
					defer workers.Done()
					results <- StRegisterClientKey(ctx, *credential.ClientId, bytes.Repeat([]byte{9}, 32))
				}()
			}
			stClientKeyRegistrationAwait(tb, admitted, results)
			stClientKeyRegistrationAwait(tb, admitted, results)
			close(seal)
			for index := 0; index < 2; index++ {
				if err := <-results; err == nil {
					tb.Fatal("real authority fault registered a member", fault)
				}
			}
			stClientKeyRegistrationAssertNoRow(tb, *credential.ClientId)
		}
	})
}

// A client retired after genuine authentication still fails its own locked
// Sql check; that failure cannot skip another member's original publication.
func TestStClientKeyRegistrationCohortFailedMemberPreservesSiblingPublication(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, credential, cfg, cohorts := newStClientKeyRegistrationFixture(tb, 1, 0)
		endpoint := stClientKeyRegistrationConnectEndpoint(tb)
		ctx, cancel := context.WithCancel(tb.Context())
		var workers sync.WaitGroup
		tb.Cleanup(func() { cancel(); workers.Wait() })
		clientId, deviceId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(tb.Context(), credential.NetworkId, deviceId, clientId, "independent-registration", "test")
		sibling := credential.Client(deviceId, clientId)
		admitted, seal := make(chan struct{}, 2), make(chan struct{})
		cohorts.afterAdmissionForTest = func() { admitted <- struct{}{} }
		cohorts.beforeSealForTest = func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-seal:
			}
		}
		key := bytes.Repeat([]byte{9}, 32)
		firstResult, siblingResult := make(chan error, 1), make(chan error, 1)
		workers.Add(2)
		go func() {
			defer workers.Done()
			firstResult <- stClientKeyRegistrationRequest(ctx, endpoint.URL, credential.Sign(), key)
		}()
		go func() {
			defer workers.Done()
			siblingResult <- stClientKeyRegistrationRequest(ctx, endpoint.URL, sibling.Sign(), key)
		}()
		stClientKeyRegistrationAwait(tb, admitted, firstResult)
		stClientKeyRegistrationAwait(tb, admitted, siblingResult)
		server.Db(tb.Context(), func(conn server.PgConn) {
			if _, err := conn.Exec(tb.Context(), `UPDATE network_client SET active = false WHERE client_id = $1`, *credential.ClientId); err != nil {
				tb.Fatal(err)
			}
		})
		close(seal)
		if err := <-firstResult; err == nil {
			tb.Fatal("inactive authenticated client acquired a signed registration")
		}
		if err := <-siblingResult; err != nil {
			tb.Fatal("one member's Sql failure skipped its sibling", err)
		}
		stClientKeyRegistrationAssertNoRow(tb, *credential.ClientId)
		stClientKeyRegistrationAssertStored(tb, fixture, cfg, sibling, key, fixture.snapshot().boundary)
		if requests, methods := fixture.counts(); requests != 5 || methods != 21 {
			tb.Fatal("mixed member result changed shared source census", requests, methods)
		}
	})
}
