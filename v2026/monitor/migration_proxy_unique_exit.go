package monitor

import (
	"fmt"

	"github.com/urnetwork/server/v2026"
)

// The unique-exit hosted proxies' schema (server db_proxy_unique_exit_schema.go,
// HOSTED-PROXY-UNIQUE-EXIT.md §13.2): eleven additive migrations, each
// versioned by its position in the migration list.
var proxyUniqueExitMigrationContracts = proxyUniqueExitMigrationContractsFor(server.ProxyUniqueExitMigrationVersion())

const (
	// state IN ('pending', 'active'), as the catalog renders it
	proxyExitAssignmentLiveStates = "((state)::text = ANY ((ARRAY['pending'::character varying, 'active'::character varying])::text[]))"
	// state IN ('pending', 'active', 'replacing')
	proxyExitAssignmentUnreleasedStates = "((state)::text = ANY ((ARRAY['pending'::character varying, 'active'::character varying, 'replacing'::character varying])::text[]))"
	// state = 'released'
	proxyExitAssignmentReleasedState = "((state)::text = 'released'::text)"
)

// A unique partial index is pinned by its complete definition: uniqueness
// over a different predicate would hold different rows apart.
func proxyUniqueExitUniquePartialIndex(table, name, columns, predicate string) string {
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_index i WHERE i.indrelid=to_regclass('public.%s')
 AND i.indexrelid=to_regclass('public.%s') AND i.indisvalid AND i.indisready AND i.indisunique
 AND pg_get_indexdef(i.indexrelid)=%s)`,
		table, name, snMainnetMigrationLiteral("CREATE UNIQUE INDEX "+name+" ON public."+table+" USING btree ("+columns+") WHERE "+predicate))
}

func proxyUniqueExitMigrationContractsFor(v server.ProxyUniqueExitMigrationVersions) []snMainnetMigrationContract {
	return []snMainnetMigrationContract{
		migrationCurrentSuffix(v.EgressKeyColumns, "unique-exit keyed egress on provider egress locations",
			snMainnetMigrationColumns("provider_egress_location",
				snMainnetMigrationColumn{name: "egress_key", kind: "bytea"},
				snMainnetMigrationColumn{name: "egress_key_observed_at", kind: "timestamp without time zone"},
				snMainnetMigrationColumn{name: "egress_key_stable_count", kind: "integer", notNull: true, defaultExpression: "0"})),
		migrationCurrentSuffix(v.UniqueExitTable, "unique-exit proxy configs",
			migrationFp2Table("proxy_unique_exit"),
			snMainnetMigrationColumns("proxy_unique_exit",
				snMainnetMigrationColumn{name: "proxy_client_id", kind: "uuid", notNull: true},
				snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
				snMainnetMigrationColumn{name: "replace_mode", kind: "character varying(8)", notNull: true},
				snMainnetMigrationColumn{name: "specs_json", kind: "text", notNull: true},
				snMainnetMigrationColumn{name: "state", kind: "character varying(16)", notNull: true},
				snMainnetMigrationColumn{name: "state_reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
				snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true},
				snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
			snMainnetMigrationConstraints("proxy_unique_exit", "PRIMARY KEY (proxy_client_id)")),
		migrationCurrentSuffix(v.UniqueExitNetworkIndex, "unique-exit proxy configs by network",
			financialIndexArtifact("proxy_unique_exit", "proxy_unique_exit_network_id", "network_id")),
		migrationCurrentSuffix(v.AssignmentTable, "unique-exit assignments",
			migrationFp2Table("proxy_exit_assignment"),
			snMainnetMigrationColumns("proxy_exit_assignment",
				snMainnetMigrationColumn{name: "assignment_id", kind: "uuid", notNull: true},
				snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
				snMainnetMigrationColumn{name: "proxy_client_id", kind: "uuid", notNull: true},
				snMainnetMigrationColumn{name: "provider_client_id", kind: "uuid", notNull: true},
				snMainnetMigrationColumn{name: "egress_key", kind: "bytea", notNull: true},
				snMainnetMigrationColumn{name: "state", kind: "character varying(16)", notNull: true},
				snMainnetMigrationColumn{name: "location_id", kind: "uuid"},
				snMainnetMigrationColumn{name: "country_code", kind: "character varying(2)", notNull: true, defaultExpression: "''::character varying"},
				snMainnetMigrationColumn{name: "estimated_bytes_per_second", kind: "bigint", notNull: true, defaultExpression: "0"},
				snMainnetMigrationColumn{name: "assigned_at", kind: "timestamp without time zone", notNull: true},
				snMainnetMigrationColumn{name: "confirmed_at", kind: "timestamp without time zone"},
				snMainnetMigrationColumn{name: "failing_since", kind: "timestamp without time zone"},
				snMainnetMigrationColumn{name: "failing_reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
				snMainnetMigrationColumn{name: "released_at", kind: "timestamp without time zone"},
				snMainnetMigrationColumn{name: "release_reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"}),
			snMainnetMigrationConstraints("proxy_exit_assignment", "PRIMARY KEY (assignment_id)")),
		migrationCurrentSuffix(v.AssignmentNetworkEgressKeyLiveIndex, "one live unique exit per network and egress key",
			proxyUniqueExitUniquePartialIndex("proxy_exit_assignment", "proxy_exit_assignment_network_egress_key_live",
				"network_id, egress_key", proxyExitAssignmentLiveStates)),
		migrationCurrentSuffix(v.AssignmentProxyLiveIndex, "one unreleased assignment per unique-exit proxy",
			proxyUniqueExitUniquePartialIndex("proxy_exit_assignment", "proxy_exit_assignment_proxy_client_id_live",
				"proxy_client_id", proxyExitAssignmentUnreleasedStates)),
		migrationCurrentSuffix(v.AssignmentProviderLiveIndex, "live unique-exit pins by provider",
			snMainnetMigrationPartialIndex("proxy_exit_assignment", "proxy_exit_assignment_provider_client_id_live",
				"provider_client_id", proxyExitAssignmentLiveStates)),
		migrationCurrentSuffix(v.AssignmentAssignedAtLiveIndex, "unreleased unique-exit assignments by assignment time",
			snMainnetMigrationPartialIndex("proxy_exit_assignment", "proxy_exit_assignment_assigned_at_live",
				"assigned_at", proxyExitAssignmentUnreleasedStates)),
		migrationCurrentSuffix(v.AssignmentProxyReleasedIndex, "released unique-exit assignments by proxy",
			snMainnetMigrationPartialIndex("proxy_exit_assignment", "proxy_exit_assignment_proxy_client_id_released",
				"proxy_client_id, released_at", proxyExitAssignmentReleasedState)),
		migrationCurrentSuffix(v.AssignmentNetworkReleasedIndex, "released unique-exit assignments by network",
			snMainnetMigrationPartialIndex("proxy_exit_assignment", "proxy_exit_assignment_network_id_released",
				"network_id, released_at", proxyExitAssignmentReleasedState)),
		migrationCurrentSuffix(v.AssignmentReleasedAtIndex, "released unique-exit assignments by release time",
			snMainnetMigrationPartialIndex("proxy_exit_assignment", "proxy_exit_assignment_released_at",
				"released_at", proxyExitAssignmentReleasedState)),
	}
}
