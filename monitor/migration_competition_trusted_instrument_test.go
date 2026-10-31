// The trusted instrument overlay (v900) through the public migration signal:
// published prefixes on both sides of it are healthy, and faults in its own
// and its retained custody reach the drift alert under v900.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The 2026-10-30 page: at heads 900 and 901 the signal reported the v897
// custody absent, because 900 legitimately replaces the source authorization
// body that v897 pins. Every published prefix around the overlay is coherent.
func TestMigrationsTrustedInstrumentPublishedPrefixesHaveNoDrift(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{897, 899, 900, 901} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("published prefix %d reports drift: %s", head, drift)
			}
		}
	})
}

// v897 stays required through 899 and retires exactly at 900, whose own
// contract pins the 900 bodies and not the body 900 replaced.
func TestMigrationsTrustedInstrumentSuccessorContractCustody(t *testing.T) {
	predecessor := migrationCurrentSuffixTestContract(t, 897)
	contract := migrationCurrentSuffixTestContract(t, 900)
	if predecessor.artifact.removedVersion != 900 || contract.artifact.rowColumn != 311 || contract.artifact.removedVersion != 0 {
		t.Fatal("trusted instrument successor has wrong lifetime or column")
	}
	lockDigest, productIntent, sourceAuthorized := server.CompetitionTrustedInstrumentBodiesSql()
	for _, pin := range []string{
		"public.competition_v2_instrument_lock_digest(jsonb)",
		"public.competition_v2_instrument_product_intent(jsonb,text,text,text)",
		"public.competition_v2_source_authorized(jsonb,text,text,text)",
		"public.competition_v2_snow_review_findings_valid_for_policy(jsonb,text)",
		"competition_v2_winner_checkpoint_before_insert",
		"competition_v2_review_identity",
		"p.prosrc=" + snMainnetMigrationLiteral(lockDigest),
		"p.prosrc=" + snMainnetMigrationLiteral(productIntent),
		"p.prosrc=" + snMainnetMigrationLiteral(sourceAuthorized),
	} {
		if !strings.Contains(contract.query, pin) {
			t.Fatalf("successor omitted %.80q", pin)
		}
	}
	replaced := snMainnetMigrationLiteral(server.CompetitionRepositoryPatchSourceBodySql())
	if strings.Contains(contract.query, "p.prosrc="+replaced) {
		t.Fatal("successor still accepts the source authorization body that 900 replaced")
	}
	// Without its two instrument functions, and with the replaced source body
	// swapped back, the successor is the v897 predicate byte for byte: nothing
	// else in the carried custody is loosened or dropped.
	instruments := "(" + snMainnetMigrationFunction("competition_v2_instrument_lock_digest(jsonb)", "text", "sql", "i", false, lockDigest, "commits") + " AND " +
		snMainnetMigrationFunction("competition_v2_instrument_product_intent(jsonb,text,text,text)", "jsonb", "plpgsql", "v", false, productIntent, "intent", "env", "competition", "season") + " AND "
	carried, found := strings.CutPrefix(contract.query, instruments)
	if !found || !strings.HasSuffix(carried, ")") || strings.Count(carried, snMainnetMigrationLiteral(sourceAuthorized)) != 1 {
		t.Fatal("successor does not lead with the exact instrument functions")
	}
	carried = strings.Replace(strings.TrimSuffix(carried, ")"), snMainnetMigrationLiteral(sourceAuthorized), replaced, 1)
	if carried != predecessor.query {
		t.Fatal("successor changes the carried v897 custody beyond the source authorization body")
	}
	for _, test := range []struct {
		head, bad int
		drift     bool
	}{{899, 897, true}, {900, 897, false}, {901, 897, false}, {899, 900, false}, {900, 900, true}, {901, 900, true}} {
		row := syntheticMigrationArtifactRow(test.head)
		row[test.bad-589] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.head), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if got := requireAlertClassCount(alerts, "migration-schema-drift") != 0; got != test.drift {
			t.Fatalf("head %d with v%d absent: drift=%t, want %t", test.head, test.bad, got, test.drift)
		}
	}
}

// A wrong, missing or misconfigured 900 body is still drift at the head,
// owned by v900. The body 900 replaced is no longer accepted, and the 900
// body is not accepted before its migration applies.
func TestMigrationsTrustedInstrumentPublishedSchemaAndFaults(t *testing.T) {
	_, _, sourceAuthorized := server.CompetitionTrustedInstrumentBodiesSql()
	projection := "\n intent:=competition_v2_instrument_product_intent(intent,env,competition,season);"
	pathBaseStart := strings.Index(sourceAuthorized, "  IF COALESCE(intent->>'candidate_path_base','')<>''")
	pathBaseEnd := strings.Index(sourceAuthorized, "  IF evaluated_round.staging IS DISTINCT FROM")
	if !strings.Contains(sourceAuthorized, projection) || pathBaseStart < 0 || pathBaseEnd <= pathBaseStart {
		t.Fatal("900 source authorization lacks its projection or path-base check")
	}
	withoutProjection := strings.Replace(sourceAuthorized, projection, "", 1)
	withoutPathBase := sourceAuthorized[:pathBaseStart] + sourceAuthorized[pathBaseEnd:]
	source := func(body string) string {
		return `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS $fault$` + body + `$fault$`
	}
	successorOwner := fmt.Sprintf("%s@v900", migrationCurrentSuffixTestContract(t, 900).artifact.name)
	predecessorOwner := fmt.Sprintf("%s@v897", migrationCurrentSuffixTestContract(t, 897).artifact.name)
	faults := []struct {
		name  string
		head  int
		sql   string
		owner string
	}{
		{"source body that 900 replaced restored", 901, source(server.CompetitionRepositoryPatchSourceBodySql()), successorOwner},
		{"source body without the instrument projection", 901, source(withoutProjection), successorOwner},
		{"source body without the path-base check", 901, source(withoutPathBase), successorOwner},
		{"source authority accepts everything", 901, source(" BEGIN RETURN; END "), successorOwner},
		{"source authority privilege changed", 901, `ALTER FUNCTION competition_v2_source_authorized(jsonb,text,text,text) SECURITY DEFINER`, successorOwner},
		{"lock digest absent", 901, `DROP FUNCTION competition_v2_instrument_lock_digest(jsonb)`, successorOwner},
		{"lock digest absent with same-name overload", 901, `DROP FUNCTION competition_v2_instrument_lock_digest(jsonb); CREATE FUNCTION competition_v2_instrument_lock_digest(commits text) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT '' $$`, successorOwner},
		{"lock digest omits development snapshots", 901, `CREATE OR REPLACE FUNCTION competition_v2_instrument_lock_digest(commits jsonb) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT encode(sha256(convert_to(commits::text,'UTF8')),'hex') $$`, successorOwner},
		{"lock digest volatile", 901, `ALTER FUNCTION competition_v2_instrument_lock_digest(jsonb) VOLATILE`, successorOwner},
		{"lock digest strict", 901, `ALTER FUNCTION competition_v2_instrument_lock_digest(jsonb) STRICT`, successorOwner},
		{"product intent absent", 901, `DROP FUNCTION competition_v2_instrument_product_intent(jsonb,text,text,text)`, successorOwner},
		{"product intent passes every intent through", 901, `CREATE OR REPLACE FUNCTION competition_v2_instrument_product_intent(intent jsonb, env text, competition text, season text) RETURNS jsonb LANGUAGE plpgsql AS $$ BEGIN RETURN intent; END $$`, successorOwner},
		{"product intent privilege changed", 901, `ALTER FUNCTION competition_v2_instrument_product_intent(jsonb,text,text,text) SECURITY DEFINER`, successorOwner},
		{"product intent configured", 901, `ALTER FUNCTION competition_v2_instrument_product_intent(jsonb,text,text,text) SET search_path = public`, successorOwner},
		{"retained policy dispatcher accepts everything", 901, `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid_for_policy(request jsonb,policy_name text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT true $$`, successorOwner},
		{"retained identity trigger disabled", 901, `ALTER TABLE competition_integrity_review DISABLE TRIGGER competition_v2_review_identity`, successorOwner},
		{"900 source body before its migration", 899, source(sourceAuthorized), predecessorOwner},
	}
	type observation struct {
		name, owner string
		alerts      []Alert
	}
	var observations []observation
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{899, 901} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("coherent prefix %d reports drift: %s", head, drift)
			}
			for _, fault := range faults {
				if fault.head != head {
					continue
				}
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s: %v", fault.name, err)
					}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
					if err != nil {
						t.Fatalf("inspect %s: %v", fault.name, err)
					}
					observations = append(observations, observation{fault.name, fault.owner, alerts})
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("rolled-back faults at %d left drift: %s", head, drift)
			}
		}
	})
	if len(observations) != len(faults) {
		t.Fatalf("observed %d trusted instrument faults, want %d", len(observations), len(faults))
	}
	for _, observed := range observations {
		alert := requireAlertClass(t, observed.alerts, "migration-schema-drift")
		if !strings.Contains(alert.Markdown(), observed.owner) {
			t.Fatalf("%s lost its owning migration: %s", observed.name, alert.Markdown())
		}
	}
}
