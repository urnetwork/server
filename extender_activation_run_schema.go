package server

// Which run of the extender process sent each activation (connect/EXTENDER.md
// G3): the run's id, when the process started, and how the run before it
// ended when its host said so, on the activation history row. A restart is
// then a new run id among an extender's activations. Nullable columns with no
// default: the migration rewrites nothing, the running release, which names
// the columns it writes, writes on, and a row written before the migration or
// by an extender that predates the run reads null. The short lock wait is so
// the alter never queues against the activation path: one that cannot have the
// table within it fails and is run again.
const extenderActivationRunSchemaSql = `
		SET LOCAL lock_timeout = '2s';
		ALTER TABLE network_extender_activation
			ADD COLUMN run_id varchar(32) NULL,
			ADD COLUMN run_start_time timestamp NULL,
			ADD COLUMN previous_exit varchar(16) NULL;
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func ExtenderActivationRunSchemaMigrationVersion() int {
	return sqlMigrationVersion(extenderActivationRunSchemaSql)
}
