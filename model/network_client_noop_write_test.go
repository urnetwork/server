// The contract-usage stamp and the connect auth_time refresh skip the
// statements that would change nothing.
package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Statements issued with the observing context.
type noopWriteStatements struct {
	stateLock  sync.Mutex
	statements []string
}

type noopWriteStatementsKey struct{}

func (self *noopWriteStatements) record(ctx context.Context, sql string) {
	if marker, _ := ctx.Value(noopWriteStatementsKey{}).(*noopWriteStatements); marker != self {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statements = append(self.statements, strings.Join(strings.Fields(sql), " "))
}

func (self *noopWriteStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(ctx, data.SQL)
	return ctx
}

func (self *noopWriteStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *noopWriteStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.record(ctx, query.SQL)
	}
	return ctx
}

func (self *noopWriteStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (self *noopWriteStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Returns how many statements recorded since the last take contain fragment,
// and forgets them.
func (self *noopWriteStatements) take(fragment string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, statement := range self.statements {
		if strings.Contains(statement, fragment) {
			count += 1
		}
	}
	self.statements = nil
	return count
}

// The caller must defer the returned close inside its TestEnv callback.
func observeNoopWriteStatements(t testing.TB, ctx context.Context) (context.Context, *noopWriteStatements, func()) {
	t.Helper()
	statements := &noopWriteStatements{}
	scope, err := server.NewTestPgQueryScope(ctx, statements)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, noopWriteStatementsKey{}, statements), statements, func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

// A top-level client with childCount children, the shape of a hosted proxy and
// its per-window egress clients.
func testCreateParentWithChildren(ctx context.Context, t testing.TB, childCount int) (networkId server.Id, parentId server.Id, childIds []server.Id) {
	networkId = server.NewId()
	adminUserId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "synthetic-noop-"+networkId.String(), adminUserId)
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    adminUserId,
	})
	parentResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "top level device", DeviceSpec: "test"}, userSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parentResult.Error, nil)
	parentId = *parentResult.ClientId
	for range childCount {
		childResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "window client", DeviceSpec: "test", SourceClientId: &parentId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, childResult.Error, nil)
		childIds = append(childIds, *childResult.ClientId)
	}
	return
}

// A hosted proxy's contracts are paid by a new child per window. The throttle
// was keyed by payer, so each window's first contract ran a stamp transaction
// that the top-level row's fresh marker made a no-op: 98% of 46M stamps in
// 56 h on 2026-10-26. Children of one top-level client share its gate, so
// only the first window's contract stamps.
func TestContractTimeStampGateSharesTheTopLevelIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_ResetContractTimeStampGate()
		defer Testing_ResetContractTimeStampGate()

		networkId, parentId, childIds := testCreateParentWithChildren(ctx, t, 4)
		destNetworkId, _, destIds := testCreateParentWithChildren(ctx, t, 1)

		observedCtx, statements, closeObservation := observeNoopWriteStatements(t, ctx)
		defer closeObservation()

		for i, childId := range childIds {
			_, err := CreateContractNoEscrow(observedCtx, networkId, childId, destNetworkId, destIds[0], ByteCount(1024))
			connect.AssertEqual(t, err, nil)
			stamps := statements.take("SET contract_time")
			if i == 0 && stamps != 1 {
				t.Fatalf("the first window's contract ran %d stamp statements, want 1", stamps)
			}
			if 0 < i && stamps != 0 {
				t.Fatalf("window %d of one top-level client ran %d stamp statements, want 0", i, stamps)
			}
		}

		// the stamp still lands on the top-level row and never on a child
		connect.AssertEqual(t, testGetNetworkClientContractTime(ctx, parentId) != nil, true)
		for _, childId := range childIds {
			connect.AssertEqual(t, testGetNetworkClientContractTime(ctx, childId) == nil, true)
		}
	})
}

// The gate follows the marker the database reports, not the time of the
// probe. A marker another process set 59 min 58 s ago is fresh for 2 s more;
// once that passes, the next contract must stamp. A gate that recorded the
// probe time skipped stamps for up to an hour after the marker went stale.
func TestContractTimeStampGateFollowsTheStoredMarker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_ResetContractTimeStampGate()
		defer Testing_ResetContractTimeStampGate()

		_, parentId, _ := testCreateParentWithChildren(ctx, t, 0)
		almostStale := server.NowUtc().Add(-clientAuthTimeRefreshMinInterval + 2*time.Second)
		testSetNetworkClientContractTime(ctx, parentId, almostStale)

		// fresh: the probe changes nothing
		StampTopLevelClientContractTime(ctx, parentId)
		unchanged := testGetNetworkClientContractTime(ctx, parentId)
		connect.AssertEqual(t, unchanged != nil && unchanged.Sub(almostStale).Abs() < time.Millisecond, true)

		time.Sleep(3 * time.Second)

		// stale now: the stamp must write
		StampTopLevelClientContractTime(ctx, parentId)
		stamped := testGetNetworkClientContractTime(ctx, parentId)
		if stamped == nil || !almostStale.Add(time.Minute).Before(*stamped) {
			t.Fatal("a stale marker was not stamped: the gate kept the probe time instead of the stored marker")
		}
	})
}

// A child's connect refreshed its own auth_time and its parent's in two
// statements; one statement does both.
func TestConnectNetworkClientRefreshesAuthTimesInOneStatement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, parentId, childIds := testCreateParentWithChildren(ctx, t, 1)
		staleTime := server.NowUtc().Add(-2 * clientAuthTimeRefreshMinInterval)
		testSetNetworkClientAuthTime(ctx, parentId, staleTime)
		testSetNetworkClientAuthTime(ctx, childIds[0], staleTime)

		observedCtx, statements, closeObservation := observeNoopWriteStatements(t, ctx)
		defer closeObservation()

		_, _, _, _, err := ConnectNetworkClient(observedCtx, childIds[0], "192.0.2.10:7000", server.NewId())
		connect.AssertEqual(t, err, nil)
		if refreshes := statements.take("SET auth_time"); refreshes != 1 {
			t.Fatalf("a child's connect ran %d auth_time statements, want 1", refreshes)
		}

		for _, clientId := range []server.Id{parentId, childIds[0]} {
			age := server.NowUtc().Sub(testGetNetworkClientAuthTime(ctx, clientId))
			connect.AssertEqual(t, 0 <= age && age < time.Minute, true)
		}
	})
}

// A client's second connect within the interval, to the same process, knows
// both rows are fresh and runs no auth_time statement; the rows keep the values
// the first connect left.
func TestConnectNetworkClientSkipsKnownFreshAuthTimes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, parentId, childIds := testCreateParentWithChildren(ctx, t, 1)
		staleTime := server.NowUtc().Add(-2 * clientAuthTimeRefreshMinInterval)
		testSetNetworkClientAuthTime(ctx, parentId, staleTime)
		testSetNetworkClientAuthTime(ctx, childIds[0], staleTime)

		observedCtx, statements, closeObservation := observeNoopWriteStatements(t, ctx)
		defer closeObservation()

		for _, clientId := range []server.Id{childIds[0], parentId} {
			_, _, _, _, err := ConnectNetworkClient(observedCtx, clientId, "192.0.2.11:7000", server.NewId())
			connect.AssertEqual(t, err, nil)
			statements.take("SET auth_time")
			before := map[server.Id]time.Time{
				parentId:    testGetNetworkClientAuthTime(ctx, parentId),
				childIds[0]: testGetNetworkClientAuthTime(ctx, childIds[0]),
			}

			_, _, _, _, err = ConnectNetworkClient(observedCtx, clientId, "192.0.2.11:7001", server.NewId())
			connect.AssertEqual(t, err, nil)
			if refreshes := statements.take("SET auth_time"); refreshes != 0 {
				t.Fatalf("a repeat connect with known fresh rows ran %d auth_time statements, want 0", refreshes)
			}
			for id, authTime := range before {
				connect.AssertEqual(t, testGetNetworkClientAuthTime(ctx, id).Equal(authTime), true)
			}
		}
	})
}
