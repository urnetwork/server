package server

// Equality on the same normalized function expression leaves a bounded ordered
// due-range seek. The existing global poll index remains the ordinary lane.
const TaskClaimFunctionIndexSql = `CREATE INDEX pending_task_function_poll_order
ON pending_task (regexp_replace(function_name, '/v[0-9]+', '', 'g'),
    available_block, run_priority DESC, run_max_time_seconds DESC, task_id)`
