package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.10's retention (abuse_retention_model.go; ABUSE-M5-16) on MG5's tables on an injected clock, in a build with
// sidecar_pending: each ttl at its boundary beside a twin a day inside it, a held network's rows kept until its hold
// ends, an open case never swept, an access-log row outliving its case, a stopped pass resuming, standing rows gone
// past their until_time, and a held network's deletion copying what it removes.

// The clock the retention cases run at.
var testRetentionNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// A case of network in state, closed at closeTime when not nil, with one action and one appeal.
func testRetentionCase(t testing.TB, ctx context.Context, networkId server.Id, state string, closeTime *time.Time) server.Id {
	t.Helper()
	caseId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO abuse_case (case_id, network_id, category, target_kind, target, reporter_class, join_kind, join_key, state, create_time, close_time)
			VALUES ($1, $2, 'phishing', 'host', 'shop.root.example', 'web_risk', 'host', 'shop.root.example', $3, $4, $5)
		`, caseId, networkId, state, testRetentionNow.AddDate(-5, 0, 0), closeTime))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO abuse_action (action_id, case_id, network_id, category, kind, target, by_machine, review_state, create_time)
			VALUES ($1, $2, $3, 'phishing', 'host_kill', 'shop.root.example', true, 'upheld', $4)
		`, server.NewId(), caseId, networkId, testRetentionNow.AddDate(-5, 0, 0)))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO abuse_appeal (appeal_id, case_id, network_id, user_id, reason, state, create_time)
			VALUES ($1, $2, $3, $4, 'not mine', 'denied', $5)
		`, server.NewId(), caseId, networkId, server.NewId(), testRetentionNow.AddDate(-5, 0, 0)))
	})
	return caseId
}

// A legal request naming networkIds, closed at closeTime.
func testRetentionLegalRequest(t testing.TB, ctx context.Context, caseId server.Id, networkIds []server.Id, closeTime time.Time) server.Id {
	t.Helper()
	requestId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO legal_request (request_id, case_id, instrument, country, network_ids, production_state, create_time, close_time)
			VALUES ($1, $2, 'subpoena', 'US', $3, 'produced', $4, $5)
		`, requestId, caseId, networkIds, closeTime.AddDate(0, -1, 0), closeTime))
	})
	return requestId
}

// An access-log row naming caseId, read at accessTime.
func testRetentionAccessLog(t testing.TB, ctx context.Context, caseId server.Id, accessTime time.Time) server.Id {
	t.Helper()
	logId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO abuse_access_log (log_id, reviewer_id, lookup_kind, case_id, access_time)
			VALUES ($1, $2, 'case', $3, $4)
		`, logId, server.NewId(), caseId, accessTime))
	})
	return logId
}

// Whether a row of table has the key.
func testRetentionExists(t testing.TB, ctx context.Context, table string, keyColumn string, key server.Id) (exists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE `+keyColumn+` = $1)`, key).Scan(&exists))
	})
	return
}

// A hold row on network, live until expireTime, placed at placeTime.
func testRetentionHold(t testing.TB, ctx context.Context, networkId server.Id, placeTime time.Time, expireTime time.Time) server.Id {
	t.Helper()
	caseId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO abuse_legal_hold (network_id, case_id, part, kind, scope, place_time, expire_time, copy_state)
			VALUES ($1, $2, 0, 'preserve_2703f', '"network"', $3, $4, 'copied')
		`, networkId, caseId, placeTime, expireTime))
	})
	return caseId
}

// Each ttl at its boundary: a case closed a day past 13 months goes with its action and appeal while its twin a
// day inside stays; a legal request and an access-log row a day past 3 years go and their twins stay; an open case
// goes never; an access-log row outlives the case it names.
func TestAbuseRetentionTtlsAtTheirBoundaries(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		caseCutoff, legalCutoff := abuseRetentionCutoffs(testRetentionNow, settings)
		networkId := server.NewId()

		pastCase := testRetentionCase(t, ctx, networkId, "closed", new(caseCutoff.AddDate(0, 0, -1)))
		insideCase := testRetentionCase(t, ctx, networkId, "closed", new(caseCutoff.AddDate(0, 0, 1)))
		openCase := testRetentionCase(t, ctx, networkId, "open", new(caseCutoff.AddDate(-2, 0, 0)))
		pastRequest := testRetentionLegalRequest(t, ctx, pastCase, []server.Id{networkId}, legalCutoff.AddDate(0, 0, -1))
		insideRequest := testRetentionLegalRequest(t, ctx, pastCase, []server.Id{networkId}, legalCutoff.AddDate(0, 0, 1))
		pastLog := testRetentionAccessLog(t, ctx, pastCase, legalCutoff.AddDate(0, 0, -1))
		insideLog := testRetentionAccessLog(t, ctx, pastCase, legalCutoff.AddDate(0, 0, 1))

		result := RemoveExpiredAbuseRows(ctx, testRetentionNow, settings)
		if result.CaseCount != 1 || result.ActionCount != 1 || result.AppealCount != 1 || result.LegalRequestCount != 1 || result.AccessLogCount != 1 || result.More {
			t.Fatalf("%+v", result)
		}
		for _, check := range []struct {
			table, column string
			key           server.Id
			exists        bool
		}{
			{"abuse_case", "case_id", pastCase, false},
			{"abuse_action", "case_id", pastCase, false},
			{"abuse_appeal", "case_id", pastCase, false},
			{"abuse_case", "case_id", insideCase, true},
			{"abuse_action", "case_id", insideCase, true},
			{"abuse_case", "case_id", openCase, true},
			{"legal_request", "request_id", pastRequest, false},
			{"legal_request", "request_id", insideRequest, true},
			{"abuse_access_log", "log_id", pastLog, false},
			// its case is gone, its own ttl is not
			{"abuse_access_log", "log_id", insideLog, true},
		} {
			if got := testRetentionExists(t, ctx, check.table, check.column, check.key); got != check.exists {
				t.Errorf("%s %s: exists %t", check.table, check.key, got)
			}
		}
	})
}

// A held network's rows past their ttls stay while the hold stands, and go once it has ended; a legal request
// naming a held network beside another stays too.
func TestAbuseRetentionSkipsAHeldNetworkUntilItsHoldEnds(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		caseCutoff, legalCutoff := abuseRetentionCutoffs(testRetentionNow, settings)
		heldNetwork := server.NewId()
		otherNetwork := server.NewId()
		expireTime := testRetentionNow.AddDate(0, 0, 30)
		testRetentionHold(t, ctx, heldNetwork, testRetentionNow.AddDate(0, 0, -60), expireTime)

		heldCase := testRetentionCase(t, ctx, heldNetwork, "closed", new(caseCutoff.AddDate(0, 0, -10)))
		otherCase := testRetentionCase(t, ctx, otherNetwork, "closed", new(caseCutoff.AddDate(0, 0, -10)))
		heldRequest := testRetentionLegalRequest(t, ctx, heldCase, []server.Id{otherNetwork, heldNetwork}, legalCutoff.AddDate(0, 0, -10))
		heldLog := testRetentionAccessLog(t, ctx, heldCase, legalCutoff.AddDate(0, 0, -10))

		result := RemoveExpiredAbuseRows(ctx, testRetentionNow, settings)
		if result.CaseCount != 1 || result.LegalRequestCount != 0 || result.AccessLogCount != 0 {
			t.Fatalf("under the hold: %+v", result)
		}
		if testRetentionExists(t, ctx, "abuse_case", "case_id", otherCase) || !testRetentionExists(t, ctx, "abuse_case", "case_id", heldCase) ||
			!testRetentionExists(t, ctx, "legal_request", "request_id", heldRequest) || !testRetentionExists(t, ctx, "abuse_access_log", "log_id", heldLog) {
			t.Fatal("the sweep did not skip exactly the held network's rows")
		}

		// the hold ends: the sweep resumes with no notice
		result = RemoveExpiredAbuseRows(ctx, expireTime, settings)
		if result.CaseCount != 1 || result.LegalRequestCount != 1 || result.AccessLogCount != 1 {
			t.Fatalf("after the hold: %+v", result)
		}
	})
}

// A pass stopped by its bound after one batch leaves the rest, which the next pass takes.
func TestAbuseRetentionResumesAStoppedPass(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		settings.RetentionBatchSize = 1
		settings.RetentionPassMaxTime = 0
		caseCutoff, _ := abuseRetentionCutoffs(testRetentionNow, settings)
		networkId := server.NewId()
		caseIds := []server.Id{}
		for i := range 3 {
			caseIds = append(caseIds, testRetentionCase(t, ctx, networkId, "closed", new(caseCutoff.AddDate(0, 0, -1-i))))
		}
		result := RemoveExpiredAbuseRows(ctx, testRetentionNow, settings)
		if result.CaseCount != 1 || !result.More {
			t.Fatalf("the stopped pass: %+v", result)
		}
		settings.RetentionPassMaxTime = time.Minute
		result = RemoveExpiredAbuseRows(ctx, testRetentionNow, settings)
		if result.CaseCount != 2 || result.More {
			t.Fatalf("the resumed pass: %+v", result)
		}
		for _, caseId := range caseIds {
			if testRetentionExists(t, ctx, "abuse_case", "case_id", caseId) {
				t.Fatal("a case stayed")
			}
		}
	})
}

// A restricted row past its until_time goes at the standing pass, which keeps §21.7's state only while current.
func TestAbuseStandingRowGoesPastItsUntilTime(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "standing-"+networkId.String()[24:], server.NewId())
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_abuse_state (network_id, state, case_id, until_time, update_time)
				VALUES ($1, 'restricted', $2, $3, $4)
			`, networkId, server.NewId(), now.Add(-time.Hour), now.AddDate(0, 0, -31)))
		})
		RunAbuseStandingPass(ctx, now, DefaultAbuseSettings())
		if testRetentionExists(t, ctx, "network_abuse_state", "network_id", networkId) {
			t.Fatal("a standing row stayed past its until_time")
		}
	})
}

// A held network's deletion reads as done, and each live hold keeps the rows it removed, every credential left
// out; an unheld network's deletion writes nothing to the holds.
func TestRemoveNetworkUnderAHoldCopiesWhatItRemoves(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		networkName := "held-" + networkId.String()[24:]
		userAuth := Testing_CreateNetwork(ctx, networkId, networkName, server.NewId())
		caseId := testRetentionHold(t, ctx, networkId, server.NowUtc().AddDate(0, 0, -1), server.NowUtc().AddDate(0, 0, 80))
		expiredCaseId := testRetentionHold(t, ctx, networkId, server.NowUtc().AddDate(0, -4, 0), server.NowUtc().AddDate(0, 0, -1))

		if removed, _ := RemoveNetwork(ctx, networkId, nil); !removed {
			t.Fatal("the deletion was refused")
		}
		settings := DefaultAbuseHoldCopySettings()
		tables := map[string][][]*string{}
		var credentialSeen bool
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT copied_rows FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2 AND part > 0 ORDER BY part`, networkId, caseId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var body []byte
					server.Raise(result.Scan(&body))
					header, lines, err := ReadHoldCopyPart(body, settings)
					server.Raise(err)
					if header.Kind != HoldCopyPartKindDeletion {
						t.Errorf("a part of kind %s", header.Kind)
					}
					for _, column := range header.Columns {
						if strings.Contains(column, "hash") || strings.Contains(column, "salt") || strings.Contains(column, "jwt") || strings.Contains(column, "lookup") {
							credentialSeen = true
						}
					}
					for _, line := range lines {
						values := []*string{}
						server.Raise(json.Unmarshal(line, &values))
						tables[header.Table] = append(tables[header.Table], values)
					}
				}
			})
		})
		if credentialSeen {
			t.Fatal("a hold kept a credential column")
		}
		networkRows := tables["network"]
		if len(networkRows) != 1 || *networkRows[0][1] != networkName {
			t.Fatalf("the network row: %v", tables)
		}
		userRows := tables["network_user"]
		if len(userRows) != 1 || userRows[0][3] == nil || *userRows[0][3] != userAuth {
			t.Fatalf("the user rows: %v", userRows)
		}
		var expiredParts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2 AND part > 0`, networkId, expiredCaseId).Scan(&expiredParts))
		})
		if expiredParts != 0 {
			t.Fatalf("an ended hold took %d parts", expiredParts)
		}

		unheldId := server.NewId()
		Testing_CreateNetwork(ctx, unheldId, "unheld-"+unheldId.String()[24:], server.NewId())
		if removed, _ := RemoveNetwork(ctx, unheldId, nil); !removed {
			t.Fatal("an unheld deletion was refused")
		}
		var parts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_legal_hold WHERE network_id = $1`, unheldId).Scan(&parts))
		})
		if parts != 0 {
			t.Fatalf("an unheld deletion wrote %d hold rows", parts)
		}
	})
}
