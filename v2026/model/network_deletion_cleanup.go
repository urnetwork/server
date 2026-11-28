// The cleanup after an account deletion (owner 2026-11-16, "clean up
// everything on account deletion"; support item 8364). The deletion itself
// (RemoveNetworkWithRemovals, the lag guest expiry) removes the network, its
// users and their sign-ins in one transaction and refuses every credential of
// the network from then on (session.RetireNetworkRecords). Until this cleanup,
// everything else stayed: the network's clients and devices, their keys,
// connections, locations and probe state, and the network's settings, codes,
// campaigns and analytics. The deletion now hands its network to this cleanup
// in the same transaction (a task, controller.RemoveDeletedNetworkData), and
// `bringyourctl network cleanup-deleted` runs it for the networks deleted
// before.
//
// What goes (every table that names a network, user, client or device is
// either deleted from or kept, deletedNetworkKeptTables):
//   - at once, the network's and its users' own rows: settings, API keys,
//     auth codes, OAuth grants, referral code and its referee link, feedback,
//     log upload records, the email campaigns' state and the app analytics
//     events, device associations, reset and verification codes;
//   - its active clients are deactivated (deactivateLockedNetworkClientsInTx:
//     the revocation records, the provide keys, and the connect, proxy and
//     contract closes that follow);
//   - each client is hard deleted ClientReapAfterDeactivate after its
//     deactivation, as a retired hosted window is, with every row keyed by
//     the client (its connections, locations, location and reliability
//     folds, keys, certificates, probe and stats rows, hosted proxy and
//     unique exit) and its Redis state. Its signed key history then goes
//     through its own retention (st_client_key_history_retention.go), which
//     the client's delete starts;
//   - once no client is left, the network's devices with their audit state
//     and events.
//
// What stays: the financial records (payments, balances, payouts, store and
// crypto purchases), the settlement and SN25 evidence (contracts, close
// reports, signed keys and consents, provider work journal, epoch records),
// and the audit and statistics rows other retention already ends. None is
// read here.
//
// Every run first checks that the network row is gone, and does nothing for
// a network that exists; a user's rows go only once that user is gone. Each
// step deletes at most RowBatchCount rows of one table, or ClientBatchCount
// clients, per statement, in short transactions, and every step is
// idempotent: a run that stops anywhere leaves the rest for the next run,
// which finds it again by the network id. A client's rows go before the
// client or in the transaction that deletes it, and its Redis state before
// it, so a run that stops leaves nothing keyed by a client that is gone.
//
// Functions are safe for concurrent use; two runs for the same network
// contend only on row locks, which the deletes skip or wait for briefly.
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A deleted network and the users it had, as the deletion hands them to the
// cleanup.
type DeletedNetwork struct {
	NetworkId server.Id `json:"network_id"`
	// the network's users at its deletion; their own rows go once their user
	// rows are gone
	UserIds []server.Id `json:"user_ids,omitempty"`
}

type DeletedNetworkCleanupSettings struct {
	// A deactivated client of a deleted network is hard deleted this long
	// after its deactivation. It is a contract endpoint for at most its
	// contract lifespan and the close linger, about 75 minutes, and usage
	// metering reads its row about two minutes after settlement; a day leaves
	// both far behind, as for a retired hosted window
	// (RetiredHostedNetworkClientReapSettings). Zero deletes at once.
	ClientReapAfterDeactivate time.Duration
	// clients one transaction deactivates or deletes, with their dependents;
	// non-positive counts as 1
	ClientBatchCount int
	// rows one statement deletes from one table; non-positive counts as 1
	RowBatchCount int
	// a run starts no batch after this long, and reports that work is left
	RunBudget time.Duration
	// bounds each lock wait of the cleanup's transactions
	LockTimeout time.Duration
	// bounds each count statement of CountDeletedNetworkData (the operator's
	// dry run): a count that runs longer is reported in TimedOutCounts and the
	// counting goes on with the next table. Zero leaves the counts unbounded.
	CountStatementTimeout time.Duration
	// how soon a run that stopped on its budget continues
	ContinueInterval time.Duration
	// how soon a run that failed is retried
	RetryInterval time.Duration
	// bounds a stranded claim of the cleanup task; well over RunBudget
	TaskMaxTime time.Duration
}

func DefaultDeletedNetworkCleanupSettings() *DeletedNetworkCleanupSettings {
	return &DeletedNetworkCleanupSettings{
		ClientReapAfterDeactivate: 24 * time.Hour,
		// a client has a few rows in most of its tables and at most a few
		// hundred in its probe history, so a batch stays well under a second
		ClientBatchCount: 200,
		RowBatchCount:    5000,
		RunBudget:        2 * time.Minute,
		LockTimeout:      5 * time.Second,
		ContinueInterval: 1 * time.Minute,
		RetryInterval:    10 * time.Minute,
		TaskMaxTime:      15 * time.Minute,
	}
}

func (self *DeletedNetworkCleanupSettings) clientBatchCount() int {
	return max(self.ClientBatchCount, 1)
}

func (self *DeletedNetworkCleanupSettings) rowBatchCount() int {
	return max(self.RowBatchCount, 1)
}

// What one run did and what is left.
type DeletedNetworkCleanupResult struct {
	// the network row exists, so the run did nothing
	NetworkExists bool `json:"network_exists,omitempty"`
	// rows deleted, by table, and clients deactivated as
	// DeletedNetworkCleanupDeactivatedClients
	RowCounts map[string]int64 `json:"row_counts,omitempty"`
	// nothing of the network is left to delete
	Complete bool `json:"complete,omitempty"`
	// the run budget ended the run with work left
	More bool `json:"more,omitempty"`
	// the time the earliest remaining client becomes due, when the clients
	// left wait for ClientReapAfterDeactivate
	NextClientDueTime *time.Time `json:"next_client_due_time,omitempty"`
}

func (self *DeletedNetworkCleanupResult) add(table string, count int64) {
	if count <= 0 {
		return
	}
	if self.RowCounts == nil {
		self.RowCounts = map[string]int64{}
	}
	self.RowCounts[table] += count
}

// The counter label of the clients a cleanup deactivated, in RowCounts.
const DeletedNetworkCleanupDeactivatedClients = "network_client_deactivated"

// One table of the network, a user, a client or a device. A batch deletes at
// most RowBatchCount rows, chosen by keyColumn = ANY($1) through the table's
// index on keyColumn, and named by the primary key. A table keyed by a device
// also matches its network_id = $3, so a device's rows go only where the
// deleted network wrote them.
type deletedNetworkTable struct {
	table     string
	keyColumn string
	// the primary key columns, which name the rows of a batch
	keyColumns []string
	// a device's rows that match only the deleted network's
	matchNetwork bool
	// the table's own batch delete, with the same arguments, for a row that
	// must wait for others to go first
	ownDeleteSql string
}

func (self *deletedNetworkTable) deleteSql() string {
	if self.ownDeleteSql != "" {
		return self.ownDeleteSql
	}
	keys := ""
	for i, column := range self.keyColumns {
		if 0 < i {
			keys += ", "
		}
		keys += column
	}
	networkMatch := ""
	if self.matchNetwork {
		networkMatch = " AND network_id = $3"
	}
	return fmt.Sprintf(
		`
			DELETE FROM %s
			WHERE (%s) IN (
				SELECT %s
				FROM %s
				WHERE %s = ANY($1)%s
				LIMIT $2
			)
		`,
		self.table,
		keys,
		keys,
		self.table,
		self.keyColumn,
		networkMatch,
	)
}

func (self *deletedNetworkTable) countSql() string {
	networkMatch := ""
	if self.matchNetwork {
		networkMatch = " AND network_id = $2"
	}
	return fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE %s = ANY($1)%s`,
		self.table,
		self.keyColumn,
		networkMatch,
	)
}

// The network's own rows. Every key column has an index that leads with it,
// except in the tables marked small, which hold a few thousand rows at most.
var deletedNetworkTables = []*deletedNetworkTable{
	{table: "account_preferences", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	{table: "account_api_key", keyColumn: "network_id", keyColumns: []string{"api_key_id"}},
	// small: no network index
	{table: "auth_code_redemption", keyColumn: "network_id", keyColumns: []string{"code_sha256", "request_id"}},
	{table: "account_feedback", keyColumn: "network_id", keyColumns: []string{"feedback_id"}},
	// the log files themselves expire from their bucket after
	// controller.FeedbackLogRetention
	{table: "feedback_log_upload", keyColumn: "network_id", keyColumns: []string{"feedback_log_upload_id"}},
	// small: no network index; the messages hold the sign-in address
	{table: "account_message_outbox", keyColumn: "network_id", keyColumns: []string{"message_id"}},
	{table: "network_referral_code", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	// the network as a referee; a link to it as the referrer stays with its
	// referee (the referral grant skips a gone network)
	{table: "network_referral", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	// the network as a referee: its first link, its qualification and its
	// days of app use (network_referral_terms.go)
	{table: "network_referral_referee", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	{table: "network_referral_app_day", keyColumn: "network_id", keyColumns: []string{"network_id", "day"}},
	{table: "network_mail_context", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	{table: "network_onboarding", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	{table: "network_onboarding_email", keyColumn: "network_id", keyColumns: []string{"message_id"}},
	{table: "network_onboarding_event", keyColumn: "network_id", keyColumns: []string{"event_id"}},
	{table: "network_onboarding_offer", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	// small: no network index. A code given to the network is spent; a null
	// network would offer it again, so the row goes
	{table: "network_onboarding_apple_offer_code", keyColumn: "network_id", keyColumns: []string{"code"}},
	{table: "network_reengagement", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	{table: "network_reengagement_email", keyColumn: "network_id", keyColumns: []string{"message_id"}},
	{table: "network_winback_offer", keyColumn: "network_id", keyColumns: []string{"offer_id"}},
	{table: "network_embed", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	{table: "network_top_level_client_limit", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	// small: no network index
	{table: "bulk_client_removal_quota", keyColumn: "network_id", keyColumns: []string{"bulk_client_removal_quota_id"}},
	{table: "exclude_network_client_location", keyColumn: "network_id", keyColumns: []string{"network_id", "client_location_id"}},
	{table: "device_add_history", keyColumn: "network_id", keyColumns: []string{"network_id", "add_time", "device_add_id"}},
	// small: no network-leading index
	{table: "device_association_name", keyColumn: "network_id", keyColumns: []string{"device_association_id", "network_id"}},
	{table: "device_adopt", keyColumn: "owner_network_id", keyColumns: []string{"device_association_id"}},
	{table: "device_share", keyColumn: "source_network_id", keyColumns: []string{"device_association_id"}},
	{table: "device_share", keyColumn: "guest_network_id", keyColumns: []string{"device_association_id"}},
	{table: "network_connection_reliability_score", keyColumn: "network_id", keyColumns: []string{"network_id", "country_location_id"}},
	{table: "network_connection_reliability_window", keyColumn: "network_id", keyColumns: []string{"network_id", "bucket_number"}},
	{table: "network_connection_reliability_window_score", keyColumn: "network_id", keyColumns: []string{"network_id", "country_location_id"}},
	{table: "lag_guest", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	// small: the lag.dev tracks hold the client's location; a result goes with
	// its track (deletedNetworkLagResultSql)
	{table: "lag_ticket", keyColumn: "network_id", keyColumns: []string{"ticket_id"}},
	{table: "lag_track", keyColumn: "network_id", keyColumns: []string{"track_id"}},
	// the drift tests hold a reference and a verdict (lag.dev/DRIFTTEST.md
	// R1); a test's tracks go with lag_track, and the test after its runs
	// (deletedNetworkLagDriftRunSql)
	{table: "lag_drift_test", keyColumn: "network_id", keyColumns: []string{"drift_test_id"}, ownDeleteSql: deletedNetworkLagDriftTestSql},
	{table: "lag_drift_network", keyColumn: "network_id", keyColumns: []string{"network_id"}},
	// small
	{table: "oauth_authorization_code", keyColumn: "network_id", keyColumns: []string{"code_hash"}},
	{table: "oauth_consent", keyColumn: "network_id", keyColumns: []string{"user_id", "client_id"}},
	{table: "oauth_refresh_token", keyColumn: "network_id", keyColumns: []string{"token_hash"}},
}

// The rows of a user that is gone.
var deletedUserTables = []*deletedNetworkTable{
	{table: "user_auth_reset", keyColumn: "user_id", keyColumns: []string{"user_auth_reset_id"}},
	{table: "user_auth_verify", keyColumn: "user_id", keyColumns: []string{"user_auth_verify_id"}},
	{table: "network_user_action_attempt", keyColumn: "user_id", keyColumns: []string{"network_user_action_attempt_id"}},
	{table: "oauth_consent", keyColumn: "user_id", keyColumns: []string{"user_id", "client_id"}},
	{table: "oauth_refresh_token", keyColumn: "user_id", keyColumns: []string{"token_hash"}},
	// small: no user index
	{table: "oauth_authorization_code", keyColumn: "user_id", keyColumns: []string{"code_hash"}},
}

// A client's rows that can be many: its probe and stats history. They go in
// bounded batches before the client's own transaction.
var deletedClientHistoryTables = []*deletedNetworkTable{
	{table: "network_client_location", keyColumn: "client_id", keyColumns: []string{"connection_id"}},
	{table: "provider_egress_health_history", keyColumn: "client_id", keyColumns: []string{"run_id", "measured_at"}},
	{table: "provider_egress_url_security", keyColumn: "client_id", keyColumns: []string{"client_id", "url_key"}},
	{table: "provider_url_probe_run", keyColumn: "client_id", keyColumns: []string{"client_id", "claim_ordinal"}},
	{table: "search_provider_stats", keyColumn: "client_id", keyColumns: []string{"period_start", "client_id"}},
	{table: "verify_provider_stats", keyColumn: "client_id", keyColumns: []string{"period_start", "client_id"}},
	{table: "network_client_data_usage", keyColumn: "client_id", keyColumns: []string{"client_id", "period_start"}},
	{table: "provide_key_change", keyColumn: "client_id", keyColumns: []string{"client_id", "change_time"}},
	{table: "client_reliability_running", keyColumn: "client_id", keyColumns: []string{"client_id", "lookback_index"}},
	{table: "client_connection_reliability_score", keyColumn: "client_id", keyColumns: []string{"client_id", "lookback_index"}},
}

// A client's rows that are few, deleted with the client in its transaction.
var deletedClientTables = []string{
	"network_client_location_reliability",
	"network_client_provider_intent",
	"network_client_role",
	"network_client_acl_group",
	"network_client_data_cap",
	"client_tls_certificate",
	"provide_key",
	"provide_capability",
	"provider_egress_health",
	"provider_egress_location",
	"provider_egress_probe_attempt",
	"provider_egress_probe_cycle",
	"provider_intent_probe_priority",
	"provider_modern_evidence",
	"provider_blackhole_check",
	"provider_bandwidth",
	"provider_zero_carry",
}

// A device's audit rows, those the deleted network wrote.
var deletedDeviceTables = []*deletedNetworkTable{
	{table: "audit_device_event", keyColumn: "device_id", keyColumns: []string{"event_time", "device_id", "event_id"}, matchNetwork: true},
	{table: "audit_device_state", keyColumn: "device_id", keyColumns: []string{"device_id"}, matchNetwork: true},
	{table: "audit_provider_state", keyColumn: "device_id", keyColumns: []string{"device_id"}, matchNetwork: true},
}

// The kinds of rows a deleted account keeps.
const (
	// payments, purchases, balances, payouts and the records that tie them
	// to the account: kept for legal, tax and dispute reasons (owner decides
	// any retention limit)
	deletedNetworkKeepFinancial = "financial"
	// contracts, close reports, signed keys, consents and bindings, the
	// provider work journal and the epoch and SN25 records: kept as
	// settlement evidence, each under its own retention
	deletedNetworkKeepSettlement = "settlement evidence"
	// rows another retention already ends, or aggregates and statistics
	// that hold no more than an id
	deletedNetworkKeepRetention = "own retention or aggregate"
	// the prober's own system accounts, removed by the prober shard reap
	deletedNetworkKeepSystem = "system account"
	// the record that the account was deleted (deleted_network_model.go),
	// which a store purchase's move to its owner's new account reads
	deletedNetworkKeepDeletionRecord = "deletion record"
)

// Every table that names a network, user, client or device and keeps its rows
// when the account is deleted, with why. Every other such table is deleted
// from (deletedNetworkDeletedTableNames); TestEveryAccountTableIsClassified
// checks the schema against both, so a new table must be classified.
var deletedNetworkKeptTables = map[string]string{
	"account_balance":                  deletedNetworkKeepFinancial,
	"account_payment":                  deletedNetworkKeepFinancial,
	"account_payment_bonus":            deletedNetworkKeepFinancial,
	"account_point":                    deletedNetworkKeepFinancial,
	"account_wallet":                   deletedNetworkKeepFinancial,
	"apple_offer_code_binding":         deletedNetworkKeepFinancial,
	"apple_subscription_move":          deletedNetworkKeepFinancial,
	"apple_subscription_transaction":   deletedNetworkKeepFinancial,
	"circle_transfer_observation":      deletedNetworkKeepFinancial,
	"circle_transfer_request":          deletedNetworkKeepFinancial,
	"circle_uc":                        deletedNetworkKeepFinancial,
	"crypto_payment_intent":            deletedNetworkKeepFinancial,
	"gamma_account":                    deletedNetworkKeepFinancial,
	"gamma_deposit_hotkey":             deletedNetworkKeepFinancial,
	"gamma_payment_intent":             deletedNetworkKeepFinancial,
	"gift_order":                       deletedNetworkKeepFinancial,
	"legacy_settlement_intent":         deletedNetworkKeepFinancial,
	"operator_block_network_traffic":   deletedNetworkKeepFinancial,
	"payment_plan_assignment":          deletedNetworkKeepFinancial,
	"payment_plan_reliability_partial": deletedNetworkKeepFinancial,
	"payment_plan_sweep":               deletedNetworkKeepFinancial,
	"payment_reconciliation_event":     deletedNetworkKeepFinancial,
	"payout_wallet":                    deletedNetworkKeepFinancial,
	"play_purchase_binding":            deletedNetworkKeepFinancial,
	"pro_grant_cycle":                  deletedNetworkKeepFinancial,
	"solana_payment_intent":            deletedNetworkKeepFinancial,
	"solana_unfulfilled_payment":       deletedNetworkKeepFinancial,
	"stripe_customer":                  deletedNetworkKeepFinancial,
	"stripe_invoice":                   deletedNetworkKeepFinancial,
	"stripe_invoice_payment_failure":   deletedNetworkKeepFinancial,
	"stripe_refund":                    deletedNetworkKeepFinancial,
	"subscription_payment":             deletedNetworkKeepFinancial,
	"subscription_renewal":             deletedNetworkKeepFinancial,
	"test_balance_drain":               deletedNetworkKeepFinancial,
	"transfer_balance":                 deletedNetworkKeepFinancial,
	"transfer_balance_code":            deletedNetworkKeepFinancial,
	"transfer_escrow_sweep":            deletedNetworkKeepFinancial,

	"audit_contract_event":                deletedNetworkKeepSettlement,
	"audit_extender_event":                deletedNetworkKeepSettlement,
	"client_reliability":                  deletedNetworkKeepSettlement,
	"contract_close_report_evidence":      deletedNetworkKeepSettlement,
	"contract_extender":                   deletedNetworkKeepSettlement,
	"contract_participant":                deletedNetworkKeepSettlement,
	"ctf_award":                           deletedNetworkKeepSettlement,
	"ctf_census_member":                   deletedNetworkKeepSettlement,
	"ctf_presence":                        deletedNetworkKeepSettlement,
	"hotkey_network_delegation":           deletedNetworkKeepSettlement,
	"hotkey_network_delegation_challenge": deletedNetworkKeepSettlement,
	"hotkey_wallet_mapping_submitter":     deletedNetworkKeepSettlement,
	"network_extender":                    deletedNetworkKeepSettlement,
	"network_wallet_mapping_challenge":    deletedNetworkKeepSettlement,
	"network_wallet_mapping_consent":      deletedNetworkKeepSettlement,
	"provider_client_verdict":             deletedNetworkKeepSettlement,
	"provider_egress_exit_day":            deletedNetworkKeepSettlement,
	"provider_work_owner":                 deletedNetworkKeepSettlement,
	"provider_work_request":               deletedNetworkKeepSettlement,
	"provider_work_session_event":         deletedNetworkKeepSettlement,
	"provider_work_session_head":          deletedNetworkKeepSettlement,
	"provider_work_session_receipt":       deletedNetworkKeepSettlement,
	"sn_fleet_auto_authorization":         deletedNetworkKeepSettlement,
	"sn_fleet_auto_device":                deletedNetworkKeepSettlement,
	"sn_fleet_auto_generation":            deletedNetworkKeepSettlement,
	"sn_fleet_auto_member":                deletedNetworkKeepSettlement,
	"sn_fleet_auto_relay":                 deletedNetworkKeepSettlement,
	"sn_fleet_auto_top_up":                deletedNetworkKeepSettlement,
	"sn_fleet_binding":                    deletedNetworkKeepSettlement,
	"sn_top_fleet":                        deletedNetworkKeepSettlement,
	"sn_top_network":                      deletedNetworkKeepSettlement,
	"sn_top_network_daily":                deletedNetworkKeepSettlement,
	"sn_trail_hop":                        deletedNetworkKeepSettlement,
	// the client's delete starts their 7-day retention
	// (st_client_key_history_retention.go)
	"st_client_key_gone":             deletedNetworkKeepSettlement,
	"st_client_key_head":             deletedNetworkKeepSettlement,
	"st_client_key_history":          deletedNetworkKeepSettlement,
	"st_fleet_binding_original":      deletedNetworkKeepSettlement,
	"st_fleet_binding_signature":     deletedNetworkKeepSettlement,
	"st_payout_leaf":                 deletedNetworkKeepSettlement,
	"st_payout_wallet_resolution":    deletedNetworkKeepSettlement,
	"st_platform_endpoint":           deletedNetworkKeepSettlement,
	"st_provider_wallet_history":     deletedNetworkKeepSettlement,
	"st_verified_wallet_consent":     deletedNetworkKeepSettlement,
	"st_wallet":                      deletedNetworkKeepSettlement,
	"transfer_contract":              deletedNetworkKeepSettlement,
	"verify_cohort_census":           deletedNetworkKeepSettlement,
	"verify_original_request_closed": deletedNetworkKeepSettlement,
	"verify_original_request_lookup": deletedNetworkKeepSettlement,
	"wallet_mapping_challenge":       deletedNetworkKeepSettlement,
	"wallet_mapping_consent":         deletedNetworkKeepSettlement,

	// the network created and deleted events the network statistics replay
	"audit_network_event": deletedNetworkKeepRetention,
	// 180 days (AuditEventExpiration); no index leads with its device
	"audit_provider_event": deletedNetworkKeepRetention,
	// 24 hours after it completes (session.ExpireSessionReceipts)
	"network_session_operation":          deletedNetworkKeepRetention,
	"network_session_index_outbox":       deletedNetworkKeepRetention,
	"network_extender_block_report":      deletedNetworkKeepRetention,
	"network_extender_latency":           deletedNetworkKeepRetention,
	"provider_bandwidth_quota":           deletedNetworkKeepRetention,
	"provider_egress_country_site_tally": deletedNetworkKeepRetention,
	"provider_egress_h3_place":           deletedNetworkKeepRetention,
	"provider_egress_ipv6_place":         deletedNetworkKeepRetention,
	// the OAuth applications: its client_id names an application, not ours
	"oauth_client": deletedNetworkKeepRetention,

	"prober_identity":  deletedNetworkKeepSystem,
	"prober_shard_run": deletedNetworkKeepSystem,

	"deleted_network": deletedNetworkKeepDeletionRecord,
}

// Every table that names a network, user, client or device whose rows go with
// a deleted account: in the deletion's own transaction (RemoveNetworkWithRemovals),
// by a foreign key's cascade, or by this cleanup.
func deletedNetworkDeletedTableNames() map[string]bool {
	tables := map[string]bool{
		// the deletion's transaction
		"network":                      true,
		"network_user":                 true,
		"network_user_auth_password":   true,
		"network_user_auth_sso":        true,
		"network_user_auth_wallet":     true,
		"network_user_auth_seedphrase": true,
		// the cascade of network's delete
		"network_client_registration": true,
		// the cleanup's own statements
		"network_client":             true,
		"device":                     true,
		"network_client_connection":  true,
		"proxy_device_config":        true,
		"proxy_client":               true,
		"proxy_unique_exit":          true,
		"proxy_exit_assignment":      true,
		"auth_code":                  true,
		"network_points_leaderboard": true,
		"lag_drift_run":              true,
	}
	for _, table := range deletedNetworkTables {
		tables[table.table] = true
	}
	for _, table := range deletedUserTables {
		tables[table.table] = true
	}
	for _, table := range deletedClientHistoryTables {
		tables[table.table] = true
	}
	for _, table := range deletedClientTables {
		tables[table] = true
	}
	for _, table := range deletedDeviceTables {
		tables[table.table] = true
	}
	return tables
}

// The roles of the network's auth codes go before the codes. Small: auth_code
// has no network-leading index.
const deletedNetworkAuthCodeRoleSql = `
	DELETE FROM auth_code_role
	WHERE auth_code_role.auth_code_id IN (
		SELECT auth_code.auth_code_id
		FROM auth_code
		WHERE auth_code.network_id = ANY($1) OR auth_code.user_id = ANY($3)
		LIMIT $2
	)
`

const deletedNetworkAuthCodeSql = `
	DELETE FROM auth_code
	WHERE auth_code.auth_code_id IN (
		SELECT auth_code.auth_code_id
		FROM auth_code
		WHERE
			(auth_code.network_id = ANY($1) OR auth_code.user_id = ANY($3)) AND
			NOT EXISTS (
				SELECT 1 FROM auth_code_role
				WHERE auth_code_role.auth_code_id = auth_code.auth_code_id
			)
		LIMIT $2
	)
`

// A lag.dev result goes with its track or ticket, which go after it.
const deletedNetworkLagResultSql = `
	DELETE FROM lag_result
	WHERE lag_result.lag_result_id IN (
		SELECT lag_result.lag_result_id
		FROM lag_result
		WHERE
			lag_result.track_id IN (SELECT track_id FROM lag_track WHERE network_id = ANY($1)) OR
			lag_result.ticket_id IN (SELECT ticket_id FROM lag_ticket WHERE network_id = ANY($1))
		LIMIT $2
	)
`

// A drift test's runs, which name no network, go before it.
const deletedNetworkLagDriftRunSql = `
	DELETE FROM lag_drift_run
	WHERE (lag_drift_run.drift_test_id, lag_drift_run.run) IN (
		SELECT lag_drift_run.drift_test_id, lag_drift_run.run
		FROM lag_drift_run
		WHERE lag_drift_run.drift_test_id IN (SELECT drift_test_id FROM lag_drift_test WHERE network_id = ANY($1))
		LIMIT $2
	)
`

// A drift test goes once none of its runs is left, so no run outlives the
// test by which it is found.
const deletedNetworkLagDriftTestSql = `
	DELETE FROM lag_drift_test
	WHERE lag_drift_test.drift_test_id IN (
		SELECT lag_drift_test.drift_test_id
		FROM lag_drift_test
		WHERE
			lag_drift_test.network_id = ANY($1) AND
			NOT EXISTS (SELECT 1 FROM lag_drift_run WHERE lag_drift_run.drift_test_id = lag_drift_test.drift_test_id)
		LIMIT $2
	)
`

// The network's rows in the points leaderboard snapshots, by the snapshots'
// primary key. A new snapshot leaves a gone network out (it joins network),
// and old snapshots are pruned, so this only ends them sooner.
const deletedNetworkPointsLeaderboardSql = `
	DELETE FROM network_points_leaderboard
	WHERE
		network_points_leaderboard.network_id = ANY($1) AND
		network_points_leaderboard.snapshot_id IN (
			SELECT network_points_leaderboard_snapshot.snapshot_id
			FROM network_points_leaderboard_snapshot
		)
`

// Rows deleted by the account deletion cleanup, by table.
var deletedNetworkCleanupRowsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_deleted_network_cleanup_rows_total",
	Help: "Rows the account deletion cleanup deleted, by table, and clients it deactivated (network_client_deactivated)",
}, []string{"table"})

// Cleanup runs, by outcome: complete, waiting (clients wait to become due),
// more (the run budget ended the run), network_exists (refused) and error.
var deletedNetworkCleanupRunsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_deleted_network_cleanup_runs_total",
	Help: "Account deletion cleanup runs, by outcome",
}, []string{"outcome"})

const (
	DeletedNetworkCleanupOutcomeComplete      = "complete"
	DeletedNetworkCleanupOutcomeWaiting       = "waiting"
	DeletedNetworkCleanupOutcomeMore          = "more"
	DeletedNetworkCleanupOutcomeNetworkExists = "network_exists"
	DeletedNetworkCleanupOutcomeError         = "error"
)

func init() {
	for _, outcome := range []string{
		DeletedNetworkCleanupOutcomeComplete,
		DeletedNetworkCleanupOutcomeWaiting,
		DeletedNetworkCleanupOutcomeMore,
		DeletedNetworkCleanupOutcomeNetworkExists,
		DeletedNetworkCleanupOutcomeError,
	} {
		deletedNetworkCleanupRunsCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(deletedNetworkCleanupRowsCounter)
	prometheus.MustRegister(deletedNetworkCleanupRunsCounter)
}

func countDeletedNetworkCleanupRows(table string, count int64) {
	if 0 < count {
		deletedNetworkCleanupRowsCounter.WithLabelValues(table).Add(float64(count))
	}
}

// Whether the network row exists, in the transaction that is about to delete
// its data. The cleanup touches nothing of a network that exists.
func deletedNetworkExistsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (exists bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`,
		networkId,
	).Scan(&exists))
	return
}

// The users of userIds whose rows are gone; a user that exists keeps its rows.
func deletedUserIdsInTx(ctx context.Context, tx server.PgTx, userIds []server.Id) []server.Id {
	goneUserIds := []server.Id{}
	if len(userIds) == 0 {
		return goneUserIds
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT candidate.user_id
			FROM unnest($1::uuid[]) AS candidate(user_id)
			WHERE NOT EXISTS (
				SELECT 1 FROM network_user
				WHERE network_user.user_id = candidate.user_id
			)
		`,
		idStrings(userIds),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userId server.Id
			server.Raise(result.Scan(&userId))
			goneUserIds = append(goneUserIds, userId)
		}
	})
	return goneUserIds
}

// The users that rows still name together with the deleted networks: the
// deletion of an account before 2026-11-16 left no record of its admin user,
// so the cleanup of a past deletion finds the user here, in the rows that
// name both. Only users whose rows are gone are ever touched
// (deletedUserIdsInTx). Reads only.
func deletedNetworkUserIds(ctx context.Context, networkIds []server.Id) []server.Id {
	userIds := []server.Id{}
	if len(networkIds) == 0 {
		return userIds
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		userIds = []server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT DISTINCT named.user_id
				FROM (
					SELECT user_id FROM account_feedback WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM auth_code WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM feedback_log_upload WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM lag_guest WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM oauth_consent WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM oauth_refresh_token WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM oauth_authorization_code WHERE network_id = ANY($1)
					UNION ALL
					SELECT user_id FROM subscription_payment WHERE network_id = ANY($1)
				) AS named
				WHERE named.user_id IS NOT NULL
			`,
			idStrings(networkIds),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var userId server.Id
				server.Raise(result.Scan(&userId))
				userIds = append(userIds, userId)
			}
		})
	}, server.TxReadCommitted, server.OptReadOnly())
	return userIds
}

// Removes what a deleted network left, as far as RunBudget allows: its own
// rows and its users', then its clients, then its devices. A client is
// deactivated first and hard deleted ClientReapAfterDeactivate later, so a
// network deleted just now keeps its clients, inactive, for that long. now is
// the clock the client delay reads. Panics on a database or Redis error; what
// committed before stays done.
func RemoveDeletedNetworkData(
	ctx context.Context,
	deletedNetwork *DeletedNetwork,
	now time.Time,
	settings *DeletedNetworkCleanupSettings,
) (cleanupResult *DeletedNetworkCleanupResult) {
	cleanupResult = &DeletedNetworkCleanupResult{}
	networkId := deletedNetwork.NetworkId
	deadline := time.Now().Add(settings.RunBudget)
	budgetLeft := func() bool {
		return time.Now().Before(deadline) && ctx.Err() == nil
	}
	// what committed counts even when a later step fails
	defer func() {
		for table, count := range cleanupResult.RowCounts {
			countDeletedNetworkCleanupRows(table, count)
		}
		if r := recover(); r != nil {
			deletedNetworkCleanupRunsCounter.WithLabelValues(DeletedNetworkCleanupOutcomeError).Inc()
			panic(r)
		}
		switch {
		case cleanupResult.NetworkExists:
			deletedNetworkCleanupRunsCounter.WithLabelValues(DeletedNetworkCleanupOutcomeNetworkExists).Inc()
		case cleanupResult.Complete:
			deletedNetworkCleanupRunsCounter.WithLabelValues(DeletedNetworkCleanupOutcomeComplete).Inc()
		case cleanupResult.More:
			deletedNetworkCleanupRunsCounter.WithLabelValues(DeletedNetworkCleanupOutcomeMore).Inc()
		default:
			deletedNetworkCleanupRunsCounter.WithLabelValues(DeletedNetworkCleanupOutcomeWaiting).Inc()
		}
	}()

	// a run whose context is done before it starts touches nothing
	if !budgetLeft() {
		cleanupResult.More = true
		return
	}
	// a network that exists is not touched; every transaction below checks
	// again before it deletes
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		cleanupResult.NetworkExists = deletedNetworkExistsInTx(ctx, tx, networkId)
	}, server.TxReadCommitted, server.OptReadOnly(), server.TxLockTimeout(settings.LockTimeout))
	if cleanupResult.NetworkExists {
		glog.Infof("[dnc]network %s exists; the deleted network cleanup touches nothing\n", networkId)
		return
	}

	// the network's and its users' own rows; the users the deletion handed
	// over, and those the network's rows name, found before those rows go
	userIds := append(slices.Clone(deletedNetwork.UserIds), deletedNetworkUserIds(ctx, []server.Id{networkId})...)
	for {
		if !budgetLeft() {
			cleanupResult.More = true
			return
		}
		full := removeDeletedNetworkRows(ctx, []server.Id{networkId}, userIds, settings, cleanupResult)
		if !full {
			break
		}
	}

	// the active clients go inactive first
	for {
		if !budgetLeft() {
			cleanupResult.More = true
			return
		}
		batchCount, deactivatedCount := deactivateDeletedNetworkClients(ctx, networkId, settings)
		cleanupResult.add(DeletedNetworkCleanupDeactivatedClients, deactivatedCount)
		if batchCount < int64(settings.clientBatchCount()) {
			break
		}
	}

	// then the clients that are due, with everything keyed by them
	for {
		if !budgetLeft() {
			cleanupResult.More = true
			return
		}
		removedCount := removeDeletedNetworkClients(ctx, networkId, now, settings, cleanupResult)
		if removedCount < int64(settings.clientBatchCount()) {
			break
		}
	}

	// clients that are not due yet wait; the devices go after the last client
	var remainingClientCount int64
	var nextDueTime *time.Time
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		remainingClientCount = 0
		nextDueTime = nil
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT
					count(*),
					min(COALESCE(network_client.deactivate_time, network_client.create_time))
				FROM network_client
				WHERE network_client.network_id = $1
			`,
			networkId,
		).Scan(&remainingClientCount, &nextDueTime))
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	if 0 < remainingClientCount {
		if nextDueTime != nil {
			due := nextDueTime.UTC().Add(settings.ClientReapAfterDeactivate)
			cleanupResult.NextClientDueTime = &due
		}
		return
	}

	for {
		if !budgetLeft() {
			cleanupResult.More = true
			return
		}
		removedCount := removeDeletedNetworkDevices(ctx, networkId, settings, cleanupResult)
		if removedCount < int64(settings.clientBatchCount()) {
			break
		}
	}

	cleanupResult.Complete = true
	return
}

// The deleted networks among networkIds that still have a client or a device,
// which need RemoveDeletedNetworkData one at a time. A network that exists is
// never among them. Reads only.
func DeletedNetworksWithClientsOrDevices(ctx context.Context, networkIds []server.Id) map[server.Id]bool {
	networkIdSet := map[server.Id]bool{}
	if len(networkIds) == 0 {
		return networkIdSet
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		networkIdSet = map[server.Id]bool{}
		result, err := tx.Query(
			ctx,
			`
				SELECT candidate.network_id
				FROM unnest($1::uuid[]) AS candidate(network_id)
				WHERE
					NOT EXISTS (
						SELECT 1 FROM network
						WHERE network.network_id = candidate.network_id
					) AND (
						EXISTS (
							SELECT 1 FROM network_client
							WHERE network_client.network_id = candidate.network_id
						) OR EXISTS (
							SELECT 1 FROM device
							WHERE device.network_id = candidate.network_id
						)
					)
			`,
			idStrings(networkIds),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIdSet[networkId] = true
			}
		})
	}, server.TxReadCommitted, server.OptReadOnly())
	return networkIdSet
}

// Removes the own rows of many deleted networks together, the networks'
// tables of RemoveDeletedNetworkData, in passes of one transaction and one
// round trip each, for `bringyourctl network cleanup-deleted`: most networks a
// past deletion left have no client or device left
// (DeletedNetworksWithClientsOrDevices), only a few rows, so a pass for many
// networks replaces many runs. The rows of the users their rows name go too
// (deletedNetworkUserIds). Networks and users that exist are left alone.
// Returns the rows deleted by table.
func RemoveDeletedNetworksRows(
	ctx context.Context,
	networkIds []server.Id,
	settings *DeletedNetworkCleanupSettings,
) map[string]int64 {
	cleanupResult := &DeletedNetworkCleanupResult{}
	defer func() {
		for table, count := range cleanupResult.RowCounts {
			countDeletedNetworkCleanupRows(table, count)
		}
	}()
	userIds := deletedNetworkUserIds(ctx, networkIds)
	for {
		full := removeDeletedNetworkRows(ctx, networkIds, userIds, settings, cleanupResult)
		if !full || ctx.Err() != nil {
			break
		}
	}
	if cleanupResult.RowCounts == nil {
		return map[string]int64{}
	}
	return cleanupResult.RowCounts
}

// One statement of a pipelined batch and the table its deleted rows count
// under. A statement with a row limit reports when it filled its batch.
type deletedNetworkStatement struct {
	table   string
	sql     string
	args    []any
	limited bool
}

// Sends the statements as one pipelined batch in tx and counts the rows each
// deleted into tableCounts. Returns true when a limited statement deleted a
// whole batch, so another pass has work.
func execDeletedNetworkStatementsInTx(
	ctx context.Context,
	tx server.PgTx,
	statements []*deletedNetworkStatement,
	rowBatchCount int,
	tableCounts map[string]int64,
) (full bool) {
	if len(statements) == 0 {
		return false
	}
	server.BatchInTx(
		ctx,
		tx,
		func(batch server.PgBatch) {
			for _, statement := range statements {
				batch.Queue(statement.sql, statement.args...)
			}
		},
		func(results server.PgBatchResults) {
			for _, statement := range statements {
				tag := server.RaisePgResult(results.Exec())
				tableCounts[statement.table] += tag.RowsAffected()
				if statement.limited && int64(rowBatchCount) <= tag.RowsAffected() {
					full = true
				}
			}
		},
	)
	return
}

// One batch of each table of the networks and the gone users, in one
// transaction and one round trip. Networks that exist and users that exist
// are left alone. Returns true when some table filled its batch, so another
// pass has work.
func removeDeletedNetworkRows(
	ctx context.Context,
	networkIds []server.Id,
	userIds []server.Id,
	settings *DeletedNetworkCleanupSettings,
	cleanupResult *DeletedNetworkCleanupResult,
) (full bool) {
	rowBatchCount := settings.rowBatchCount()
	tableCounts := map[string]int64{}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		// an attempt that does not commit deleted nothing
		full = false
		tableCounts = map[string]int64{}
		goneNetworkIds := []server.Id{}
		for _, networkId := range networkIds {
			if !deletedNetworkExistsInTx(ctx, tx, networkId) {
				goneNetworkIds = append(goneNetworkIds, networkId)
			}
		}
		goneUserIds := deletedUserIdsInTx(ctx, tx, userIds)
		if len(goneNetworkIds) == 0 && len(goneUserIds) == 0 {
			return
		}
		// one pass over the tables, in dependency order: roles before codes,
		// results before tracks
		networkIdStrings := idStrings(goneNetworkIds)
		userIdStrings := idStrings(goneUserIds)
		statements := []*deletedNetworkStatement{
			{table: "auth_code_role", sql: deletedNetworkAuthCodeRoleSql, args: []any{networkIdStrings, rowBatchCount, userIdStrings}, limited: true},
			{table: "auth_code", sql: deletedNetworkAuthCodeSql, args: []any{networkIdStrings, rowBatchCount, userIdStrings}, limited: true},
		}
		if 0 < len(goneNetworkIds) {
			statements = append(
				statements,
				&deletedNetworkStatement{table: "lag_result", sql: deletedNetworkLagResultSql, args: []any{networkIdStrings, rowBatchCount}, limited: true},
				&deletedNetworkStatement{table: "lag_drift_run", sql: deletedNetworkLagDriftRunSql, args: []any{networkIdStrings, rowBatchCount}, limited: true},
				&deletedNetworkStatement{table: "network_points_leaderboard", sql: deletedNetworkPointsLeaderboardSql, args: []any{networkIdStrings}},
			)
			for _, table := range deletedNetworkTables {
				statements = append(statements, &deletedNetworkStatement{
					table:   table.table,
					sql:     table.deleteSql(),
					args:    []any{networkIdStrings, rowBatchCount},
					limited: true,
				})
			}
		}
		for _, table := range deletedUserTables {
			if len(goneUserIds) == 0 {
				break
			}
			statements = append(statements, &deletedNetworkStatement{
				table:   table.table,
				sql:     table.deleteSql(),
				args:    []any{userIdStrings, rowBatchCount},
				limited: true,
			})
		}
		full = execDeletedNetworkStatementsInTx(ctx, tx, statements, rowBatchCount, tableCounts)
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	for table, count := range tableCounts {
		cleanupResult.add(table, count)
	}
	return
}

// Deactivates one batch of the network's active clients, newest first, as a
// client removal does, with their active descendants. A derived client is
// minted after its parent, so newest first takes a hosted proxy's windows in
// batches before the proxy itself, and no batch pulls a large descendant tree
// into its transaction. Returns how many clients of the batch it deactivated,
// and how many in all.
func deactivateDeletedNetworkClients(
	ctx context.Context,
	networkId server.Id,
	settings *DeletedNetworkCleanupSettings,
) (batchCount int64, deactivatedCount int64) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		batchCount = 0
		deactivatedCount = 0
		if deletedNetworkExistsInTx(ctx, tx, networkId) {
			return
		}
		clientIds := []server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT client_id
				FROM network_client
				WHERE
					network_id = $1 AND
					active
				ORDER BY client_id DESC
				LIMIT $2
				FOR UPDATE
			`,
			networkId,
			settings.clientBatchCount(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
		if len(clientIds) == 0 {
			return
		}
		var err2 error
		batchCount, deactivatedCount, err2 = deactivateLockedNetworkClientsCountedInTx(withSessionFreePath(ctx, session.SessionFreePathNetworkDeletion), tx, clientIds, networkId)
		server.Raise(err2)
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	return
}

// Hard deletes one batch of the network's inactive clients that are due:
// first their Redis state and their history rows, then, in one transaction,
// every other row keyed by them and the clients themselves. Returns how many
// clients it deleted.
func removeDeletedNetworkClients(
	ctx context.Context,
	networkId server.Id,
	now time.Time,
	settings *DeletedNetworkCleanupSettings,
	cleanupResult *DeletedNetworkCleanupResult,
) (removedCount int64) {
	dueTime := now.UTC().Add(-settings.ClientReapAfterDeactivate)
	clientBatchCount := settings.clientBatchCount()
	rowBatchCount := settings.rowBatchCount()

	// the batch, and what its Redis cleanup needs
	clientIds := []server.Id{}
	deviceIds := []server.Id{}
	proxyIds := []server.Id{}
	released := []releasedProxyEgress{}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		clientIds = []server.Id{}
		deviceIds = []server.Id{}
		proxyIds = []server.Id{}
		released = []releasedProxyEgress{}
		if deletedNetworkExistsInTx(ctx, tx, networkId) {
			return
		}
		result, err := tx.Query(
			ctx,
			`
				SELECT client_id, device_id
				FROM network_client
				WHERE
					network_id = $1 AND
					NOT active AND
					COALESCE(deactivate_time, create_time) < $2
				ORDER BY client_id
				LIMIT $3
			`,
			networkId,
			dueTime,
			clientBatchCount,
		)
		deviceIdSet := map[server.Id]bool{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var deviceId *server.Id
				server.Raise(result.Scan(&clientId, &deviceId))
				clientIds = append(clientIds, clientId)
				if deviceId != nil && !deviceIdSet[*deviceId] {
					deviceIdSet[*deviceId] = true
					deviceIds = append(deviceIds, *deviceId)
				}
			}
		})
		if len(clientIds) == 0 {
			return
		}
		result, err = tx.Query(
			ctx,
			`
				SELECT proxy_device_config.proxy_id
				FROM proxy_device_config
				WHERE proxy_device_config.client_id = ANY($1)
			`,
			idStrings(clientIds),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var proxyId server.Id
				server.Raise(result.Scan(&proxyId))
				proxyIds = append(proxyIds, proxyId)
			}
		})
		if 0 < len(proxyIds) {
			result, err = tx.Query(
				ctx,
				`
					SELECT proxy_client.client_id, proxy_client.client_ipv4
					FROM proxy_client
					WHERE proxy_client.proxy_id = ANY($1)
				`,
				idStrings(proxyIds),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var item releasedProxyEgress
					server.Raise(result.Scan(&item.ClientID, &item.IPv4))
					released = append(released, item)
				}
			})
		}
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	if len(clientIds) == 0 {
		return 0
	}

	// Redis first: every delete there is idempotent, so a run that stops
	// after it repeats it for the same clients, which are still present
	removeReapedClientRedisState(ctx, clientIds)
	forEachReapedClientRedisChunk(clientIds, func(chunk []server.Id) {
		server.Redis(ctx, func(r server.RedisClient) {
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, clientId := range chunk {
					pipe.Del(ctx, residentKey(clientId))
				}
				return nil
			})
			server.Raise(err)
		})
	})
	if 0 < len(proxyIds) {
		server.Redis(ctx, func(r server.RedisClient) {
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, proxyId := range proxyIds {
					pipe.Del(ctx, proxyDeviceConfigKey(proxyId))
				}
				return nil
			})
			server.Raise(err)
		})
		clearReleasedProxyEgress(ctx, released)
	}

	// the history rows, a bounded batch of every table per pass
	clientIdStrings := idStrings(clientIds)
	historyStatements := []*deletedNetworkStatement{}
	for _, table := range deletedClientHistoryTables {
		historyStatements = append(historyStatements, &deletedNetworkStatement{
			table:   table.table,
			sql:     table.deleteSql(),
			args:    []any{clientIdStrings, rowBatchCount},
			limited: true,
		})
	}
	for {
		full := false
		tableCounts := map[string]int64{}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tableCounts = map[string]int64{}
			full = execDeletedNetworkStatementsInTx(ctx, tx, historyStatements, rowBatchCount, tableCounts)
		}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
		for table, count := range tableCounts {
			cleanupResult.add(table, count)
		}
		if !full {
			break
		}
	}

	// the device audit rows the network wrote for the batch's devices, which
	// go while the clients still name them
	if 0 < len(deviceIds) {
		removeDeletedNetworkDeviceRows(ctx, networkId, deviceIds, settings, cleanupResult)
	}

	// the clients and their remaining rows, together
	tableCounts := map[string]int64{}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		removedCount = 0
		tableCounts = map[string]int64{}
		if deletedNetworkExistsInTx(ctx, tx, networkId) {
			return
		}
		// relock: only clients still inactive, due and of the network go
		lockedClientIds := []server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT client_id
				FROM network_client
				WHERE
					client_id = ANY($1) AND
					network_id = $2 AND
					NOT active AND
					COALESCE(deactivate_time, create_time) < $3
				ORDER BY client_id
				FOR UPDATE SKIP LOCKED
			`,
			clientIdStrings,
			networkId,
			dueTime,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				lockedClientIds = append(lockedClientIds, clientId)
			}
		})
		if len(lockedClientIds) == 0 {
			return
		}
		lockedIdStrings := idStrings(lockedClientIds)

		// the provider work journal's endpoint fences, before the connection
		// rows its trigger journals (as the prober shard reap)
		providerWorkLockCurrentSessionMutationInTx(ctx, tx, lockedClientIds...)

		statements := []*deletedNetworkStatement{
			{
				table: "network_client_connection",
				sql: `
					WITH connection AS MATERIALIZED (
						SELECT connection_id
						FROM network_client_connection
						WHERE client_id = ANY($1)
					), deleted_location AS (
						DELETE FROM network_client_location
						WHERE connection_id IN (SELECT connection_id FROM connection)
					), deleted_latency AS (
						DELETE FROM network_client_latency
						WHERE connection_id IN (SELECT connection_id FROM connection)
					), deleted_speed AS (
						DELETE FROM network_client_speed
						WHERE connection_id IN (SELECT connection_id FROM connection)
					)
					DELETE FROM network_client_connection
					WHERE connection_id IN (SELECT connection_id FROM connection)
				`,
				args: []any{lockedIdStrings},
			},
		}
		for _, table := range deletedClientTables {
			statements = append(statements, &deletedNetworkStatement{
				table: table,
				sql:   fmt.Sprintf(`DELETE FROM %s WHERE client_id = ANY($1)`, table),
				args:  []any{lockedIdStrings},
			})
		}
		// a hosted proxy's host rows, its configuration and its unique exit;
		// the host rows first, while the configuration names them
		statements = append(
			statements,
			&deletedNetworkStatement{
				table: "proxy_client_change",
				sql: `
					DELETE FROM proxy_client_change
					WHERE proxy_id IN (
						SELECT proxy_id FROM proxy_device_config WHERE client_id = ANY($1)
					)
				`,
				args: []any{lockedIdStrings},
			},
			&deletedNetworkStatement{
				table: "proxy_client",
				sql: `
					DELETE FROM proxy_client
					WHERE proxy_id IN (
						SELECT proxy_id FROM proxy_device_config WHERE client_id = ANY($1)
					)
				`,
				args: []any{lockedIdStrings},
			},
			&deletedNetworkStatement{
				table: "proxy_device_config",
				sql:   `DELETE FROM proxy_device_config WHERE client_id = ANY($1)`,
				args:  []any{lockedIdStrings},
			},
			&deletedNetworkStatement{
				table: "proxy_unique_exit",
				sql:   `DELETE FROM proxy_unique_exit WHERE proxy_client_id = ANY($1)`,
				args:  []any{lockedIdStrings},
			},
			// every state, through the live and released indexes
			&deletedNetworkStatement{
				table: "proxy_exit_assignment",
				sql: `
					DELETE FROM proxy_exit_assignment
					WHERE
						proxy_client_id = ANY($1) AND
						state IN ('pending', 'active', 'replacing', 'released')
				`,
				args: []any{lockedIdStrings},
			},
			// the clients; the delete trigger records each signed key
			// history's client as gone, which starts its retention
			&deletedNetworkStatement{
				table: "network_client",
				sql:   `DELETE FROM network_client WHERE client_id = ANY($1)`,
				args:  []any{lockedIdStrings},
			},
		)
		execDeletedNetworkStatementsInTx(ctx, tx, statements, 0, tableCounts)
		removedCount = tableCounts["network_client"]
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	for table, count := range tableCounts {
		cleanupResult.add(table, count)
	}
	// a client another transaction held stays for the next run
	return removedCount
}

// The audit rows the network wrote for the devices, in bounded batches.
func removeDeletedNetworkDeviceRows(
	ctx context.Context,
	networkId server.Id,
	deviceIds []server.Id,
	settings *DeletedNetworkCleanupSettings,
	cleanupResult *DeletedNetworkCleanupResult,
) {
	rowBatchCount := settings.rowBatchCount()
	deviceIdStrings := idStrings(deviceIds)
	statements := []*deletedNetworkStatement{}
	for _, table := range deletedDeviceTables {
		statements = append(statements, &deletedNetworkStatement{
			table:   table.table,
			sql:     table.deleteSql(),
			args:    []any{deviceIdStrings, rowBatchCount, networkId},
			limited: true,
		})
	}
	for {
		full := false
		tableCounts := map[string]int64{}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tableCounts = map[string]int64{}
			full = execDeletedNetworkStatementsInTx(ctx, tx, statements, rowBatchCount, tableCounts)
		}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
		for table, count := range tableCounts {
			cleanupResult.add(table, count)
		}
		if !full {
			break
		}
	}
}

// Deletes one batch of the network's devices that no client names, with
// their audit rows first. Returns how many devices it read, so a short batch
// ends the loop.
func removeDeletedNetworkDevices(
	ctx context.Context,
	networkId server.Id,
	settings *DeletedNetworkCleanupSettings,
	cleanupResult *DeletedNetworkCleanupResult,
) (readCount int64) {
	deviceIds := []server.Id{}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		deviceIds = []server.Id{}
		if deletedNetworkExistsInTx(ctx, tx, networkId) {
			return
		}
		result, err := tx.Query(
			ctx,
			`
				SELECT device.device_id
				FROM device
				WHERE
					device.network_id = $1 AND
					NOT EXISTS (
						SELECT 1 FROM network_client
						WHERE network_client.device_id = device.device_id
					)
				ORDER BY device.device_id
				LIMIT $2
			`,
			networkId,
			settings.clientBatchCount(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var deviceId server.Id
				server.Raise(result.Scan(&deviceId))
				deviceIds = append(deviceIds, deviceId)
			}
		})
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	if len(deviceIds) == 0 {
		return 0
	}
	removeDeletedNetworkDeviceRows(ctx, networkId, deviceIds, settings, cleanupResult)
	var deviceCount int64
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		deviceCount = 0
		if deletedNetworkExistsInTx(ctx, tx, networkId) {
			return
		}
		deviceCount = server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM device
				WHERE
					device_id = ANY($1) AND
					network_id = $2 AND
					NOT EXISTS (
						SELECT 1 FROM network_client
						WHERE network_client.device_id = device.device_id
					)
			`,
			idStrings(deviceIds),
			networkId,
		)).RowsAffected()
	}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout))
	cleanupResult.add("device", deviceCount)
	return int64(len(deviceIds))
}

// What a deleted network has left, by table: the rows the cleanup would
// delete, and its clients by state. For the dry run of `bringyourctl network
// cleanup-deleted`; it reads only. now is the clock of the client delay.
type DeletedNetworkData struct {
	RowCounts map[string]int64
	// active clients, which the cleanup deactivates first
	ActiveClientCount int64
	// inactive clients that are due for the hard delete
	DueClientCount int64
	// inactive clients that wait for ClientReapAfterDeactivate
	WaitingClientCount int64
	DeviceCount        int64
	// the counts that ran past CountStatementTimeout, by table (network_client
	// for the three client counts, device for DeviceCount), with the number of
	// network sets they timed out for. The other fields leave those rows out.
	TimedOutCounts map[string]int
}

// A statement that ran past its statement_timeout. A cancel by an operator
// (pg_cancel_backend) is not one: it ends the count with an error.
func isPgStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.QueryCanceled && strings.Contains(pgErr.Message, "statement timeout")
}

// Counts what the deleted networks left, in one read-only transaction per
// network set. Networks that exist count nothing. The client-keyed and
// device-keyed tables count the rows of the networks' clients and devices,
// and the user tables those of the users given and of the users the
// networks' rows name (deletedNetworkUserIds). With CountStatementTimeout set,
// each count runs under its own savepoint and statement timeout, and a count
// past it is reported in TimedOutCounts instead of holding the transaction.
func CountDeletedNetworkData(
	ctx context.Context,
	networkIds []server.Id,
	userIds []server.Id,
	now time.Time,
	settings *DeletedNetworkCleanupSettings,
) *DeletedNetworkData {
	data := &DeletedNetworkData{RowCounts: map[string]int64{}, TimedOutCounts: map[string]int{}}
	dueTime := now.UTC().Add(-settings.ClientReapAfterDeactivate)
	userIds = append(slices.Clone(userIds), deletedNetworkUserIds(ctx, networkIds)...)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		data = &DeletedNetworkData{RowCounts: map[string]int64{}, TimedOutCounts: map[string]int{}}
		goneNetworkIds := []server.Id{}
		for _, networkId := range networkIds {
			if !deletedNetworkExistsInTx(ctx, tx, networkId) {
				goneNetworkIds = append(goneNetworkIds, networkId)
			}
		}
		goneUserIds := deletedUserIdsInTx(ctx, tx, userIds)
		networkIdStrings := idStrings(goneNetworkIds)
		bounded := 0 < settings.CountStatementTimeout
		if bounded {
			// the counts below only; the existence checks above stay unbounded
			_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", max(settings.CountStatementTimeout.Milliseconds(), 1)))
			server.Raise(err)
		}
		// Runs one count. When the counts are bounded it runs under its own
		// savepoint: a count past the timeout rolls back to the savepoint, is
		// recorded in TimedOutCounts and the next count runs.
		scan := func(table string, sql string, args []any, dest ...any) bool {
			if !bounded {
				server.Raise(tx.QueryRow(ctx, sql, args...).Scan(dest...))
				return true
			}
			savepoint, err := tx.Begin(ctx)
			server.Raise(err)
			if err := savepoint.QueryRow(ctx, sql, args...).Scan(dest...); err != nil {
				if !isPgStatementTimeout(err) {
					server.Raise(err)
				}
				server.Raise(savepoint.Rollback(ctx))
				data.TimedOutCounts[table] += 1
				return false
			}
			server.Raise(savepoint.Commit(ctx))
			return true
		}
		count := func(table string, sql string, args ...any) {
			var n int64
			if scan(table, sql, args, &n) && 0 < n {
				data.RowCounts[table] += n
			}
		}
		for _, table := range deletedNetworkTables {
			count(table.table, table.countSql(), networkIdStrings)
		}
		for _, table := range deletedUserTables {
			count(table.table, table.countSql(), idStrings(goneUserIds))
		}
		count("auth_code", `SELECT count(*) FROM auth_code WHERE network_id = ANY($1) OR user_id = ANY($2)`, networkIdStrings, idStrings(goneUserIds))
		count("network_points_leaderboard", `SELECT count(*) FROM network_points_leaderboard WHERE network_id = ANY($1) AND snapshot_id IN (SELECT snapshot_id FROM network_points_leaderboard_snapshot)`, networkIdStrings)

		scan(
			"network_client",
			`
				SELECT
					count(*) FILTER (WHERE active),
					count(*) FILTER (WHERE NOT active AND COALESCE(deactivate_time, create_time) < $2),
					count(*) FILTER (WHERE NOT active AND $2 <= COALESCE(deactivate_time, create_time))
				FROM network_client
				WHERE network_id = ANY($1)
			`,
			[]any{networkIdStrings, dueTime},
			&data.ActiveClientCount, &data.DueClientCount, &data.WaitingClientCount,
		)
		scan(
			"device",
			`SELECT count(*) FROM device WHERE network_id = ANY($1)`,
			[]any{networkIdStrings},
			&data.DeviceCount,
		)

		clientIdsSql := `SELECT client_id FROM network_client WHERE network_id = ANY($1)`
		for _, table := range deletedClientHistoryTables {
			count(table.table, fmt.Sprintf(`SELECT count(*) FROM %s WHERE client_id IN (%s)`, table.table, clientIdsSql), networkIdStrings)
		}
		for _, table := range deletedClientTables {
			count(table, fmt.Sprintf(`SELECT count(*) FROM %s WHERE client_id IN (%s)`, table, clientIdsSql), networkIdStrings)
		}
		count("network_client_connection", fmt.Sprintf(`SELECT count(*) FROM network_client_connection WHERE client_id IN (%s)`, clientIdsSql), networkIdStrings)
		count("proxy_device_config", fmt.Sprintf(`SELECT count(*) FROM proxy_device_config WHERE client_id IN (%s)`, clientIdsSql), networkIdStrings)
		count("proxy_unique_exit", fmt.Sprintf(`SELECT count(*) FROM proxy_unique_exit WHERE proxy_client_id IN (%s)`, clientIdsSql), networkIdStrings)
		deviceIdsSql := `SELECT device_id FROM device WHERE network_id = ANY($1) UNION SELECT device_id FROM network_client WHERE network_id = ANY($1) AND device_id IS NOT NULL`
		for _, table := range deletedDeviceTables {
			count(table.table, fmt.Sprintf(`SELECT count(*) FROM %s WHERE device_id IN (%s) AND network_id = ANY($1)`, table.table, deviceIdsSql), networkIdStrings)
		}
	}, server.TxReadCommitted, server.OptReadOnly(), server.TxLockTimeout(settings.LockTimeout))
	return data
}

// Where a discovery of deleted networks resumes: the anchor table and the
// last network id it read there.
type DeletedNetworkDiscoveryCursor struct {
	AnchorIndex    int       `json:"anchor_index"`
	AfterNetworkId server.Id `json:"after_network_id"`
}

// A table whose rows name a network, read for network ids whose network is
// gone. An indexed anchor is read one distinct id at a time through an index
// that leads with its column (a loose index scan); a small one is read whole.
type deletedNetworkAnchor struct {
	table  string
	column string
	small  bool
}

// Every table the cleanup deletes from by network, so a network that left
// anything behind is found. The small tables hold a few thousand rows.
var deletedNetworkAnchors = func() []*deletedNetworkAnchor {
	anchors := []*deletedNetworkAnchor{
		{table: "network_client", column: "network_id"},
		{table: "device", column: "network_id"},
	}
	for _, table := range deletedNetworkTables {
		small := false
		switch table.table {
		case "auth_code_redemption",
			"account_message_outbox",
			"network_onboarding_apple_offer_code",
			"bulk_client_removal_quota",
			"device_association_name",
			"lag_ticket",
			"lag_track",
			"oauth_authorization_code",
			"oauth_consent",
			"oauth_refresh_token":
			small = true
		}
		anchors = append(anchors, &deletedNetworkAnchor{
			table:  table.table,
			column: table.keyColumn,
			small:  small,
		})
	}
	anchors = append(anchors, &deletedNetworkAnchor{table: "auth_code", column: "network_id", small: true})
	return anchors
}()

// The anchors' names, in discovery order, for reports.
func DeletedNetworkDiscoveryAnchors() []string {
	names := []string{}
	for _, anchor := range deletedNetworkAnchors {
		names = append(names, anchor.table+"."+anchor.column)
	}
	return names
}

func (self *deletedNetworkAnchor) loosePageSql() string {
	return fmt.Sprintf(
		`
			WITH RECURSIVE scan(network_id, n) AS (
				(
					SELECT %[1]s.%[2]s, 1
					FROM %[1]s
					WHERE %[1]s.%[2]s > $1
					ORDER BY %[1]s.%[2]s
					LIMIT 1
				)
				UNION ALL
				SELECT
					(
						SELECT %[1]s.%[2]s
						FROM %[1]s
						WHERE %[1]s.%[2]s > scan.network_id
						ORDER BY %[1]s.%[2]s
						LIMIT 1
					),
					scan.n + 1
				FROM scan
				WHERE scan.network_id IS NOT NULL AND scan.n < $2
			)
			SELECT
				scan.network_id,
				NOT EXISTS (
					SELECT 1 FROM network
					WHERE network.network_id = scan.network_id
				)
			FROM scan
			WHERE scan.network_id IS NOT NULL
			ORDER BY scan.network_id
		`,
		self.table,
		self.column,
	)
}

func (self *deletedNetworkAnchor) smallSql() string {
	return fmt.Sprintf(
		`
			SELECT DISTINCT %[1]s.%[2]s
			FROM %[1]s
			WHERE
				%[1]s.%[2]s IS NOT NULL AND
				NOT EXISTS (
					SELECT 1 FROM network
					WHERE network.network_id = %[1]s.%[2]s
				)
		`,
		self.table,
		self.column,
	)
}

// Reads one page of deleted network ids: at most pageCount distinct network
// ids of the cursor's anchor table, of which it returns those whose network
// is gone. done is true once the last anchor is read to its end. Each page is
// one short read-only statement.
func FindDeletedNetworkIds(
	ctx context.Context,
	cursor DeletedNetworkDiscoveryCursor,
	pageCount int,
) (deletedNetworkIds []server.Id, next DeletedNetworkDiscoveryCursor, done bool) {
	next = cursor
	deletedNetworkIds = []server.Id{}
	if len(deletedNetworkAnchors) <= cursor.AnchorIndex {
		return deletedNetworkIds, next, true
	}
	anchor := deletedNetworkAnchors[cursor.AnchorIndex]
	pageCount = max(pageCount, 1)
	endOfAnchor := false
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		deletedNetworkIds = []server.Id{}
		next = cursor
		endOfAnchor = false
		if anchor.small {
			result, err := tx.Query(ctx, anchor.smallSql())
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var networkId server.Id
					server.Raise(result.Scan(&networkId))
					deletedNetworkIds = append(deletedNetworkIds, networkId)
				}
			})
			endOfAnchor = true
			return
		}
		readCount := 0
		result, err := tx.Query(ctx, anchor.loosePageSql(), cursor.AfterNetworkId, pageCount)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var gone bool
				server.Raise(result.Scan(&networkId, &gone))
				readCount += 1
				next.AfterNetworkId = networkId
				if gone {
					deletedNetworkIds = append(deletedNetworkIds, networkId)
				}
			}
		})
		endOfAnchor = readCount < pageCount
	}, server.TxReadCommitted, server.OptReadOnly())
	if endOfAnchor {
		next = DeletedNetworkDiscoveryCursor{AnchorIndex: cursor.AnchorIndex + 1}
	}
	return deletedNetworkIds, next, len(deletedNetworkAnchors) <= next.AnchorIndex
}

// Every deleted network id of a full discovery, in id order. For tests and a
// small database; the batch tool pages with FindDeletedNetworkIds.
func Testing_FindAllDeletedNetworkIds(ctx context.Context, pageCount int) []server.Id {
	networkIds := map[server.Id]bool{}
	cursor := DeletedNetworkDiscoveryCursor{}
	for {
		page, next, done := FindDeletedNetworkIds(ctx, cursor, pageCount)
		for _, networkId := range page {
			networkIds[networkId] = true
		}
		cursor = next
		if done {
			break
		}
	}
	sortedNetworkIds := slices.Collect(maps.Keys(networkIds))
	slices.SortFunc(sortedNetworkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return sortedNetworkIds
}
