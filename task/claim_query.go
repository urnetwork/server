// Candidate eligibility and ordering are shared by the live cursor and its
// database plan controls. Target saturation is snapshotted once per claim;
// local reservations still arbitrate changes after that snapshot.
package task

import "fmt"

// Retain the complete existing eligibility boundary, including versioned
// aliases and scoped post retries, before the ordered fallback limit.
func (self *TaskWorker) claimCandidatesQuery(nowBlock int64, candidateLimit int, includeGroupArgs ...bool) (string, []any) {
	return self.taskCandidatesQuery(nowBlock, candidateLimit, false, includeGroupArgs...)
}

// Discovery takes no row lock. The live claimer admits each exact queue key
// before revalidating and locking that candidate on the same transaction.
func (self *TaskWorker) claimOwnershipCandidatesQuery(nowBlock int64, candidateLimit int, includeGroupArgs bool) (string, []any) {
	return self.taskCandidatesQuery(nowBlock, candidateLimit, true, includeGroupArgs)
}

func (self *TaskWorker) taskCandidatesQuery(nowBlock int64, candidateLimit int, ownershipFirst bool, includeGroupArgs ...bool) (string, []any) {
	groupArgsColumn := ""
	if len(includeGroupArgs) != 0 && includeGroupArgs[0] {
		groupArgsColumn = ", args_json"
	}
	rowLock := "FOR UPDATE SKIP LOCKED"
	if ownershipFirst {
		groupArgsColumn += ", run_once_key, available_block"
		rowLock = ""
	}
	claimPredicate := ""
	queryArgs := []any{nowBlock, candidateLimit}
	if self.settings.ClaimRegisteredTargetsOnly {
		functionNames := make([]string, 0, len(self.targets))
		for functionName := range self.targets {
			functionNames = append(functionNames, functionName)
		}
		// Match the same version-normalized aliases as dispatch. RunPost is a
		// wrapper: admitting its name alone would execute excluded post hooks.
		// Invalid or orphaned wrappers stay available to the ordinary worker;
		// they cannot poison this profile's queue or consume its candidate limit.
		claimPredicate = `
			AND CASE WHEN regexp_replace(function_name, '/v[0-9]+', '', 'g') = $4 THEN
				EXISTS (
					SELECT 1 FROM finished_task
					WHERE finished_task.task_id = CASE
						WHEN pg_input_is_valid(pending_task.args_json, 'jsonb')
						THEN CASE WHEN (pending_task.args_json::jsonb ->> 'task_id') ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
							THEN (pending_task.args_json::jsonb ->> 'task_id')::uuid
							END
						END
					AND regexp_replace(finished_task.function_name, '/v[0-9]+', '', 'g') = ANY($3)
				)
			ELSE regexp_replace(function_name, '/v[0-9]+', '', 'g') = ANY($3)
			END
		`
		queryArgs = append(queryArgs, functionNames, functionName(self.RunPost))
	}
	if excluded := self.saturatedClaimFunctionNames(); len(excluded) != 0 {
		claimPredicate += fmt.Sprintf(`
			AND NOT (regexp_replace(function_name, '/v[0-9]+', '', 'g') = ANY($%d))
		`, len(queryArgs)+1)
		queryArgs = append(queryArgs, excluded)
	}
	return `
			SELECT
				task_id,
				function_name,
				run_priority,
				run_max_time_seconds` + groupArgsColumn + `
			FROM pending_task
			WHERE available_block <= $1
		` + claimPredicate + `
			ORDER BY available_block, run_priority DESC, run_max_time_seconds DESC
			LIMIT $2
			` + rowLock + `
		`, queryArgs
}
