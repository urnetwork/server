// Optional payer traversal waits for both online indexes; finance stays chronological.
package model

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
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

// Each bounded task entry validates this one index outside financial locks.
// Unlike the optional old scheduler, a concurrent cache refresh cannot be
// mistaken for missing schema and send a healthy payer task into error backoff.
func legacySettlementPayerDueIndexReady(ctx context.Context) (ready bool) {
	bounded, cancel := context.WithTimeout(ctx, legacySettlementPayerIndexBudget)
	defer cancel()
	server.HandleError(func() {
		server.Db(bounded, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(bounded, legacySettlementPayerDueIndexSql).Scan(&ready))
		}, server.OptNoRetry())
	}, func(error) { ready = false })
	return
}

// Local resource reads and the catalog borrow/query share a nonretrying budget.
// A malformed, oversized, unavailable or canceled resource takes the same safe
// scheduling fallback; no new financial decision is made here.
func legacySettlementPayerIndexesReady(ctx context.Context) bool {
	bounded, cancel := context.WithTimeout(ctx, legacySettlementPayerIndexBudget)
	defer cancel()
	resource, err := server.Vault.SimpleResource(server.DefaultPgVaultResourceName)
	if err != nil {
		return false
	}
	raw, err := resource.BytesBoundedE(bounded, 16*1024)
	if err != nil {
		return false
	}
	return legacySettlementPayerIndexes.load(bounded, sha256.Sum256(raw), time.Now, readLegacySettlementPayerIndexes)
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
