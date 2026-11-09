//go:build sidecar_pending

package model

// §7.10's removal chain and the rev/ objects it owes (sidecar_removal_model.go,
// sidecar_rev_model.go), on Postgres with MG1 and T2's FakeBlobs: a removed or
// reaped bound client's object lands after the commit, an unbound client owes
// none, a crash between the commit and the PUT leaves the task to land it, a
// rolled back removal owes nothing, and an effect applied twice has one result.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A network and one client of it bound to a new sidecar, and the network's own
// credential's session.
type testRemovalNetwork struct {
	networkId      server.Id
	networkSession *session.ClientSession
	clientId       server.Id
	sidecarId      server.Id
}

func newTestRemovalNetwork(t testing.TB, ctx context.Context, bound bool) *testRemovalNetwork {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "sidecar-removal", userId)
	networkSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: "sidecar-removal"})
	authResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, networkSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	network := &testRemovalNetwork{
		networkId:      networkId,
		networkSession: networkSession,
		clientId:       *byJwt.ClientId,
		sidecarId:      server.NewId(),
	}
	if bound {
		record := testSidecarBindingRecord(t, networkId, network.sidecarId, testSidecarKey(t), 1)
		record.ClientId = network.clientId
		if outcome, _ := BindSidecarIdentity(ctx, record, server.NowUtc()); outcome != SidecarBindAppended {
			t.Fatalf("bind: %s", outcome)
		}
	}
	return network
}

// Installs a rev/ writer on FakeBlobs for the test.
func installTestRevStore(t testing.TB) *testRevStore {
	t.Helper()
	s := newTestRevStore(t)
	// the removal's own clock is real time, so the fake starts at it
	s.clock.AdvanceTo(time.Now())
	SetSidecarRevStore(s.store)
	t.Cleanup(func() { SetSidecarRevStore(nil) })
	return s
}

// The client's object, nil for none.
func (self *testRevStore) clientRecord(t testing.TB, network *testRemovalNetwork) *sidecar.RevocationRecord {
	t.Helper()
	return self.held(t, &SidecarRevocation{NetworkId: network.networkId, Kind: sidecar.RevocationKindClient, SubjectId: network.clientId})
}

// The pending write tasks owing an object of subjectId.
func pendingRevWrites(t testing.TB, ctx context.Context, subjectId server.Id) []*WriteSidecarRevArgs {
	t.Helper()
	owed := []*WriteSidecarRevArgs{}
	for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if !strings.HasSuffix(pending.FunctionName, "WriteSidecarRevTask") {
			continue
		}
		args := &WriteSidecarRevArgs{}
		if err := json.Unmarshal([]byte(pending.ArgsJson), args); err != nil {
			t.Fatal(err)
		}
		for _, revocation := range args.Revocations {
			if revocation.SubjectId == subjectId {
				owed = append(owed, args)
				break
			}
		}
	}
	return owed
}

func removeTestClient(t testing.TB, network *testRemovalNetwork) {
	t.Helper()
	result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: network.clientId}, network.networkSession)
	if err != nil || result.Error != nil {
		t.Fatalf("remove: %v %+v", err, result)
	}
}

// A removed bound client's object lands after the commit, its not_before past
// the deactivation, and a token issued before it is void at the checker.
func TestRemoveClientWritesItsRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		s := installTestRevStore(t)
		network := newTestRemovalNetwork(t, ctx, true)
		issuedAt := time.Now().Truncate(time.Second)
		removeTestClient(t, network)

		record := s.clientRecord(t, network)
		if record == nil {
			t.Fatal("a removed bound client has no rev/ object")
		}
		if record.Reason != sidecar.RevocationReasonRemoved {
			t.Fatalf("reason %s", record.Reason)
		}
		var deactivateTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, network.clientId).Scan(&deactivateTime))
		})
		if record.NotBefore.Before(deactivateTime) {
			t.Fatalf("not_before %v before the deactivation %v", record.NotBefore, deactivateTime)
		}
		settings := sidecar.DefaultRevocationSettings()
		settings.Clock = s.clock
		checker := sidecar.NewRevocationChecker(s.blobs, testRevVerify, settings)
		principal := &sidecar.Principal{
			NetworkId: [16]byte(network.networkId),
			ClientId:  [16]byte(network.clientId),
			Audience:  "evs",
			IssuedAt:  issuedAt,
		}
		if err := checker.Check(ctx, principal); !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
			t.Fatalf("the removed client's token at evs: %v", err)
		}
	})
}

// A client bound to no sidecar can hold no service token, so it owes no object.
func TestRemoveUnboundClientOwesNoRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		s := installTestRevStore(t)
		network := newTestRemovalNetwork(t, ctx, false)
		removeTestClient(t, network)
		if s.clientRecord(t, network) != nil {
			t.Fatal("an unbound client got a rev/ object")
		}
		if owed := pendingRevWrites(t, ctx, network.clientId); len(owed) != 0 {
			t.Fatalf("an unbound client owes %d writes", len(owed))
		}
	})
}

// A crash between the commit and the PUT: the post-commit write never runs, and
// the write task the removal's transaction scheduled lands the object.
func TestRevocationLandsByTaskAfterCrashBeforePut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		s := installTestRevStore(t)
		Testing_SidecarRevSkipPostCommit = func() bool { return true }
		defer func() { Testing_SidecarRevSkipPostCommit = nil }()
		network := newTestRemovalNetwork(t, ctx, true)
		removeTestClient(t, network)
		if s.clientRecord(t, network) != nil {
			t.Fatal("the crashed post-commit write landed")
		}
		owed := pendingRevWrites(t, ctx, network.clientId)
		if len(owed) != 1 {
			t.Fatalf("%d write tasks owed", len(owed))
		}
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		if _, err := WriteSidecarRevTask(owed[0], clientSession); err != nil {
			t.Fatal(err)
		}
		if s.clientRecord(t, network) == nil {
			t.Fatal("the task did not land the object")
		}
	})
}

// A removal whose transaction rolls back owes nothing and writes nothing.
func TestRolledBackRemovalOwesNoRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		s := installTestRevStore(t)
		network := newTestRemovalNetwork(t, ctx, true)
		rollback := errors.New("the removal's transaction fails after its effects")
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("the transaction did not fail")
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				removal := NewClientRemoval(network.networkId, []server.Id{network.clientId}, sidecar.RevocationReasonRemoved, server.NowUtc())
				server.Raise(ApplyRemovalEffectsInTx(ctx, tx, removal))
				panic(rollback)
			}, server.OptNoRetry())
		}()
		if s.clientRecord(t, network) != nil {
			t.Fatal("a rolled back removal wrote its object")
		}
		if owed := pendingRevWrites(t, ctx, network.clientId); len(owed) != 0 {
			t.Fatalf("a rolled back removal owes %d writes", len(owed))
		}
	})
}

// §19.10 applies a removal again: the second application lands nothing new, the
// object keeps the first's not_before, and a registered effect that keeps its
// own idempotency sees both runs.
func TestRemovalEffectAppliedTwiceHasOneResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		s := installTestRevStore(t)
		network := newTestRemovalNetwork(t, ctx, true)
		runCount := 0
		RegisterRemovalEffect("test_count", func(ctx context.Context, tx server.PgTx, removal *Removal) error {
			runCount += 1
			return nil
		})
		defer Testing_UnregisterRemovalEffect("test_count")

		at := server.NowUtc()
		apply := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				removal := NewClientRemoval(network.networkId, []server.Id{network.clientId}, sidecar.RevocationReasonRemoved, at)
				server.Raise(ApplyRemovalEffectsInTx(ctx, tx, removal))
			})
		}
		apply()
		first := s.clientRecord(t, network)
		if first == nil {
			t.Fatal("no object after the first application")
		}
		putCount := s.blobs.RequestCount("meta-1", "put")
		apply()
		second := s.clientRecord(t, network)
		if !second.NotBefore.Equal(first.NotBefore) || !second.Time.Equal(first.Time) {
			t.Fatalf("the second application changed the object: %+v then %+v", first, second)
		}
		if s.blobs.RequestCount("meta-1", "put") != putCount {
			t.Fatal("the second application sent a PUT")
		}
		if runCount != 2 {
			t.Fatalf("the effect ran %d times", runCount)
		}
	})
}

// The reaper's deactivation of an abandoned bound client owes its object too.
func TestReapedBoundClientWritesItsRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		s := installTestRevStore(t)
		network := newTestRemovalNetwork(t, ctx, true)
		// abandoned: no auth since before the reap's bound, and no connection
		RemoveDisconnectedNetworkClients(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour))
		var active bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, network.clientId).Scan(&active))
		})
		if active {
			t.Fatal("the reaper did not reach the client, so this proves nothing")
		}
		record := s.clientRecord(t, network)
		if record == nil || record.Reason != sidecar.RevocationReasonDeactivated {
			t.Fatalf("the reaped client's object %+v", record)
		}
	})
}
