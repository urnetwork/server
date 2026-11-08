package model

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// MG5's §13 part (TLS-M5-12) on a private database: the tables follow the tag,
// every column maps to cert-1, tls-3 or tls-4, hourly leaves are made and
// retired by the registry, a §21.9 hold keeps the one hour it covers, and an
// hour's counts reach tls_connection_day once however often its retirement is
// tried.

// §13's four tables.
var tlsSchemaTables = []string{
	"network_tls_cert",
	"network_tls_suspension",
	"tls_connection_record",
	"tls_connection_day",
}

// Which of the four the database holds.
func tlsSchemaPresentTables(ctx context.Context) []string {
	present := []string{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, table := range tlsSchemaTables {
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
// once, the record table partitioned.
func TestTlsSchemaFollowsTheTag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		present := tlsSchemaPresentTables(ctx)
		if !server.SidecarMg5Linked() {
			if len(present) != 0 {
				t.Fatalf("a binary that does not link MG5 has %v", present)
			}
			if tlsConnectionRecordPartitionFamily.IsPartitioned(ctx) {
				t.Fatal("the family reports partitioned without MG5")
			}
			if _, err := WriteTlsConnectionRecords(ctx, []*TlsConnectionRecord{tlsTestRecord(server.NewId(), server.NowUtc())}); !errors.Is(err, ErrTlsConnectionRecordsUnlinked) {
				t.Fatalf("a write without MG5 answered %v", err)
			}
			return
		}
		if !slices.Equal(present, tlsSchemaTables) {
			t.Fatalf("a binary that links MG5 has %v of %v", present, tlsSchemaTables)
		}
		if !tlsConnectionRecordPartitionFamily.IsPartitioned(ctx) {
			t.Fatal("tls_connection_record is not partitioned")
		}
	})
}

// Each column, with the row of §2.8's list that shows it and words of that
// row naming it (I-18). TEST-M1-03's full schema guard is not on server yet;
// this keeps the four tables from growing a column without a row meanwhile.
var tlsSchemaColumnRows = map[string][2]string{
	"network_tls_cert.cert_id":               {"cert-1", "cert_id"},
	"network_tls_cert.network_id":            {"cert-1", "network_id"},
	"network_tls_cert.client_id":             {"cert-1", "client_id"},
	"network_tls_cert.sidecar_id":            {"cert-1", "sidecar_id"},
	"network_tls_cert.identifier_set_sha256": {"cert-1", "identifier-set hash"},
	"network_tls_cert.spki_sha256":           {"cert-1", "SPKI hash"},
	"network_tls_cert.attestation":           {"cert-1", "the identity-key attestation"},
	"network_tls_cert.csr":                   {"cert-1", "the CSR (public key and names)"},
	"network_tls_cert.ca":                    {"cert-1", "CA"},
	"network_tls_cert.account_uri":           {"cert-1", "account URI"},
	"network_tls_cert.profile":               {"cert-1", "profile"},
	"network_tls_cert.serial":                {"cert-1", "serial"},
	"network_tls_cert.not_before":            {"cert-1", "validity"},
	"network_tls_cert.not_after":             {"cert-1", "validity"},
	"network_tls_cert.ari_window_start":      {"cert-1", "ARI window"},
	"network_tls_cert.ari_window_end":        {"cert-1", "ARI window"},
	"network_tls_cert.state":                 {"cert-1", "state"},
	"network_tls_cert.successor_cert_id":     {"cert-1", "successor"},
	"network_tls_cert.chain":                 {"cert-1", "Team custom-domain chains"},

	"network_tls_suspension.network_id": {"tls-4", "kill-switch entries"},
	"network_tls_suspension.host":       {"tls-4", "a host or all the network's hosts"},
	"network_tls_suspension.reason":     {"tls-4", "reason"},
	"network_tls_suspension.until_time": {"tls-4", "until"},

	"tls_connection_record.connection_id": {"tls-3", "connection_id"},
	"tls_connection_record.network_id":    {"tls-3", "network_id"},
	"tls_connection_record.host":          {"tls-3", "host from SNI"},
	"tls_connection_record.start_time":    {"tls-3", "start"},
	"tls_connection_record.end_time":      {"tls-3", "duration"},
	"tls_connection_record.byte_count":    {"tls-3", "bytes"},
	"tls_connection_record.visitor_hash":  {"tls-3", "a /24 or /48 hash under a daily-rotated key"},

	"tls_connection_day.network_id":       {"tls-3", "daily counts per network and host"},
	"tls_connection_day.host":             {"tls-3", "daily counts per network and host"},
	"tls_connection_day.day":              {"tls-3", "daily counts per network and host"},
	"tls_connection_day.connection_count": {"tls-3", "daily counts per network and host"},
	"tls_connection_day.byte_count":       {"tls-3", "daily counts per network and host"},
}

// A row of §2.8's list: its id line and the indented lines under it.
func tlsMetadataRowText(listBytes []byte, rowId string) string {
	lines := strings.Split(string(listBytes), "\n")
	for i, line := range lines {
		if line != rowId+":" {
			continue
		}
		row := []string{}
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "  ") {
				break
			}
			row = append(row, next)
		}
		return strings.Join(row, "\n")
	}
	return ""
}

// Every column of the four tables maps to its row, whose store names the
// table and whose words name the column, and no mapped column is missing.
func TestTlsSchemaColumnsMapToMetadataRows(t *testing.T) {
	listPath := "../../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		columns := map[string]bool{}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT table_name || '.' || column_name
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = ANY($1)
			`, tlsSchemaTables)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					server.Raise(result.Scan(&column))
					columns[column] = true
				}
			})
		})
		for column := range columns {
			mapped, ok := tlsSchemaColumnRows[column]
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
		for column := range tlsSchemaColumnRows {
			if !columns[column] {
				t.Errorf("%s is mapped but does not exist", column)
			}
		}
	})
}

// A record of network ending at endTime, from a synthetic visitor hash.
func tlsTestRecord(networkId server.Id, endTime time.Time) *TlsConnectionRecord {
	return &TlsConnectionRecord{
		ConnectionId: server.NewId(),
		NetworkId:    networkId,
		Host:         "www",
		StartTime:    endTime.Add(-90 * time.Second),
		EndTime:      endTime,
		ByteCount:    4096,
		VisitorHash:  []byte("0123456789abcdef"),
	}
}

// The leaves of tls_connection_record, by name.
func tlsTestLeafNames(t testing.TB, ctx context.Context) []string {
	var leaves []server.PartitionLeaf
	var err error
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		leaves, err = tlsConnectionRecordPartitionFamily.ListLeaves(ctx, conn, tlsConnectionRecordTable)
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

// How many records of network the table holds.
func tlsTestRecordCount(ctx context.Context, networkId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM tls_connection_record WHERE network_id = $1`, networkId).Scan(&count))
	})
	return
}

// A day's counts of network and host, zero when it has no row.
func tlsTestDayCounts(ctx context.Context, networkId server.Id, host string, day time.Time) (connectionCount int64, byteCount int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT connection_count, byte_count FROM tls_connection_day WHERE network_id = $1 AND host = $2 AND day = $3`, networkId, host, day)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&connectionCount, &byteCount))
			}
		})
	})
	return
}

// A part-0 hold on network, placed at placeTime and ending at expireTime.
func tlsTestPlaceHold(ctx context.Context, networkId server.Id, placeTime time.Time, expireTime time.Time) server.Id {
	caseId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO abuse_legal_hold (network_id, case_id, part, kind, scope, place_time, expire_time)
			VALUES ($1, $2, 0, 'preserve_2703f', '"network"', $3, $4)
			`,
			networkId,
			caseId,
			placeTime.UTC(),
			expireTime.UTC(),
		))
	})
	return caseId
}

// Runway made at 23:30 UTC reaches the next day's hours under
// <table>_hYYYYMMDDHH names, with no gap at midnight, and records ending on
// either side of midnight land in their own hours.
func TestTlsConnectionHoursAcrossAUtcDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 1, 0))
		now := day.Add(23*time.Hour + 30*time.Minute)
		report := tlsConnectionRecordPartitionFamily.EnsureRunway(ctx, now)
		if len(report.Errors) != 0 || !report.CurrentCovered || report.FutureRanges != DefaultTlsConnectionSettings().AheadHours {
			t.Fatalf("runway report %+v", report)
		}
		names := tlsTestLeafNames(t, ctx)
		want := []string{}
		for hour := day.Add(22 * time.Hour); !hour.After(day.Add(29 * time.Hour)); hour = hour.Add(time.Hour) {
			want = append(want, "tls_connection_record_h"+hour.Format("2006010215"))
		}
		if !slices.Equal(names, want) {
			t.Fatalf("leaves %v, want %v", names, want)
		}
		// every leaf is unlogged, so no physical copy of the primary holds a
		// record (xops-dump-physical-copies)
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
		records := []*TlsConnectionRecord{
			tlsTestRecord(networkId, day.Add(23*time.Hour+59*time.Minute)),
			tlsTestRecord(networkId, day.Add(24*time.Hour+time.Minute)),
		}
		result, err := writeTlsConnectionRecordsAt(ctx, records, now.Add(31*time.Minute), DefaultTlsConnectionSettings())
		if err != nil || result.WrittenCount != 2 {
			t.Fatalf("write %+v, %v", result, err)
		}
		for _, leaf := range []string{"tls_connection_record_h" + day.Add(23*time.Hour).Format("2006010215"), "tls_connection_record_h" + day.Add(24*time.Hour).Format("2006010215")} {
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM `+leaf+` WHERE network_id = $1`, networkId).Scan(&count))
			})
			if count != 1 {
				t.Errorf("%s holds %d of the records", leaf, count)
			}
		}
	})
}

// A batch makes its own hour when no runway has, stores each record once
// however often it is written, and drops, counting, what no row may hold and
// what ends in an expired hour or too far ahead.
func TestTlsConnectionRecordsWriteOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		settings := DefaultTlsConnectionSettings()
		now := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 2, 0)).Add(10*time.Hour + 5*time.Minute)
		networkId := server.NewId()
		good := []*TlsConnectionRecord{
			tlsTestRecord(networkId, now.Add(-time.Minute)),
			tlsTestRecord(networkId, now.Add(-2*time.Minute)),
		}
		noHash := tlsTestRecord(networkId, now)
		noHash.VisitorHash = nil
		backwards := tlsTestRecord(networkId, now)
		backwards.StartTime = backwards.EndTime.Add(time.Second)
		noNetwork := tlsTestRecord(server.Id{}, now)
		expired := tlsTestRecord(networkId, now.Add(-settings.RecordRetention-2*time.Hour))
		ahead := tlsTestRecord(networkId, now.Add(settings.MaxFutureSkew+time.Minute))
		batch := append(slices.Clone(good), noHash, backwards, noNetwork, expired, ahead)
		for attempt := range 2 {
			result, err := writeTlsConnectionRecordsAt(ctx, batch, now, settings)
			if err != nil {
				t.Fatalf("attempt %d: %v", attempt, err)
			}
			if result.WrittenCount != 2 || result.RefusedCount != 3 || result.LateCount != 2 {
				t.Fatalf("attempt %d: %+v", attempt, result)
			}
		}
		if count := tlsTestRecordCount(ctx, networkId); count != 2 {
			t.Fatalf("%d records stored", count)
		}
		// the write made only the hour it needed: no hour of the expired or
		// the far-ahead record
		names := tlsTestLeafNames(t, ctx)
		if !slices.Equal(names, []string{"tls_connection_record_h" + now.Format("2006010215")}) {
			t.Fatalf("leaves %v", names)
		}
	})
}

// Records with no address column at all: what a capture of the table holds is
// ids, a host, times, a count and the keyed hash (I-13).
func TestTlsConnectionRecordsHoldNoAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		types := map[string]string{}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT column_name, data_type
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name IN ('tls_connection_record', 'tls_connection_day')
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
		for column, dataType := range types {
			if dataType == "inet" || dataType == "cidr" || strings.Contains(column, "ip") || strings.Contains(column, "addr") {
				t.Errorf("%s (%s) reads as an address", column, dataType)
			}
		}
	})
}

// An hour retires once its upper bound is 23 h past, so its oldest record has
// lived 24 h, adding its counts to the day as it goes; the hour after it waits
// its own turn.
func TestTlsConnectionHourRetiresAt24Hours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		family := tlsConnectionRecordPartitionFamily
		hour := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 3, 0)).Add(5 * time.Hour)
		networkId := server.NewId()
		writeAt := hour.Add(90 * time.Minute)
		result, err := writeTlsConnectionRecordsAt(ctx, []*TlsConnectionRecord{
			tlsTestRecord(networkId, hour.Add(10*time.Minute)),
			tlsTestRecord(networkId, hour.Add(20*time.Minute)),
			tlsTestRecord(networkId, hour.Add(70*time.Minute)),
		}, writeAt, DefaultTlsConnectionSettings())
		if err != nil || result.WrittenCount != 3 {
			t.Fatalf("write %+v, %v", result, err)
		}
		retireAt := hour.Add(24 * time.Hour)
		report, _ := family.Retire(ctx, retireAt.Add(-time.Second), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if len(report.Dropped) != 0 {
			t.Fatalf("dropped %v a second before 24 h", report.Dropped)
		}
		report, _ = family.Retire(ctx, retireAt, server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if !slices.Equal(report.Dropped, []string{"tls_connection_record_h" + hour.Format("2006010215")}) {
			t.Fatalf("dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
		if count := tlsTestRecordCount(ctx, networkId); count != 1 {
			t.Fatalf("%d records left, want the next hour's one", count)
		}
		connectionCount, byteCount := tlsTestDayCounts(ctx, networkId, "www", server.PartitionGranularityDaily.RangeStart(hour))
		if connectionCount != 2 || byteCount != 2*4096 {
			t.Fatalf("the day counts %d connections and %d bytes", connectionCount, byteCount)
		}
		days := GetTlsConnectionDays(ctx, networkId, server.PartitionGranularityDaily.RangeStart(hour))
		if len(days) != 1 || days[0].ConnectionCount != 2 {
			t.Fatalf("days %+v", days)
		}
	})
}

// A hold placed inside one hour keeps that hour, whose rows of its network
// predate the placement, and only that hour: the hour before it held none of
// the network's rows and the hour after began after the placement. The kept
// hour goes once the hold no longer stands, here by its expiry.
func TestTlsConnectionHoldKeepsOnlyItsHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		family := tlsConnectionRecordPartitionFamily
		settings := DefaultTlsConnectionSettings()
		hour := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 4, 0)).Add(8 * time.Hour)
		heldNetworkId := server.NewId()
		otherNetworkId := server.NewId()
		write := func(records ...*TlsConnectionRecord) {
			result, err := writeTlsConnectionRecordsAt(ctx, records, records[0].EndTime.Add(time.Minute), settings)
			if err != nil || result.WrittenCount != len(records) {
				t.Fatalf("write %+v, %v", result, err)
			}
		}
		write(tlsTestRecord(otherNetworkId, hour.Add(-30*time.Minute)))
		write(tlsTestRecord(heldNetworkId, hour.Add(10*time.Minute)), tlsTestRecord(otherNetworkId, hour.Add(15*time.Minute)))
		write(tlsTestRecord(heldNetworkId, hour.Add(70*time.Minute)))
		// placed at hour+30m, until a year from the test's real now
		tlsTestPlaceHold(ctx, heldNetworkId, hour.Add(30*time.Minute), server.NowUtc().AddDate(1, 0, 0))

		report, _ := family.Retire(ctx, hour.Add(26*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		heldLeaf := "tls_connection_record_h" + hour.Format("2006010215")
		wantDropped := []string{
			"tls_connection_record_h" + hour.Add(-time.Hour).Format("2006010215"),
			"tls_connection_record_h" + hour.Add(time.Hour).Format("2006010215"),
		}
		if !slices.Equal(report.Dropped, wantDropped) {
			t.Fatalf("dropped %v, want %v", report.Dropped, wantDropped)
		}
		if reason := report.Blocked[heldLeaf]; !strings.Contains(reason, "has not copied") {
			t.Fatalf("the held hour stays for %q", reason)
		}
		// the held hour's counts wait with it, and the others' have landed
		if connectionCount, _ := tlsTestDayCounts(ctx, heldNetworkId, "www", server.PartitionGranularityDaily.RangeStart(hour)); connectionCount != 1 {
			t.Fatalf("the held network's day counts %d, want the unheld hour's one", connectionCount)
		}

		// the hold lapses: the hour goes, and its counts land
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE abuse_legal_hold SET expire_time = $2 WHERE network_id = $1`, heldNetworkId, server.NowUtc().Add(-time.Minute)))
		})
		report, _ = family.Retire(ctx, hour.Add(26*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if !slices.Equal(report.Dropped, []string{heldLeaf}) {
			t.Fatalf("after the hold, dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
		if connectionCount, _ := tlsTestDayCounts(ctx, heldNetworkId, "www", server.PartitionGranularityDaily.RangeStart(hour)); connectionCount != 2 {
			t.Fatalf("the held network's day counts %d", connectionCount)
		}
	})
}

// A retirement refused inside its own transaction, after the roll-up ran,
// rolls the roll-up back with it, so the retry adds the hour's counts once.
func TestTlsConnectionRollUpOnceAcrossARetriedRetirement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		settings := DefaultTlsConnectionSettings()
		hour := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 5, 0)).Add(3 * time.Hour)
		networkId := server.NewId()
		result, err := writeTlsConnectionRecordsAt(ctx, []*TlsConnectionRecord{
			tlsTestRecord(networkId, hour.Add(5*time.Minute)),
			tlsTestRecord(networkId, hour.Add(6*time.Minute)),
			tlsTestRecord(networkId, hour.Add(7*time.Minute)),
		}, hour.Add(10*time.Minute), settings)
		if err != nil || result.WrittenCount != 3 {
			t.Fatalf("write %+v, %v", result, err)
		}

		// the same declaration with a precondition that passes before the drop
		// and refuses inside it on the first retirement alone
		family := newTlsConnectionRecordPartitionFamily(settings)
		calls := 0
		family.DropPrecondition = func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string {
			calls += 1
			if calls == 2 {
				return "refused after the roll-up"
			}
			return ""
		}
		report, _ := family.Retire(ctx, hour.Add(25*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if len(report.Dropped) != 0 || !strings.Contains(report.Blocked["tls_connection_record_h"+hour.Format("2006010215")], "refused after the roll-up") {
			t.Fatalf("the first retirement %+v", report)
		}
		if connectionCount, _ := tlsTestDayCounts(ctx, networkId, "www", server.PartitionGranularityDaily.RangeStart(hour)); connectionCount != 0 {
			t.Fatalf("a refused retirement left %d connections counted", connectionCount)
		}
		report, _ = family.Retire(ctx, hour.Add(25*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if len(report.Dropped) != 1 {
			t.Fatalf("the retry %+v", report)
		}
		connectionCount, byteCount := tlsTestDayCounts(ctx, networkId, "www", server.PartitionGranularityDaily.RangeStart(hour))
		if connectionCount != 3 || byteCount != 3*4096 {
			t.Fatalf("after the retry the day counts %d connections and %d bytes", connectionCount, byteCount)
		}
	})
}

// Daily counts go once 13 months old, a day inside that stays, and a held
// network's old counts stay until its hold lapses.
func TestTlsConnectionDaysKept13Months(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		cutoff := tlsConnectionDayCutoff(now, DefaultTlsConnectionSettings())
		networkId := server.NewId()
		heldNetworkId := server.NewId()
		insert := func(networkId server.Id, day time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO tls_connection_day (network_id, host, day, connection_count, byte_count) VALUES ($1, 'www', $2, 1, 1)`,
					networkId,
					day,
				))
			})
		}
		insert(networkId, cutoff.AddDate(0, 0, -1))
		insert(networkId, cutoff)
		insert(heldNetworkId, cutoff.AddDate(0, 0, -1))
		tlsTestPlaceHold(ctx, heldNetworkId, now.AddDate(0, 0, -2), now.AddDate(0, 3, 0))

		RemoveExpiredTlsConnectionDays(ctx, now)
		if days := GetTlsConnectionDays(ctx, networkId, cutoff.AddDate(-1, 0, 0)); len(days) != 1 || !days[0].Day.Equal(cutoff) {
			t.Fatalf("the unheld network keeps %+v", days)
		}
		if days := GetTlsConnectionDays(ctx, heldNetworkId, cutoff.AddDate(-1, 0, 0)); len(days) != 1 {
			t.Fatalf("the held network keeps %+v", days)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE abuse_legal_hold SET expire_time = $2 WHERE network_id = $1`, heldNetworkId, now.Add(-time.Minute)))
		})
		RemoveExpiredTlsConnectionDays(ctx, now)
		if days := GetTlsConnectionDays(ctx, heldNetworkId, cutoff.AddDate(-1, 0, 0)); len(days) != 0 {
			t.Fatalf("after its hold, the network keeps %+v", days)
		}
	})
}
