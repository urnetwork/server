package server

// The durable record of a provider judged by a probed exit that differs from
// every network it connects from (model/provider_egress_exit_model.go): one
// row per client and UTC day of the probes' observations, counting the
// differing judgements of that day and those among them with no connection in
// the exit's address family. The judgement itself lives in Redis only while
// its probe is fresh, so a settlement epoch reads this after the epoch. It
// holds no address. Rows are kept on account deletion as settlement evidence.
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
