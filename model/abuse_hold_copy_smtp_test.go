//go:build sidecar_pending

package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtp-3's rows in §21.9's copy (abuse_hold_copy_smtp.go; ABUSE-M9-01) on MG9's table, in a build with
// sidecar_pending: a held network's day of send records waits for its hold's copy, the copy takes the rows that
// existed at placement and nothing more, each with smtp-3's fields alone, and the day then drops at 30 days.

// Writes send records of network starting at each time, the writer's clock just past the last.
func testHoldCopySmtpSends(t testing.TB, ctx context.Context, networkId server.Id, startTimes ...time.Time) {
	t.Helper()
	sends := []*smtpedgeservice.SmtpSendRecord{}
	for _, startTime := range startTimes {
		sends = append(sends, smtpTestSend(sidecar.Id(networkId), startTime))
	}
	result, err := insertSmtpSendsAt(ctx, sends, startTimes[len(startTimes)-1].Add(time.Minute), DefaultSmtpSendSettings())
	if err != nil || result.StoredCount != len(sends) {
		t.Fatalf("write %+v, %v", result, err)
	}
}

// Every copied send row of the hold, as its values.
func testHoldCopiedSmtpLines(t testing.TB, ctx context.Context, networkId server.Id, caseId server.Id) (lines [][]*string, columns []string) {
	t.Helper()
	settings := DefaultAbuseHoldCopySettings()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT copied_rows FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2 AND part > 0 ORDER BY part`, networkId, caseId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var body []byte
				server.Raise(result.Scan(&body))
				header, partLines, err := ReadHoldCopyPart(body, settings)
				server.Raise(err)
				if header.Kind != HoldCopyPartKindRows || header.Table != smtpSendTable {
					continue
				}
				columns = header.Columns
				for _, line := range partLines {
					values := []*string{}
					server.Raise(json.Unmarshal(line, &values))
					lines = append(lines, values)
				}
			}
		})
	})
	return
}

func TestHoldCopyKeepsAHeldSendDayUntilCopied(t *testing.T) {
	requireLegalHoldTables(t)
	if !server.SidecarMg9Linked() {
		t.Skip("smtp_send is MG9's: run with -tags sidecar_pending")
	}
	if !HoldCopyRegistered(smtpSendTable) {
		t.Fatal("smtp-3's rows are not registered for the copy")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 9, 0))
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		// another network's day before, which holds back nothing
		testHoldCopySmtpSends(t, ctx, server.NewId(), day.Add(-15*time.Hour))
		// two sends of one start, which the copy's cursor tells apart by send_id
		testHoldCopySmtpSends(t, ctx, networkId, day.Add(9*time.Hour), day.Add(9*time.Hour), day.Add(10*time.Hour))
		placeTime := day.Add(12 * time.Hour)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		// written after the placement: new collection, which the copy never takes
		testHoldCopySmtpSends(t, ctx, networkId, day.Add(14*time.Hour), day.Add(30*time.Hour))
		heldLeaf := "smtp_send_p" + day.Format("20060102")
		beforeLeaf := "smtp_send_p" + day.AddDate(0, 0, -1).Format("20060102")
		afterLeaf := "smtp_send_p" + day.AddDate(0, 0, 1).Format("20060102")
		now := day.AddDate(0, 0, 32)

		first := RetireSmtpSendDays(ctx, now, time.Now().Add(time.Minute))
		if slices.Contains(first.Dropped, heldLeaf) || !strings.Contains(first.Blocked[heldLeaf], "has not copied") ||
			!strings.Contains(first.Blocked[heldLeaf], caseId.String()) {
			t.Fatalf("with the copy pending: %+v", first)
		}
		if !slices.Contains(first.Dropped, beforeLeaf) || !slices.Contains(first.Dropped, afterLeaf) {
			t.Fatalf("the unheld days: %+v", first)
		}

		complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings())
		if err != nil || !complete {
			t.Fatalf("%t, %v", complete, err)
		}
		lines, columns := testHoldCopiedSmtpLines(t, ctx, networkId, caseId)
		if len(lines) != 3 {
			t.Fatalf("copied %d rows, want the 3 before the placement", len(lines))
		}
		networkIndex := slices.Index(columns, "network_id")
		startIndex := slices.Index(columns, "start_time")
		if networkIndex < 0 || startIndex < 0 || len(columns) != 18 {
			t.Fatalf("the copy's columns %v", columns)
		}
		sendIds := map[string]bool{}
		for _, line := range lines {
			if len(line) != len(columns) || line[networkIndex] == nil || *line[networkIndex] != networkId.String() {
				t.Fatalf("a copied row %v", line)
			}
			startTime, err := time.Parse("2006-01-02 15:04:05", *line[startIndex])
			if err != nil || !startTime.Before(placeTime) {
				t.Fatalf("a copied row started at %v (%v), after the placement", startTime, err)
			}
			sendIds[*line[slices.Index(columns, "send_id")]] = true
		}
		if len(sendIds) != 3 {
			t.Fatalf("the copy took %d distinct sends", len(sendIds))
		}

		second := RetireSmtpSendDays(ctx, now, time.Now().Add(time.Minute))
		if !slices.Contains(second.Dropped, heldLeaf) {
			t.Fatalf("after the copy: %+v", second)
		}
	})
}
