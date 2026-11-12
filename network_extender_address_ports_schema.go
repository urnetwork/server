package server

// The carrier ports of each extender address (connect/EXTENDER.md S10): a
// router's mapping forwards one family's carriers on ports it picks, while the
// extender's other family answers on the bound ones, so each address keeps the
// tcp and udp ports its own activation was probed on, beside the dns ports it
// already keeps. Nullable columns with no default: the migration rewrites
// nothing, the running release, which names the columns it writes, writes on,
// and an address written before the migration or by that release reads null,
// which is the extender's ports, as every address had them before. The short
// lock wait is so the alter never queues against the activations and the
// uptime probes: one that cannot have the table within it fails and is run
// again.
const networkExtenderAddressPortsSchemaSql = `
		SET LOCAL lock_timeout = '2s';
		ALTER TABLE network_extender_address
			ADD COLUMN tcp_port int NULL,
			ADD COLUMN udp_port int NULL;
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func NetworkExtenderAddressPortsSchemaMigrationVersion() int {
	return sqlMigrationVersion(networkExtenderAddressPortsSchemaSql)
}
