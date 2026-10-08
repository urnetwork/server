// Catalog checks keep rollout absence separate from failures of installed
// provenance. Every query and write uses the caller's one transaction.
package model

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
)

// Only expected evidence gaps use this refusal. SQL, corrupt originals and
// missing ownership fences must reach the owning transaction as failures.
var errProviderWorkEvidenceUnavailable = errors.New("provider work original evidence is unavailable")

// An installed journal still needs its endpoint fences when optional original
// tables are absent. A malformed installed head fails at its first real query.
func providerWorkSessionSchemaReadyInTx(ctx context.Context, tx server.PgTx) bool {
	server.Raise(ctx.Err())
	var ready bool
	err := tx.QueryRow(ctx, `SELECT to_regclass('provider_work_session_head') IS NOT NULL`).Scan(&ready)
	server.Raise(errors.Join(err, ctx.Err()))
	return ready
}

// Inspect names without referencing absent relations or columns in a planned
// statement. There is no process cache: readiness belongs to this database and
// snapshot. Concurrent destructive DDL or other later SQL failures abort the
// owner; they cannot turn an incomplete operation into a committed receipt.
func providerWorkOriginalSchemaReadyInTx(ctx context.Context, tx server.PgTx) bool {
	server.Raise(ctx.Err())
	var ready bool
	err := tx.QueryRow(ctx, `SELECT
 to_regprocedure('provider_work_session_append(uuid,uuid,text,uuid)') IS NOT NULL
 AND NOT EXISTS (
  SELECT 1 FROM (VALUES
   ('provider_work_session_head', ARRAY['client_id','sequence']),
   ('provider_work_session_event', ARRAY['client_id','network_id','connection_id','sequence','kind','observed_at','extender_id','transaction_id']),
   ('provider_work_session_receipt', ARRAY['client_id','sequence','receipt_hash','original']),
   ('provider_work_reservation_original', ARRAY['contract_id','source_id','source_sequence','destination_id','destination_sequence','receipt_hash','original']),
   ('provider_work_stream_original', ARRAY['stream_id','origin_contract_id','receipt_hash','original']),
   ('provider_work_stream_contract', ARRAY['contract_id','stream_id']),
   ('provider_work_outcome_original', ARRAY['contract_id','receipt_hash','original']),
   ('transfer_contract', ARRAY['usage_origin_is_source'])
  ) AS required(relation_name,column_names)
  CROSS JOIN LATERAL unnest(required.column_names) AS required_column(name)
  WHERE NOT EXISTS (
   SELECT 1 FROM pg_attribute a
   WHERE a.attrelid=to_regclass(required.relation_name)
    AND a.attname=required_column.name AND a.attnum>0 AND NOT a.attisdropped
  )
 )`).Scan(&ready)
	server.Raise(errors.Join(err, ctx.Err()))
	return ready
}
