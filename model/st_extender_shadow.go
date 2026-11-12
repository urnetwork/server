// The SN25 extender pool's shadow reports (sn WHITEPAPER.md §8.6.7): for each
// epoch before E_x whose census was captured, what the pool would have paid,
// built from the real census and contracts without committing anything,
// recorded once for review and never paid.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// An epoch's recorded shadow report: the report's bytes and their ledger object
// reference.
type StLedgerV2ExtenderShadow struct {
	Epoch      uint64
	ReportRef  string
	Report     []byte
	CreateTime time.Time
}

// Records an epoch's shadow report once: a report already recorded is kept, and
// returned, whatever this one holds, so the first report built for an epoch is
// the one reviewed.
func RecordStLedgerV2ExtenderShadow(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, reportRef string, report []byte) *StLedgerV2ExtenderShadow {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_ledger_v2_extender_shadow (deployment_key, no_id, epoch, report_ref, report, create_time)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
		`, key, int64(noId), int64(epoch), reportRef, report, server.NowUtc()))
	})
	return GetStLedgerV2ExtenderShadow(ctx, deploymentKey, noId, epoch)
}

// An epoch's recorded shadow report, nil before one is recorded.
func GetStLedgerV2ExtenderShadow(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *StLedgerV2ExtenderShadow {
	key := requireStDeploymentKey(deploymentKey)
	var shadow *StLedgerV2ExtenderShadow
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT report_ref, report, create_time FROM st_ledger_v2_extender_shadow
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
		`, key, int64(noId), int64(epoch))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				shadow = &StLedgerV2ExtenderShadow{Epoch: epoch}
				server.Raise(result.Scan(&shadow.ReportRef, &shadow.Report, &shadow.CreateTime))
			}
		})
	})
	return shadow
}
