// Catalog readiness for completed-run priority is separate from live delivery.
package monitor

// Pacing and eligibility writes must invalidate the materialized ready set.
// Match the exact body and binding: a same-name no-op, disabled trigger, or
// conditional/column-incomplete trigger cannot preserve scheduler correctness.
const migrationUrlCompletionReadyTrigger = `EXISTS (
	SELECT 1 FROM pg_trigger AS trigger_record
	JOIN pg_proc AS function_record ON function_record.oid=trigger_record.tgfoid
	JOIN pg_namespace AS namespace ON namespace.oid=function_record.pronamespace
	JOIN pg_language AS language ON language.oid=function_record.prolang
	WHERE trigger_record.tgrelid=to_regclass('public.provider_egress_probe_cycle')
	AND trigger_record.tgname='provider_url_probe_ready_invalidate'
	AND NOT trigger_record.tgisinternal AND trigger_record.tgtype=19
	AND trigger_record.tgenabled IN ('O','A') AND trigger_record.tgqual IS NULL
	AND trigger_record.tgnargs=0 AND trigger_record.tgargs=''::bytea
	AND ARRAY(SELECT attribute.attname::text
		FROM unnest(trigger_record.tgattr) AS updated(attnum)
		JOIN pg_attribute AS attribute ON attribute.attrelid=trigger_record.tgrelid
		AND attribute.attnum=updated.attnum ORDER BY attribute.attname)
		=ARRAY['eligible','next_attempt_at']::text[]
	AND namespace.nspname='public' AND function_record.proname='provider_url_probe_ready_invalidate'
	AND function_record.pronargs=0 AND function_record.prorettype='trigger'::regtype
	AND function_record.prokind='f' AND function_record.provolatile='v'
	AND NOT function_record.prosecdef AND NOT function_record.proisstrict
	AND function_record.proconfig IS NULL AND language.lanname='plpgsql'
	AND btrim(regexp_replace(function_record.prosrc,'[[:space:]]+',' ','g'))=
		btrim(regexp_replace($ready_body$
			BEGIN
				IF NEW.next_attempt_at IS DISTINCT FROM OLD.next_attempt_at OR NOT NEW.eligible THEN
					NEW.completed_priority_ready := false;
				END IF;
				RETURN NEW;
			END
		$ready_body$,'[[:space:]]+',' ','g'))
)`

// Positional expressions for versions 732 through 739. Catalog-only lookups
// remain queryable before any future relation, trigger, or index exists.
var migrationUrlCompletionArtifactQueries = []string{
	"(" + migrationFp2Columns("provider_egress_probe_cycle",
		migrationFp2Column{name: "claim_ordinal", kind: "bigint", notNull: true, defaultExpression: "0"},
		migrationFp2Column{name: "completed_run_count", kind: "bigint", notNull: true, defaultExpression: "0"},
		migrationFp2Column{name: "completed_next_expiry_at", kind: "timestamp without time zone"},
		migrationFp2Column{name: "completed_priority_ready", kind: "boolean", notNull: true, defaultExpression: "false"}) + " AND " +
		migrationFp2Constraint("provider_egress_probe_cycle", "c", "CHECK ((claim_ordinal >= 0))") + " AND " +
		migrationFp2Constraint("provider_egress_probe_cycle", "c", "CHECK ((completed_run_count >= 0))") + " AND " +
		migrationFp2Table("provider_url_probe_run") + " AND " +
		migrationFp2Columns("provider_url_probe_run",
			migrationFp2Column{name: "client_id", kind: "uuid", notNull: true},
			migrationFp2Column{name: "claim_ordinal", kind: "bigint", notNull: true},
			migrationFp2Column{name: "claimed_at", kind: "timestamp without time zone", notNull: true},
			migrationFp2Column{name: "reported_completed_at", kind: "timestamp without time zone"},
			migrationFp2Column{name: "completed_at", kind: "timestamp without time zone"},
			migrationFp2Column{name: "received_at", kind: "timestamp without time zone"},
			migrationFp2Column{name: "probe_failure", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
			migrationFp2Column{name: "counted", kind: "boolean", notNull: true, defaultExpression: "false"}) + " AND " +
		migrationFp2Constraint("provider_url_probe_run", "p", "PRIMARY KEY (client_id, claim_ordinal)") + " AND " +
		migrationFp2Constraint("provider_url_probe_run", "c", "CHECK ((claim_ordinal > 0))") + " AND " +
		migrationFp2Constraint("provider_url_probe_run", "c", "CHECK (((completed_at IS NULL) = (received_at IS NULL)))") + " AND " +
		migrationFp2Constraint("provider_url_probe_run", "c", "CHECK (((completed_at IS NULL) OR ((claimed_at <= completed_at) AND (completed_at <= received_at))))") + " AND " +
		migrationFp2Constraint("provider_url_probe_run", "c", "CHECK (((NOT counted) OR (completed_at IS NOT NULL)))") + " AND " +
		migrationFp2Index("provider_url_probe_run", "provider_url_probe_run_pkey", "client_id, claim_ordinal", "", true) + " AND " +
		migrationFp2Index("provider_url_probe_run", "provider_url_probe_run_active", "client_id, completed_at, claim_ordinal", "counted", false) + " AND " +
		migrationFp2Index("provider_url_probe_run", "provider_url_probe_run_retention", "claimed_at, client_id, claim_ordinal", "(NOT counted)", false) + " AND " +
		migrationUrlCompletionReadyTrigger + ")",
	// Ready rows sort by exact rolling count, then due clock and stable ID.
	migrationFp2Index("provider_egress_probe_cycle", "provider_probe_cycle_completed_ready",
		"completed_run_count, next_attempt_at, client_id", "(eligible AND completed_priority_ready)", false),
	migrationFp2Index("provider_egress_probe_cycle", "provider_probe_cycle_slot_completed_ready",
		"slot_id, completed_run_count, next_attempt_at, client_id", "(eligible AND completed_priority_ready)", false),
	// Waiting and expiry heads are ordered independently of completed count.
	migrationFp2Index("provider_egress_probe_cycle", "provider_probe_cycle_completed_waiting",
		"next_attempt_at, client_id", "(eligible AND (NOT completed_priority_ready))", false),
	migrationFp2Index("provider_egress_probe_cycle", "provider_probe_cycle_slot_completed_waiting",
		"slot_id, next_attempt_at, client_id", "(eligible AND (NOT completed_priority_ready))", false),
	migrationFp2Index("provider_egress_probe_cycle", "provider_probe_cycle_completed_expiry",
		"completed_next_expiry_at, client_id", "(completed_next_expiry_at IS NOT NULL)", false),
	migrationFp2Index("provider_egress_probe_cycle", "provider_probe_cycle_slot_completed_expiry",
		"slot_id, completed_next_expiry_at, client_id", "(completed_next_expiry_at IS NOT NULL)", false),
	migrationFp2Index("transfer_balance", "transfer_balance_active_network_end_start_id",
		"network_id, end_time, start_time, balance_id", "active", false),
}
