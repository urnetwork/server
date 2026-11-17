package evidencemove

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// A position in a source's order: the last item a run completed. The zero
// cursor is before every item.
type Cursor struct {
	ClientId   server.Id `json:"client_id"`
	Generation int64     `json:"generation"`
}

func (self Cursor) String() string {
	if self == (Cursor{}) {
		return "the start"
	}
	return fmt.Sprintf("%s/%d (client created %s)", self.ClientId, self.Generation, self.Time().Format(time.RFC3339))
}

// Client ids lead with their creation millisecond, 48 bits big endian
// (connect.NewId), so the cursor's client id dates how far through the
// history a run is.
func (self Cursor) Time() time.Time {
	id := self.ClientId
	ms := int64(id[0])<<40 | int64(id[1])<<32 | int64(id[2])<<24 | int64(id[3])<<16 | int64(id[4])<<8 | int64(id[5])
	return time.UnixMilli(ms).UTC()
}

// Content hashes in a stable order.
type Source interface {
	// At most limit items strictly after cursor, in order; none at the end.
	Next(ctx context.Context, after Cursor, limit int) ([]Item, error)
}

// The evidence of st_client_key_history in primary key order, (client_id,
// generation): each row's evidence_hash, and its evidence's byte length,
// which PostgreSQL reads from the value's header without detoasting it. Each
// batch is one short indexed statement, so no snapshot outlives it. A row
// inserted behind the cursor during a run is not read until a later pass; its
// evidence is written at the sharded key already. A row the retention
// deletes before the cursor reaches it leaves its flat object, an orphan.
type ClientKeyHistorySource struct {
	// bounds each statement (0: one minute)
	StatementTimeout time.Duration
}

const clientKeyHistorySourceSql = `
	SELECT client_id, generation, evidence_hash, octet_length(evidence)
	FROM st_client_key_history
	WHERE (client_id, generation) > ($1, $2)
	ORDER BY client_id, generation
	LIMIT $3
`

func (self *ClientKeyHistorySource) Next(ctx context.Context, after Cursor, limit int) (items []Item, resultErr error) {
	if limit < 1 {
		return nil, errors.New("a source read needs a positive limit")
	}
	timeout := self.StatementTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	err := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())))
			result, err := tx.Query(ctx, clientKeyHistorySourceSql, after.ClientId, after.Generation, limit)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var item Item
					var size *int64
					server.Raise(result.Scan(&item.Cursor.ClientId, &item.Cursor.Generation, &item.Hash, &size))
					if size != nil {
						item.Size = *size
					}
					items = append(items, item)
				}
			})
		}, server.OptReadOnly())
	})
	if err != nil {
		return nil, raisedError(err)
	}
	return items, nil
}

// A raise recovered by server.HandleError, as an error.
func raisedError(raised any) error {
	if err, ok := raised.(error); ok {
		return err
	}
	return fmt.Errorf("%v", raised)
}

// The row estimate PostgreSQL keeps for st_client_key_history (pg_class),
// read without scanning the table.
func ClientKeyHistoryRowEstimate(ctx context.Context) (rows int64, pages int64, resultErr error) {
	err := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT reltuples::bigint, relpages::bigint
				FROM pg_class
				WHERE oid = 'public.st_client_key_history'::regclass
			`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&rows, &pages))
				}
			})
		}, server.OptReadOnly())
	})
	if err != nil {
		return 0, 0, raisedError(err)
	}
	return rows, pages, nil
}

// About rows rows of st_client_key_history drawn uniformly by page
// (TABLESAMPLE SYSTEM): the sampled pages only are read, so the statement is
// short whatever the table's size. Rows on one page were written together,
// so the sample is clustered by page.
func SampleClientKeyHistory(ctx context.Context, rows int, pages int64, seed int64) (items []Item, resultErr error) {
	if rows < 1 || pages < 1 {
		return nil, errors.New("a sample needs positive rows and pages")
	}
	// about 30 rows a page; at least one page
	percent := min(100, max(100*float64(rows)/30/float64(pages), 100/float64(pages)))
	err := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL statement_timeout = 120000"))
			result, err := tx.Query(ctx, fmt.Sprintf(`
				SELECT client_id, generation, evidence_hash, octet_length(evidence)
				FROM st_client_key_history TABLESAMPLE SYSTEM (%.9f) REPEATABLE (%d)
			`, percent, seed))
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var item Item
					var size *int64
					server.Raise(result.Scan(&item.Cursor.ClientId, &item.Cursor.Generation, &item.Hash, &size))
					if size != nil {
						item.Size = *size
					}
					items = append(items, item)
				}
			})
		}, server.OptReadOnly())
	})
	if err != nil {
		return nil, raisedError(err)
	}
	return items, nil
}
