// Which connect handlers are live. A handler that heartbeat within two
// heartbeat timeouts is live. A stale handler is live by the judgement handler
// expiry makes (network_client_handler_retirement.go): while its process still
// stamps the provider liveness writer set or, when the set cannot vouch, while
// fewer handlers heartbeat than went stale. A heartbeat is a database write, so
// a failing database path stops every heartbeat at once; a reader that took a
// stale heartbeat as an ended process then read the whole fleet as gone. Before
// the readers took the stale handlers judged live, a heartbeat outage longer
// than two heartbeat timeouts published every provider location as
// disconnected, wrote an offline audit event for every provider, refused every
// Quality candidate as offline, and emptied the ARIN coverage readout.
//
// Each reader keeps its fresh-heartbeat join and also joins the stale handlers
// judged live (handler_id = ANY of them). The judgement reads the handlers from
// the database and then the writer set from Redis with no database connection
// held, so a reader judges before it opens its own connection or transaction.
// A judgement includes the handlers that go stale within
// liveStaleNetworkClientHandlersLookahead, so a handler that goes stale between
// the judgement and the reader's query is in it. Background readers judge once
// per run (readLiveStaleNetworkClientHandlerIds); request-path readers reuse
// one judgement per process (defaultLiveStaleNetworkClientHandlers).
//
// Safe for concurrent use.
package model

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// How long a process reuses its judgement of the stale handlers for its
// request-path readers.
const liveStaleNetworkClientHandlersMaxAge = 15 * time.Second

// A judgement older than this is no judgement: until one succeeds, the
// request-path readers take no stale handler as live, as they did before.
const liveStaleNetworkClientHandlersMaxStaleness = 2 * liveStaleNetworkClientHandlersMaxAge

// A judgement covers the handlers that go stale within this much of it, so a
// reused judgement already holds a handler that goes stale while it is in use.
const liveStaleNetworkClientHandlersLookahead = liveStaleNetworkClientHandlersMaxStaleness

// The budget of a request-path judgement: one database read and one writer set
// read, detached from the request that found the judgement due.
const liveStaleNetworkClientHandlersJudgeTimeout = 5 * time.Second

// The queries readStaleNetworkClientHandlers needs, which a pooled connection
// and a transaction both answer.
type networkClientHandlerQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The handlers stale at minTime, oldest first and at most
// networkClientHandlerExpiryCandidateLimit of them, and how many handlers
// heartbeat at or after minTime.
func readStaleNetworkClientHandlers(
	ctx context.Context,
	querier networkClientHandlerQuerier,
	minTime time.Time,
) (staleHandlerIds []server.Id, freshCount int) {
	result, err := querier.Query(
		ctx,
		`SELECT handler_id FROM network_client_handler WHERE heartbeat_time < $1 ORDER BY heartbeat_time, handler_id LIMIT $2`,
		minTime.UTC(),
		networkClientHandlerExpiryCandidateLimit,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var handlerId server.Id
			server.Raise(result.Scan(&handlerId))
			staleHandlerIds = append(staleHandlerIds, handlerId)
		}
	})
	server.Raise(querier.QueryRow(
		ctx,
		`SELECT count(*) FROM network_client_handler WHERE heartbeat_time >= $1`,
		minTime.UTC(),
	).Scan(&freshCount))
	return
}

// The stale handlers of one judgement and the verdict on each.
type staleNetworkClientHandlerVerdicts struct {
	staleHandlerIds      []server.Id
	freshCount           int
	expiredHandlerIds    []server.Id
	deferredStampedCount int
	deferredFleetCount   int
}

// Judges the stale handlers by the writer set's stamps (readStamps) and the
// fleet (selectExpiredNetworkClientHandlers). The caller holds no database
// connection: readStamps reads Redis.
func judgeStaleNetworkClientHandlers(
	ctx context.Context,
	staleHandlerIds []server.Id,
	freshCount int,
	readStamps func(ctx context.Context, handlerIds []server.Id) (stampedHandlerIds map[server.Id]bool, stampsKnown bool),
) *staleNetworkClientHandlerVerdicts {
	verdicts := &staleNetworkClientHandlerVerdicts{
		staleHandlerIds: staleHandlerIds,
		freshCount:      freshCount,
	}
	if len(staleHandlerIds) == 0 {
		return verdicts
	}
	stampedHandlerIds, stampsKnown := readStamps(ctx, staleHandlerIds)
	verdicts.expiredHandlerIds, verdicts.deferredStampedCount, verdicts.deferredFleetCount = selectExpiredNetworkClientHandlers(
		staleHandlerIds,
		freshCount,
		stampedHandlerIds,
		stampsKnown,
	)
	return verdicts
}

// The stale handlers judged live: every stale handler not expired. Never nil,
// so a query binds an empty array rather than null.
func (self *staleNetworkClientHandlerVerdicts) liveHandlerIds() []server.Id {
	expired := make(map[server.Id]bool, len(self.expiredHandlerIds))
	for _, handlerId := range self.expiredHandlerIds {
		expired[handlerId] = true
	}
	liveHandlerIds := make([]server.Id, 0, len(self.staleHandlerIds)-len(self.expiredHandlerIds))
	for _, handlerId := range self.staleHandlerIds {
		if !expired[handlerId] {
			liveHandlerIds = append(liveHandlerIds, handlerId)
		}
	}
	return liveHandlerIds
}

// The cutoff of a reader's judgement at now (see the file header).
func liveStaleNetworkClientHandlersMinTime(now time.Time) time.Time {
	return now.Add(-2*NetworkClientHandlerHeartbeatTimeout + liveStaleNetworkClientHandlersLookahead)
}

// The stale handlers judged live now, for a background reader that judges once
// before it opens its own connection or transaction: the handlers on the
// maintenance pool, then the writer set with no connection held.
func readLiveStaleNetworkClientHandlerIds(ctx context.Context) []server.Id {
	var staleHandlerIds []server.Id
	var freshCount int
	minTime := liveStaleNetworkClientHandlersMinTime(server.NowUtc())
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		staleHandlerIds, freshCount = readStaleNetworkClientHandlers(ctx, tx, minTime)
	}, server.TxReadCommitted)
	return judgeStaleNetworkClientHandlers(ctx, staleHandlerIds, freshCount, readNetworkClientHandlerStamps).liveHandlerIds()
}

// The stale handlers judged live now, for the request-path cache: the handlers
// read once on the primary without retry, then the writer set with no
// connection held. A database failure is returned rather than raised; a
// programming fault still raises.
func judgeLiveStaleNetworkClientHandlerIds(ctx context.Context) (liveHandlerIds []server.Id, returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err, ok := r.(error)
		if _, fault := r.(runtime.Error); fault || !ok {
			panic(r)
		}
		liveHandlerIds, returnErr = nil, err
	}()
	var staleHandlerIds []server.Id
	var freshCount int
	minTime := liveStaleNetworkClientHandlersMinTime(server.NowUtc())
	server.Db(ctx, func(conn server.PgConn) {
		staleHandlerIds, freshCount = readStaleNetworkClientHandlers(ctx, conn, minTime)
	}, server.OptReadOnly(), server.OptNoRetry())
	return judgeStaleNetworkClientHandlers(ctx, staleHandlerIds, freshCount, readNetworkClientHandlerStamps).liveHandlerIds(), nil
}

// The stale handlers judged live, judged at most every
// liveStaleNetworkClientHandlersMaxAge of this process's monotonic clock by one
// caller at a time while the others use the previous judgement.
type liveStaleNetworkClientHandlers struct {
	judge func(ctx context.Context) ([]server.Id, error)
	now   func() time.Time

	stateLock  sync.Mutex
	handlerIds []server.Id
	// zero before the first judgement
	judgeTime time.Time
	judging   bool
}

var defaultLiveStaleNetworkClientHandlers = &liveStaleNetworkClientHandlers{
	judge: judgeLiveStaleNetworkClientHandlerIds,
	now:   time.Now,
}

// The stale handlers judged live. A failed judgement keeps the previous one
// until it is liveStaleNetworkClientHandlersMaxStaleness old; after that, and
// before the first, there are none. Never nil.
func (self *liveStaleNetworkClientHandlers) get(ctx context.Context) []server.Id {
	now := self.now()
	judge := false
	var handlerIds []server.Id
	var judgeTime time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		handlerIds, judgeTime = self.handlerIds, self.judgeTime
		if !self.judging && (judgeTime.IsZero() || liveStaleNetworkClientHandlersMaxAge <= now.Sub(judgeTime)) {
			self.judging = true
			judge = true
		}
	}()
	if judge {
		var judgedHandlerIds []server.Id
		err := errors.New("stale handlers were not judged")
		func() {
			// a raise ends the judgement too, and the next caller judges again
			defer func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.judging = false
				if err == nil {
					self.handlerIds, self.judgeTime = judgedHandlerIds, now
				}
			}()
			// bounded, and detached from the request that found the judgement
			// due, which may end before the judgement does
			judgeCtx, judgeCancel := context.WithTimeout(context.WithoutCancel(ctx), liveStaleNetworkClientHandlersJudgeTimeout)
			defer judgeCancel()
			judgedHandlerIds, err = self.judge(judgeCtx)
		}()
		if err == nil {
			return nonNilHandlerIds(judgedHandlerIds)
		}
	}
	if judgeTime.IsZero() || liveStaleNetworkClientHandlersMaxStaleness < now.Sub(judgeTime) {
		return []server.Id{}
	}
	return nonNilHandlerIds(handlerIds)
}

// Forgets the judgement, so the next read judges again.
func (self *liveStaleNetworkClientHandlers) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.handlerIds, self.judgeTime = nil, time.Time{}
}

// handlerIds, or an empty slice for nil, so a query binds an empty array.
func nonNilHandlerIds(handlerIds []server.Id) []server.Id {
	if handlerIds == nil {
		return []server.Id{}
	}
	return handlerIds
}
