package model

import (
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
	// §7's sidecar tables (MG1): the admin event log past its 400 days, and
	// auth keys and API keys past their expiry
	RetentionTableNetworkAdminEvent = "network_admin_event"
	RetentionTableNetworkAuthKey    = "network_auth_key"
	RetentionTableAccountApiKey     = "account_api_key"
	// §8's sidecar table (MG2): acks past their 7 days
	RetentionTableNetworkAclAck = "network_acl_ack"
	// §8's sidecar table (MG6): invites whose 7-day window closed
	RetentionTableNetworkAclShare = "network_acl_share"
	// §13's sidecar table (MG5): tls-3's daily counts past their 13 months
	RetentionTableTlsConnectionDay = "tls_connection_day"
	// §15's sidecar table (MG7): host keys 30 days after their retirement
	RetentionTableMailHostKey = "mail_host_key"
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
	RetentionTableNetworkAdminEvent,
	RetentionTableNetworkAuthKey,
	RetentionTableAccountApiKey,
	RetentionTableNetworkAclAck,
	RetentionTableNetworkAclShare,
	RetentionTableTlsConnectionDay,
	RetentionTableMailHostKey,
}

// Rows the expiry and retention tasks deleted, by table: their rate signal,
// in place of per-row logs.
var retentionDeletedRowsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_retention_deleted_rows_total",
	Help: "Rows the expiry and retention tasks deleted, by table",
}, []string{"table"})

func init() {
	for _, table := range retentionTables {
		retentionDeletedRowsCounter.WithLabelValues(table)
	}
	prometheus.MustRegister(retentionDeletedRowsCounter)
}

// Counts rows a retention delete removed from one of retentionTables.
func countRetentionDeletedRows(table string, count int64) {
	if 0 < count {
		retentionDeletedRowsCounter.WithLabelValues(table).Add(float64(count))
	}
}
