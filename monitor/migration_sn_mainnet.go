// Mainnet evidence custody is checked independently of the numeric migration
// head. Every predicate reads catalogs only, including before its tables exist.
package monitor

import (
	"fmt"
	"strings"
)

// One ordered source supplies the positional row contract and its SQL.
type snMainnetMigrationContract struct {
	artifact migrationArtifact
	query    string
}

// Columns pin typmods, defaults, nullability and ordinary stored semantics.
type snMainnetMigrationColumn struct {
	name, kind, defaultExpression string
	notNull                       bool
}

// Preserve the published 614-through-head positional protocol.
func snMainnetMigrationArtifacts() []migrationArtifact {
	artifacts := make([]migrationArtifact, 0, len(snMainnetMigrationContracts))
	for _, contract := range snMainnetMigrationContracts {
		artifacts = append(artifacts, contract.artifact)
	}
	return artifacts
}

// Append after v769's expression, before the original FROM version clause.
func snMainnetMigrationArtifactQueries() string {
	queries := make([]string, 0, len(snMainnetMigrationContracts))
	for _, contract := range snMainnetMigrationContracts {
		queries = append(queries, contract.query)
	}
	return ",\n" + strings.Join(queries, ",\n")
}

// Versions and labels are source literals, never live SQL inputs.
func snMainnetMigration(version int, name string, predicates ...string) snMainnetMigrationContract {
	return snMainnetMigrationContract{
		artifact: migrationArtifact{name: name, requiredVersion: version, rowColumn: version - 589},
		query:    "(" + strings.Join(predicates, " AND ") + ")",
	}
}

// Quote only source-owned literals, retaining exact function body bytes.
func snMainnetMigrationLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Direct catalog reads do not require expanding the historical column CTE.
func snMainnetMigrationColumns(table string, columns ...snMainnetMigrationColumn) string {
	conditions := make([]string, 0, len(columns))
	for _, column := range columns {
		defaultExpression := "NULL"
		if column.defaultExpression != "" {
			defaultExpression = snMainnetMigrationLiteral(column.defaultExpression)
		}
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
 SELECT 1 FROM pg_attribute a
 LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
 WHERE a.attrelid=to_regclass('public.%s') AND a.attname=%s
 AND a.attnum>0 AND NOT a.attisdropped AND a.attnotnull=%t
 AND a.attgenerated='' AND a.attidentity=''
 AND format_type(a.atttypid,a.atttypmod)=%s
 AND pg_get_expr(d.adbin,d.adrelid) IS NOT DISTINCT FROM %s)`,
			table, snMainnetMigrationLiteral(column.name), column.notNull,
			snMainnetMigrationLiteral(column.kind), defaultExpression))
	}
	return "(" + strings.Join(conditions, " AND ") + ")"
}

// Validated, nondeferrable keys also require their backing indexes to be ready.
func snMainnetMigrationConstraints(table string, definitions ...string) string {
	values := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		values = append(values, "("+snMainnetMigrationLiteral(definition)+")")
	}
	return financialConstraintsArtifact(table, strings.Join(values, ","))
}

// A declared foreign key is insufficient when its internal RI triggers have
// been disabled or changed. These migrations publish only immediate NO ACTION.
func snMainnetMigrationForeignKey(table, referenced, definition string) string {
	return fmt.Sprintf(`(NOT EXISTS (
 SELECT 1 FROM (VALUES
 ('%[1]s','RI_FKey_check_ins',5),('%[1]s','RI_FKey_check_upd',17),
 ('%[2]s','RI_FKey_noaction_del',9),('%[2]s','RI_FKey_noaction_upd',17)
 ) expected(table_name,function_name,trigger_type)
 WHERE NOT EXISTS (
 SELECT 1 FROM pg_constraint k JOIN pg_trigger t ON t.tgconstraint=k.oid
 JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE k.conrelid=to_regclass('public.%[1]s') AND k.contype='f'
 AND pg_get_constraintdef(k.oid)=%[3]s AND k.convalidated
 AND NOT k.condeferrable AND NOT k.condeferred
 AND t.tgrelid=to_regclass('public.'||expected.table_name) AND t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgtype=expected.trigger_type
 AND t.tgqual IS NULL AND t.tgnargs=0 AND NOT t.tgdeferrable AND NOT t.tginitdeferred
 AND n.nspname='pg_catalog' AND p.proname=expected.function_name)))`,
		table, referenced, snMainnetMigrationLiteral(definition))
}

// No new mutable-parent foreign key may erase independently retained originals.
func snMainnetMigrationForeignKeyCensus(table string, definitions ...string) string {
	predicate := ""
	if len(definitions) != 0 {
		values := make([]string, 0, len(definitions))
		for _, definition := range definitions {
			values = append(values, snMainnetMigrationLiteral(definition))
		}
		predicate = " AND pg_get_constraintdef(oid) NOT IN (" + strings.Join(values, ",") + ")"
	}
	return "NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public." + table + "') AND contype='f'" + predicate + ")"
}

// A partial index is pinned by its complete definition, predicate included:
// the same keys over different rows would serve a different read.
func snMainnetMigrationPartialIndex(table, name, columns, predicate string) string {
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_index i WHERE i.indrelid=to_regclass('public.%s')
 AND i.indexrelid=to_regclass('public.%s') AND i.indisvalid AND i.indisready
 AND pg_get_indexdef(i.indexrelid)=%s)`,
		table, name, snMainnetMigrationLiteral("CREATE INDEX "+name+" ON public."+table+" USING btree ("+columns+") WHERE "+predicate))
}

// Operative trigger events, row/statement scope and complete function identity
// remain separate predicates so all functions are inspected exactly once.
func snMainnetMigrationTrigger(table, trigger, function string, mask int) string {
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_trigger t
 WHERE t.tgrelid=to_regclass('public.%s') AND t.tgname=%s
 AND t.tgfoid=to_regprocedure(%s) AND NOT t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgtype=%d AND t.tgqual IS NULL
 AND t.tgnargs=0 AND t.tgattr=''::int2vector
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred)`,
		table, snMainnetMigrationLiteral(trigger), snMainnetMigrationLiteral("public."+function+"()"), mask)
}

// Defaults, privileges, volatility, strictness and the full body are authority;
// a look-alike procedure name cannot replace a wire lock or append-only guard.
func snMainnetMigrationFunction(signature, result, language, volatility string, strict bool, body string, arguments ...string) string {
	argumentNames := "NULL::text[]"
	if len(arguments) != 0 {
		values := make([]string, 0, len(arguments))
		for _, argument := range arguments {
			values = append(values, snMainnetMigrationLiteral(argument))
		}
		argumentNames = "ARRAY[" + strings.Join(values, ",") + "]::text[]"
	}
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_proc p
 WHERE p.oid=to_regprocedure(%s) AND p.prorettype=to_regtype(%s)
 AND p.prolang=(SELECT oid FROM pg_language WHERE lanname=%s)
 AND p.prokind='f' AND p.provolatile=%s AND p.proparallel='u'
 AND NOT p.prosecdef AND NOT p.proleakproof AND p.proisstrict=%t
 AND NOT p.proretset AND p.pronargdefaults=0 AND p.provariadic=0
 AND p.proargmodes IS NULL AND p.proargnames IS NOT DISTINCT FROM %s
 AND p.proconfig IS NULL AND p.prosrc=%s)`,
		snMainnetMigrationLiteral("public."+signature), snMainnetMigrationLiteral(result),
		snMainnetMigrationLiteral(language), snMainnetMigrationLiteral(volatility), strict,
		argumentNames, snMainnetMigrationLiteral(body))
}

// Fresh installation already uses the compatible projection. Historical
// installations may retain their exact old bodies until v779, whose separate
// mandatory predicate admits only the repaired helper and both consumers.
func snMainnetMigrationPriorFunction(original, repaired, projection string) string {
	return "(" + original + " OR (" + projection + " AND " + repaired + "))"
}

// Exact published function bodies, including the supported pre-779 profiles.
const snMainnetVerifyOriginalBody = `
BEGIN RAISE EXCEPTION 'verification originals are append-only'; END
`

const snMainnetProviderOriginalBody = `
BEGIN RAISE EXCEPTION 'provider work originals are append-only'; END
`

const snMainnetWalletOriginalBody = `
BEGIN RAISE EXCEPTION 'wallet mapping originals are append-only'; END
`

const snMainnetVerifyCaptureLegacyBody = `
DECLARE body jsonb;
BEGIN
 body := convert_from(NEW.original_body,'UTF8')::jsonb;
 INSERT INTO verify_original_request_lookup VALUES
 (NEW.trail_id,NEW.previous_depth,(body->'trail'->>'ClientId')::uuid,
 COALESCE(body->'scope','null'::jsonb),decode(body->>'request_message','base64'),decode(body->>'request_signature','base64'));
 RETURN NEW;
END
`

const snMainnetVerifyFenceLegacyBody = `
DECLARE body jsonb; message bytea; signature bytea;
BEGIN
 body := convert_from(NEW.original_body,'UTF8')::jsonb;
 message := decode(body->>'request_message','base64');
 signature := decode(body->>'request_signature','base64');
 PERFORM verify_original_request_wire_lock(message,signature);
 IF EXISTS (SELECT 1 FROM verify_original_request_closed c
  WHERE c.client_id=(body->'trail'->>'ClientId')::uuid
  AND c.scope_json=COALESCE(body->'scope','null'::jsonb)
  AND c.request_message=message AND c.request_signature=signature) THEN
  RAISE EXCEPTION 'verification request permanently closed' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
`

const snMainnetVerifyWireLockBody = `
 SELECT pg_advisory_xact_lock(775,('x'||substr(encode(sha256(message||signature),'hex'),1,8))::bit(32)::int);
`

const snMainnetVerifyReceivedFenceBody = `
BEGIN
 PERFORM verify_original_request_wire_lock(NEW.request_message,NEW.request_signature);
 IF EXISTS (SELECT 1 FROM verify_original_request_lookup r
  WHERE r.client_id=NEW.client_id AND r.scope_json=NEW.scope_json
  AND r.request_message=NEW.request_message AND r.request_signature=NEW.request_signature) THEN
  RAISE EXCEPTION 'verification request already received' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
`

const snMainnetProviderEndpointBody = `
BEGIN
 IF NOT pg_try_advisory_xact_lock(776,('x'||substr(md5(client::text),1,8))::bit(32)::int) THEN
  RAISE EXCEPTION 'provider work endpoint is busy' USING ERRCODE='40001';
 END IF;
END;
`

const snMainnetProviderSessionAppendBody = `
DECLARE next_sequence bigint; owner_network uuid;
BEGIN
 PERFORM provider_work_endpoint_lock(client);
 INSERT INTO provider_work_session_head(client_id,sequence) VALUES(client,1)
 ON CONFLICT(client_id) DO UPDATE SET sequence=provider_work_session_head.sequence+1
 RETURNING sequence INTO next_sequence;
 IF event_kind='retire' THEN
  SELECT network_id INTO owner_network FROM provider_work_session_event
   WHERE client_id=client AND connection_id=connection AND kind='admit' ORDER BY sequence DESC LIMIT 1;
 ELSE
  SELECT network_id INTO owner_network FROM network_client WHERE client_id=client;
 END IF;
 INSERT INTO provider_work_session_event(client_id,sequence,network_id,connection_id,kind,observed_at,extender_id,transaction_id)
 VALUES(client,next_sequence,owner_network,connection,event_kind,clock_timestamp() AT TIME ZONE 'UTC',extender,txid_current());
 RETURN next_sequence;
END;
`

const snMainnetProviderSessionMutationBody = `
BEGIN
 IF TG_OP='UPDATE' AND ROW(OLD.client_id,OLD.connection_id,OLD.connected,OLD.extender_id)
  IS NOT DISTINCT FROM ROW(NEW.client_id,NEW.connection_id,NEW.connected,NEW.extender_id) THEN RETURN NEW; END IF;
 IF TG_OP<>'INSERT' AND OLD.connected THEN
  PERFORM provider_work_session_append(OLD.client_id,OLD.connection_id,'retire',NULL);
 END IF;
 IF TG_OP<>'DELETE' AND NEW.connected THEN
  PERFORM provider_work_session_append(NEW.client_id,NEW.connection_id,'admit',NEW.extender_id);
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END;
`

const snMainnetProviderStatementFenceBody = `
BEGIN
 RETURN NULL;
END;
`

const snMainnetProviderHeadBody = `
BEGIN
 IF TG_OP='UPDATE' AND NEW.client_id=OLD.client_id AND NEW.sequence=OLD.sequence+1 THEN RETURN NEW; END IF;
 RAISE EXCEPTION 'provider work journal birth cannot be reset';
END;
`

const snMainnetVerifyIndexProjectionBody = `
DECLARE
 prefix constant text := $schema${"schema":"urnetwork-verify-original-transition-v1\u0000",$schema$;
 encoded text;
 body jsonb;
BEGIN
 IF octet_length(original) NOT BETWEEN 1 AND 65536 THEN
  RAISE EXCEPTION 'invalid verification original index size' USING ERRCODE='22023';
 END IF;
 encoded := convert_from(original,'UTF8');
 IF left(encoded,length(prefix)) <> prefix THEN
  RAISE EXCEPTION 'invalid verification original schema prefix' USING ERRCODE='22023';
 END IF;
 body := ('{' || substring(encoded FROM length(prefix)+1))::jsonb;
 IF body ? 'schema' THEN
  RAISE EXCEPTION 'duplicate verification original schema' USING ERRCODE='22023';
 END IF;
 RETURN body;
END
`

const snMainnetVerifyCaptureBody = `
DECLARE body jsonb;
BEGIN
 body := verify_original_request_index_body(NEW.original_body);
 INSERT INTO verify_original_request_lookup VALUES
 (NEW.trail_id,NEW.previous_depth,(body->'trail'->>'ClientId')::uuid,
 COALESCE(body->'scope','null'::jsonb),decode(body->>'request_message','base64'),decode(body->>'request_signature','base64'));
 RETURN NEW;
END
`

const snMainnetVerifyFenceBody = `
DECLARE body jsonb; message bytea; signature bytea;
BEGIN
 body := verify_original_request_index_body(NEW.original_body);
 message := decode(body->>'request_message','base64');
 signature := decode(body->>'request_signature','base64');
 PERFORM verify_original_request_wire_lock(message,signature);
 IF EXISTS (SELECT 1 FROM verify_original_request_closed c
  WHERE c.client_id=(body->'trail'->>'ClientId')::uuid
  AND c.scope_json=COALESCE(body->'scope','null'::jsonb)
  AND c.request_message=message AND c.request_signature=signature) THEN
  RAISE EXCEPTION 'verification request permanently closed' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
`

// The complete late prefix remains ordered with its catalog row columns.
var snMainnetMigrationContracts = []snMainnetMigrationContract{
	snMainnetMigration(770, "original verification transitions and retained signed evidence",
		migrationFp2Table("verify_original_transition"),
		snMainnetMigrationColumns("verify_original_transition",
			snMainnetMigrationColumn{name: "trail_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "previous_depth", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "observed_time", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "original_body", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original_signature", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("verify_original_transition",
			"PRIMARY KEY (trail_id, previous_depth)",
			"CHECK (((previous_depth >= 0) AND (previous_depth <= 16)))",
			"CHECK (((octet_length(original_body) >= 1) AND (octet_length(original_body) <= 65536)))",
			"CHECK ((octet_length(original_signature) = 64))"),
		financialIndexArtifact("verify_original_transition", "verify_original_transition_window", "observed_time, trail_id, previous_depth"),
		snMainnetMigrationForeignKeyCensus("verify_original_transition"),
		snMainnetMigrationTrigger("verify_original_transition", "verify_original_append_only", "verify_original_append_only_guard", 27),
		migrationFp2Table("verify_original_pending"),
		snMainnetMigrationColumns("verify_original_pending",
			snMainnetMigrationColumn{name: "trail_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "previous_depth", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "recovery_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("verify_original_pending",
			"PRIMARY KEY (trail_id)"),
		financialIndexArtifact("verify_original_pending", "verify_original_pending_due", "recovery_time, trail_id"),
		snMainnetMigrationForeignKeyCensus("verify_original_pending"),
		snMainnetMigrationColumns("verify_trail",
			snMainnetMigrationColumn{name: "original_state", kind: "bytea"}),
		snMainnetMigrationTrigger("verify_trail", "verify_trail_original_append_only", "verify_original_append_only_guard", 27),
		snMainnetMigrationColumns("st_provider_wallet_history",
			snMainnetMigrationColumn{name: "original_message", kind: "text"},
			snMainnetMigrationColumn{name: "original_signature", kind: "text"}),
		snMainnetMigrationConstraints("st_provider_wallet_history",
			"CHECK (((original_message IS NULL) = (original_signature IS NULL)))"),
		snMainnetMigrationColumns("st_event",
			snMainnetMigrationColumn{name: "original_log", kind: "bytea"}),
		snMainnetMigrationConstraints("st_event",
			"CHECK (((original_log IS NULL) OR ((octet_length(original_log) >= 1) AND (octet_length(original_log) <= 65536))))"),
		snMainnetMigrationTrigger("st_event", "st_event_original_append_only", "verify_original_append_only_guard", 27),
		migrationFp2Table("st_fleet_binding_original"),
		snMainnetMigrationColumns("st_fleet_binding_original",
			snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original_body", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "observed_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("st_fleet_binding_original",
			"PRIMARY KEY (deployment_key, client_id, generation, receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original_body) >= 1) AND (octet_length(original_body) <= 16384)))"),
		snMainnetMigrationForeignKeyCensus("st_fleet_binding_original"),
		snMainnetMigrationTrigger("st_fleet_binding_original", "st_fleet_binding_original_append_only", "verify_original_append_only_guard", 27),
		snMainnetMigrationFunction("verify_original_append_only_guard()", "trigger", "plpgsql", "v", false, snMainnetVerifyOriginalBody)),
	snMainnetMigration(771, "original provider work requests cuts and authority",
		migrationFp2Table("provider_work_request"),
		snMainnetMigrationColumns("provider_work_request",
			snMainnetMigrationColumn{name: "request_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "request_id", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "public_key", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "epoch", kind: "numeric(20,0)", notNull: true},
			snMainnetMigrationColumn{name: "kind", kind: "character varying(5)", notNull: true},
			snMainnetMigrationColumn{name: "issued_at", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "expires_at", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "retained_at", kind: "timestamp without time zone", notNull: true, defaultExpression: "timezone('utc'::text, now())"}),
		snMainnetMigrationConstraints("provider_work_request",
			"PRIMARY KEY (request_hash)",
			"UNIQUE (request_id)",
			"UNIQUE (domain_hash, client_id, generation, epoch, kind)",
			"CHECK ((octet_length(request_hash) = 32))",
			"CHECK ((octet_length(request_id) = 16))",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK ((octet_length(client_id) = 16))",
			"CHECK ((octet_length(generation) = 16))",
			"CHECK ((octet_length(public_key) = 32))",
			"CHECK (((epoch >= (0)::numeric) AND (epoch <= '18446744073709551615'::numeric)))",
			"CHECK (((kind)::text = ANY ((ARRAY['start'::character varying, 'end'::character varying])::text[])))",
			"CHECK (((expires_at > issued_at) AND ((expires_at - issued_at) <= 3600)))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 8192)))"),
		financialIndexArtifact("provider_work_request", "provider_work_request_owner", "domain_hash, client_id, generation, public_key, expires_at"),
		snMainnetMigrationForeignKeyCensus("provider_work_request"),
		snMainnetMigrationTrigger("provider_work_request", "provider_work_request_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_request", "provider_work_request_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_cut"),
		snMainnetMigrationColumns("provider_work_cut",
			snMainnetMigrationColumn{name: "request_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "cut_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "retained_at", kind: "timestamp without time zone", notNull: true, defaultExpression: "timezone('utc'::text, now())"}),
		snMainnetMigrationConstraints("provider_work_cut",
			"PRIMARY KEY (request_hash)",
			"CHECK ((octet_length(cut_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 8388608)))",
			"FOREIGN KEY (request_hash) REFERENCES provider_work_request(request_hash)"),
		financialIndexArtifact("provider_work_cut", "provider_work_cut_hash", "cut_hash"),
		snMainnetMigrationForeignKey("provider_work_cut", "provider_work_request", "FOREIGN KEY (request_hash) REFERENCES provider_work_request(request_hash)"),
		snMainnetMigrationForeignKeyCensus("provider_work_cut", "FOREIGN KEY (request_hash) REFERENCES provider_work_request(request_hash)"),
		snMainnetMigrationTrigger("provider_work_cut", "provider_work_cut_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_cut", "provider_work_cut_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_authority"),
		snMainnetMigrationColumns("provider_work_authority",
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "epoch", kind: "numeric(20,0)", notNull: true},
			snMainnetMigrationColumn{name: "authority_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "retained_at", kind: "timestamp without time zone", notNull: true, defaultExpression: "timezone('utc'::text, now())"}),
		snMainnetMigrationConstraints("provider_work_authority",
			"PRIMARY KEY (domain_hash, epoch)",
			"UNIQUE (authority_hash)",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((epoch >= (0)::numeric) AND (epoch <= '18446744073709551615'::numeric)))",
			"CHECK ((octet_length(authority_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 2097152)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_authority"),
		snMainnetMigrationTrigger("provider_work_authority", "provider_work_authority_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_authority", "provider_work_authority_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_window"),
		snMainnetMigrationColumns("provider_work_window",
			snMainnetMigrationColumn{name: "artifact_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "authority_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "retained_at", kind: "timestamp without time zone", notNull: true, defaultExpression: "timezone('utc'::text, now())"}),
		snMainnetMigrationConstraints("provider_work_window",
			"PRIMARY KEY (artifact_hash, authority_hash)",
			"CHECK ((octet_length(artifact_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 8388608)))",
			"FOREIGN KEY (authority_hash) REFERENCES provider_work_authority(authority_hash)"),
		snMainnetMigrationForeignKey("provider_work_window", "provider_work_authority", "FOREIGN KEY (authority_hash) REFERENCES provider_work_authority(authority_hash)"),
		snMainnetMigrationForeignKeyCensus("provider_work_window", "FOREIGN KEY (authority_hash) REFERENCES provider_work_authority(authority_hash)"),
		snMainnetMigrationTrigger("provider_work_window", "provider_work_window_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_window", "provider_work_window_truncate_guard", "provider_work_original_guard", 34),
		snMainnetMigrationFunction("provider_work_original_guard()", "trigger", "plpgsql", "v", false, snMainnetProviderOriginalBody)),
	snMainnetMigration(772, "original provider wallet mapping consent",
		migrationFp2Table("wallet_mapping_challenge"),
		snMainnetMigrationColumns("wallet_mapping_challenge",
			snMainnetMigrationColumn{name: "nonce", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "expires_at", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "message", kind: "text", notNull: true}),
		snMainnetMigrationConstraints("wallet_mapping_challenge",
			"PRIMARY KEY (nonce)",
			"CHECK ((octet_length(nonce) = 32))",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK (((octet_length(message) >= 1) AND (octet_length(message) <= 8192)))"),
		financialIndexArtifact("wallet_mapping_challenge", "wallet_mapping_challenge_owner", "domain_hash, client_id, expires_at"),
		snMainnetMigrationForeignKeyCensus("wallet_mapping_challenge"),
		snMainnetMigrationTrigger("wallet_mapping_challenge", "wallet_mapping_challenge_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("wallet_mapping_challenge", "wallet_mapping_challenge_truncate_guard", "wallet_mapping_original_guard", 34),
		migrationFp2Table("wallet_mapping_consent"),
		snMainnetMigrationColumns("wallet_mapping_consent",
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "original_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "nonce", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "accepted_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("wallet_mapping_consent",
			"PRIMARY KEY (domain_hash, client_id, generation)",
			"UNIQUE (original_hash)",
			"UNIQUE (nonce)",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK ((octet_length(original_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 12288)))",
			"FOREIGN KEY (nonce) REFERENCES wallet_mapping_challenge(nonce)"),
		snMainnetMigrationForeignKey("wallet_mapping_consent", "wallet_mapping_challenge", "FOREIGN KEY (nonce) REFERENCES wallet_mapping_challenge(nonce)"),
		snMainnetMigrationForeignKeyCensus("wallet_mapping_consent", "FOREIGN KEY (nonce) REFERENCES wallet_mapping_challenge(nonce)"),
		snMainnetMigrationTrigger("wallet_mapping_consent", "wallet_mapping_consent_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("wallet_mapping_consent", "wallet_mapping_consent_truncate_guard", "wallet_mapping_original_guard", 34),
		snMainnetMigrationFunction("wallet_mapping_original_guard()", "trigger", "plpgsql", "v", false, snMainnetWalletOriginalBody)),
	snMainnetMigration(773, "original verification request lookup and capture",
		migrationFp2Table("verify_original_request"),
		snMainnetMigrationColumns("verify_original_request",
			snMainnetMigrationColumn{name: "request_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "trail_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "previous_depth", kind: "integer", notNull: true}),
		snMainnetMigrationConstraints("verify_original_request",
			"PRIMARY KEY (request_hash)",
			"CHECK ((octet_length(request_hash) = 32))",
			"FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"),
		snMainnetMigrationForeignKey("verify_original_request", "verify_original_transition", "FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"),
		snMainnetMigrationForeignKeyCensus("verify_original_request", "FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"),
		snMainnetMigrationTrigger("verify_original_request", "verify_original_request_append_only", "verify_original_append_only_guard", 27),
		snMainnetMigrationTrigger("verify_original_request", "verify_original_request_no_truncate", "verify_original_append_only_guard", 34),
		migrationFp2Table("verify_original_request_lookup"),
		snMainnetMigrationColumns("verify_original_request_lookup",
			snMainnetMigrationColumn{name: "trail_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "previous_depth", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "scope_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "request_message", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "request_signature", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("verify_original_request_lookup",
			"PRIMARY KEY (trail_id, previous_depth)",
			"CHECK (((octet_length(request_message) >= 1) AND (octet_length(request_message) <= 2048)))",
			"CHECK ((octet_length(request_signature) = 64))",
			"FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"),
		financialIndexArtifact("verify_original_request_lookup", "verify_original_request_lookup_identity", "client_id, sha256(request_message), sha256(request_signature)"),
		snMainnetMigrationForeignKey("verify_original_request_lookup", "verify_original_transition", "FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"),
		snMainnetMigrationForeignKeyCensus("verify_original_request_lookup", "FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"),
		snMainnetMigrationTrigger("verify_original_request_lookup", "verify_original_request_lookup_append_only", "verify_original_append_only_guard", 27),
		snMainnetMigrationTrigger("verify_original_request_lookup", "verify_original_request_lookup_no_truncate", "verify_original_append_only_guard", 34),
		snMainnetMigrationTrigger("verify_original_transition", "verify_original_request_capture", "verify_original_request_capture", 5),
		snMainnetMigrationPriorFunction(snMainnetMigrationFunction("verify_original_request_capture()", "trigger", "plpgsql", "v", false, snMainnetVerifyCaptureLegacyBody), snMainnetMigrationFunction("verify_original_request_capture()", "trigger", "plpgsql", "v", false, snMainnetVerifyCaptureBody), snMainnetMigrationFunction("verify_original_request_index_body(bytea)", "jsonb", "plpgsql", "i", true, snMainnetVerifyIndexProjectionBody, "original"))),
	snMainnetMigration(774, "original SDK provider owner enrollment",
		migrationFp2Table("provider_work_owner"),
		snMainnetMigrationColumns("provider_work_owner",
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "public_key", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "owner_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "key_registration", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "retained_at", kind: "timestamp without time zone", notNull: true, defaultExpression: "timezone('utc'::text, now())"}),
		snMainnetMigrationConstraints("provider_work_owner",
			"PRIMARY KEY (domain_hash, client_id, generation)",
			"UNIQUE (owner_hash)",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK ((octet_length(client_id) = 16))",
			"CHECK ((octet_length(generation) = 16))",
			"CHECK ((octet_length(public_key) = 32))",
			"CHECK ((octet_length(owner_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 4096)))",
			"CHECK (((octet_length(key_registration) >= 1) AND (octet_length(key_registration) <= 8192)))"),
		financialIndexArtifact("provider_work_owner", "provider_work_owner_client", "client_id"),
		financialIndexArtifact("provider_work_owner", "provider_work_owner_identity", "domain_hash, client_id, public_key, generation"),
		snMainnetMigrationForeignKeyCensus("provider_work_owner"),
		snMainnetMigrationTrigger("provider_work_owner", "provider_work_owner_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_owner", "provider_work_owner_truncate_guard", "provider_work_original_guard", 34)),
	snMainnetMigration(775, "original verification closure and wire fences",
		migrationFp2Table("verify_original_request_closed"),
		snMainnetMigrationColumns("verify_original_request_closed",
			snMainnetMigrationColumn{name: "request_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "scope_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "request_message", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "request_signature", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "closure_original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "receipt_body", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "receipt_signature", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("verify_original_request_closed",
			"PRIMARY KEY (request_hash)",
			"CHECK ((octet_length(request_hash) = 32))",
			"CHECK (((octet_length(request_message) >= 1) AND (octet_length(request_message) <= 2048)))",
			"CHECK ((octet_length(request_signature) = 64))",
			"CHECK (((octet_length(closure_original) >= 1) AND (octet_length(closure_original) <= 8192)))",
			"CHECK (((octet_length(receipt_body) >= 1) AND (octet_length(receipt_body) <= 4096)))",
			"CHECK ((octet_length(receipt_signature) = 64))"),
		financialIndexArtifact("verify_original_request_closed", "verify_original_request_closed_identity", "client_id, sha256(request_message), sha256(request_signature)"),
		snMainnetMigrationForeignKeyCensus("verify_original_request_closed"),
		snMainnetMigrationTrigger("verify_original_request_closed", "verify_original_request_no_received_tombstone", "verify_original_request_no_received_tombstone", 7),
		snMainnetMigrationTrigger("verify_original_request_closed", "verify_original_request_closed_append_only", "verify_original_append_only_guard", 27),
		snMainnetMigrationTrigger("verify_original_request_closed", "verify_original_request_closed_no_truncate", "verify_original_append_only_guard", 34),
		snMainnetMigrationTrigger("verify_original_transition", "verify_original_request_closed_fence", "verify_original_request_closed_fence", 7),
		snMainnetMigrationFunction("verify_original_request_wire_lock(bytea,bytea)", "void", "sql", "v", false, snMainnetVerifyWireLockBody, "message", "signature"),
		snMainnetMigrationFunction("verify_original_request_no_received_tombstone()", "trigger", "plpgsql", "v", false, snMainnetVerifyReceivedFenceBody),
		snMainnetMigrationPriorFunction(snMainnetMigrationFunction("verify_original_request_closed_fence()", "trigger", "plpgsql", "v", false, snMainnetVerifyFenceLegacyBody), snMainnetMigrationFunction("verify_original_request_closed_fence()", "trigger", "plpgsql", "v", false, snMainnetVerifyFenceBody), snMainnetMigrationFunction("verify_original_request_index_body(bytea)", "jsonb", "plpgsql", "i", true, snMainnetVerifyIndexProjectionBody, "original"))),
	snMainnetMigration(776, "original provider session reservation and outcome custody",
		migrationFp2Table("provider_work_session_head"),
		snMainnetMigrationColumns("provider_work_session_head",
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "sequence", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("provider_work_session_head",
			"PRIMARY KEY (client_id)",
			"CHECK ((sequence > 0))"),
		snMainnetMigrationForeignKeyCensus("provider_work_session_head"),
		snMainnetMigrationTrigger("provider_work_session_head", "provider_work_session_head_guard", "provider_work_session_head_guard", 27),
		snMainnetMigrationTrigger("provider_work_session_head", "provider_work_session_head_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_session_event"),
		snMainnetMigrationColumns("provider_work_session_event",
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "connection_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "kind", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "observed_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "extender_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "transaction_id", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("provider_work_session_event",
			"PRIMARY KEY (client_id, sequence)",
			"CHECK ((sequence > 0))",
			"CHECK ((kind = ANY (ARRAY['baseline'::text, 'admit'::text, 'retire'::text])))"),
		financialIndexArtifact("provider_work_session_event", "provider_work_session_event_transaction", "transaction_id, client_id, sequence"),
		financialIndexArtifact("provider_work_session_event", "provider_work_session_event_connection", "client_id, connection_id, sequence"),
		snMainnetMigrationForeignKeyCensus("provider_work_session_event"),
		snMainnetMigrationTrigger("provider_work_session_event", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_session_event", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_session_receipt"),
		snMainnetMigrationColumns("provider_work_session_receipt",
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_session_receipt",
			"PRIMARY KEY (client_id, sequence)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))",
			"FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)"),
		snMainnetMigrationForeignKey("provider_work_session_receipt", "provider_work_session_event", "FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)"),
		snMainnetMigrationForeignKeyCensus("provider_work_session_receipt", "FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)"),
		snMainnetMigrationTrigger("provider_work_session_receipt", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_session_receipt", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_reservation_original"),
		snMainnetMigrationColumns("provider_work_reservation_original",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "source_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "source_sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "destination_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "destination_sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_reservation_original",
			"PRIMARY KEY (contract_id)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_reservation_original"),
		snMainnetMigrationTrigger("provider_work_reservation_original", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_reservation_original", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_stream_original"),
		snMainnetMigrationColumns("provider_work_stream_original",
			snMainnetMigrationColumn{name: "stream_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "origin_contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_stream_original",
			"PRIMARY KEY (stream_id)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_stream_original"),
		snMainnetMigrationTrigger("provider_work_stream_original", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_stream_original", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_stream_contract"),
		snMainnetMigrationColumns("provider_work_stream_contract",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "stream_id", kind: "uuid", notNull: true}),
		snMainnetMigrationConstraints("provider_work_stream_contract",
			"PRIMARY KEY (contract_id)",
			"FOREIGN KEY (stream_id) REFERENCES provider_work_stream_original(stream_id)"),
		snMainnetMigrationForeignKey("provider_work_stream_contract", "provider_work_stream_original", "FOREIGN KEY (stream_id) REFERENCES provider_work_stream_original(stream_id)"),
		snMainnetMigrationForeignKeyCensus("provider_work_stream_contract", "FOREIGN KEY (stream_id) REFERENCES provider_work_stream_original(stream_id)"),
		snMainnetMigrationTrigger("provider_work_stream_contract", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_stream_contract", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_outcome_original"),
		snMainnetMigrationColumns("provider_work_outcome_original",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_outcome_original",
			"PRIMARY KEY (contract_id)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_outcome_original"),
		snMainnetMigrationTrigger("provider_work_outcome_original", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_outcome_original", "original_truncate_guard", "provider_work_original_guard", 34),
		snMainnetMigrationTrigger("network_client_connection", "provider_work_session_statement_fence", "provider_work_session_statement_fence", 30),
		snMainnetMigrationTrigger("network_client_connection", "provider_work_session_mutation", "provider_work_session_mutation", 29),
		snMainnetMigrationFunction("provider_work_endpoint_lock(uuid)", "void", "plpgsql", "v", false, snMainnetProviderEndpointBody, "client"),
		snMainnetMigrationFunction("provider_work_session_append(uuid,uuid,text,uuid)", "bigint", "plpgsql", "v", false, snMainnetProviderSessionAppendBody, "client", "connection", "event_kind", "extender"),
		snMainnetMigrationFunction("provider_work_session_mutation()", "trigger", "plpgsql", "v", false, snMainnetProviderSessionMutationBody),
		snMainnetMigrationFunction("provider_work_session_statement_fence()", "trigger", "plpgsql", "v", false, snMainnetProviderStatementFenceBody),
		snMainnetMigrationFunction("provider_work_session_head_guard()", "trigger", "plpgsql", "v", false, snMainnetProviderHeadBody)),
	snMainnetMigration(777, "operator gas policy and durable liability reservations",
		migrationFp2Table("st_operator_gas_budget"),
		snMainnetMigrationColumns("st_operator_gas_budget",
			snMainnetMigrationColumn{name: "scope_key", kind: "character varying(160)", notNull: true},
			snMainnetMigrationColumn{name: "chain_id", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "genesis_hash", kind: "character varying(66)", notNull: true},
			snMainnetMigrationColumn{name: "no_id", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "approver_public_key", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "current_revision", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "current_policy_sha256", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "maximum_lifetime_wei", kind: "numeric(78,0)", notNull: true},
			snMainnetMigrationColumn{name: "maximum_lifetime_attempts", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("st_operator_gas_budget",
			"PRIMARY KEY (scope_key)",
			"UNIQUE (chain_id, genesis_hash, no_id)",
			"CHECK ((chain_id > 0))",
			"CHECK ((no_id > 0))",
			"CHECK ((current_revision >= 0))",
			"CHECK ((maximum_lifetime_wei > (0)::numeric))",
			"CHECK ((maximum_lifetime_attempts > 0))"),
		snMainnetMigrationForeignKeyCensus("st_operator_gas_budget"),
		migrationFp2Table("st_operator_gas_policy"),
		snMainnetMigrationColumns("st_operator_gas_policy",
			snMainnetMigrationColumn{name: "policy_sha256", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "scope_key", kind: "character varying(160)", notNull: true},
			snMainnetMigrationColumn{name: "revision", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "policy_json", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "authority_json", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("st_operator_gas_policy",
			"PRIMARY KEY (policy_sha256)",
			"UNIQUE (scope_key, revision)",
			"CHECK ((revision >= 0))",
			"FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		snMainnetMigrationForeignKey("st_operator_gas_policy", "st_operator_gas_budget", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		snMainnetMigrationForeignKeyCensus("st_operator_gas_policy", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		migrationFp2Table("st_operator_gas_account"),
		snMainnetMigrationColumns("st_operator_gas_account",
			snMainnetMigrationColumn{name: "chain_id", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "genesis_hash", kind: "character varying(66)", notNull: true},
			snMainnetMigrationColumn{name: "from_address", kind: "character varying(42)", notNull: true},
			snMainnetMigrationColumn{name: "scope_key", kind: "character varying(160)", notNull: true},
			snMainnetMigrationColumn{name: "initial_history_sha256", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("st_operator_gas_account",
			"PRIMARY KEY (chain_id, genesis_hash, from_address)",
			"FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		financialIndexArtifact("st_operator_gas_account", "st_operator_gas_account_scope", "scope_key"),
		snMainnetMigrationForeignKey("st_operator_gas_account", "st_operator_gas_budget", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		snMainnetMigrationForeignKeyCensus("st_operator_gas_account", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		migrationFp2Table("st_operator_gas_intent"),
		snMainnetMigrationColumns("st_operator_gas_intent",
			snMainnetMigrationColumn{name: "logical_key", kind: "character varying(255)", notNull: true},
			snMainnetMigrationColumn{name: "scope_key", kind: "character varying(160)", notNull: true},
			snMainnetMigrationColumn{name: "original_policy_sha256", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "maximum_liability_wei", kind: "numeric(78,0)", notNull: true},
			snMainnetMigrationColumn{name: "maximum_attempts", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("st_operator_gas_intent",
			"PRIMARY KEY (logical_key)",
			"CHECK ((maximum_liability_wei > (0)::numeric))",
			"CHECK ((maximum_attempts > 0))",
			"FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)",
			"FOREIGN KEY (original_policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)"),
		snMainnetMigrationForeignKey("st_operator_gas_intent", "st_operator_gas_budget", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		snMainnetMigrationForeignKey("st_operator_gas_intent", "st_operator_gas_policy", "FOREIGN KEY (original_policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)"),
		snMainnetMigrationForeignKeyCensus("st_operator_gas_intent", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)", "FOREIGN KEY (original_policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)"),
		migrationFp2Table("st_operator_gas_reservation"),
		snMainnetMigrationColumns("st_operator_gas_reservation",
			snMainnetMigrationColumn{name: "intent_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "attempt", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "scope_key", kind: "character varying(160)", notNull: true},
			snMainnetMigrationColumn{name: "logical_key", kind: "character varying(255)", notNull: true},
			snMainnetMigrationColumn{name: "policy_sha256", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "kind", kind: "character varying(16)", notNull: true},
			snMainnetMigrationColumn{name: "unsigned_transaction", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "signing_hash", kind: "character varying(66)", notNull: true},
			snMainnetMigrationColumn{name: "maximum_liability_wei", kind: "numeric(78,0)", notNull: true},
			snMainnetMigrationColumn{name: "signed_tx_hash", kind: "character varying(66)"},
			snMainnetMigrationColumn{name: "historical", kind: "boolean", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("st_operator_gas_reservation",
			"PRIMARY KEY (intent_id, attempt)",
			"CHECK ((attempt > 0))",
			"CHECK (((kind)::text = ANY ((ARRAY['execution'::character varying, 'cancellation'::character varying])::text[])))",
			"CHECK ((maximum_liability_wei > (0)::numeric))",
			"FOREIGN KEY (intent_id) REFERENCES st_transaction_intent(intent_id)",
			"FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)",
			"FOREIGN KEY (logical_key) REFERENCES st_operator_gas_intent(logical_key)",
			"FOREIGN KEY (policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)"),
		financialIndexArtifact("st_operator_gas_reservation", "st_operator_gas_reservation_scope", "scope_key"),
		financialIndexArtifact("st_operator_gas_reservation", "st_operator_gas_reservation_logical", "logical_key"),
		snMainnetMigrationForeignKey("st_operator_gas_reservation", "st_transaction_intent", "FOREIGN KEY (intent_id) REFERENCES st_transaction_intent(intent_id)"),
		snMainnetMigrationForeignKey("st_operator_gas_reservation", "st_operator_gas_budget", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"),
		snMainnetMigrationForeignKey("st_operator_gas_reservation", "st_operator_gas_intent", "FOREIGN KEY (logical_key) REFERENCES st_operator_gas_intent(logical_key)"),
		snMainnetMigrationForeignKey("st_operator_gas_reservation", "st_operator_gas_policy", "FOREIGN KEY (policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)"),
		snMainnetMigrationForeignKeyCensus("st_operator_gas_reservation", "FOREIGN KEY (intent_id) REFERENCES st_transaction_intent(intent_id)", "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)", "FOREIGN KEY (logical_key) REFERENCES st_operator_gas_intent(logical_key)", "FOREIGN KEY (policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)")),
	snMainnetMigration(778, "original provider open observation custody",
		migrationFp2Table("provider_work_open_original"),
		snMainnetMigrationColumns("provider_work_open_original",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "epoch", kind: "numeric(20,0)", notNull: true},
			snMainnetMigrationColumn{name: "block_number", kind: "numeric(20,0)", notNull: true},
			snMainnetMigrationColumn{name: "block_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "boundary_time", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "observed_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "reservation_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_open_original",
			"PRIMARY KEY (contract_id, epoch, block_hash)",
			"UNIQUE (receipt_hash)",
			"CHECK (((epoch >= (0)::numeric) AND (epoch <= '18446744073709551615'::numeric)))",
			"CHECK (((block_number >= (0)::numeric) AND (block_number <= '18446744073709551615'::numeric)))",
			"CHECK ((octet_length(block_hash) = 32))",
			"CHECK ((observed_at >= boundary_time))",
			"CHECK ((octet_length(reservation_hash) = 32))",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_open_original"),
		snMainnetMigrationTrigger("provider_work_open_original", "provider_work_open_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_open_original", "provider_work_open_truncate_guard", "provider_work_original_guard", 34)),
	snMainnetMigration(779, "strict verification original JSON projection",
		snMainnetMigrationFunction("verify_original_request_index_body(bytea)", "jsonb", "plpgsql", "i", true, snMainnetVerifyIndexProjectionBody, "original"),
		snMainnetMigrationFunction("verify_original_request_capture()", "trigger", "plpgsql", "v", false, snMainnetVerifyCaptureBody),
		snMainnetMigrationFunction("verify_original_request_closed_fence()", "trigger", "plpgsql", "v", false, snMainnetVerifyFenceBody)),
	snMainnetNativeFeeMigrationContract(),
	snMainnetMigration(781, "transfer balance grant kind metadata",
		migrationFp2Table("transfer_balance"),
		snMainnetMigrationColumns("transfer_balance",
			snMainnetMigrationColumn{name: "grant_kind", kind: "character varying(32)"})),
	// Fresh v776 already installs these exact bodies. The appended repair is
	// still independently required at v782 for previously installed functions.
	snMainnetMigration(782, "per-client provider work session contention repair",
		snMainnetMigrationFunction("provider_work_endpoint_lock(uuid)", "void", "plpgsql", "v", false, snMainnetProviderEndpointBody, "client"),
		snMainnetMigrationFunction("provider_work_session_statement_fence()", "trigger", "plpgsql", "v", false, snMainnetProviderStatementFenceBody)),
	// Delivery claims due rows, releases held rows and removes finished rows
	// through the three partial indexes, each over exactly its own rows.
	snMainnetMigration(783, "transactional account message outbox",
		migrationFp2Table("account_message_outbox"),
		snMainnetMigrationColumns("account_message_outbox",
			snMainnetMigrationColumn{name: "message_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "message_key", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "user_auth", kind: "character varying(256)", notNull: true},
			snMainnetMigrationColumn{name: "template_name", kind: "character varying(64)", notNull: true},
			snMainnetMigrationColumn{name: "template_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "deliver_time", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "attempt_count", kind: "integer", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "claim_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "claim_until", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "last_error", kind: "character varying(512)"},
			snMainnetMigrationColumn{name: "sent_time", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "abandon_time", kind: "timestamp without time zone"}),
		snMainnetMigrationConstraints("account_message_outbox",
			"PRIMARY KEY (message_id)",
			"UNIQUE (message_key)",
			"CHECK ((attempt_count >= 0))"),
		snMainnetMigrationPartialIndex("account_message_outbox", "account_message_outbox_due", "deliver_time, message_id",
			"((deliver_time IS NOT NULL) AND (sent_time IS NULL) AND (abandon_time IS NULL))"),
		snMainnetMigrationPartialIndex("account_message_outbox", "account_message_outbox_held", "template_name, create_time, message_id",
			"(deliver_time IS NULL)"),
		snMainnetMigrationPartialIndex("account_message_outbox", "account_message_outbox_finished", "create_time, message_id",
			"((sent_time IS NOT NULL) OR (abandon_time IS NOT NULL))")),
	// A constant default would date every new update record at the oldest
	// position, and the poll would again read by update id alone.
	snMainnetMigration(784, "search update transaction ids",
		migrationFp2Table("search_value_update"),
		snMainnetMigrationColumns("search_value_update",
			snMainnetMigrationColumn{name: "xid", kind: "xid8", notNull: true, defaultExpression: "pg_current_xact_id()"})),
	// The concurrent build is valid and ready only once it has completed.
	snMainnetMigration(785, "search update commit-order index",
		financialIndexArtifact("search_value_update", "search_value_update_realm_xid_update_id", "realm, xid, update_id")),
	// Network consent chains mirror the provider chains of 772 keyed by network,
	// and the settled earning wallets are append-only through the same guard.
	snMainnetMigration(786, "network wallet mapping consent and settled earning wallets",
		migrationFp2Table("network_wallet_mapping_challenge"),
		snMainnetMigrationColumns("network_wallet_mapping_challenge",
			snMainnetMigrationColumn{name: "nonce", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "expires_at", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "message", kind: "text", notNull: true}),
		snMainnetMigrationConstraints("network_wallet_mapping_challenge",
			"PRIMARY KEY (nonce)",
			"CHECK ((octet_length(nonce) = 32))",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK (((octet_length(message) >= 1) AND (octet_length(message) <= 8192)))"),
		financialIndexArtifact("network_wallet_mapping_challenge", "network_wallet_mapping_challenge_owner", "domain_hash, network_id, expires_at"),
		snMainnetMigrationForeignKeyCensus("network_wallet_mapping_challenge"),
		snMainnetMigrationTrigger("network_wallet_mapping_challenge", "network_wallet_mapping_challenge_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("network_wallet_mapping_challenge", "network_wallet_mapping_challenge_truncate_guard", "wallet_mapping_original_guard", 34),
		migrationFp2Table("network_wallet_mapping_consent"),
		snMainnetMigrationColumns("network_wallet_mapping_consent",
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "original_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "nonce", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "accepted_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("network_wallet_mapping_consent",
			"PRIMARY KEY (domain_hash, network_id, generation)",
			"UNIQUE (original_hash)",
			"UNIQUE (nonce)",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK ((octet_length(original_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 12288)))",
			"FOREIGN KEY (nonce) REFERENCES network_wallet_mapping_challenge(nonce)"),
		snMainnetMigrationForeignKey("network_wallet_mapping_consent", "network_wallet_mapping_challenge", "FOREIGN KEY (nonce) REFERENCES network_wallet_mapping_challenge(nonce)"),
		snMainnetMigrationForeignKeyCensus("network_wallet_mapping_consent", "FOREIGN KEY (nonce) REFERENCES network_wallet_mapping_challenge(nonce)"),
		snMainnetMigrationTrigger("network_wallet_mapping_consent", "network_wallet_mapping_consent_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("network_wallet_mapping_consent", "network_wallet_mapping_consent_truncate_guard", "wallet_mapping_original_guard", 34),
		migrationFp2Table("st_payout_wallet_resolution"),
		snMainnetMigrationColumns("st_payout_wallet_resolution",
			snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
			snMainnetMigrationColumn{name: "epoch", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "no_id", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "mode", kind: "character varying(16)", notNull: true},
			snMainnetMigrationColumn{name: "coldkey", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "consent_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "consent_generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "head_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "head_generation", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("st_payout_wallet_resolution",
			"PRIMARY KEY (deployment_key, epoch, no_id, client_id)",
			"CHECK ((octet_length(coldkey) = 32))",
			"CHECK ((octet_length(consent_hash) = 32))",
			"CHECK (((consent_generation >= 1) AND (consent_generation <= 4096)))",
			"CHECK ((octet_length(head_hash) = 32))",
			"CHECK (((head_generation >= 1) AND (head_generation <= 4096)))"),
		// v789 replaces the closed mode set with one that adds hotkey; either
		// keeps it closed here, and v789 requires its own
		"("+snMainnetMigrationConstraints("st_payout_wallet_resolution", snMainnetResolutionNetworkModes)+" OR "+snMainnetMigrationConstraints("st_payout_wallet_resolution", snMainnetResolutionHotkeyModes)+")",
		snMainnetMigrationForeignKeyCensus("st_payout_wallet_resolution"),
		snMainnetMigrationTrigger("st_payout_wallet_resolution", "st_payout_wallet_resolution_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("st_payout_wallet_resolution", "st_payout_wallet_resolution_truncate_guard", "wallet_mapping_original_guard", 34)),
	// Global hotkey consent chains are keyed by subnet and hotkey and have no
	// challenge, so each submitting network's links to its hotkeys bound them;
	// delegations mirror the network consents of 786. The settled earning
	// wallets gain mode hotkey, with every hotkey column or none.
	snMainnetMigration(789, "hotkey wallet consent, network delegation and hotkey earning wallets",
		migrationFp2Table("hotkey_wallet_mapping_consent"),
		snMainnetMigrationColumns("hotkey_wallet_mapping_consent",
			snMainnetMigrationColumn{name: "subnet_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "hotkey", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "original_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "accepted_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("hotkey_wallet_mapping_consent",
			"PRIMARY KEY (subnet_hash, hotkey, generation)",
			"UNIQUE (original_hash)",
			"CHECK ((octet_length(subnet_hash) = 32))",
			"CHECK ((octet_length(hotkey) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK ((octet_length(original_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 12288)))"),
		snMainnetMigrationForeignKeyCensus("hotkey_wallet_mapping_consent"),
		snMainnetMigrationTrigger("hotkey_wallet_mapping_consent", "hotkey_wallet_mapping_consent_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("hotkey_wallet_mapping_consent", "hotkey_wallet_mapping_consent_truncate_guard", "wallet_mapping_original_guard", 34),
		migrationFp2Table("hotkey_wallet_mapping_submitter"),
		snMainnetMigrationColumns("hotkey_wallet_mapping_submitter",
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "hotkey", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("hotkey_wallet_mapping_submitter",
			"PRIMARY KEY (network_id, hotkey)",
			"CHECK ((octet_length(hotkey) = 32))"),
		snMainnetMigrationForeignKeyCensus("hotkey_wallet_mapping_submitter"),
		snMainnetMigrationTrigger("hotkey_wallet_mapping_submitter", "hotkey_wallet_mapping_submitter_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("hotkey_wallet_mapping_submitter", "hotkey_wallet_mapping_submitter_truncate_guard", "wallet_mapping_original_guard", 34),
		migrationFp2Table("hotkey_network_delegation_challenge"),
		snMainnetMigrationColumns("hotkey_network_delegation_challenge",
			snMainnetMigrationColumn{name: "nonce", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "expires_at", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "message", kind: "text", notNull: true}),
		snMainnetMigrationConstraints("hotkey_network_delegation_challenge",
			"PRIMARY KEY (nonce)",
			"CHECK ((octet_length(nonce) = 32))",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK (((octet_length(message) >= 1) AND (octet_length(message) <= 8192)))"),
		financialIndexArtifact("hotkey_network_delegation_challenge", "hotkey_network_delegation_challenge_owner", "domain_hash, network_id, expires_at"),
		snMainnetMigrationForeignKeyCensus("hotkey_network_delegation_challenge"),
		snMainnetMigrationTrigger("hotkey_network_delegation_challenge", "hotkey_network_delegation_challenge_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("hotkey_network_delegation_challenge", "hotkey_network_delegation_challenge_truncate_guard", "wallet_mapping_original_guard", 34),
		migrationFp2Table("hotkey_network_delegation"),
		snMainnetMigrationColumns("hotkey_network_delegation",
			snMainnetMigrationColumn{name: "domain_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "original_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "nonce", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "accepted_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("hotkey_network_delegation",
			"PRIMARY KEY (domain_hash, network_id, generation)",
			"UNIQUE (original_hash)",
			"UNIQUE (nonce)",
			"CHECK ((octet_length(domain_hash) = 32))",
			"CHECK (((generation >= 1) AND (generation <= 4096)))",
			"CHECK ((octet_length(original_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 12288)))",
			"FOREIGN KEY (nonce) REFERENCES hotkey_network_delegation_challenge(nonce)"),
		snMainnetMigrationForeignKey("hotkey_network_delegation", "hotkey_network_delegation_challenge", "FOREIGN KEY (nonce) REFERENCES hotkey_network_delegation_challenge(nonce)"),
		snMainnetMigrationForeignKeyCensus("hotkey_network_delegation", "FOREIGN KEY (nonce) REFERENCES hotkey_network_delegation_challenge(nonce)"),
		snMainnetMigrationTrigger("hotkey_network_delegation", "hotkey_network_delegation_guard", "wallet_mapping_original_guard", 27),
		snMainnetMigrationTrigger("hotkey_network_delegation", "hotkey_network_delegation_truncate_guard", "wallet_mapping_original_guard", 34),
		snMainnetMigrationColumns("st_payout_wallet_resolution",
			snMainnetMigrationColumn{name: "hotkey", kind: "bytea"},
			snMainnetMigrationColumn{name: "hotkey_consent_hash", kind: "bytea"},
			snMainnetMigrationColumn{name: "hotkey_consent_generation", kind: "bigint"},
			snMainnetMigrationColumn{name: "hotkey_consent_head_hash", kind: "bytea"},
			snMainnetMigrationColumn{name: "hotkey_consent_head_generation", kind: "bigint"}),
		snMainnetMigrationConstraints("st_payout_wallet_resolution",
			snMainnetResolutionHotkeyModes,
			"CHECK ((octet_length(hotkey) = 32))",
			"CHECK ((octet_length(hotkey_consent_hash) = 32))",
			"CHECK (((hotkey_consent_generation >= 1) AND (hotkey_consent_generation <= 4096)))",
			"CHECK ((octet_length(hotkey_consent_head_hash) = 32))",
			"CHECK (((hotkey_consent_head_generation >= 1) AND (hotkey_consent_head_generation <= 4096)))",
			snMainnetResolutionHotkeyColumns,
			snMainnetResolutionHotkeyMode),
		// the closed set of 786 beside it would refuse every hotkey resolution
		"NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.st_payout_wallet_resolution') AND contype='c' AND pg_get_constraintdef(oid)="+snMainnetMigrationLiteral(snMainnetResolutionNetworkModes)+")"),
}

// The closed sets of st_payout_wallet_resolution.mode that v786 and v789
// publish, and v789's rules that a hotkey resolution has every hotkey column
// and only a hotkey resolution has any.
const snMainnetResolutionNetworkModes = "CHECK (((mode)::text = ANY ((ARRAY['provider'::character varying, 'network'::character varying])::text[])))"
const snMainnetResolutionHotkeyModes = "CHECK (((mode)::text = ANY ((ARRAY['provider'::character varying, 'network'::character varying, 'hotkey'::character varying])::text[])))"
const snMainnetResolutionHotkeyColumns = "CHECK ((num_nonnulls(hotkey, hotkey_consent_hash, hotkey_consent_generation, hotkey_consent_head_hash, hotkey_consent_head_generation) = ANY (ARRAY[0, 5])))"
const snMainnetResolutionHotkeyMode = "CHECK ((((mode)::text = 'hotkey'::text) = (hotkey IS NOT NULL)))"
