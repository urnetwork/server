package server

import "strings"

// Keep the published SQL and order byte-identical. Locate the four existing
// migration identities by a unique DDL anchor, never by a mutable list index.
func LagDriftMigrationVersions() [4]int {
	anchors := [...]string{
		"CREATE TABLE IF NOT EXISTS lag_drift_test (",
		"CREATE TABLE IF NOT EXISTS lag_drift_calibration (",
		"ADD COLUMN IF NOT EXISTS client_ip_country_code varchar(2) NULL,",
		"ADD COLUMN IF NOT EXISTS k_early double precision NULL;",
	}
	var versions [len(anchors)]int
	for i, migration := range migrations {
		sql, ok := migration.(*SqlMigration)
		if !ok {
			continue
		}
		for j, anchor := range anchors {
			if !strings.Contains(sql.sql, anchor) {
				continue
			}
			if versions[j] != 0 {
				panic("ambiguous lag-drift migration anchor: " + anchor)
			}
			versions[j] = i + 1
		}
	}
	for i, version := range versions {
		if version == 0 || i > 0 && versions[i-1] >= version {
			panic("missing or out-of-order lag-drift migration: " + anchors[i])
		}
	}
	return versions
}
