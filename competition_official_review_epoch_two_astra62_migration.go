// The epoch-two reviewer ID append changes no retained rows or import authority.
package server

import "strings"

func competitionOfficialReviewEpochTwoAstra62Migration() string {
	return `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION competition_official_review_epoch_two_findings_valid(request jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $official_two_findings$` +
		CompetitionOfficialReviewEpochTwoAstra62FindingsBodySql() + "$official_two_findings$;\n"
}

// The original ID remains exact for stored reviews. A missing ID still fails
// closed; every other predicate is byte-identical to the published definition.
func CompetitionOfficialReviewEpochTwoAstra62FindingsBodySql() string {
	const old = `request->>'reviewer_model' IS DISTINCT FROM 'gpt-6-astra'`
	const next = `(request->>'reviewer_model' IS DISTINCT FROM 'gpt-6-astra' AND request->>'reviewer_model' IS DISTINCT FROM 'gpt-6.2-astra')`
	body := CompetitionOfficialReviewEpochTwoBodiesSql().FindingsValid
	if strings.Count(body, old) != 1 {
		panic("epoch-two Astra 6.2 reviewer predicate predecessor absent or ambiguous")
	}
	return strings.Replace(body, old, next, 1)
}

// The append's position follows the actual published schema prefix.
func CompetitionOfficialReviewEpochTwoAstra62MigrationVersion() int {
	sql := competitionOfficialReviewEpochTwoAstra62Migration()
	version := 0
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			if version != 0 {
				panic("epoch-two Astra 6.2 reviewer migration registered twice")
			}
			version = index + 1
		}
	}
	return version
}
