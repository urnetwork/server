package model

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The tables the retention tasks delete from, as their counter labels. Labels
// come from this list only, never from data.
const (
	RetentionTableNetworkUserActionAttempt = "network_user_action_attempt"
	RetentionTableNetworkCreateAttempt     = "network_create_attempt"
	RetentionTableUserAuthReset            = "user_auth_reset"
	RetentionTableDeviceAddHistory         = "device_add_history"
	RetentionTableNetworkNameReclaim       = "network_name_reclaim"
	RetentionTableProviderBlackholeCheck   = "provider_blackhole_check"
	RetentionTableProviderBandwidthQuota   = "provider_bandwidth_quota"
	// the gated extender tier's release ledger
	RetentionTableNetworkExtenderReleaseRequest = "network_extender_release_request"
	RetentionTableNetworkExtenderRelease        = "network_extender_release"
	RetentionTableNetworkExtenderBlockReport    = "network_extender_block_report"
	// gone clients' signed key history
	RetentionTableStClientKeyHistory = "st_client_key_history"
	RetentionTableStClientKeyHead    = "st_client_key_head"
	RetentionTableStClientKeyGone    = "st_client_key_gone"
	// the session state retention (session_state_retention.go)
	RetentionTableProviderWorkSessionHead = "provider_work_session_head"
	RetentionTableAuditDeviceState        = "audit_device_state"
	// the location fold, by its retention and with its client
	// (network_client_location_reliability_retention.go)
	RetentionTableNetworkClientLocationReliability = "network_client_location_reliability"
	// the lingering origins' side table, past its prune time
	// (contract_origin_linger.go)
	RetentionTableContractOriginLinger = "contract_origin_linger"
)

var retentionTables = []string{
	RetentionTableNetworkUserActionAttempt,
	RetentionTableNetworkCreateAttempt,
	RetentionTableUserAuthReset,
	RetentionTableDeviceAddHistory,
	RetentionTableNetworkNameReclaim,
	RetentionTableProviderBlackholeCheck,
	RetentionTableProviderBandwidthQuota,
	RetentionTableNetworkExtenderReleaseRequest,
	RetentionTableNetworkExtenderRelease,
	RetentionTableNetworkExtenderBlockReport,
	RetentionTableStClientKeyHistory,
	RetentionTableStClientKeyHead,
	RetentionTableStClientKeyGone,
	RetentionTableProviderWorkSessionHead,
	RetentionTableAuditDeviceState,
	RetentionTableNetworkClientLocationReliability,
	RetentionTableContractOriginLinger,
}

// Rows the expiry and retention tasks deleted, by table: their rate signal,
// in place of per-row logs.
var retentionDeletedRowsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_retention_deleted_rows_total",
	Help: "Rows the expiry and retention tasks deleted, by table",
}, []string{"table"})

// The rows due for deletion that a table's retention counted at its last
// probe in this process, at most its probe's bound, and when it counted them.
// With urnetwork_retention_deleted_rows_total they are the retention stall
// alert (warp grafana/alerting/retention-stall.yml): no rows deleted for 6
// hours while the newest count says rows are due. Each process keeps the
// count of its own last probe, so a reader takes the count of the newest
// probe. A table's series exist only once its retention has probed in the
// process.
var retentionDueRowsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_retention_due_rows",
	Help: "Rows due for deletion that the table's retention counted at its last probe in this process, at most its probe's bound",
}, []string{"table"})

var retentionDueRowsTimestampGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_retention_due_rows_timestamp_seconds",
	Help: "When the table's retention last counted its due rows in this process",
}, []string{"table"})

func init() {
	for _, table := range retentionTables {
		retentionDeletedRowsCounter.WithLabelValues(table)
	}
	prometheus.MustRegister(retentionDeletedRowsCounter, retentionDueRowsGauge, retentionDueRowsTimestampGauge)
}

// Counts rows a retention delete removed from one of retentionTables.
func countRetentionDeletedRows(table string, count int64) {
	if 0 < count {
		retentionDeletedRowsCounter.WithLabelValues(table).Add(float64(count))
	}
}

// Publishes the rows of one of retentionTables that its retention found due
// at countTime.
func recordRetentionDueRows(table string, count int64, countTime time.Time) {
	retentionDueRowsGauge.WithLabelValues(table).Set(float64(count))
	retentionDueRowsTimestampGauge.WithLabelValues(table).Set(float64(countTime.UnixMilli()) / 1000)
}
