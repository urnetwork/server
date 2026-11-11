// Host readiness follows the configured grading round, not its admission
// window. Closing submissions cannot invalidate that round's frozen control.
package controller

import (
	"context"
	"encoding/json"
	"time"
)

// The existing caller owns this connection. One statement observes round and
// host identity together, without another pool checkout or source activation.
// API callers retain their existing canonical source/grant authentication;
// physical host readiness does not introduce another worker-ACK prerequisite.
func readCompetitionReadinessHosts(ctx context.Context, reader competitionIntakeReader, settings *Settings, now time.Time) (hosts, artifactHosts, rebaselineHosts int, err error) {
	image, target := "", ""
	qualifications := map[string]string{}
	if approval := settings.readinessSecurityToolchainException; approval != nil && approval.RoundId == settings.automationRoundId && approval.Epoch == settings.distributedRoundEpoch && approval.PreviousEvaluatorImageDigest == settings.EvaluatorImageDigest {
		image, target = approval.EvaluatorImageDigest, approval.RoundId.String()
		for _, host := range approval.Hosts {
			qualifications[host.HostId] = host.QualificationSha256
		}
	}
	qualificationJson, err := json.Marshal(qualifications)
	if err != nil {
		return 0, 0, 0, err
	}
	err = reader.QueryRow(ctx, `
		WITH configured_round AS (
			SELECT r.round_id,r.opens_at,r.finalized_at,r.automation_sealed_at,r.policy_json
			FROM competition_round r
			WHERE r.competition_id=$6 AND NOT r.canceled
			  AND CASE WHEN $7::boolean THEN
				r.staging=$8
				AND COALESCE((r.policy_json->'automation_policy'->>'enabled')::boolean,false)
				AND COALESCE((r.policy_json->'automation_policy'->>'staging')::boolean,false)=$8
				AND r.policy_json->'automation_policy'->>'environment'=$9
				AND r.policy_json->'automation_policy'->>'season_id'=$10
			  ELSE
				NOT r.staging OR (
					r.staging
					AND NOT COALESCE((r.policy_json->'automation_policy'->>'enabled')::boolean,false)
					AND NOT EXISTS (
						SELECT 1 FROM competition_round production
						WHERE production.competition_id=$6 AND NOT production.staging
					)
				)
			  END
			ORDER BY r.staging,r.epoch_number DESC
			LIMIT 1
		), current_round AS (
			SELECT round_id::text AS round_id
			FROM configured_round
			WHERE opens_at <= $5 AND finalized_at IS NULL AND automation_sealed_at IS NULL
			  AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)=$7
			  AND policy_json->>'evaluator_image_digest'=$2
			  AND policy_json->'evaluation_policy'->>'hardware_id'=$1
			  AND policy_json->'evaluation_policy'->>'host_qualification_sha256'=$3
		), execution_authority AS (
			SELECT $11::text<>'' AND EXISTS(SELECT 1 FROM current_round WHERE round_id=$13::text) AS hotfix
		)
		SELECT count(*),
		       count(*) FILTER (WHERE (self_check_json->>'artifact_storage')::boolean),
		       count(*) FILTER (
		           WHERE (self_check_json->>'rebaseline_passed')::boolean
		             AND self_check_json->>'rebaseline_round_id' =
		                 COALESCE((SELECT round_id FROM current_round),'')
		       )
		FROM competition_evaluator_host evaluator
		WHERE eligible=true AND hardware_id=$1
		  AND image_digest=CASE WHEN (SELECT hotfix FROM execution_authority) THEN $11 ELSE $2 END
		  AND CASE WHEN (SELECT hotfix FROM execution_authority) THEN
			$12::jsonb->>evaluator.host_id=self_check_json->>'qualification_sha256'
		  ELSE (self_check_json->>'qualification_sha256'=$3 OR EXISTS (
			SELECT 1 FROM current_round approved
			WHERE competition_executor_control_host(approved.round_id::uuid,evaluator.host_id)->>'qualification_sha256'
			  =self_check_json->>'qualification_sha256'
		  )) END
		  AND heartbeat_at >= $4
	`, settings.EvaluationPolicy.HardwareId, settings.EvaluatorImageDigest,
		settings.EvaluationPolicy.HostQualificationSha256,
		now.Add(-time.Duration(settings.HostHeartbeatMaxAgeSeconds)*time.Second), now,
		settings.CompetitionId, settings.AutomationPolicy.Enabled, settings.AutomationPolicy.Staging,
		settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId,
		image, string(qualificationJson), target).Scan(&hosts, &artifactHosts, &rebaselineHosts)
	return
}
