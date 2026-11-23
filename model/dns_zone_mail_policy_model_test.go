//go:build sidecar_pending

package model

// §12.7's _mta-sts behind network_verified (owner decision 630, QD-1092; dns_zone_model.go's
// getDnsZoneMailPolicyInTx and ReconcileNetworkZoneMailPolicy) on Postgres: an unverified network with mail on
// publishes none; the refresh that verifies it publishes the record, the refresh that clears it takes the record
// away, each in one journal row holding only it; a verified network's record is as DNS-M8-01 wrote it; the
// payment path's refresh on the writer's own connection does the same for a paid balance; a Team scope publishes
// none whichever way the network's verification goes (QD-1206); a suspended zone waits for Restore; and a refresh
// makes no zone for a network with none.

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A network whose admin user has no sign-in, so it is unverified, with its label in root: its id, its admin's
// and the label's apex.
func testDnsUnverifiedNetwork(t testing.TB, ctx context.Context, root string) (networkId server.Id, adminUserId server.Id, apex string) {
	t.Helper()
	networkId, adminUserId = testVerifiedNetwork(t, ctx, server.NowUtc())
	label, err := EnsureNetworkLabel(ctx, networkId, root)
	if err != nil {
		t.Fatal(err)
	}
	return networkId, adminUserId, label + "." + root
}

// The _mta-sts TXT rows the zone holds, owner to value.
func testDnsMtaStsRows(t testing.TB, ctx context.Context, root string, networkId server.Id) map[string]string {
	t.Helper()
	rows := map[string]string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, root, networkId) {
			if row.set.Type == dns.TypeTXT && strings.HasPrefix(row.set.Owner, dnsMtaStsLabel+".") {
				rows[row.set.Owner] = strings.Join(row.set.Rdata, " | ")
			}
		}
	})
	return rows
}

// The _mta-sts DNS-M8-01 writes for mailApex's mail host: v=STSv1 with the id of the policy its receivers serve.
func testDnsMtaStsValue(mailApex string) string {
	return `"v=STSv1; id=` + sidecar.MtaStsPolicyId(sidecar.MtaStsPolicy(dnsMailHostLabel+"."+mailApex, nil), nil) + `"`
}

// Sets every password sign-in of the user verified or not, as a sign-in's verification or its reversal writes it.
func testDnsSetPasswordAuthVerified(t testing.TB, ctx context.Context, userId server.Id, verified bool) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user_auth_password SET verified = $2 WHERE user_id = $1`, userId, verified))
	})
}

// An unverified network with mail on publishes every mail record DNS-M8-01 writes but _mta-sts, whose policy no
// receiver may serve for it, and a refresh that finds it still unverified writes nothing.
func TestDnsMtaStsUnverifiedNetworkPublishesNone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, _, apex := testDnsUnverifiedNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsMailOnServedGolden(root, apex); !slices.Equal(got, want) {
			t.Fatalf("an unverified network's mail on serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		before := testDnsSerialNow(t, ctx, root)
		if UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("verified with no sign-in")
		}
		requireDnsSerial(t, ctx, root, before)
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("_mta-sts %v", rows)
		}
	})
}

// The refresh that verifies a network with mail on publishes _mta-sts, in one journal row holding only it and its
// copy (QD-1190), after which the zone serves DNS-M8-01's set; a refresh with nothing changed writes nothing; and
// the refresh after the verification is cleared takes the record and its copy away in one row, the rest of the set
// standing. Until a refresh, the zone's set moves only at a write.
func TestDnsMtaStsFollowsVerificationAtTheRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, apex := testDnsUnverifiedNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		mtaStsKey := dnsservice.DnsRecordKey{Owner: dnsMtaStsLabel + "." + apex, Type: dns.TypeTXT}
		mtaStsCopyKey := dnsservice.DnsRecordKey{Owner: dnsMtaStsLabel + "." + apex, Type: dnsservice.DnsCopyType}
		// in type order, the copy's first
		sortedKeys := func(keys []dnsservice.DnsRecordKey) []dnsservice.DnsRecordKey {
			keys = slices.Clone(keys)
			slices.SortFunc(keys, func(first dnsservice.DnsRecordKey, second dnsservice.DnsRecordKey) int {
				return int(first.Type) - int(second.Type)
			})
			return keys
		}
		upsertKeys := func(row dnsservice.DnsJournalRow) []dnsservice.DnsRecordKey {
			keys := []dnsservice.DnsRecordKey{}
			for _, set := range row.Upserts {
				keys = append(keys, set.Key())
			}
			return sortedKeys(keys)
		}

		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("published before the refresh: %v", rows)
		}
		before := testDnsSerialNow(t, ctx, root)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("a verified email did not verify")
		}
		journal := testDnsJournal(t, ctx, root, before)
		if len(journal) != 1 || !slices.Equal(upsertKeys(journal[0]), []dnsservice.DnsRecordKey{mtaStsCopyKey, mtaStsKey}) || len(journal[0].Deletes) != 0 {
			t.Fatalf("the verifying refresh wrote %+v", journal)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsMailOnGolden(root, apex); !slices.Equal(got, want) {
			t.Fatalf("verified serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}

		verifiedSerial := testDnsSerialNow(t, ctx, root)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("the second refresh read unverified")
		}
		requireDnsSerial(t, ctx, root, verifiedSerial)

		testDnsSetPasswordAuthVerified(t, ctx, adminUserId, false)
		if UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("an unverified email still verifies")
		}
		journal = testDnsJournal(t, ctx, root, verifiedSerial)
		if len(journal) != 1 || len(journal[0].Upserts) != 0 || !slices.Equal(sortedKeys(journal[0].Deletes), []dnsservice.DnsRecordKey{mtaStsCopyKey, mtaStsKey}) {
			t.Fatalf("the clearing refresh wrote %+v", journal)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsMailOnServedGolden(root, apex); !slices.Equal(got, want) {
			t.Fatalf("cleared serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}

// A network verified when mail goes on publishes _mta-sts in the enable's own write, as DNS-M8-01 wrote it: the
// id of exactly the policy its receivers serve for its mail host; its refresh then has nothing to change, and a
// zone with mail off holds none however the network stands.
func TestDnsMtaStsVerifiedNetworkAsDnsM801Wrote(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, apex := testDnsUnverifiedNetwork(t, ctx, root)
		testAddSsoAuth(t, ctx, adminUserId, AuthTypeGoogle)
		if _, err := EnsureNetworkZone(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		mailOffSerial := testDnsSerialNow(t, ctx, root)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("a Google sign-in did not verify")
		}
		requireDnsSerial(t, ctx, root, mailOffSerial)
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("mail off holds %v", rows)
		}

		on, err := DnsSetMailRecords(ctx, networkId, root, true)
		if err != nil || on == nil || on.Serial != mailOffSerial+1 {
			t.Fatalf("mail on %+v %v", on, err)
		}
		want := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, want) {
			t.Fatalf("_mta-sts %v, want %v", rows, want)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsMailOnGolden(root, apex); !slices.Equal(got, want) {
			t.Fatalf("mail on serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		UpdateNetworkVerified(ctx, networkId)
		requireDnsSerial(t, ctx, root, on.Serial)
	})
}

// A paid balance verifies a network through the payment path's refresh, UpdateProNetworkInConn on the writer's
// own connection after its commit, which publishes _mta-sts; once the balance's window has ended, the next
// refresh takes it away.
func TestDnsMtaStsFollowsAPaidBalanceThroughThePaymentRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, _, apex := testDnsUnverifiedNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		now := server.NowUtc()
		testAddPaidTransferBalance(t, ctx, networkId, now.Add(-time.Hour), now.Add(time.Hour), true)
		server.Db(ctx, func(conn server.PgConn) {
			UpdateProNetworkInConn(conn, ctx, networkId)
		})
		want := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, want) {
			t.Fatalf("after the payment's refresh _mta-sts %v, want %v", rows, want)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time = $2 WHERE network_id = $1`, networkId, now.Add(-time.Minute)))
		})
		UpdateProNetwork(ctx, networkId)
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("after the window ended _mta-sts %v", rows)
		}
	})
}

// A Team scope publishes no _mta-sts whichever way its network's verification goes, since no receiver serves a
// scope's policy (QD-1206): none while unverified; at the refresh that verifies, the zone's alone, the mail scope's
// set DnsScopeRecords' for its state, its mail standing, after which the zone reads current and the next refresh
// writes nothing; and none at the refresh that clears, which takes the zone's away. A scope with mail off takes none
// either.
func TestDnsMtaStsScopePublishesNoneAtAnyVerification(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, apex := testDnsUnverifiedNetwork(t, ctx, root)
		testDnsScopeSource(t, networkId, "eng", "ops")
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng", Mail: testBool(true)}); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "ops"}); err != nil {
			t.Fatal(err)
		}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("unverified, _mta-sts %v", rows)
		}

		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypePhone, true)
		UpdateNetworkVerified(ctx, networkId)
		engApex := "eng." + apex
		want := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, want) {
			t.Fatalf("verified, _mta-sts %v, want the zone's alone %v", rows, want)
		}
		state := &DnsScopeState{Root: root, Apex: apex, Scope: "eng", Mail: true}
		server.Tx(ctx, func(tx server.PgTx) {
			state.MailPolicy = getDnsZoneMailPolicyInTx(ctx, tx, root, networkId)
		})
		if state.MailPolicy.MtaSts == nil {
			t.Fatal("the verified network's policy carries no MTA-STS policy")
		}
		wantScope, err := DnsScopeRecords(state, dnsZoneTtl())
		if err != nil {
			t.Fatal(err)
		}
		// _dmarc and b keep *.<scope>'s records through copies; no _mta-sts takes one
		wantScopeLines := testDnsWithCopyLines(dnsSystemRecordLines(wantScope), "_dmarc."+engApex, "b."+engApex)
		if got := testDnsZoneLinesBelow(t, ctx, root, networkId, engApex); !slices.Equal(got, wantScopeLines) {
			t.Fatalf("the scope's rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantScopeLines, "\n"))
		}
		stale := true
		server.Tx(ctx, func(tx server.PgTx) {
			stale = isDnsZoneMailPolicyStaleInTx(ctx, tx, root, networkId, server.NowUtc())
		}, server.TxReadCommitted)
		if stale {
			t.Fatal("a verified network whose mail scope holds no _mta-sts read stale")
		}
		verifiedSerial := testDnsSerialNow(t, ctx, root)
		UpdateNetworkVerified(ctx, networkId)
		requireDnsSerial(t, ctx, root, verifiedSerial)

		testDnsSetPasswordAuthVerified(t, ctx, adminUserId, false)
		UpdateNetworkVerified(ctx, networkId)
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("cleared, _mta-sts %v", rows)
		}
	})
}

// A suspended zone takes no reconcile: the refresh that verifies the network writes nothing while the zone's
// records are withheld, Restore returns them as they stood, and the network's next refresh publishes _mta-sts.
func TestDnsMtaStsRefreshWaitsOutASuspension(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, apex := testDnsUnverifiedNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		if err := SuspendZone(ctx, networkId, "abuse", nil); err != nil {
			t.Fatal(err)
		}
		suspendedSerial := testDnsSerialNow(t, ctx, root)
		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("a verified email did not verify")
		}
		requireDnsSerial(t, ctx, root, suspendedSerial)
		if err := Restore(ctx, networkId, ""); err != nil {
			t.Fatal(err)
		}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("Restore returned %v", rows)
		}
		UpdateNetworkVerified(ctx, networkId)
		want := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, want) {
			t.Fatalf("after Restore and a refresh _mta-sts %v, want %v", rows, want)
		}
	})
}

// A refresh waits out a reconcile in flight: a reconcile that read the network verified and wrote _mta-sts is held
// uncommitted while the verification is cleared and the clearing writer's refresh runs. That refresh waits on the
// zone's lock, an observed lock wait, and once the reconcile commits it reads the record and takes it away, so the
// zone never keeps a record its network has lost. A check that read around the writer would find no record, read
// nothing stale and leave the reconcile's record standing.
func TestDnsMtaStsRefreshWaitsOutAReconcileInFlight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, apex := testDnsUnverifiedNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)

		reconcilePid := make(chan int32, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseReconcile := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseReconcile()
		reconciled := make(chan any, 1)
		go func() {
			reconciled <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if err := reconcileDnsZoneMailPolicyInTx(ctx, tx, networkId, root); err != nil {
						panic(err)
					}
					reconcilePid <- lastOwnerTestBackendPid(ctx, tx)
					<-release
				}, server.TxReadCommitted)
			})
		}()
		blockerPid := <-reconcilePid

		testDnsSetPasswordAuthVerified(t, ctx, adminUserId, false)
		refreshed := make(chan bool, 1)
		go func() {
			refreshed <- UpdateNetworkVerified(ctx, networkId)
		}()
		if _, err := waitForLastOwnerLockWaiter(ctx, blockerPid); err != nil {
			t.Fatalf("the refresh never waited on the reconcile: %v", err)
		}
		releaseReconcile()
		if r := <-reconciled; r != nil {
			t.Fatalf("the reconcile in flight: %v", r)
		}
		if verified := <-refreshed; verified {
			t.Fatal("the clearing refresh read verified")
		}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); len(rows) != 0 {
			t.Fatalf("the zone kept %v after its network lost verification", rows)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsMailOnServedGolden(root, apex); !slices.Equal(got, want) {
			t.Fatalf("serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}

// Checks hold the zone's lock shared and never wait on one another: one held open leaves another free to read,
// within the transaction's lock bound.
func TestDnsMtaStsChecksDoNotWaitOnOneAnother(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, _ := testDnsUnverifiedNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)

		held := make(chan bool, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseCheck := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseCheck()
		checked := make(chan any, 1)
		go func() {
			checked <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					held <- isDnsZoneMailPolicyStaleInTx(ctx, tx, root, networkId, server.NowUtc())
					<-release
				}, server.TxReadCommitted)
			})
		}()
		if stale := <-held; !stale {
			t.Fatal("a verified network with no record read current")
		}
		stale := false
		server.Tx(ctx, func(tx server.PgTx) {
			stale = isDnsZoneMailPolicyStaleInTx(ctx, tx, root, networkId, server.NowUtc())
		}, server.TxReadCommitted)
		if !stale {
			t.Fatal("the second check read current")
		}
		releaseCheck()
		if r := <-checked; r != nil {
			t.Fatalf("the held check: %v", r)
		}
	})
}

// A refresh of a network with no zone makes none: no label, no zone row and no serial taken.
func TestDnsMtaStsRefreshMakesNoZone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId := testVerifiedNetwork(t, ctx, server.NowUtc())
		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
		before := testDnsSerialNow(t, ctx, root)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("a verified email did not verify")
		}
		requireDnsSerial(t, ctx, root, before)
		if label := GetNetworkLabel(ctx, networkId, root); label != "" {
			t.Fatalf("the refresh allocated %q", label)
		}
		server.Db(ctx, func(conn server.PgConn) {
			if roots := getNetworkDnsRootsInTx(ctx, conn, networkId); len(roots) != 0 {
				t.Fatalf("the refresh made zones in %v", roots)
			}
		})
	})
}
