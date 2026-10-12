// The connect.day writer runs in every Connect process, and clients reconnect
// to any of them, so its PostgreSQL work must be bounded per network and day
// across the fleet rather than per client and process.
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
)

// Counts the transactions and the onboarding event statements that every
// writer sends while observed. Safe for concurrent use.
type connectDayStatementCounter struct {
	stateLock           sync.Mutex
	beginCount          int
	eventStatementCount int
}

func (self *connectDayStatementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(data.SQL)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if strings.HasPrefix(strings.TrimSpace(sql), "begin") {
		self.beginCount += 1
	}
	if strings.Contains(sql, "network_onboarding_event") {
		self.eventStatementCount += 1
	}
	return ctx
}

func (self *connectDayStatementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *connectDayStatementCounter) counts() (beginCount int, eventStatementCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.beginCount, self.eventStatementCount
}

// Clients reconnect to any Connect process, each with its own memory, and a
// network can have many clients. Remembering clients per process wrote
// PostgreSQL once per client per process per day: on Main about 300 write
// transactions for every event stored. The fleet must write once per network
// and UTC day, and store exactly one event for it.
func TestRecordConnectDayWritesOncePerNetworkDayAcrossProcesses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const networkCount = 2
		const clientCount = 3
		const processCount = 4
		const reconnectCount = 2
		networkClientIds := map[server.Id][]server.Id{}
		for range networkCount {
			networkId := server.NewId()
			for range clientCount {
				networkClientIds[networkId] = append(networkClientIds[networkId], server.NewId())
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for networkId, clientIds := range networkClientIds {
				for _, clientId := range clientIds {
					server.RaisePgResult(tx.Exec(ctx,
						`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
						clientId, networkId,
					))
				}
			}
		})
		processSeen := connectDaySeen
		defer func() {
			connectDaySeen = processSeen
		}()

		// every client of every network reconnects to every process
		connectFleet := func(day time.Time) (beginCount int, eventStatementCount int) {
			statements := &connectDayStatementCounter{}
			scope, err := server.NewTestPgQueryScope(ctx, statements)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := scope.Close(); err != nil {
					t.Error(err)
				}
			}()
			for process := range processCount {
				// another process: its own memory, the fleet's Redis and PostgreSQL
				connectDaySeen = &connectDayCache{}
				for reconnect := range reconnectCount {
					connectAt := day.Add(time.Duration(1+process*reconnectCount+reconnect) * time.Hour)
					for networkId, clientIds := range networkClientIds {
						for _, clientId := range clientIds {
							waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt))
						}
					}
				}
			}
			return statements.counts()
		}

		day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
		connectCount := networkCount * clientCount * processCount * reconnectCount
		for dayIndex := range 2 {
			beginCount, eventStatementCount := connectFleet(day.AddDate(0, 0, dayIndex))
			if beginCount != networkCount || eventStatementCount != networkCount {
				t.Fatalf(
					"day %d: %d connects of %d networks across %d processes ran %d transactions and %d event statements, want %d of each",
					dayIndex,
					connectCount,
					networkCount,
					processCount,
					beginCount,
					eventStatementCount,
					networkCount,
				)
			}
			for networkId := range networkClientIds {
				if count := connectDayEventCount(t, ctx, networkId); count != dayIndex+1 {
					t.Fatalf("day %d: network stored %d connect.day events, want %d", dayIndex, count, dayIndex+1)
				}
			}
		}
	})
}

// The per-process memory is bounded. Clearing it at the bound made every
// remembered entry pay its database write again on its next connect, so past
// the bound the memory keeps its day and only stops adding.
func TestConnectDayCacheKeepsItsDayPastTheBound(t *testing.T) {
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	c := &connectDayCache{}
	firstNetworkId := server.NewId()
	connect.AssertEqual(t, true, c.remember(firstNetworkId, day))
	for range connectDayMaxCached - 1 {
		c.remember(server.NewId(), day)
	}
	// past the bound a network is checked on every connect, never remembered
	overflowNetworkId := server.NewId()
	connect.AssertEqual(t, true, c.remember(overflowNetworkId, day))
	connect.AssertEqual(t, true, c.remember(overflowNetworkId, day))
	// what was remembered stays remembered for the day
	connect.AssertEqual(t, false, c.remember(firstNetworkId, day))
	// a new day starts over
	connect.AssertEqual(t, true, c.remember(firstNetworkId, day.Add(24*time.Hour)))
	connect.AssertEqual(t, false, c.remember(firstNetworkId, day.Add(24*time.Hour)))
}
