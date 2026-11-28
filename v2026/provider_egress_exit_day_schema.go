package server

// The record of a provider judged by a probed exit that differs from every
// network it connects from: one row per client and UTC day of the probes'
// observations, counting the differing judgements of that day and those among
// them with no connection in the exit's address family. It holds no address.
//
// Unused since 2026-11-21. The owner decided a proxied or bridged address
// counts the same as an owned one, so judging a provider by its probed exit
// and the SN25 exit rule that read this record were reverted. Migrations are
// append-only and Main applied this one, so the table, its migration and the
// monitor's contract stay; nothing reads or writes it. The rows written before
// the revert stay too, kept on account deletion as every row is.
const providerEgressExitDaySchemaSql = `
	CREATE TABLE IF NOT EXISTS provider_egress_exit_day (
		day date NOT NULL,
		client_id uuid NOT NULL,
		network_id uuid NOT NULL,
		differs_count integer NOT NULL,
		cross_family_count integer NOT NULL,
		first_observed_at timestamp NOT NULL,
		last_observed_at timestamp NOT NULL,
		PRIMARY KEY (day, client_id)
	)
`

// The position of the migration that creates the record, for the monitor's
// contract.
func ProviderEgressExitDayMigrationVersion() int {
	return sqlMigrationVersion(providerEgressExitDaySchemaSql)
}
