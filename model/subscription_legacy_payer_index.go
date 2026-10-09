// Optional payer traversal waits for both online indexes; finance stays chronological.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

const legacySettlementPayerIndexBudget = 250 * time.Millisecond
const legacySettlementPayerIndexCacheLifetime = 5 * time.Second

// Two fixed catalog identities, including their complete definitions. A missing
// row is false rather than a null ignored by bool_and. This reads no intent rows.
// Readiness is not a lock against later ddl; any cached positive expires in five
// seconds and every page retains its original deadline and financial authority.
const legacySettlementPayerIndexSql = `SELECT bool_and(COALESCE(
 index_relation.relkind='i'
 AND index_state.indrelid=to_regclass('public.legacy_settlement_intent')
 AND index_state.indisvalid AND index_state.indisready AND index_state.indislive
 AND pg_get_indexdef(index_relation.oid)=expected.definition, false))
 FROM (VALUES
 ('public.legacy_settlement_intent_payer_due',
  'CREATE INDEX legacy_settlement_intent_payer_due ON public.legacy_settlement_intent USING btree (shard, payer_network_id, next_attempt_time, contract_id) WHERE (payer_network_id IS NOT NULL)'),
 ('public.legacy_settlement_intent_payer_missing',
  'CREATE INDEX legacy_settlement_intent_payer_missing ON public.legacy_settlement_intent USING btree (shard, contract_id) WHERE (payer_network_id IS NULL)')
 ) AS expected(name,definition)
 LEFT JOIN pg_class AS index_relation ON index_relation.oid=to_regclass(expected.name)
 LEFT JOIN pg_index AS index_state ON index_state.indexrelid=index_relation.oid`

// One process entry bounds memory and probe concurrency. An in-flight refresh
// never blocks other pages: they retain chronological service. The resource
// digest isolates database configuration changes and test databases without
// retaining credentials or creating an unbounded per-database map.
type legacySettlementPayerIndexCache struct {
	stateLock  sync.Mutex
	identity   [sha256.Size]byte
	expires    time.Time
	ready      bool
	refreshing bool
}

var legacySettlementPayerIndexes legacySettlementPayerIndexCache

// Dispatch and payer execution need only this read index. An invalid optional
// NULL-registration index must not disable already-registered payer service.
const legacySettlementPayerDueIndexSql = `SELECT COALESCE(
 index_relation.relkind='i'
 AND index_state.indrelid=to_regclass('public.legacy_settlement_intent')
 AND index_state.indisvalid AND index_state.indisready AND index_state.indislive
 AND pg_get_indexdef(index_relation.oid)=
 'CREATE INDEX legacy_settlement_intent_payer_due ON public.legacy_settlement_intent USING btree (shard, payer_network_id, next_attempt_time, contract_id) WHERE (payer_network_id IS NOT NULL)', false)
 FROM (VALUES ('public.legacy_settlement_intent_payer_due')) AS expected(name)
 LEFT JOIN pg_class AS index_relation ON index_relation.oid=to_regclass(expected.name)
 LEFT JOIN pg_index AS index_state ON index_state.indexrelid=index_relation.oid`

// Each bounded task entry uses a live exact-resource positive observation or
// validates this one index outside financial locks. A concurrent cache refresh
// alone is never mistaken for missing schema: a cache miss still probes.
// This is one bounded observation, not a persistent schema verdict. In
// particular, a deadline or read error must not be reported as an invalid index.
// No raw database errors, resource values or caller identities are retained.
type LegacySettlementPayerIndexReadiness struct {
	Outcome   string `json:"outcome"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Cached    bool   `json:"cached,omitempty"`
}

func legacySettlementPayerDueIndexReady(ctx context.Context) bool {
	return legacySettlementPayerDueIndexObservation(ctx).Outcome == "ready"
}

func legacySettlementPayerDueIndexObservation(ctx context.Context) LegacySettlementPayerIndexReadiness {
	return observeLegacySettlementPayerIndexWithCache(ctx,
		cachedLegacySettlementPayerIndexesReady, readLegacySettlementPayerDueIndex, time.Now)
}

// The existing both-index check proves both complete definitions. Reuse only
// its still-live positive observation for this exact
// database resource. A miss does not refresh, wait for another refresher, retry
// a query, or extend the existing five-second expiry.
func cachedLegacySettlementPayerIndexesReady(ctx context.Context) bool {
	// A cold, negative or expired cache adds no resource read to the ordinary
	// probe. Identity must still be rechecked after reading any potential hit.
	if !legacySettlementPayerIndexes.hasReadyObservation(time.Now()) {
		return false
	}
	resource, err := server.Vault.SimpleResource(server.DefaultPgVaultResourceName)
	if err != nil {
		return false
	}
	raw, err := resource.BytesBoundedE(ctx, 16*1024)
	if err != nil {
		return false
	}
	return legacySettlementPayerIndexes.readyObservation(ctx, sha256.Sum256(raw), time.Now())
}

func (self *legacySettlementPayerIndexCache) hasReadyObservation(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ready && now.Before(self.expires)
}

func (self *legacySettlementPayerIndexCache) readyObservation(ctx context.Context, identity [sha256.Size]byte, now time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.identity == identity && self.ready && now.Before(self.expires)
}

func observeLegacySettlementPayerIndexWithCache(ctx context.Context,
	cached func(context.Context) bool, read func(context.Context) (bool, error), now func() time.Time,
) LegacySettlementPayerIndexReadiness {
	usedCache := false
	observed := observeLegacySettlementPayerDueIndex(ctx, func(bounded context.Context) (bool, error) {
		if cached(bounded) {
			usedCache = true
			return true, nil
		}
		return read(bounded)
	}, now)
	observed.Cached = usedCache && observed.Outcome == "ready"
	return observed
}

func observeLegacySettlementPayerDueIndex(ctx context.Context,
	read func(context.Context) (bool, error), now func() time.Time,
) (observation LegacySettlementPayerIndexReadiness) {
	started := now()
	bounded, cancel := context.WithTimeout(ctx, legacySettlementPayerIndexBudget)
	defer cancel()
	ready, err := read(bounded)
	observation.ElapsedMs = max(time.Duration(0), now().Sub(started)).Milliseconds()
	switch {
	case errors.Is(bounded.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		observation.Outcome = "deadline"
	case errors.Is(bounded.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		observation.Outcome = "canceled"
	case err != nil:
		observation.Outcome = "read_error"
	case !ready:
		observation.Outcome = "catalog_invalid"
	default:
		observation.Outcome = "ready"
	}
	return
}

func readLegacySettlementPayerDueIndex(ctx context.Context) (ready bool, resultErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, legacySettlementPayerDueIndexSql).Scan(&ready))
		}, server.OptNoRetry())
	}, func(err error) { ready, resultErr = false, err })
	return
}

// Local resource reads and the catalog borrow/query share a nonretrying budget.
// A malformed, oversized, unavailable or canceled resource takes the same safe
// scheduling fallback; no new financial decision is made here.
func legacySettlementPayerIndexesReady(ctx context.Context) bool {
	bounded, cancel := context.WithTimeout(ctx, legacySettlementPayerIndexBudget)
	defer cancel()
	ready, _ := readLegacySettlementPayerIndexesWithCache(bounded)
	return ready
}

// Every refresh owns its context; neither pool waiting nor an unavailable
// catalog can consume the rest of a healthy page's fifteen-second allowance.
func readLegacySettlementPayerIndexes(ctx context.Context) (ready bool, resultErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, legacySettlementPayerIndexSql).Scan(&ready))
		}, server.OptNoRetry())
	}, func(err error) { resultErr = err })
	return
}

// Registration already validates both complete definitions. Bind that read to
// its exact bounded resource so subsequent due checks can reuse the same proof.
func readLegacySettlementPayerIndexesWithCache(ctx context.Context) (bool, error) {
	return readLegacySettlementPayerIndexesForCache(ctx, &legacySettlementPayerIndexes,
		func(ctx context.Context) ([sha256.Size]byte, error) {
			resource, err := server.Vault.SimpleResource(server.DefaultPgVaultResourceName)
			if err != nil {
				return [sha256.Size]byte{}, err
			}
			raw, err := resource.BytesBoundedE(ctx, 16*1024)
			return sha256.Sum256(raw), err
		}, time.Now, readLegacySettlementPayerIndexes)
}

// Invocation-local catalog and clock seams exercise the production publisher
// from a cold cache without changing process state or manually seeding proof.
func readLegacySettlementPayerIndexesForCache(ctx context.Context, cache *legacySettlementPayerIndexCache,
	resourceIdentity func(context.Context) ([sha256.Size]byte, error), now func() time.Time, read func(context.Context) (bool, error),
) (ready bool, resultErr error) {
	identity, err := resourceIdentity(ctx)
	if err != nil {
		return false, err
	}
	ready = cache.load(ctx, identity, now, func(ctx context.Context) (bool, error) {
		var fullReady bool
		fullReady, resultErr = read(ctx)
		if !fullReady || resultErr != nil {
			return false, resultErr
		}
		// The catalog and resource read share this same original context.
		// A changed resource cannot inherit the prior query's positive proof.
		var observedIdentity [sha256.Size]byte
		observedIdentity, resultErr = resourceIdentity(ctx)
		if resultErr == nil && observedIdentity != identity {
			resultErr = errors.New("legacy payer index resource changed during validation")
		}
		return resultErr == nil, resultErr
	})
	return
}

// Publish only after the bounded check returns. Error, cancellation or an
// expired observation caches false; expiry is measured from refresh admission,
// never extended by a late response. Callbacks run outside the state lock.
func (self *legacySettlementPayerIndexCache) load(ctx context.Context, identity [sha256.Size]byte,
	now func() time.Time, check func(context.Context) (bool, error)) (ready bool) {
	if ctx.Err() != nil {
		return false
	}
	started := now()
	self.stateLock.Lock()
	if self.refreshing {
		self.stateLock.Unlock()
		return false
	}
	if self.identity == identity && started.Before(self.expires) {
		ready = self.ready
		self.stateLock.Unlock()
		return
	}
	self.refreshing = true
	self.stateLock.Unlock()
	expires := started.Add(legacySettlementPayerIndexCacheLifetime)
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.identity, self.expires = identity, expires
		self.ready, self.refreshing = ready, false
	}()
	var checkErr error
	server.HandleError(func() { ready, checkErr = check(ctx) }, func(err error) { checkErr = err })
	ready = ready && checkErr == nil && ctx.Err() == nil && now().Before(expires)
	return
}
