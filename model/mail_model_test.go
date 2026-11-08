package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// MG7's §15 part (MAIL-M7-21) on a private database: the tables follow the tag,
// every column maps to mx-2 or mx-3, mail_connection_record keeps unlogged hourly
// leaves that a hold keeps one at a time, host keys retire and go 30 days on,
// and the Postgres MxEdgeStore passes the engine's store suite.

// §15.15's four tables.
var mailSchemaTables = []string{
	"mail_network",
	"mail_host_key",
	"mail_direct_mx",
	"mail_connection_record",
}

// Which of the four the database holds.
func mailSchemaPresentTables(ctx context.Context) []string {
	present := []string{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, table := range mailSchemaTables {
			var found *string
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&found))
			if found != nil {
				present = append(present, table)
			}
		}
	})
	return present
}

// The group is absent without the tag and present with it, every table at
// once, the record table partitioned; without it every call refuses or reads
// nothing.
func TestMailSchemaFollowsTheTag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		present := mailSchemaPresentTables(ctx)
		if !server.SidecarMg7Linked() {
			if len(present) != 0 {
				t.Fatalf("a binary that does not link MG7 has %v", present)
			}
			if mailConnectionRecordPartitionFamily.IsPartitioned(ctx) {
				t.Fatal("the family reports partitioned without MG7")
			}
			if _, err := WriteMailConnectionRecords(ctx, []*MailConnectionRecord{mailTestRecord(server.NewId(), server.NowUtc())}); !errors.Is(err, ErrMailUnlinked) {
				t.Fatalf("a write without MG7 answered %v", err)
			}
			if GetMailNetwork(ctx, server.NewId()) != nil || ListMailHostKeys(ctx, server.NewId()) != nil {
				t.Fatal("a read without MG7 answered rows")
			}
			if err := NewMxEdgeStore(nil).Ping(ctx); !errors.Is(err, ErrMailUnlinked) {
				t.Fatalf("a ping without MG7 answered %v", err)
			}
			return
		}
		if !slices.Equal(present, mailSchemaTables) {
			t.Fatalf("a binary that links MG7 has %v of %v", present, mailSchemaTables)
		}
		if !mailConnectionRecordPartitionFamily.IsPartitioned(ctx) {
			t.Fatal("mail_connection_record is not partitioned")
		}
	})
}

// Each column, with the row of §2.8's list that shows it and words of that
// row naming it (I-18).
var mailSchemaColumnRows = map[string][2]string{
	"mail_network.network_id":  {"mx-3", "mail-enabled networks"},
	"mail_network.create_time": {"mx-3", "mail-enabled networks"},

	"mail_host_key.network_id":          {"mx-3", "mail host keys"},
	"mail_host_key.host":                {"mx-3", "host mx or mx.<scope>"},
	"mail_host_key.receiver_sidecar_id": {"mx-3", "receiver sidecar_id"},
	"mail_host_key.spki_sha256":         {"mx-3", "SPKI hash"},
	"mail_host_key.state":               {"mx-3", "state"},
	"mail_host_key.publish_time":        {"mx-3", "publish and retire times"},
	"mail_host_key.retire_time":         {"mx-3", "publish and retire times"},

	"mail_direct_mx.network_id":          {"mx-3", "direct-MX receivers and addresses"},
	"mail_direct_mx.receiver_sidecar_id": {"mx-3", "direct-MX receivers and addresses"},
	"mail_direct_mx.address":             {"mx-3", "direct-MX receivers and addresses"},
	"mail_direct_mx.state":               {"mx-3", "direct-MX receivers and addresses"},
	"mail_direct_mx.verify_time":         {"mx-3", "verify direct MX"},

	"mail_connection_record.network_id": {"mx-2", "receiving network"},
	"mail_connection_record.start_time": {"mx-2", "time"},
	"mail_connection_record.byte_count": {"mx-2", "size"},
	"mail_connection_record.result":     {"mx-2", "result"},
}

// The columns of the four tables, table.column.
func mailSchemaColumns(ctx context.Context) map[string]bool {
	columns := map[string]bool{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT table_name || '.' || column_name
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ANY($1)
		`, mailSchemaTables)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var column string
				server.Raise(result.Scan(&column))
				columns[column] = true
			}
		})
	})
	return columns
}

// Every column of the four tables maps to its row, whose store names the
// table and whose words name the column, and no mapped column is missing.
// TEST-M1-03's full schema guard is not on server yet; this keeps the four
// tables from growing a column without a row meanwhile.
func TestMailSchemaColumnsMapToMetadataRows(t *testing.T) {
	listPath := "../../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		columns := mailSchemaColumns(context.Background())
		for column := range columns {
			mapped, ok := mailSchemaColumnRows[column]
			if !ok {
				t.Errorf("%s maps to no row of §2.8's list", column)
				continue
			}
			row := tlsMetadataRowText(listBytes, mapped[0])
			table, _, _ := strings.Cut(column, ".")
			if !strings.Contains(row, table) {
				t.Errorf("%s: row %s does not store %s", column, mapped[0], table)
			}
			if !strings.Contains(row, mapped[1]) {
				t.Errorf("%s: row %s does not say %q", column, mapped[0], mapped[1])
			}
		}
		for column := range mailSchemaColumnRows {
			if !columns[column] {
				t.Errorf("%s is mapped but does not exist", column)
			}
		}
	})
}

// No column of the records holds or names a sender's address, a burner label
// or a mail address (mx-2): a capture of the table's columns.
func TestMailConnectionRecordsHoldNoAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		types := map[string]string{}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT column_name, data_type
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = 'mail_connection_record'
			`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					var dataType string
					server.Raise(result.Scan(&column, &dataType))
					types[column] = dataType
				}
			})
		})
		if len(types) != 4 {
			t.Fatalf("columns %v", types)
		}
		for column, dataType := range types {
			if dataType == "inet" || dataType == "cidr" || strings.Contains(column, "ip") || strings.Contains(column, "addr") ||
				strings.Contains(column, "sender") || strings.Contains(column, "burner") || strings.Contains(column, "label") || strings.Contains(column, "mail_from") {
				t.Errorf("%s (%s) reads as an address or a label", column, dataType)
			}
		}
	})
}

// A record of network starting at startTime.
func mailTestRecord(networkId server.Id, startTime time.Time) *MailConnectionRecord {
	return &MailConnectionRecord{
		NetworkId: networkId,
		StartTime: startTime,
		ByteCount: 2048,
		Result:    "spliced",
	}
}

// The leaves of mail_connection_record, by name.
func mailTestLeafNames(t testing.TB, ctx context.Context) []string {
	var leaves []server.PartitionLeaf
	var err error
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		leaves, err = mailConnectionRecordPartitionFamily.ListLeaves(ctx, conn, mailConnectionRecordTable)
	})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, leaf := range leaves {
		names = append(names, leaf.Name)
	}
	slices.Sort(names)
	return names
}

// Runway made at 23:30 UTC reaches the next day's hours, unlogged, with no gap
// at midnight, and records starting either side of midnight land in their own
// hours; what no row may hold and what is late are dropped and counted.
func TestMailConnectionHoursAcrossAUtcDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		settings := DefaultMailConnectionSettings()
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 1, 0))
		now := day.Add(23*time.Hour + 30*time.Minute)
		report := mailConnectionRecordPartitionFamily.EnsureRunway(ctx, now)
		if len(report.Errors) != 0 || !report.CurrentCovered || report.FutureRanges != settings.AheadHours {
			t.Fatalf("runway report %+v", report)
		}
		names := mailTestLeafNames(t, ctx)
		want := []string{}
		for hour := day.Add(22 * time.Hour); !hour.After(day.Add(29 * time.Hour)); hour = hour.Add(time.Hour) {
			want = append(want, "mail_connection_record_h"+hour.Format("2006010215"))
		}
		if !slices.Equal(names, want) {
			t.Fatalf("leaves %v, want %v", names, want)
		}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			for _, name := range names {
				var persistence string
				server.Raise(conn.QueryRow(ctx, `SELECT relpersistence::text FROM pg_class WHERE oid = to_regclass($1)`, "public."+name).Scan(&persistence))
				if persistence != "u" {
					t.Errorf("%s has persistence %q, not unlogged", name, persistence)
				}
			}
		})

		networkId := server.NewId()
		badResult := mailTestRecord(networkId, now)
		badResult.Result = "Spliced!"
		noNetwork := mailTestRecord(server.Id{}, now)
		negative := mailTestRecord(networkId, now)
		negative.ByteCount = -1
		// the batch is written at 00:01, just past midnight
		writeNow := now.Add(31 * time.Minute)
		late := mailTestRecord(networkId, writeNow.Add(-settings.RecordRetention-2*time.Hour))
		ahead := mailTestRecord(networkId, writeNow.Add(settings.MaxFutureSkew+time.Minute))
		result, err := writeMailConnectionRecordsAt(ctx, []*MailConnectionRecord{
			mailTestRecord(networkId, day.Add(23*time.Hour+59*time.Minute)),
			mailTestRecord(networkId, day.Add(24*time.Hour+time.Minute)),
			badResult, noNetwork, negative, late, ahead,
		}, writeNow, settings)
		if err != nil || result.WrittenCount != 2 || result.RefusedCount != 3 || result.LateCount != 2 {
			t.Fatalf("write %+v, %v", result, err)
		}
		records := GetMailConnectionRecords(ctx, networkId, day, day.AddDate(0, 0, 2))
		if len(records) != 2 || records[0].StartTime.Hour() != 23 || records[1].StartTime.Hour() != 0 {
			t.Fatalf("records %+v", records)
		}
	})
}

// An hour retires once 23 h past its end, so its oldest record lived 24 h; a
// hold placed inside an hour keeps that hour alone until it no longer stands.
func TestMailConnectionHoldKeepsOnlyItsHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		settings := DefaultMailConnectionSettings()
		hour := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 2, 0)).Add(8 * time.Hour)
		heldNetworkId := server.NewId()
		otherNetworkId := server.NewId()
		write := func(records ...*MailConnectionRecord) {
			result, err := writeMailConnectionRecordsAt(ctx, records, records[0].StartTime.Add(time.Minute), settings)
			if err != nil || result.WrittenCount != len(records) {
				t.Fatalf("write %+v, %v", result, err)
			}
		}
		write(mailTestRecord(otherNetworkId, hour.Add(-30*time.Minute)))
		write(mailTestRecord(heldNetworkId, hour.Add(10*time.Minute)))
		write(mailTestRecord(heldNetworkId, hour.Add(70*time.Minute)))
		tlsTestPlaceHold(ctx, heldNetworkId, hour.Add(30*time.Minute), server.NowUtc().AddDate(1, 0, 0))

		// a second before the earliest hour's 24 h, nothing goes
		report := RetireMailConnectionHours(ctx, hour.Add(23*time.Hour-time.Second), time.Now().Add(time.Minute))
		if len(report.Dropped) != 0 {
			t.Fatalf("dropped %v before 24 h", report.Dropped)
		}
		report = RetireMailConnectionHours(ctx, hour.Add(26*time.Hour), time.Now().Add(time.Minute))
		heldLeaf := "mail_connection_record_h" + hour.Format("2006010215")
		wantDropped := []string{
			"mail_connection_record_h" + hour.Add(-time.Hour).Format("2006010215"),
			"mail_connection_record_h" + hour.Add(time.Hour).Format("2006010215"),
		}
		if !slices.Equal(report.Dropped, wantDropped) {
			t.Fatalf("dropped %v, want %v", report.Dropped, wantDropped)
		}
		if reason := report.Blocked[heldLeaf]; !strings.Contains(reason, "has not copied") {
			t.Fatalf("the held hour stays for %q", reason)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE abuse_legal_hold SET expire_time = $2 WHERE network_id = $1`, heldNetworkId, server.NowUtc().Add(-time.Minute)))
		})
		report = RetireMailConnectionHours(ctx, hour.Add(26*time.Hour), time.Now().Add(time.Minute))
		if !slices.Equal(report.Dropped, []string{heldLeaf}) {
			t.Fatalf("after the hold, dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
	})
}

// Mail turns on once: a second enable keeps the first create_time, which
// starts §14.7's send gate.
func TestEnableMailKeepsItsFirstTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		if GetMailNetwork(ctx, networkId) != nil {
			t.Fatal("mail on before enabling")
		}
		var first *MailNetwork
		server.Tx(ctx, func(tx server.PgTx) {
			mailNetwork, created, err := EnableMailInTx(ctx, tx, networkId)
			if err != nil || !created {
				t.Fatalf("enable %v, %v", created, err)
			}
			first = mailNetwork
		})
		server.Tx(ctx, func(tx server.PgTx) {
			mailNetwork, created, err := EnableMailInTx(ctx, tx, networkId)
			if err != nil || created || !mailNetwork.CreateTime.Equal(first.CreateTime) {
				t.Fatalf("enable again %+v, %v, %v", mailNetwork, created, err)
			}
		})
		if got := GetMailNetwork(ctx, networkId); got == nil || !got.CreateTime.Equal(first.CreateTime) {
			t.Fatalf("read %+v", got)
		}
	})
}

// A key of networkId's receiver for host.
func mailTestHostKey(networkId server.Id, host string, receiverSidecarId server.Id, spki string) *MailHostKey {
	spkiSha256 := sha256.Sum256([]byte(spki))
	return &MailHostKey{
		NetworkId:         networkId,
		Host:              host,
		ReceiverSidecarId: receiverSidecarId,
		SpkiSha256:        spkiSha256[:],
	}
}

// A key publishes once however often a receiver re-registers it, a removal
// retires every key of its receiver once, other receivers' keys stand, and a
// key no row can hold is refused.
func TestMailHostKeysPublishAndRetire(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		receiverId := server.NewId()
		otherReceiverId := server.NewId()
		upsert := func(key *MailHostKey) (changed bool, err error) {
			server.Tx(ctx, func(tx server.PgTx) {
				changed, err = UpsertMailHostKeyInTx(ctx, tx, key)
			})
			return
		}
		for _, key := range []*MailHostKey{
			mailTestHostKey(networkId, "mx", receiverId, "a"),
			mailTestHostKey(networkId, "mx.eng", receiverId, "b"),
			mailTestHostKey(networkId, "mx", otherReceiverId, "c"),
		} {
			if changed, err := upsert(key); err != nil || !changed {
				t.Fatalf("publish %v, %v", changed, err)
			}
			// a re-registration after restore_required changes nothing
			if changed, err := upsert(key); err != nil || changed {
				t.Fatalf("publish again %v, %v", changed, err)
			}
		}
		for _, bad := range []*MailHostKey{
			mailTestHostKey(networkId, "www", receiverId, "d"),
			mailTestHostKey(networkId, "mx.Eng", receiverId, "d"),
			mailTestHostKey(networkId, "mx.-eng", receiverId, "d"),
			mailTestHostKey(networkId, "mx."+strings.Repeat("a", 64), receiverId, "d"),
			mailTestHostKey(server.Id{}, "mx", receiverId, "d"),
			{NetworkId: networkId, Host: "mx", ReceiverSidecarId: receiverId, SpkiSha256: []byte{1, 2, 3}},
		} {
			if _, err := upsert(bad); !errors.Is(err, ErrMailHostKeyInvalid) {
				t.Fatalf("%+v answered %v", bad, err)
			}
		}

		var retired []*MailHostKey
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			retired, err = RetireMailHostKeysInTx(ctx, tx, networkId, receiverId)
			if err != nil {
				t.Fatal(err)
			}
		})
		if len(retired) != 2 {
			t.Fatalf("retired %d keys", len(retired))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			again, err := RetireMailHostKeysInTx(ctx, tx, networkId, receiverId)
			if err != nil || len(again) != 0 {
				t.Fatalf("a second removal retired %d, %v", len(again), err)
			}
		})
		keys := ListMailHostKeys(ctx, networkId)
		if len(keys) != 3 || keys[0].ReceiverSidecarId != otherReceiverId || keys[0].State != MailHostKeyStatePublished {
			t.Fatalf("keys %+v", keys)
		}
		for _, key := range keys[1:] {
			if key.State != MailHostKeyStateRetired || key.RetireTime.IsZero() {
				t.Fatalf("a removed receiver's key %+v", key)
			}
		}
		// published again, a retired key is a new publication
		if changed, err := upsert(mailTestHostKey(networkId, "mx", receiverId, "a")); err != nil || !changed {
			t.Fatalf("republish %v, %v", changed, err)
		}
	})
}

// A retired key goes once 30 days past its retirement and not a second
// sooner, a held network keeping its keys until the hold lapses.
func TestRetiredMailHostKeysGoAfter30Days(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		heldNetworkId := server.NewId()
		receiverId := server.NewId()
		retireTime := server.NowUtc().Add(-time.Hour)
		for _, key := range []*MailHostKey{
			mailTestHostKey(networkId, "mx", receiverId, "a"),
			mailTestHostKey(heldNetworkId, "mx", receiverId, "b"),
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				if _, err := UpsertMailHostKeyInTx(ctx, tx, key); err != nil {
					t.Fatal(err)
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE mail_host_key SET state = 'retired', retire_time = $2 WHERE network_id = $1`, key.NetworkId, retireTime))
			})
		}
		tlsTestPlaceHold(ctx, heldNetworkId, retireTime.Add(-time.Hour), server.NowUtc().AddDate(0, 3, 0))
		settings := DefaultMailHostKeySettings()

		RemoveRetiredMailHostKeys(ctx, retireTime.Add(settings.RetiredRetention))
		if keys := ListMailHostKeys(ctx, networkId); len(keys) != 1 {
			t.Fatalf("at exactly 30 days, %d keys", len(keys))
		}
		removedCount, _ := RemoveRetiredMailHostKeys(ctx, retireTime.Add(settings.RetiredRetention+time.Second))
		if removedCount != 1 || len(ListMailHostKeys(ctx, networkId)) != 0 {
			t.Fatalf("removed %d", removedCount)
		}
		if keys := ListMailHostKeys(ctx, heldNetworkId); len(keys) != 1 {
			t.Fatalf("the held network kept %d keys", len(keys))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE abuse_legal_hold SET expire_time = $2 WHERE network_id = $1`, heldNetworkId, server.NowUtc().Add(-time.Minute)))
		})
		RemoveRetiredMailHostKeys(ctx, retireTime.Add(settings.RetiredRetention+time.Second))
		if keys := ListMailHostKeys(ctx, heldNetworkId); len(keys) != 0 {
			t.Fatalf("after its hold, the network kept %d keys", len(keys))
		}
	})
}

// A network names at most two direct-MX receivers, a probe moves a row, a
// receiver's removal and DELETE take theirs, and a non-public address is
// refused.
func TestMailDirectMxRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		first := server.NewId()
		second := server.NewId()
		third := server.NewId()
		// documentation addresses: synthetic, and public unicast to Go
		firstAddress := netip.MustParseAddr("192.0.2.25")
		secondAddress := netip.MustParseAddr("2001:db8::25")
		put := func(receiverId server.Id, address netip.Addr) (err error) {
			server.Tx(ctx, func(tx server.PgTx) {
				err = PutMailDirectMxInTx(ctx, tx, networkId, receiverId, address)
			})
			return
		}
		if err := put(first, firstAddress); err != nil {
			t.Fatal(err)
		}
		if err := put(first, firstAddress); err != nil {
			t.Fatalf("the same row again: %v", err)
		}
		if err := put(second, secondAddress); err != nil {
			t.Fatal(err)
		}
		if err := put(third, netip.MustParseAddr("198.51.100.25")); !errors.Is(err, ErrMailDirectMxFull) {
			t.Fatalf("a third receiver: %v", err)
		}
		for _, bad := range []string{"127.0.0.1", "10.0.0.25", "169.254.0.25", "fe80::25", "224.0.0.25", "::"} {
			if err := put(first, netip.MustParseAddr(bad)); !errors.Is(err, ErrMailDirectMxInvalid) {
				t.Fatalf("%s answered %v", bad, err)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(SetMailDirectMxProbeInTx(ctx, tx, networkId, first, firstAddress, true))
			server.Raise(SetMailDirectMxProbeInTx(ctx, tx, networkId, second, secondAddress, false))
		})
		rows := ListMailDirectMx(ctx, networkId)
		if len(rows) != 2 {
			t.Fatalf("rows %+v", rows)
		}
		for _, row := range rows {
			switch row.ReceiverSidecarId {
			case first:
				if row.State != MailDirectMxStateVerified || row.VerifyTime.IsZero() || row.Address != firstAddress {
					t.Errorf("first %+v", row)
				}
			case second:
				if row.State != MailDirectMxStateFailing || !row.VerifyTime.IsZero() || row.Address != secondAddress {
					t.Errorf("second %+v", row)
				}
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			removedCount, err := RemoveMailDirectMxReceiverInTx(ctx, tx, networkId, first)
			if err != nil || removedCount != 1 {
				t.Fatalf("removal %d, %v", removedCount, err)
			}
			removedCount, err = RemoveMailDirectMxReceiverInTx(ctx, tx, networkId, first)
			if err != nil || removedCount != 0 {
				t.Fatalf("a second removal %d, %v", removedCount, err)
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			removedCount, err := RemoveMailDirectMxInTx(ctx, tx, networkId)
			if err != nil || removedCount != 1 {
				t.Fatalf("delete %d, %v", removedCount, err)
			}
		})
		if rows := ListMailDirectMx(ctx, networkId); len(rows) != 0 {
			t.Fatalf("rows after delete %+v", rows)
		}
	})
}

// A network's deletion takes its mail row and direct MX and retires its host
// keys, leaving another network's alone.
func TestNetworkDeletionTakesItsMailRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		receiverId := server.NewId()
		for _, id := range []server.Id{networkId, otherNetworkId} {
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := EnableMailInTx(ctx, tx, id)
				server.Raise(err)
				_, err = UpsertMailHostKeyInTx(ctx, tx, mailTestHostKey(id, "mx", receiverId, "a"))
				server.Raise(err)
				server.Raise(PutMailDirectMxInTx(ctx, tx, id, receiverId, netip.MustParseAddr("192.0.2.25")))
			})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			removeMailNetworkInTx(ctx, tx, networkId)
		})
		if GetMailNetwork(ctx, networkId) != nil || len(ListMailDirectMx(ctx, networkId)) != 0 {
			t.Fatal("the deleted network kept mail or direct MX")
		}
		if keys := ListMailHostKeys(ctx, networkId); len(keys) != 1 || keys[0].State != MailHostKeyStateRetired {
			t.Fatalf("the deleted network's keys %+v", keys)
		}
		if GetMailNetwork(ctx, otherNetworkId) == nil || len(ListMailDirectMx(ctx, otherNetworkId)) != 1 {
			t.Fatal("another network lost its rows")
		}
		if keys := ListMailHostKeys(ctx, otherNetworkId); len(keys) != 1 || keys[0].State != MailHostKeyStatePublished {
			t.Fatalf("another network's keys %+v", keys)
		}
	})
}

// The Postgres store keeps what the engine's store suite asks of every
// MxEdgeStore, the fake's included.
func TestMxEdgeStorePassesTheSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		store := NewMxEdgeStore(nil)
		sidecartest.RunMxEdgeStoreSuite(t, store, func(ctx context.Context, networkId sidecar.Id, since time.Time) ([]*mxedgeservice.MxConnectionRecord, error) {
			return Testing_ReadMxConnectionRecords(ctx, networkId, since), nil
		})
	})
}
