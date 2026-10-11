// Orphan provide keys are paged independently of deletion membership. Locked
// tuple addresses stay inside one transaction; committed keys own Redis cleanup.
package model

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Select the complete cursor page before checking parent membership. Each
// absent-parent child is locked through its full primary key. A waited update
// can return a newer tuple address, so deletion uses a fresh statement snapshot
// in the same read-committed transaction while those child locks remain held.
const orphanProvideKeyPageSqlTemplate = `
	WITH slice AS MATERIALIZED (
		SELECT client_id, provide_mode
		FROM provide_key
		WHERE {{cursor}}
		ORDER BY client_id, provide_mode
		LIMIT {{limit}}
	), locked AS MATERIALIZED (
		SELECT matched.ctid
		FROM slice
		CROSS JOIN LATERAL (
			SELECT sweep_target.ctid
			FROM provide_key AS sweep_target
			WHERE sweep_target.client_id = slice.client_id
				AND sweep_target.provide_mode = slice.provide_mode
				AND NOT EXISTS (
					SELECT 1 FROM network_client
					WHERE network_client.client_id = sweep_target.client_id
					LIMIT 1 OFFSET 0
				)
			LIMIT 1 OFFSET 0
			FOR UPDATE OF sweep_target
		) AS matched
	), bound AS MATERIALIZED (
		SELECT client_id, provide_mode FROM slice
		ORDER BY client_id DESC, provide_mode DESC LIMIT 1
	)
	SELECT (SELECT count(*) FROM slice), ARRAY(SELECT ctid::text FROM locked),
		bound.client_id, bound.provide_mode
	FROM bound
`

var orphanProvideKeyFirstPageSql = strings.NewReplacer(
	"{{cursor}}", "true", "{{limit}}", "$1",
).Replace(orphanProvideKeyPageSqlTemplate)

var orphanProvideKeyResumedPageSql = strings.NewReplacer(
	"{{cursor}}", "(client_id, provide_mode) > ($1, $2)", "{{limit}}", "$3",
).Replace(orphanProvideKeyPageSqlTemplate)

// The locked addresses are the only deletion candidates. A parent visible in
// this fresh snapshot conservatively retains its child. RETURNING remains the
// authority for post-commit Redis cleanup and the reported deletion count.
const orphanProvideKeyDeleteSql = `
	DELETE FROM provide_key AS sweep_delete
	WHERE ctid = ANY($1::text[]::tid[])
		AND NOT EXISTS (
			SELECT 1 FROM network_client
			WHERE network_client.client_id = sweep_delete.client_id
			LIMIT 1 OFFSET 0
		)
	RETURNING sweep_delete.client_id, sweep_delete.provide_mode
`

// The selected endpoint advances even when every selected key has a parent.
type orphanProvideKeyPage struct {
	scanned         int64
	gotBound        bool
	lastClientId    server.Id
	lastProvideMode ProvideMode
	deleted         map[server.Id][]ProvideMode
}

// Consume the lock query before deletion so waited tuple versions are visible
// in the next statement snapshot. The caller owns the transaction and retry.
func sweepOrphanProvideKeyPageInTx(ctx context.Context, tx server.PgTx, first bool,
	cursorClientId server.Id, cursorProvideMode ProvideMode, sliceSize int,
) (page orphanProvideKeyPage) {
	query, args := orphanProvideKeyFirstPageSql, []any{sliceSize}
	if !first {
		query, args = orphanProvideKeyResumedPageSql, []any{cursorClientId, cursorProvideMode, sliceSize}
	}
	var locked []string
	err := tx.QueryRow(ctx, query, args...).Scan(&page.scanned, &locked, &page.lastClientId, &page.lastProvideMode)
	if err == pgx.ErrNoRows {
		return
	}
	server.Raise(err)
	page.gotBound = true
	if len(locked) == 0 {
		return
	}
	page.deleted = map[server.Id][]ProvideMode{}
	result, err := tx.Query(ctx, orphanProvideKeyDeleteSql, locked)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var provideMode ProvideMode
			server.Raise(result.Scan(&clientId, &provideMode))
			page.deleted[clientId] = append(page.deleted[clientId], provideMode)
		}
	})
	return
}

// Traverse all keys in bounded pages and clear only committed deleted mirrors.
func sweepOrphanProvideKeys(ctx context.Context, sliceSize int) (removedCount int64) {
	var cursorClientId server.Id
	var cursorProvideMode ProvideMode
	first := true
	for {
		var page orphanProvideKeyPage
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// Keep addresses, returned keys and cursor metadata local to this
			// attempt. A transaction retry must start from a fresh snapshot.
			page = orphanProvideKeyPage{}
			page = sweepOrphanProvideKeyPageInTx(ctx, tx, first, cursorClientId, cursorProvideMode, sliceSize)
		}, server.TxReadCommitted)

		server.Redis(ctx, func(r server.RedisClient) {
			for clientId, provideModes := range page.deleted {
				pipe := r.TxPipeline()
				pipe.Del(ctx, provideModesKey(clientId))
				for _, provideMode := range provideModes {
					pipe.Del(ctx, provideModeSecretKeyKey(clientId, provideMode))
				}
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			}
		})
		for _, modes := range page.deleted {
			removedCount += int64(len(modes))
		}
		if !page.gotBound || page.scanned < int64(sliceSize) {
			return
		}
		cursorClientId, cursorProvideMode, first = page.lastClientId, page.lastProvideMode, false
	}
}
