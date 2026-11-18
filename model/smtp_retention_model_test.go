//go:build sidecar_pending

package model

// smtpedge's retention on a private database (SMTP-M9-17; §14.14, §21.9): smtp_send's days made ahead and dropped
// at 30 days, a held network's day kept while its hold lives and dropped once it ends; the 13-month rows going and a
// held network's staying; sealed copies cleared at 30 days with their rows kept; a retired selector out of DNS at 7
// days and its row 30 days after that, not before, a burner's copy rows going with its last selector; and the RUA
// check's page of one day's networks.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// The leaves of smtp_send, by name.
func smtpTestSendLeaves(t testing.TB, ctx context.Context) []string {
	t.Helper()
	names := []string{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		leaves, err := smtpSendPartitionFamily.ListLeaves(ctx, conn, "smtp_send")
		if err != nil {
			t.Fatal(err)
		}
		for _, leaf := range leaves {
			names = append(names, leaf.Name)
		}
	})
	slices.Sort(names)
	return names
}

// The runway leaves every day of its window, AheadDays ahead, and an expired day drops once its 30 days have
// passed after its end, a day short of them staying.
func TestSmtpSendDaysAreMadeAheadAndDroppedAtThirtyDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpSendSettings()
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(1, 0, 0))
		report := EnsureSmtpSendRunway(ctx, day.Add(12*time.Hour))
		if len(report.Errors) != 0 || !report.CurrentCovered || report.FutureRanges < settings.AheadDays {
			t.Fatalf("runway %+v", report)
		}
		leaves := smtpTestSendLeaves(t, ctx)
		for ahead := 0; ahead <= settings.AheadDays; ahead++ {
			name := "smtp_send_p" + day.AddDate(0, 0, ahead).Format("20060102")
			if !slices.Contains(leaves, name) {
				t.Fatalf("no %s in %v", name, leaves)
			}
		}

		networkId := sidecar.NewId()
		old := smtpTestSend(networkId, day.Add(10*time.Hour))
		young := smtpTestSend(networkId, day.AddDate(0, 0, 1).Add(10*time.Hour))
		if result, err := insertSmtpSendsAt(ctx, []*smtpedgeservice.SmtpSendRecord{old, young}, young.StartTime, settings); err != nil || result.StoredCount != 2 {
			t.Fatalf("write %+v, %v", result, err)
		}
		// the first day ends at day+1d: 30 days after that it expires, the second a day later
		now := day.AddDate(0, 0, 31)
		retired := RetireSmtpSendDays(ctx, now, time.Now().Add(time.Minute))
		oldLeaf := "smtp_send_p" + day.Format("20060102")
		youngLeaf := "smtp_send_p" + day.AddDate(0, 0, 1).Format("20060102")
		if !slices.Contains(retired.Dropped, oldLeaf) || slices.Contains(retired.Dropped, youngLeaf) || len(retired.Blocked) != 0 {
			t.Fatalf("retired %+v", retired)
		}
		sends, err := FindSmtpSends(ctx, &smtpedgeservice.SmtpJoinKey{NetworkId: networkId})
		if err != nil || len(sends) != 1 || string(sends[0].SendId) != string(young.SendId) {
			t.Fatalf("left %d sends, %v", len(sends), err)
		}
		if again := RetireSmtpSendDays(ctx, now, time.Now().Add(time.Minute)); len(again.Dropped) != 0 {
			t.Fatalf("a second run dropped %v", again.Dropped)
		}
	})
}

// A day holding rows of a network a live hold covers stays past its 30 days, said in Blocked each run, and holds
// back no other network's day; once the hold ends the day drops. smtp_send registers no copy until ABUSE-M9-01, so
// the hold keeps the day whole while it lives: the closed direction, which loses no row.
func TestSmtpSendHeldDayStaysUntilItsHoldEnds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpSendSettings()
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(1, 0, 0))
		heldNetworkId := sidecar.NewId()
		otherNetworkId := sidecar.NewId()
		held := smtpTestSend(heldNetworkId, day.Add(9*time.Hour))
		other := smtpTestSend(otherNetworkId, day.AddDate(0, 0, 1).Add(9*time.Hour))
		if result, err := insertSmtpSendsAt(ctx, []*smtpedgeservice.SmtpSendRecord{held, other}, other.StartTime, settings); err != nil || result.StoredCount != 2 {
			t.Fatalf("write %+v, %v", result, err)
		}
		// placed after the held day's rows, live by the real clock the drop's check reads
		caseId := tlsTestPlaceHold(ctx, server.Id(heldNetworkId), day.Add(12*time.Hour), server.NowUtc().AddDate(2, 0, 0))
		now := day.AddDate(0, 0, 32)
		heldLeaf := "smtp_send_p" + day.Format("20060102")
		otherLeaf := "smtp_send_p" + day.AddDate(0, 0, 1).Format("20060102")
		first := RetireSmtpSendDays(ctx, now, time.Now().Add(time.Minute))
		if _, blocked := first.Blocked[heldLeaf]; !blocked || slices.Contains(first.Dropped, heldLeaf) || !slices.Contains(first.Dropped, otherLeaf) {
			t.Fatalf("with the hold %+v", first)
		}
		if !strings.Contains(first.Blocked[heldLeaf], caseId.String()) {
			t.Fatalf("the reason %q names no case", first.Blocked[heldLeaf])
		}
		if sends, err := FindSmtpSends(ctx, &smtpedgeservice.SmtpJoinKey{NetworkId: heldNetworkId}); err != nil || len(sends) != 1 {
			t.Fatalf("the held network's sends %d, %v", len(sends), err)
		}
		// the hold ends
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE abuse_legal_hold SET expire_time = $2 WHERE case_id = $1`, caseId, server.NowUtc().Add(-time.Minute)))
		})
		second := RetireSmtpSendDays(ctx, now, time.Now().Add(time.Minute))
		if !slices.Contains(second.Dropped, heldLeaf) || len(second.Blocked) != 0 {
			t.Fatalf("after the hold %+v", second)
		}
	})
}

// smtp_complaint, smtp_dmarc_daily and smtp_halt keep rows inside 13 months and lose those past them, a held
// network's staying.
func TestSmtpReportRowsGoAtThirteenMonths(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpRetentionSettings()
		now := time.Date(2032, time.June, 15, 12, 0, 0, 0, time.UTC)
		pastTime := now.AddDate(0, -13, 0).Add(-time.Hour)
		insideTime := now.AddDate(0, -13, 0).Add(time.Hour)
		networkId := sidecar.NewId()
		heldNetworkId := sidecar.NewId()
		tlsTestPlaceHold(ctx, server.Id(heldNetworkId), now.AddDate(-2, 0, 0), server.NowUtc().AddDate(10, 0, 0))

		complaints := map[string]*smtpedgeservice.SmtpComplaint{
			"past":   smtpTestComplaint(networkId, pastTime),
			"inside": smtpTestComplaint(networkId, insideTime),
			"held":   smtpTestComplaint(heldNetworkId, pastTime),
		}
		for _, complaint := range complaints {
			if err := InsertSmtpComplaint(ctx, complaint); err != nil {
				t.Fatal(err)
			}
		}
		aggregate := func(networkId sidecar.Id, day time.Time) {
			report := smtpTestAggregate("reporter.example", smtpTestReportId(), networkId, 7)
			report.Day = smtpUtcDay(day)
			report.ArrivalTime = day
			if inserted, err := InsertSmtpAggregate(ctx, report); err != nil || !inserted {
				t.Fatalf("aggregate %t, %v", inserted, err)
			}
		}
		aggregate(networkId, smtpUtcDay(now).AddDate(0, -13, -1))
		aggregate(networkId, smtpUtcDay(now).AddDate(0, -13, 1))
		aggregate(heldNetworkId, smtpUtcDay(now).AddDate(0, -13, -1))
		halts := map[string]*smtpedgeservice.SmtpHalt{
			"past":   {HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: pastTime},
			"inside": {HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: insideTime},
			"held":   {HaltId: sidecar.NewId(), NetworkId: heldNetworkId, Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: pastTime},
		}
		for _, halt := range halts {
			if err := SetSmtpHalt(ctx, halt); err != nil {
				t.Fatal(err)
			}
		}

		result := RemoveExpiredSmtpRows(ctx, now, settings)
		if result.More || result.RemovedCounts[RetentionTableSmtpComplaint] != 1 || result.RemovedCounts[RetentionTableSmtpDmarcDaily] != 1 || result.RemovedCounts[RetentionTableSmtpHalt] != 1 {
			t.Fatalf("removed %+v", result)
		}
		count := func(query string, args ...any) (count int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, query, args...).Scan(&count))
			})
			return
		}
		for name, complaint := range complaints {
			left := count(`SELECT COUNT(*) FROM smtp_complaint WHERE complaint_id = $1`, server.Id(complaint.ComplaintId))
			if (name == "past") != (left == 0) {
				t.Errorf("complaint %s: %d left", name, left)
			}
		}
		if left := count(`SELECT COUNT(*) FROM smtp_dmarc_daily WHERE network_id = $1`, server.Id(networkId)); left != 1 {
			t.Errorf("the network's aggregate days: %d left", left)
		}
		if left := count(`SELECT COUNT(*) FROM smtp_dmarc_daily WHERE network_id = $1`, server.Id(heldNetworkId)); left != 1 {
			t.Errorf("the held network's aggregate days: %d left", left)
		}
		for name, halt := range halts {
			left := count(`SELECT COUNT(*) FROM smtp_halt WHERE halt_id = $1`, server.Id(halt.HaltId))
			if (name == "past") != (left == 0) {
				t.Errorf("halt %s: %d left", name, left)
			}
		}
	})
}

// A sealed copy goes 30 days after its report arrived and its row stays with every field; a younger copy and a
// held network's stay sealed.
func TestSmtpSealedReportClearedAtThirtyDaysAndTheRowKept(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpRetentionSettings()
		now := time.Date(2032, time.June, 15, 12, 0, 0, 0, time.UTC)
		networkId := sidecar.NewId()
		heldNetworkId := sidecar.NewId()
		tlsTestPlaceHold(ctx, server.Id(heldNetworkId), now.AddDate(0, -2, 0), server.NowUtc().AddDate(10, 0, 0))
		old := smtpTestComplaint(networkId, now.Add(-30*24*time.Hour-time.Minute))
		young := smtpTestComplaint(networkId, now.Add(-30*24*time.Hour+time.Minute))
		held := smtpTestComplaint(heldNetworkId, now.Add(-40*24*time.Hour))
		for _, complaint := range []*smtpedgeservice.SmtpComplaint{old, young, held} {
			if err := InsertSmtpComplaint(ctx, complaint); err != nil {
				t.Fatal(err)
			}
		}
		clearedCount, more := ClearExpiredSmtpSealedReports(ctx, now, settings)
		if clearedCount != 1 || more {
			t.Fatalf("cleared %d, more %t", clearedCount, more)
		}
		sealed := func(complaint *smtpedgeservice.SmtpComplaint) *smtpedgeservice.SmtpComplaint {
			for _, stored := range GetSmtpComplaints(ctx, server.Id(complaint.NetworkId), time.Time{}) {
				if stored.ComplaintId == complaint.ComplaintId {
					return stored
				}
			}
			return nil
		}
		stored := sealed(old)
		if stored == nil || len(stored.SealedReport) != 0 || stored.Type != old.Type || stored.Domain != old.Domain || stored.JoinResult != old.JoinResult {
			t.Fatalf("the old row %+v", stored)
		}
		if stored := sealed(young); stored == nil || string(stored.SealedReport) != string(young.SealedReport) {
			t.Fatal("the young copy was cleared")
		}
		if stored := sealed(held); stored == nil || string(stored.SealedReport) != string(held.SealedReport) {
			t.Fatal("the held network's copy was cleared")
		}
		if again, _ := ClearExpiredSmtpSealedReports(ctx, now, settings); again != 0 {
			t.Fatalf("a second run cleared %d", again)
		}
	})
}

// An RSA-2048 key's DER SubjectPublicKeyInfo and its key record.
func smtpTestDkimPublicKey(t testing.TB) ([]byte, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der, "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
}

// A selector of the 26 lowercase base32 characters of 16 random octets that selectors take
// (plan-SMTP-small-readings#5).
func smtpTestSelector() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		panic(err)
	}
	return base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(random)
}

// Publishes a key under selector at domain in root's zone, its row and its record in one transaction, as POST
// /smtp/dkim-keys does, retired at retireTime when that is set.
func smtpTestPublishDkimKey(t testing.TB, ctx context.Context, root string, networkId server.Id, sidecarId server.Id, selector string, domain string, burnerLabel string, retireTime time.Time) {
	t.Helper()
	der, value := smtpTestDkimPublicKey(t)
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			key := &smtpedgeservice.SmtpDkimKey{
				Selector:  selector,
				Domain:    domain,
				NetworkId: sidecar.Id(networkId),
				SidecarId: sidecar.Id(sidecarId),
				Algorithm: SmtpDkimAlgorithmRsaSha256,
				PublicKey: der,
			}
			if _, err := InsertSmtpDkimKeyInTx(ctx, tx, key, time.Date(2031, time.December, 1, 0, 0, 0, 0, time.UTC)); err != nil {
				panic(dnsWriteRefusal{err: err})
			}
			write := DnsPutDkimSelectorInTx
			if burnerLabel != "" {
				write = DnsPutBurnerSelectorInTx
			}
			if _, err := write(ctx, tx, &DnsDkimSelector{Root: root, NetworkId: networkId, Selector: selector, BurnerLabel: burnerLabel, Value: value, SidecarId: &sidecarId}); err != nil {
				panic(dnsWriteRefusal{err: err})
			}
			if !retireTime.IsZero() {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE smtp_dkim_key SET state = $3, retire_time = $4 WHERE selector = $1 AND domain = $2`,
					selector,
					domain,
					SmtpDkimKeyStateRetired,
					retireTime.UTC(),
				))
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The state and remove time of a key's row, "" for none.
func smtpTestDkimKeyState(ctx context.Context, selector string, domain string) (state string, removeTime *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state, remove_time FROM smtp_dkim_key WHERE selector = $1 AND domain = $2`, selector, domain)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state, &removeTime))
			}
		})
	})
	return
}

// A retired selector stays in DNS until 7 days after its retirement and leaves it then, its row moving to removed,
// and the row goes 30 days after that, not a minute before; a burner's last selector takes its label's copy rows
// with it, and a published selector is never touched.
func TestSmtpRetiredSelectorLeavesDnsAtSevenDaysAndItsRowThirtyDaysLater(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpRetentionSettings()
		// mail on, so the burner's label has *'s records to copy
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		root, networkId, apex := world.root, world.networkId, world.apex
		domain := strings.TrimSuffix(apex, ".")
		sidecarId := server.NewId()
		retireTime := time.Date(2032, time.March, 1, 12, 0, 0, 0, time.UTC)

		retired := smtpTestSelector()
		current := smtpTestSelector()
		burner := smtpTestSelector()
		burnerLabel := "maple-1234"
		burnerDomain := burnerLabel + "." + domain
		smtpTestPublishDkimKey(t, ctx, root, networkId, sidecarId, retired, domain, "", retireTime)
		smtpTestPublishDkimKey(t, ctx, root, networkId, sidecarId, current, domain, "", time.Time{})
		smtpTestPublishDkimKey(t, ctx, root, networkId, sidecarId, burner, burnerDomain, burnerLabel, retireTime)
		served := func(owner string, rrType uint16) bool {
			_, held := testDnsSnapshotRows(t, ctx, root)[dnsservice.DnsRecordKey{Owner: owner, Type: rrType}]
			return held
		}
		// the burner label's copy of *'s records (D-DNS-11), whatever key the copy row takes
		copied := func(owner string) bool {
			for key, set := range testDnsSnapshotRows(t, ctx, root) {
				if key.Owner == owner && set.Kind == dnsservice.DnsRecordKindCopy {
					return true
				}
			}
			return false
		}
		retiredOwner := retired + "._domainkey." + apex
		currentOwner := current + "._domainkey." + apex
		burnerOwner := burner + "._domainkey." + burnerLabel + "." + apex
		if !served(retiredOwner, dns.TypeTXT) || !served(burnerOwner, dns.TypeTXT) || !copied(burnerLabel+"."+apex) {
			t.Fatal("the selectors were not published")
		}

		// a minute short of the 7 days nothing leaves DNS
		early := ExpireSmtpDkimKeys(ctx, retireTime.Add(settings.DkimRetiredKeepTimeout-time.Minute), settings)
		if early.RemovedFromDnsCount != 0 || early.FailedCount != 0 || !served(retiredOwner, dns.TypeTXT) {
			t.Fatalf("early %+v", early)
		}
		removeTime := retireTime.Add(settings.DkimRetiredKeepTimeout + time.Minute)
		due := ExpireSmtpDkimKeys(ctx, removeTime, settings)
		if due.RemovedFromDnsCount != 2 || due.FailedCount != 0 || due.DeletedCount != 0 {
			t.Fatalf("due %+v", due)
		}
		if served(retiredOwner, dns.TypeTXT) || served(burnerOwner, dns.TypeTXT) || copied(burnerLabel+"."+apex) {
			t.Fatal("a retired selector, or the burner's copy, is still served")
		}
		if !served(currentOwner, dns.TypeTXT) {
			t.Fatal("the current selector left DNS")
		}
		for _, key := range [][2]string{{retired, domain}, {burner, burnerDomain}} {
			state, keyRemoveTime := smtpTestDkimKeyState(ctx, key[0], key[1])
			if state != SmtpDkimKeyStateRemoved || keyRemoveTime == nil || !keyRemoveTime.Equal(removeTime) {
				t.Fatalf("the row %s %v", state, keyRemoveTime)
			}
			// a removed key still checks reports
			if found, err := FindSmtpDkimKey(ctx, key[0], key[1]); err != nil || found == nil {
				t.Fatalf("the removed key %v, %v", found, err)
			}
		}
		if state, _ := smtpTestDkimKeyState(ctx, current, domain); state != SmtpDkimKeyStatePublished {
			t.Fatalf("the current key is %s", state)
		}

		// the rows stay until 30 days after they left DNS
		kept := ExpireSmtpDkimKeys(ctx, removeTime.Add(settings.DkimRemovedRowRetention-time.Minute), settings)
		if kept.DeletedCount != 0 || kept.RemovedFromDnsCount != 0 {
			t.Fatalf("kept %+v", kept)
		}
		gone := ExpireSmtpDkimKeys(ctx, removeTime.Add(settings.DkimRemovedRowRetention+time.Minute), settings)
		if gone.DeletedCount != 2 {
			t.Fatalf("gone %+v", gone)
		}
		for _, key := range [][2]string{{retired, domain}, {burner, burnerDomain}} {
			if state, _ := smtpTestDkimKeyState(ctx, key[0], key[1]); state != "" {
				t.Fatalf("the row of %s is %s", key[0], state)
			}
		}
		if state, _ := smtpTestDkimKeyState(ctx, current, domain); state != SmtpDkimKeyStatePublished {
			t.Fatalf("the current key is %s", state)
		}
	})
}

// A held network's removed key keeps its row past its 30 days.
func TestSmtpRemovedKeyOfAHeldNetworkStays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpRetentionSettings()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		domain := strings.TrimSuffix(apex, ".")
		retireTime := time.Date(2032, time.March, 1, 12, 0, 0, 0, time.UTC)
		selector := smtpTestSelector()
		smtpTestPublishDkimKey(t, ctx, root, networkId, server.NewId(), selector, domain, "", retireTime)
		tlsTestPlaceHold(ctx, networkId, retireTime, server.NowUtc().AddDate(10, 0, 0))
		removeTime := retireTime.Add(settings.DkimRetiredKeepTimeout + time.Minute)
		if due := ExpireSmtpDkimKeys(ctx, removeTime, settings); due.RemovedFromDnsCount != 1 {
			t.Fatalf("due %+v", due)
		}
		if late := ExpireSmtpDkimKeys(ctx, removeTime.Add(settings.DkimRemovedRowRetention+time.Hour), settings); late.DeletedCount != 0 {
			t.Fatalf("late %+v", late)
		}
		if state, _ := smtpTestDkimKeyState(ctx, selector, domain); state != SmtpDkimKeyStateRemoved {
			t.Fatalf("the held key is %q", state)
		}
	})
}

// One day's networks with aggregate counts, in id order, each with its day's reported messages and proven
// recipients, a page at a time; TLS-RPT counts and other days' sends take no part.
func TestSmtpReconcileDaysPageReportedAgainstProven(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(1, 0, 0))
		networkIds := []sidecar.Id{sidecar.NewId(), sidecar.NewId(), sidecar.NewId()}
		slices.SortFunc(networkIds, func(a sidecar.Id, b sidecar.Id) int { return a.Cmp(b) })
		for i, networkId := range networkIds {
			report := smtpTestAggregate("reporter.example", smtpTestReportId(), networkId, uint64(10*(i+1)))
			report.Day = day
			report.ArrivalTime = day.Add(30 * time.Hour)
			if _, err := InsertSmtpAggregate(ctx, report); err != nil {
				t.Fatal(err)
			}
		}
		tlsRpt := smtpTestAggregate("reporter.example", smtpTestReportId(), networkIds[0], 0)
		tlsRpt.Kind = smtpedgeservice.SmtpAggregateKindTlsRpt
		tlsRpt.Day = day
		tlsRpt.Counts[0].MessageCount = 0
		tlsRpt.Counts[0].PolicyType = "sts"
		tlsRpt.Counts[0].SuccessCount = 900
		if _, err := InsertSmtpAggregate(ctx, tlsRpt); err != nil {
			t.Fatal(err)
		}
		// the first network sent 4 recipients that day and 50 the next; the second none
		first := smtpTestSend(networkIds[0], day.Add(time.Hour))
		first.RecipientCount = 4
		next := smtpTestSend(networkIds[0], day.AddDate(0, 0, 1).Add(time.Hour))
		next.RecipientCount = 50
		third := smtpTestSend(networkIds[2], day.Add(2*time.Hour))
		third.RecipientCount = 30
		if result, err := insertSmtpSendsAt(ctx, []*smtpedgeservice.SmtpSendRecord{first, next, third}, next.StartTime, DefaultSmtpSendSettings()); err != nil || result.StoredCount != 3 {
			t.Fatalf("write %+v, %v", result, err)
		}

		page, err := GetSmtpReconcileDays(ctx, day, server.Id{}, 2)
		if err != nil || len(page) != 2 {
			t.Fatalf("page %v, %v", page, err)
		}
		if page[0].NetworkId != networkIds[0] || page[0].ReportedMessageCount != 10 || page[0].ProvenRecipientCount != 4 || !page[0].Day.Equal(day) {
			t.Fatalf("first %+v", page[0])
		}
		if page[1].NetworkId != networkIds[1] || page[1].ReportedMessageCount != 20 || page[1].ProvenRecipientCount != 0 {
			t.Fatalf("second %+v", page[1])
		}
		rest, err := NewSmtpEdgeStore(nil).ReconcileDays(ctx, day, page[1].NetworkId, 2)
		if err != nil || len(rest) != 1 || rest[0].NetworkId != networkIds[2] || rest[0].ReportedMessageCount != 30 || rest[0].ProvenRecipientCount != 30 {
			t.Fatalf("rest %+v, %v", rest, err)
		}
		if empty, err := GetSmtpReconcileDays(ctx, day.AddDate(0, 0, 2), server.Id{}, 2); err != nil || len(empty) != 0 {
			t.Fatalf("another day %v, %v", empty, err)
		}
	})
}

// A selector whose zone refuses the delete, suspended here, stays retired and counted failed, and the run goes on
// past it to the next one, a page of one at a time; once the zone takes writes the next run takes it out.
func TestSmtpKeyExpiryGoesPastASelectorItsZoneRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSmtpRetentionSettings()
		settings.DkimExpiryBatchSize = 1
		root := testDnsRoot(t, ctx)
		suspendedNetworkId, suspendedApex := testDnsNetwork(t, ctx, root)
		networkId, apex := testDnsNetwork(t, ctx, root)
		retireTime := time.Date(2032, time.March, 1, 12, 0, 0, 0, time.UTC)
		refused := smtpTestSelector()
		taken := smtpTestSelector()
		smtpTestPublishDkimKey(t, ctx, root, suspendedNetworkId, server.NewId(), refused, strings.TrimSuffix(suspendedApex, "."), "", retireTime)
		smtpTestPublishDkimKey(t, ctx, root, networkId, server.NewId(), taken, strings.TrimSuffix(apex, "."), "", retireTime.Add(time.Minute))
		if err := SuspendZone(ctx, suspendedNetworkId, "abuse", nil); err != nil {
			t.Fatal(err)
		}
		now := retireTime.Add(settings.DkimRetiredKeepTimeout + time.Hour)
		result := ExpireSmtpDkimKeys(ctx, now, settings)
		if result.FailedCount != 1 || result.RemovedFromDnsCount != 1 {
			t.Fatalf("expiry %+v", result)
		}
		if state, _ := smtpTestDkimKeyState(ctx, refused, strings.TrimSuffix(suspendedApex, ".")); state != SmtpDkimKeyStateRetired {
			t.Fatalf("the refused key is %s", state)
		}
		if state, _ := smtpTestDkimKeyState(ctx, taken, strings.TrimSuffix(apex, ".")); state != SmtpDkimKeyStateRemoved {
			t.Fatalf("the other key is %s", state)
		}
		if err := Restore(ctx, suspendedNetworkId, ""); err != nil {
			t.Fatal(err)
		}
		if again := ExpireSmtpDkimKeys(ctx, now, settings); again.FailedCount != 0 || again.RemovedFromDnsCount != 1 {
			t.Fatalf("after the restore %+v", again)
		}
	})
}
