// Device sweep controls exercise real transitions and driver-returned rows in a disposable database.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Counts only completed state reads. The statement stays in memory for a local explain;
// arguments, identities and statement text never enter the test transcript.
type deviceSweepStateReadObserver struct {
	stateLock sync.Mutex
	statement string
	calls     int64
	rows      int64
	inflight  int64
	elapsed   time.Duration
	err       error
}

// Shadow unrelated nested contexts so another query cannot complete this observation.
func (self *deviceSweepStateReadObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	normalized := " " + strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ") + " "
	if !strings.HasPrefix(normalized, " select ") || !strings.Contains(normalized, " from audit_device_state ") {
		return context.WithValue(ctx, self, nil)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(data.Args) != 0 || (self.statement != "" && self.statement != data.SQL) {
		self.err = errors.Join(self.err, errors.New("state read observation changed statement or acquired arguments"))
	}
	self.statement = data.SQL
	self.inflight++
	return context.WithValue(ctx, self, time.Now())
}

// An error, unfinished read or non-select command cannot become a successful zero.
func (self *deviceSweepStateReadObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, matched := ctx.Value(self).(time.Time)
	if !matched {
		return
	}
	queryErr := data.Err
	if queryErr == nil && !data.CommandTag.Select() {
		queryErr = errors.New("state read completed without a select command tag")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.inflight--
	self.calls++
	self.rows += data.CommandTag.RowsAffected()
	self.elapsed += time.Since(started)
	self.err = errors.Join(self.err, queryErr)
}

// Read after the scope has joined every user of the traced pools.
func (self *deviceSweepStateReadObserver) snapshot() (string, int64, int64, time.Duration, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	err := self.err
	if self.inflight != 0 {
		err = errors.Join(err, fmt.Errorf("state read has %d unfinished observations", self.inflight))
	}
	return self.statement, self.calls, self.rows, self.elapsed, err
}

// Synthetic controls establish the observation independently of a working database.
func TestDeviceSweepStateReadObserverKeepsQueryOwnership(t *testing.T) {
	first, second := &deviceSweepStateReadObserver{}, &deviceSweepStateReadObserver{}
	query := "SELECT online FROM audit_device_state WHERE online"
	parent := first.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: query})
	child := first.TraceQueryStart(parent, nil, pgx.TraceQueryStartData{SQL: "SELECT 99"})
	first.TraceQueryEnd(child, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 99")})
	other := second.TraceQueryStart(parent, nil, pgx.TraceQueryStartData{SQL: query})
	second.TraceQueryEnd(other, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 2")})
	first.TraceQueryEnd(parent, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 3")})
	for _, c := range []struct {
		observer *deviceSweepStateReadObserver
		wantRows int64
	}{
		{observer: first, wantRows: 3},
		{observer: second, wantRows: 2},
	} {
		_, calls, rows, _, err := c.observer.snapshot()
		if calls != 1 || rows != c.wantRows || err != nil {
			t.Fatalf("state read ownership: calls=%d rows=%d want_rows=%d error=%v", calls, rows, c.wantRows, err)
		}
	}
}

// Failed and unfinished reads must not certify the expected returned-row bound.
func TestDeviceSweepStateReadObserverRejectsUnavailableCompletion(t *testing.T) {
	query := "SELECT online FROM audit_device_state WHERE online"
	observer := &deviceSweepStateReadObserver{}
	ctx := observer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: query})
	if _, _, _, _, err := observer.snapshot(); err == nil {
		t.Fatal("unfinished state read passed observation")
	}
	failure := errors.New("synthetic state read failure")
	observer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: failure})
	if _, _, _, _, err := observer.snapshot(); !errors.Is(err, failure) {
		t.Fatal("state read error was lost")
	}
	other := &deviceSweepStateReadObserver{}
	ctx = other.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: query})
	other.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("UPDATE 1")})
	if _, _, _, _, err := other.snapshot(); err == nil {
		t.Fatal("non-select completion passed observation")
	}
}

// The fixture uses ordinary client construction, with no provider key requirement.
type deviceSweepStateReadClient struct {
	networkId    server.Id
	clientId     server.Id
	connectionId server.Id
}

// Keep connection ownership outside every state/event transaction.
func newDeviceSweepStateReadClient(t testing.TB, ctx context.Context, handlerId server.Id) *deviceSweepStateReadClient {
	t.Helper()
	client := &deviceSweepStateReadClient{networkId: server.NewId(), clientId: server.NewId()}
	Testing_CreateDevice(ctx, client.networkId, server.NewId(), client.clientId, "synthetic-device-sweep", "")
	var err error
	client.connectionId, _, _, _, err = ConnectNetworkClient(ctx, client.clientId, "192.0.2.1:0", handlerId)
	if err != nil {
		t.Fatalf("connect synthetic sweep client: %v", err)
	}
	return client
}

// Only generated fixture identifiers and fixed state values enter these rows.
type deviceSweepStateReadState struct {
	deviceId  server.Id
	networkId server.Id
	online    bool
	eventTime time.Time
}

// Copy fixture rows in one transaction, with no nested connection acquisition.
func insertDeviceSweepStateReadStates(t testing.TB, ctx context.Context, states []deviceSweepStateReadState) {
	t.Helper()
	if len(states) == 0 {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		count, err := tx.CopyFrom(ctx, pgx.Identifier{"audit_device_state"},
			[]string{"device_id", "network_id", "online", "event_time"},
			pgx.CopyFromSlice(len(states), func(index int) ([]any, error) {
				state := states[index]
				return []any{state.deviceId, state.networkId, state.online, state.eventTime}, nil
			}))
		if err != nil || count != int64(len(states)) {
			t.Fatalf("seed state rows: count=%d want=%d error=%v", count, len(states), err)
		}
	})
}

// Preserve complete preexisting event values when checking append-only behavior.
type deviceSweepStateReadEvent struct {
	eventTime time.Time
	networkId server.Id
	deviceId  server.Id
	eventType string
	details   string
}

// The entire event relation belongs to this small disposable semantic fixture.
func deviceSweepStateReadEvents(ctx context.Context) map[server.Id]deviceSweepStateReadEvent {
	events := map[server.Id]deviceSweepStateReadEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT event_id, event_time, network_id, device_id, event_type, event_details
			FROM audit_device_event WHERE event_details IS NOT NULL`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var eventId server.Id
				event := deviceSweepStateReadEvent{}
				server.Raise(result.Scan(&eventId, &event.eventTime, &event.networkId, &event.deviceId, &event.eventType, &event.details))
				events[eventId] = event
			}
		})
	})
	return events
}

// Offline and absent state yield the same reconnect transition even when event
// history contains duplicates, reordered insertion, or disagreeing event clocks.
func TestSweepDeviceAuditEventsOfflineStateMatchesAbsent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		handlerId := CreateNetworkClientHandler(ctx)
		clients := []*deviceSweepStateReadClient{}
		for history := 0; history < 5; history++ {
			for _, hasOffline := range []bool{false, true} {
				client := newDeviceSweepStateReadClient(t, ctx, handlerId)
				clients = append(clients, client)
				if hasOffline {
					// The prior network differs; a reconnect takes the current network.
					insertDeviceSweepStateReadStates(t, ctx, []deviceSweepStateReadState{{
						deviceId: client.clientId, networkId: server.NewId(), online: false, eventTime: now.Add(-24 * time.Hour),
					}})
				}
				if history == 0 {
					continue
				}
				firstId, secondId := server.NewId(), server.NewId()
				first := deviceSweepStateReadEvent{eventTime: now.Add(-48 * time.Hour), networkId: client.networkId,
					deviceId: client.clientId, eventType: string(AuditEventTypeDeviceAdded), details: "synthetic-sweep-history"}
				second := deviceSweepStateReadEvent{eventTime: now.Add(-24 * time.Hour), networkId: client.networkId,
					deviceId: client.clientId, eventType: string(AuditEventTypeDeviceRemoved), details: "synthetic-sweep-history"}
				if history == 1 {
					first = second // Duplicate logical removals, each with its own primary key.
				}
				if history == 4 {
					first.eventTime, second.eventTime = second.eventTime, first.eventTime
				}
				eventIds := []server.Id{firstId, secondId}
				events := []deviceSweepStateReadEvent{first, second}
				if history == 3 {
					eventIds[0], eventIds[1] = eventIds[1], eventIds[0]
					events[0], events[1] = events[1], events[0]
				}
				server.Tx(ctx, func(tx server.PgTx) {
					for index, event := range events {
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_event
							(event_id,event_time,network_id,device_id,event_type,event_details) VALUES ($1,$2,$3,$4,$5,$6)`,
							eventIds[index], event.eventTime, event.networkId, event.deviceId, event.eventType, event.details))
					}
				})
			}
		}
		before := deviceSweepStateReadEvents(ctx)
		added, removed := SweepDeviceAuditEvents(ctx)
		if added != len(clients) || removed != 0 {
			t.Fatalf("offline/absent first sweep: added=%d removed=%d want_added=%d", added, removed, len(clients))
		}
		after := deviceSweepStateReadEvents(ctx)
		if len(after) != len(before)+len(clients) {
			t.Fatalf("first sweep event count=%d want=%d", len(after), len(before)+len(clients))
		}
		for eventId, old := range before {
			if next, exists := after[eventId]; !exists || next != old {
				t.Fatal("sweep changed a preexisting history event")
			}
		}
		newByDevice := map[server.Id]int{}
		currentNetworkIds := map[server.Id]server.Id{}
		for _, client := range clients {
			currentNetworkIds[client.clientId] = client.networkId
		}
		for eventId, event := range after {
			if _, existed := before[eventId]; !existed {
				if event.eventType != string(AuditEventTypeDeviceAdded) || event.details != AuditEventDetailsProviderSweep || event.eventTime.Before(now) || event.networkId != currentNetworkIds[event.deviceId] {
					t.Fatal("reconnect emitted an incorrect event")
				}
				newByDevice[event.deviceId]++
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, client := range clients {
				var networkId server.Id
				var online bool
				var eventTime time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT network_id,online,event_time FROM audit_device_state WHERE device_id=$1`,
					client.clientId).Scan(&networkId, &online, &eventTime))
				if networkId != client.networkId || !online || eventTime.Before(now) || newByDevice[client.clientId] != 1 {
					t.Fatal("offline/absent pair did not converge to one current online event and state")
				}
			}
		})
		added, removed = SweepDeviceAuditEvents(ctx)
		if added != 0 || removed != 0 || len(deviceSweepStateReadEvents(ctx)) != len(after) {
			t.Fatalf("unchanged sweep replayed transitions: added=%d removed=%d", added, removed)
		}
		for _, client := range clients {
			server.Raise(DisconnectNetworkClient(ctx, client.connectionId))
		}
		added, removed = SweepDeviceAuditEvents(ctx)
		if added != 0 || removed != len(clients) {
			t.Fatalf("disconnect counters: added=%d removed=%d want_removed=%d", added, removed, len(clients))
		}
		added, removed = SweepDeviceAuditEvents(ctx)
		if added != 0 || removed != 0 {
			t.Fatalf("offline replay counters: added=%d removed=%d", added, removed)
		}
		for _, client := range clients {
			var err error
			client.connectionId, _, _, _, err = ConnectNetworkClient(ctx, client.clientId, "192.0.2.1:0", handlerId)
			server.Raise(err)
		}
		added, removed = SweepDeviceAuditEvents(ctx)
		if added != len(clients) || removed != 0 {
			t.Fatalf("reactivation counters: added=%d removed=%d want_added=%d", added, removed, len(clients))
		}
		if len(deviceSweepStateReadEvents(ctx)) != len(before)+3*len(clients) {
			t.Fatal("reactivation lost or duplicated an emitted transition")
		}
	})
}

// Filtering unused history must preserve reassertion, removal, and expiration.
func TestSweepDeviceAuditEventsPreservesReassertionAndRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		handlerId := CreateNetworkClientHandler(ctx)
		fresh := newDeviceSweepStateReadClient(t, ctx, handlerId)
		stale := newDeviceSweepStateReadClient(t, ctx, handlerId)
		reconnect := newDeviceSweepStateReadClient(t, ctx, handlerId)
		_ = newDeviceSweepStateReadClient(t, ctx, handlerId) // First-seen current device.
		recentMissing, oldMissing := server.NewId(), server.NewId()
		retainedOffline, expiredOffline := server.NewId(), server.NewId()
		old := now.Add(-AuditEventExpiration - 24*time.Hour)
		freshAt := now.Add(-24 * time.Hour)
		states := []deviceSweepStateReadState{
			{deviceId: fresh.clientId, networkId: fresh.networkId, online: true, eventTime: freshAt},
			{deviceId: stale.clientId, networkId: stale.networkId, online: true, eventTime: now.Add(-auditProviderReassertInterval - 24*time.Hour)},
			{deviceId: reconnect.clientId, networkId: reconnect.networkId, online: false, eventTime: freshAt},
			{deviceId: recentMissing, networkId: server.NewId(), online: true, eventTime: freshAt},
			{deviceId: oldMissing, networkId: server.NewId(), online: true, eventTime: old},
			{deviceId: retainedOffline, networkId: server.NewId(), online: false, eventTime: freshAt},
			{deviceId: expiredOffline, networkId: server.NewId(), online: false, eventTime: old},
		}
		insertDeviceSweepStateReadStates(t, ctx, states)
		added, removed := SweepDeviceAuditEvents(ctx)
		if added != 3 || removed != 2 {
			t.Fatalf("reassert/retention counters: added=%d removed=%d want=(3,2)", added, removed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, state := range states {
				var online bool
				var eventTime time.Time
				err := conn.QueryRow(ctx, `SELECT online,event_time FROM audit_device_state WHERE device_id=$1`, state.deviceId).Scan(&online, &eventTime)
				if state.deviceId == expiredOffline {
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal("expired offline state was retained or observation failed")
					}
					continue
				}
				server.Raise(err)
				wantOnline := state.deviceId == fresh.clientId || state.deviceId == stale.clientId || state.deviceId == reconnect.clientId
				if online != wantOnline {
					t.Fatal("state transition changed online membership")
				}
				if state.deviceId == fresh.clientId || state.deviceId == retainedOffline {
					if !eventTime.Equal(state.eventTime) {
						t.Fatal("quiet state was rewritten")
					}
				} else if eventTime.Before(now) {
					t.Fatal("transition failed to refresh state time")
				}
			}
		})
		added, removed = SweepDeviceAuditEvents(ctx)
		if added != 0 || removed != 0 || len(deviceSweepStateReadEvents(ctx)) != 5 {
			t.Fatalf("reassert/retention replay: added=%d removed=%d", added, removed)
		}
	})
}

// Increasing retained offline history must not increase rows transferred to the
// transition reducer. Explain work and elapsed time are observations, not gates.
func TestSweepDeviceAuditEventsStateReadIgnoresOfflineHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		handlerId := CreateNetworkClientHandler(ctx)
		const onlineCount = 32
		states := make([]deviceSweepStateReadState, 0, onlineCount)
		for range onlineCount {
			client := newDeviceSweepStateReadClient(t, ctx, handlerId)
			states = append(states, deviceSweepStateReadState{deviceId: client.clientId, networkId: client.networkId, online: true, eventTime: now})
		}
		insertDeviceSweepStateReadStates(t, ctx, states)
		insertedOffline := 0
		for _, offlineCount := range []int{0, 4096, 65536} {
			states = make([]deviceSweepStateReadState, 0, offlineCount-insertedOffline)
			for index := insertedOffline; index < offlineCount; index++ {
				states = append(states, deviceSweepStateReadState{deviceId: server.NewId(), networkId: server.NewId(), online: false, eventTime: now.Add(-24 * time.Hour)})
			}
			insertDeviceSweepStateReadStates(t, ctx, states)
			insertedOffline = offlineCount
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, "ANALYZE audit_device_state"))
			})
			observer := &deviceSweepStateReadObserver{}
			func() {
				scope, err := server.NewTestPgQueryScope(ctx, observer)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := scope.Close(); err != nil {
						t.Error(err)
					}
				}()
				added, removed := SweepDeviceAuditEvents(ctx)
				if added != 0 || removed != 0 {
					t.Fatalf("quiet history fixture emitted transitions: added=%d removed=%d", added, removed)
				}
			}()
			statement, calls, rows, elapsed, err := observer.snapshot()
			if err != nil || calls != 1 || statement == "" {
				t.Fatalf("state read observation incomplete: calls=%d error=%v", calls, err)
			}
			var plans []struct {
				Plan struct {
					NodeType         string  `json:"Node Type"`
					ActualRows       float64 `json:"Actual Rows"`
					ActualLoops      float64 `json:"Actual Loops"`
					RowsFiltered     float64 `json:"Rows Removed by Filter"`
					SharedHitBlocks  int64   `json:"Shared Hit Blocks"`
					SharedReadBlocks int64   `json:"Shared Read Blocks"`
				} `json:"Plan"`
				ExecutionTime float64 `json:"Execution Time"`
			}
			server.Db(ctx, func(conn server.PgConn) {
				var raw []byte
				server.Raise(conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement).Scan(&raw))
				server.Raise(json.Unmarshal(raw, &plans))
			})
			if len(plans) != 1 || plans[0].Plan.ActualRows*plans[0].Plan.ActualLoops != float64(rows) {
				t.Fatal("local explain and driver completion disagree on fixture row count")
			}
			plan := plans[0]
			t.Logf("state_read_work online_rows=%d offline_rows=%d returned_rows=%d driver_ms=%.3f explain_ms=%.3f root_node=%s root_filtered_rows=%.0f root_shared_hit_blocks=%d root_shared_read_blocks=%d",
				onlineCount, offlineCount, rows, float64(elapsed)/float64(time.Millisecond), plan.ExecutionTime,
				plan.Plan.NodeType, plan.Plan.RowsFiltered, plan.Plan.SharedHitBlocks, plan.Plan.SharedReadBlocks)
			if rows != onlineCount {
				t.Errorf("state read rows=%d online=%d retained_offline=%d; offline history crossed the driver boundary", rows, onlineCount, offlineCount)
			}
		}
	})
}
