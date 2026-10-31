// The internal completed-score projection uses a bounded same-round index
// walk. This additive index does not change evaluator, job or round semantics.
package server

const competitionSubmissionMetricsMigrationSql = `
SET LOCAL lock_timeout = '5s';
CREATE INDEX competition_job_completed_score_metrics_idx ON competition_job (round_id,job_id)
WHERE state IN ('succeeded','failed') AND score_json IS NOT NULL;
`
