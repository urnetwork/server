// The fleet URL outcome bucket migration (db_provider_egress_url_fleet_bucket.go),
// read back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the
// referral terms, and its version is its position.
func TestProviderEgressUrlFleetBucketMigrationIsAppended(t *testing.T) {
	version := ProviderEgressUrlFleetBucketVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != providerEgressUrlFleetBucketMigration {
		t.Fatalf("URL fleet bucket migration version %d is not its position", version)
	}
	if referral := sqlMigrationVersion(networkReferralTermsSchemaSql); referral < 1 || version <= referral {
		t.Fatalf("URL fleet bucket migration %d is not after the referral terms migration %d", version, referral)
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly the bucket's columns; nothing existing is altered,
// rewritten, sampled or deleted.
func TestProviderEgressUrlFleetBucketMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := providerEgressUrlFleetBucketMigration.sql
	for _, marker := range []string{
		`IF to_regclass('public.provider_egress_url_fleet_bucket') IS NULL THEN`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE TABLE provider_egress_url_fleet_bucket (`,
		`bucket_start timestamp NOT NULL,`,
		`bucket_end timestamp NOT NULL,`,
		`outcome_count integer NOT NULL,`,
		`error_count integer NOT NULL,`,
		`country_outcome_count integer NOT NULL,`,
		`country_error_count integer NOT NULL,`,
		`network_outcome_count integer NOT NULL,`,
		`network_error_count integer NOT NULL,`,
		`baseline_outcome_count bigint NOT NULL,`,
		`baseline_error_count bigint NOT NULL,`,
		`discounted boolean NOT NULL,`,
		`discounted_outcome_count integer NOT NULL,`,
		`discounted_error_count integer NOT NULL,`,
		`judge_time timestamp NOT NULL,`,
		`PRIMARY KEY (bucket_start)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("URL fleet bucket migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("URL fleet bucket migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("URL fleet bucket migration touches more than its own new table")
	}
}
