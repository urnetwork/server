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

	"github.com/urnetwork/server"
)

// mx-2's rows in §21.9's copy (abuse_hold_copy_mail.go; ABUSE-M7-01) on MG7's table, in a build with
// sidecar_pending: a held network's hour of mail records waits for its hold's copy, the copy takes the rows that
// existed at placement and nothing more, holding no sender address, and the hour then drops.

// Writes mx-2 rows of network starting at each time.
func testHoldCopyMailRecords(t testing.TB, ctx context.Context, networkId server.Id, startTimes ...time.Time) {
	t.Helper()
	records := []*MailConnectionRecord{}
	for _, startTime := range startTimes {
		records = append(records, &MailConnectionRecord{NetworkId: networkId, StartTime: startTime, ByteCount: 4096, Result: "delivered"})
	}
	result, err := writeMailConnectionRecordsAt(ctx, records, startTimes[len(startTimes)-1].Add(time.Minute), DefaultMailConnectionSettings())
	if err != nil || result.WrittenCount != len(records) {
		t.Fatalf("write %+v, %v", result, err)
	}
}

func TestHoldCopyKeepsAHeldMailHourUntilCopied(t *testing.T) {
	requireLegalHoldTables(t)
	if !server.SidecarMg7Linked() {
		t.Skip("mail_connection_record is MG7's: run with -tags sidecar_pending")
	}
	if !HoldCopyRegistered(mailConnectionRecordTable) {
		t.Fatal("mx-2's rows are not registered for the copy")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		family := mailConnectionRecordPartitionFamily
		hour := testHoldCopyHour(9)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		testHoldCopyMailRecords(t, ctx, server.NewId(), hour.Add(-30*time.Minute))
		// two sessions of one start, which the copy's cursor tells apart
		testHoldCopyMailRecords(t, ctx, networkId, hour.Add(10*time.Minute), hour.Add(10*time.Minute), hour.Add(20*time.Minute))
		placeTime := hour.Add(30 * time.Minute)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		testHoldCopyMailRecords(t, ctx, networkId, hour.Add(40*time.Minute), hour.Add(70*time.Minute))
		heldLeaf := family.LeafName(mailConnectionRecordTable, server.PartitionRange{Lower: hour, Upper: hour.Add(time.Hour)})

		report, _ := family.Retire(ctx, hour.Add(26*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if slices.Contains(report.Dropped, heldLeaf) || !strings.Contains(report.Blocked[heldLeaf], "has not copied") {
			t.Fatalf("dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
		if len(report.Dropped) != 2 {
			t.Fatalf("the unheld hours: dropped %v", report.Dropped)
		}

		if complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings()); err != nil || !complete {
			t.Fatalf("%t, %v", complete, err)
		}
		lines := testHoldCopiedMailLines(t, ctx, networkId, caseId)
		if len(lines) != 3 {
			t.Fatalf("copied %d rows, want the 3 before the placement", len(lines))
		}
		for _, line := range lines {
			// network, start, bytes and result: nothing else
			if len(line) != 4 || line[0] != networkId.String() || line[3] != "delivered" {
				t.Fatalf("a copied row %v", line)
			}
		}
		report, _ = family.Retire(ctx, hour.Add(26*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if !slices.Equal(report.Dropped, []string{heldLeaf}) {
			t.Fatalf("after the copy: dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
	})
}

// Every copied mail row of the hold, as its values.
func testHoldCopiedMailLines(t testing.TB, ctx context.Context, networkId server.Id, caseId server.Id) (lines [][]string) {
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
				if header.Kind != HoldCopyPartKindRows || header.Table != mailConnectionRecordTable {
					continue
				}
				for _, line := range partLines {
					values := []string{}
					server.Raise(json.Unmarshal(line, &values))
					lines = append(lines, values)
				}
			}
		})
	})
	return
}

// §21's zone suspension, under the reason its effect writes (controller's abuse:<case_id>), withdraws the network's
// mail route at the router's next pass, so its MX takes no mail, and the lift through the case puts the route back.
func TestAbuseZoneSuspensionWithdrawsTheMailRoute(t *testing.T) {
	if !server.SidecarMg7Linked() {
		t.Skip("mail routes are MG7's: run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		if _, err := EnableMail(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		pass := testMailRoutePass(t, ctx, root, nil)
		if testMailRoute(t, ctx, label) == nil {
			t.Fatal("no route before the suspension")
		}
		if err := SuspendZone(ctx, networkId, "abuse:"+server.NewId().String(), nil); err != nil {
			t.Fatal(err)
		}
		pass = testMailRoutePass(t, ctx, root, pass.Cursor)
		if testMailRoute(t, ctx, label) != nil {
			t.Fatalf("a network §21 suspended still routes: %+v", pass)
		}
		if err := Restore(ctx, networkId, ""); err != nil {
			t.Fatal(err)
		}
		testMailRoutePass(t, ctx, root, pass.Cursor)
		if testMailRoute(t, ctx, label) == nil {
			t.Fatal("the lift left the network unrouted")
		}
	})
}
