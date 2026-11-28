package monitor

// v860 keeps the weekly bloat measurement's raw rows
// (server.dbBloatMeasurementTableSql, PARTITION-MAINTENANCE-PLAN.md A4); the
// measure_time index serves its 90-day retention delete.
const dbBloatMeasurementArtifactQuery = `(to_regclass('public.db_bloat_measurement') IS NOT NULL
		AND to_regclass('public.db_bloat_measurement_measure_time') IS NOT NULL)`
