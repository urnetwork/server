// Invalid indexes and how long each has stayed invalid, from the hot vacuum
// chain (db_maintenance_hot_vacuum.go), for the DbInvalidIndexLingering alert
// (warp grafana/alerting/db-invalid-index.yml).
//
// A concurrent build (CREATE INDEX or REINDEX CONCURRENTLY) adds the index
// invalid and validates it last, and DROP INDEX CONCURRENTLY marks it invalid
// first. One interrupted at the wrong moment leaves an invalid index behind,
// such as a rebuild's _ccnew copy: every write maintains it, no read uses it,
// and it keeps its space until it is dropped. The rotation and the hot vacuum
// drop such debris when it is provably the copy of a valid index
// (dbMaintenanceDropReindexDebris), and a cleanup that fails or a leftover of
// another kind stays. On Main (2026-11-26) a _ccnew copy left by an
// interrupted rebuild of a debit journal leaf stayed invalid with 129 MB.
//
// pg_index records no time, so each run of the chain lists the invalid
// indexes and carries, in the chain's state, when a run first found each one:
// an index found invalid in consecutive runs keeps its time, one no longer
// found is forgotten, and one dropped and created again under the same oid
// with another name starts again. An index whose table has a build in progress
// (pg_stat_progress_create_index) is being built, not left over, and is not
// listed. Each run sets the count, the longest time any of them has been
// invalid and the run's time; each taskworker process keeps the gauges of the
// last run it made, so the alert reads the process with the newest run.
package server

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
)

// One invalid index the chain found, and when a run first found it invalid.
type DbHotVacuumInvalidIndex struct {
	Name  string    `json:"name"`
	Table string    `json:"table"`
	Since time.Time `json:"since"`
}

// Every invalid index of a user schema whose table has no index build in
// progress, by oid: partitioned indexes (relkind I) and indexes being dropped
// included, temporary tables' not.
const dbInvalidIndexesQuery = `
	SELECT index_class.oid::bigint, index_namespace.nspname, index_class.relname, table_class.relname
	FROM pg_index AS index_record
	INNER JOIN pg_class AS index_class ON index_class.oid = index_record.indexrelid
	INNER JOIN pg_namespace AS index_namespace ON index_namespace.oid = index_class.relnamespace
	INNER JOIN pg_class AS table_class ON table_class.oid = index_record.indrelid
	WHERE NOT index_record.indisvalid
	  AND index_namespace.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND index_namespace.nspname NOT LIKE 'pg\_toast%'
	  AND index_namespace.nspname NOT LIKE 'pg\_temp%'
	  AND NOT EXISTS (
		SELECT 1
		FROM pg_stat_progress_create_index AS progress
		LEFT JOIN pg_class AS toast_owner ON toast_owner.reltoastrelid = progress.relid
		WHERE progress.datname = current_database()
		  AND (coalesce(toast_owner.oid, progress.relid) = index_record.indrelid
		       OR progress.index_relid = index_record.indexrelid)
	  )
	ORDER BY index_namespace.nspname, index_class.relname
`

var dbInvalidIndexesGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "db",
	Name:      "invalid_indexes",
	Help:      "Invalid indexes that no build in progress owns, at this process's last hot vacuum run",
})

var dbInvalidIndexMaxAgeGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "db",
	Name:      "invalid_index_max_age_seconds",
	Help:      "The longest any of those indexes has been found invalid by the hot vacuum chain, 0 when none, at this process's last run",
})

var dbInvalidIndexSampleTimeGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "db",
	Name:      "invalid_index_sample_timestamp_seconds",
	Help:      "When this process's hot vacuum run last listed the invalid indexes",
})

func init() {
	prometheus.MustRegister(dbInvalidIndexesGauge, dbInvalidIndexMaxAgeGauge, dbInvalidIndexSampleTimeGauge)
}

// One invalid index as a run listed it.
type dbInvalidIndex struct {
	oid    int64
	schema string
	name   string
	table  string
}

func readDbInvalidIndexes(ctx context.Context, pool *safePgPool) ([]dbInvalidIndex, error) {
	conn, err := acquirePgPoolConn(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, dbInvalidIndexesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indexes := []dbInvalidIndex{}
	for rows.Next() {
		var index dbInvalidIndex
		if err := rows.Scan(&index.oid, &index.schema, &index.name, &index.table); err != nil {
			return nil, err
		}
		indexes = append(indexes, index)
	}
	return indexes, rows.Err()
}

// The chain's invalid indexes after a run listed indexes at now: an index
// found before under the same oid and name keeps when it was first found.
func nextDbHotVacuumInvalidIndexes(previous map[string]DbHotVacuumInvalidIndex, indexes []dbInvalidIndex, now time.Time) map[string]DbHotVacuumInvalidIndex {
	next := map[string]DbHotVacuumInvalidIndex{}
	for _, index := range indexes {
		key := strconv.FormatInt(index.oid, 10)
		name := index.schema + "." + index.name
		entry := DbHotVacuumInvalidIndex{Name: name, Table: index.table, Since: now}
		if found, ok := previous[key]; ok && found.Name == name && !found.Since.IsZero() && !now.Before(found.Since) {
			entry.Since = found.Since
		}
		next[key] = entry
	}
	if len(next) == 0 {
		return nil
	}
	return next
}

// The longest any of the indexes has been invalid at now.
func dbHotVacuumInvalidIndexMaxAge(indexes map[string]DbHotVacuumInvalidIndex, now time.Time) time.Duration {
	var age time.Duration
	for _, index := range indexes {
		age = max(age, now.Sub(index.Since))
	}
	return age
}

// Lists the invalid indexes, carries when each was first found in the state,
// sets the gauges and logs any found. A run that cannot list them leaves the
// state and the gauges as they were.
func (self *dbHotVacuumRun) observeInvalidIndexes(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	indexes, err := readDbInvalidIndexes(ctx, self.pool)
	if err != nil {
		glog.Infof("[db]hot vacuum could not list the invalid indexes: %s\n", err)
		return
	}
	now := self.now().UTC()
	self.state.InvalidIndexes = nextDbHotVacuumInvalidIndexes(self.state.InvalidIndexes, indexes, now)
	dbInvalidIndexesGauge.Set(float64(len(self.state.InvalidIndexes)))
	dbInvalidIndexMaxAgeGauge.Set(dbHotVacuumInvalidIndexMaxAge(self.state.InvalidIndexes, now).Seconds())
	dbInvalidIndexSampleTimeGauge.Set(float64(now.UnixNano()) / float64(time.Second))
	if len(self.state.InvalidIndexes) == 0 {
		return
	}
	texts := []string{}
	for _, index := range self.state.InvalidIndexes {
		texts = append(texts, fmt.Sprintf("%s on %s invalid for %s", index.Name, index.Table, now.Sub(index.Since).Round(time.Second)))
	}
	slices.Sort(texts)
	glog.Warningf("[db]hot vacuum found %d invalid index(es) no build owns: %s\n", len(texts), strings.Join(texts, "; "))
}
