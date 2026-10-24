// The write bounds of network_client_location_reliability: how often a
// connected client's update_block_number is refreshed, the batched retention
// delete that reads it, and the counts of each location reliability pass.
//
// Safe for concurrent use.
package model

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// A connected client's update_block_number is rewritten only once it is this
// old (UpdateClientLocationReliabilitiesInTx), so a pass rewrites a row only
// when one of its values changed or once a day. The block number can then lag
// the client's last pass by this much, and retention allows for it.
const ClientLocationReliabilityRefreshInterval = 24 * time.Hour

// ClientLocationReliabilityRefreshInterval in reliability blocks.
const clientLocationReliabilityRefreshBlocks = int64(ClientLocationReliabilityRefreshInterval / ReliabilityBlockDuration)

// The clients the last pass computed, by identity kind. A hosted child
// identity (network_client.source_client_id set) is a client most readers
// exclude; its share is how much of the pass's work they skip.
var clientLocationReliabilityPassClients = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_client_location_reliability_pass_clients",
	Help: "Clients the last client location reliability pass computed, by identity kind: hosted_child (source_client_id set) or top_level.",
}, []string{"identity"})

// The rows the last pass inserted or rewrote; the rest kept their row.
var clientLocationReliabilityPassWrittenRows = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_client_location_reliability_pass_written_rows",
	Help: "Rows the last client location reliability pass inserted or rewrote; its other clients were unchanged and their rows were not written.",
})

func init() {
	prometheus.MustRegister(clientLocationReliabilityPassClients, clientLocationReliabilityPassWrittenRows)
}

// Publishes the counts of one pass: its clients by identity kind, and how many
// rows its upsert wrote.
func recordClientLocationReliabilityPass(clientLocationReliabilities map[server.Id]*clientLocationReliability, writtenRowCount int64) {
	hostedChildCount, topLevelCount := 0, 0
	for _, reliability := range clientLocationReliabilities {
		if reliability.hostedChild {
			hostedChildCount += 1
		} else {
			topLevelCount += 1
		}
	}
	clientLocationReliabilityPassClients.WithLabelValues("hosted_child").Set(float64(hostedChildCount))
	clientLocationReliabilityPassClients.WithLabelValues("top_level").Set(float64(topLevelCount))
	clientLocationReliabilityPassWrittenRows.Set(float64(writtenRowCount))
}

type clientLocationReliabilityRetentionSettings struct {
	// rows deleted per transaction
	batchSize int
	// a run starts no batch once this long has passed since it began; the
	// next run, 15 minutes later, continues the backlog
	budget time.Duration
	// the clock the budget is read from
	now func() time.Time
}

// 10,000 rows per transaction keeps each delete short. 90 s keeps a run under
// the task's default two-minute run time while removing far more than a
// quarter hour of retirements, even on the days hosted identities were minted
// at hundreds a second.
func defaultClientLocationReliabilityRetentionSettings() *clientLocationReliabilityRetentionSettings {
	return &clientLocationReliabilityRetentionSettings{
		batchSize: 10000,
		budget:    90 * time.Second,
		now:       server.NowUtc,
	}
}

// The largest update_block_number retention deletes at maxTime. A row is kept
// ClientLocationExpiration past its last refresh plus the refresh interval,
// so it is never removed sooner than ClientLocationExpiration after its client
// was last seen, as before the refresh was daily.
func clientLocationReliabilityRetentionMaxBlockNumber(maxTime time.Time) int64 {
	minTime := maxTime.Add(-ClientLocationExpiration - ClientLocationReliabilityRefreshInterval)
	return minTime.UTC().UnixMilli()/int64(ReliabilityBlockDuration/time.Millisecond) - 1
}

// Deletes the expired rows oldest first, one batch per transaction, until a
// batch comes back short or the budget has passed. Each batch rechecks the
// cutoff on the rows it deletes, so a row a concurrent pass just refreshed is
// kept.
func (self *clientLocationReliabilityRetentionSettings) removeOld(ctx context.Context, maxTime time.Time) (removedCount int64) {
	maxBlockNumber := clientLocationReliabilityRetentionMaxBlockNumber(maxTime)
	startTime := self.now()
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM network_client_location_reliability
				USING (
					SELECT client_id
					FROM network_client_location_reliability
					WHERE update_block_number <= $1
					ORDER BY update_block_number
					LIMIT $2
				) AS expired
				WHERE
					network_client_location_reliability.client_id = expired.client_id AND
					network_client_location_reliability.update_block_number <= $1
				`,
				maxBlockNumber,
				self.batchSize,
			))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		removedCount += batchCount
		if batchCount < int64(self.batchSize) || self.budget <= self.now().Sub(startTime) {
			return
		}
	}
}
