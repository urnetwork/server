package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// MG9's §14 part (SMTP-M9-18) on a private database: the tables follow the tag,
// the Postgres store passes smtpedgeservice's store suite whole, a day's sends
// land in that day's leaf at midnight whatever leaves exist, late, early and
// malformed records are dropped and counted, every FindSends key plans on its
// index with sequential scans off, halts hold for their day, DKIM keys look up
// without case, and every column maps to its row.

// §14.14's tables in MG9.
var smtpEdgeTables = []string{"smtp_send", "smtp_halt", "smtp_dkim_key"}

// Random octets.
func smtpTestRandom(byteCount int) []byte {
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return value
}

// A send of networkId starting at start, from 192.0.2.10.
func smtpTestSend(networkId sidecar.Id, start time.Time) *smtpedgeservice.SmtpSendRecord {
	return &smtpedgeservice.SmtpSendRecord{
		SendId:           smtpTestRandom(12),
		NetworkId:        networkId,
		ClientId:         sidecar.NewId(),
		SendingAddress:   netip.MustParseAddr("192.0.2.10"),
		RecipientDomain:  "recipient.example",
		MxHost:           "mx.recipient.example",
		StartTime:        start,
		EndTime:          start.Add(time.Minute),
		ByteCount:        4096,
		MessageIdHash:    smtpedgeservice.SmtpMessageIdHash(fmt.Sprintf("<%x@quietnet.root.example>", smtpTestRandom(8))),
		XUrRefKeyId:      1,
		RecipientCount:   3,
		MessageByteCount: 2048,
		TlsVersion:       0x0304,
		TlsLevel:         smtpedgeservice.SmtpTlsLevelDane,
		Result:           "sent",
	}
}

// The group is absent without the tag and present with it, every table at
// once, smtp_send partitioned; without it every call refuses, never answering
// empty, and the family reports itself not partitioned.
func TestSmtpEdgeTablesFollowTheTag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		present := smtpPresentTables(ctx, smtpEdgeTables)
		store := NewSmtpEdgeStore(nil)
		if !server.SidecarMg9Linked() {
			if len(present) != 0 {
				t.Fatalf("a binary that does not link MG9 has %v", present)
			}
			if smtpSendPartitionFamily.IsPartitioned(ctx) {
				t.Fatal("the family reports partitioned without MG9")
			}
			networkId := sidecar.NewId()
			for name, err := range map[string]error{
				"InsertSends": store.InsertSends(ctx, []*smtpedgeservice.SmtpSendRecord{smtpTestSend(networkId, server.NowUtc())}),
				"SetHalt":     store.SetHalt(ctx, &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: server.NowUtc()}),
			} {
				if !errors.Is(err, ErrSmtpEdgeUnlinked) {
					t.Fatalf("%s without MG9 answered %v", name, err)
				}
			}
			if sends, err := store.FindSends(ctx, &smtpedgeservice.SmtpJoinKey{NetworkId: networkId}); sends != nil || !errors.Is(err, ErrSmtpEdgeUnlinked) {
				t.Fatalf("FindSends without MG9 answered %v, %v", sends, err)
			}
			if halts, err := store.ActiveHalts(ctx); halts != nil || !errors.Is(err, ErrSmtpEdgeUnlinked) {
				t.Fatalf("ActiveHalts without MG9 answered %v, %v", halts, err)
			}
			if key, err := store.FindDkimKey(ctx, "abcdefghijklmnopqrstuvwxyz", "quietnet.root.example"); key != nil || !errors.Is(err, ErrSmtpEdgeUnlinked) {
				t.Fatalf("FindDkimKey without MG9 answered %v, %v", key, err)
			}
			if _, err := store.RecipientCount(ctx, networkId, time.Time{}); !errors.Is(err, ErrSmtpEdgeUnlinked) {
				t.Fatalf("RecipientCount without MG9 answered %v", err)
			}
			return
		}
		if !slices.Equal(present, smtpEdgeTables) {
			t.Fatalf("a binary that links MG9 has %v of %v", present, smtpEdgeTables)
		}
		if !smtpSendPartitionFamily.IsPartitioned(ctx) {
			t.Fatal("smtp_send is not partitioned")
		}
	})
}

// smtp_send's family is daily on send_day, keeps leaves ahead, expires a day at
// 30 days and leaves the drop to SMTP-M9-17's task, and keeps a held network's
// day.
func TestSmtpSendFamilyIsDailyAndRetiredByItsOwnTask(t *testing.T) {
	family := server.GetPartitionFamily("smtp_send")
	if family == nil || family != smtpSendPartitionFamily {
		t.Fatal("smtp_send's family is not registered")
	}
	settings := DefaultSmtpSendSettings()
	if family.KeyColumn != "send_day" || family.Granularity != server.PartitionGranularityDaily || family.Retention != 30*24*time.Hour ||
		family.AheadRanges != settings.AheadDays || !family.RetireByOwnTask || family.DropPrecondition == nil || family.UnloggedLeaves {
		t.Fatalf("the family %+v", family)
	}
	day := time.Date(2031, time.March, 1, 0, 0, 0, 0, time.UTC)
	r := family.RangeAt(day.Add(23*time.Hour + 59*time.Minute))
	if !r.Lower.Equal(day) || !r.Upper.Equal(day.Add(24*time.Hour)) || family.LeafName("smtp_send", r) != "smtp_send_p20310301" {
		t.Fatalf("the range %+v", r)
	}
	ctx := context.Background()
	if family.IsExpired(ctx, r.Upper.Add(30*24*time.Hour-time.Second), r) || !family.IsExpired(ctx, r.Upper.Add(30*24*time.Hour), r) {
		t.Fatal("a day does not expire 30 days after it ends")
	}
}

// The Postgres store keeps what the engine's store suite asks of every
// SmtpEdgeStore, the fake's included (D-TEST-2). The suite dates its sends in
// 2031 and 2032, so this store takes sends that far ahead of its clock.
func TestSmtpEdgeStorePassesTheSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
			return
		}
		settings := DefaultSmtpEdgeStoreSettings()
		settings.Send.MaxFutureSkew = 10 * 365 * 24 * time.Hour
		sidecartest.RunSmtpEdgeStoreSuite(t, NewSmtpEdgeStore(settings))
	})
}

// Sends at the last microsecond of a day and at 00:00 UTC land in their own
// days' leaves, each made by the insert that found none, and one send id on
// both days is two sends.
func TestSmtpSendAtMidnightLandsInItsOwnDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		settings := DefaultSmtpSendSettings()
		// a day well ahead of the runway, so only the insert can have made its leaves
		day := server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, 2, 0))
		networkId := sidecar.NewId()
		beforeMidnight := smtpTestSend(networkId, day.Add(24*time.Hour-time.Microsecond))
		atMidnight := smtpTestSend(networkId, day.Add(24*time.Hour))
		atMidnight.SendId = beforeMidnight.SendId
		result, err := insertSmtpSendsAt(ctx, []*smtpedgeservice.SmtpSendRecord{beforeMidnight, atMidnight}, atMidnight.StartTime, settings)
		if err != nil || result.StoredCount != 2 || result.RefusedCount != 0 || result.LateCount != 0 {
			t.Fatalf("write %+v, %v", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT send_day, tableoid::regclass::text FROM smtp_send WHERE network_id = $1 ORDER BY start_time`, server.Id(networkId))
			rows := []string{}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var sendDay time.Time
					var leaf string
					server.Raise(result.Scan(&sendDay, &leaf))
					rows = append(rows, sendDay.Format(time.DateOnly)+" "+leaf)
				}
			})
			want := []string{
				day.Format(time.DateOnly) + " smtp_send_p" + day.Format("20060102"),
				day.Add(24*time.Hour).Format(time.DateOnly) + " smtp_send_p" + day.Add(24*time.Hour).Format("20060102"),
			}
			if !slices.Equal(rows, want) {
				t.Fatalf("rows %v, want %v", rows, want)
			}
		})
		// the same batch again stores nothing
		result, err = insertSmtpSendsAt(ctx, []*smtpedgeservice.SmtpSendRecord{beforeMidnight, atMidnight}, atMidnight.StartTime, settings)
		if err != nil || result.StoredCount != 0 {
			t.Fatalf("a retried batch %+v, %v", result, err)
		}
	})
}

// A batch's records no row can hold, of a day past its 30 days, or starting
// further ahead than MaxFutureSkew, are dropped and counted, and its other
// records stored; the batch is no error.
func TestSmtpSendDropsLateEarlyAndMalformedRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		settings := DefaultSmtpSendSettings()
		now := server.NowUtc()
		networkId := sidecar.NewId()
		good := smtpTestSend(networkId, now.Add(-time.Hour))
		late := smtpTestSend(networkId, now.Add(-32*24*time.Hour))
		early := smtpTestSend(networkId, now.Add(settings.MaxFutureSkew+time.Minute))
		shortId := smtpTestSend(networkId, now)
		shortId.SendId = shortId.SendId[:11]
		noClient := smtpTestSend(networkId, now)
		noClient.ClientId = sidecar.Id{}
		endsFirst := smtpTestSend(networkId, now)
		endsFirst.EndTime = endsFirst.StartTime.Add(-time.Second)
		badRetry := smtpTestSend(networkId, now)
		badRetry.RetryOfSendId = smtpTestRandom(16)
		longHost := smtpTestSend(networkId, now)
		longHost.MxHost = strings.Repeat("m", 254)
		result, err := insertSmtpSendsAt(ctx, []*smtpedgeservice.SmtpSendRecord{good, late, early, shortId, noClient, endsFirst, badRetry, longHost, nil}, now, settings)
		if err != nil || result.StoredCount != 1 || result.LateCount != 2 || result.RefusedCount != 6 {
			t.Fatalf("write %+v, %v", result, err)
		}
		sends, err := FindSmtpSends(ctx, &smtpedgeservice.SmtpJoinKey{NetworkId: networkId})
		if err != nil || len(sends) != 1 || !bytes.Equal(sends[0].SendId, good.SendId) {
			t.Fatalf("stored %v, %v", sends, err)
		}
		// no leaf was made again for the expired day
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			leaf := smtpSendPartitionFamily.LeafName(smtpSendTable, smtpSendPartitionFamily.RangeAt(late.StartTime))
			var found *string
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+leaf).Scan(&found))
			if found != nil {
				t.Errorf("a late record made %s", leaf)
			}
		})
	})
}

// A leaf of smtp_send as a plan names it.
var smtpTestLeafPattern = regexp.MustCompile(`smtp_send_p[0-9]{8}`)

// With sequential scans off, each of SmtpJoinKey's selective fields plans on
// an index of every leaf it reads, never a sequential scan, a bounded window
// reading its days' leaves alone.
func TestSmtpFindSendsPlansOnItsIndexes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkId := sidecar.NewId()
		sends := []*smtpedgeservice.SmtpSendRecord{}
		// three days of sends, each day its own leaf
		for day := range 3 {
			for i := range 4 {
				sends = append(sends, smtpTestSend(networkId, now.Add(-time.Duration(day)*24*time.Hour-time.Duration(i)*time.Minute)))
			}
		}
		if _, err := insertSmtpSendsAt(ctx, sends, now, DefaultSmtpSendSettings()); err != nil {
			t.Fatal(err)
		}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE smtp_send`))
		})
		since := now.Add(-time.Hour)
		keys := map[string]*smtpedgeservice.SmtpJoinKey{
			"send id":            {SendId: sends[0].SendId},
			"Message-ID hash":    {MessageIdHash: sends[0].MessageIdHash},
			"network":            {NetworkId: networkId},
			"network and window": {NetworkId: networkId, Since: since, Until: now},
			"address and window": {SendingAddress: netip.MustParseAddr("192.0.2.10"), Since: since, Until: now},
		}
		for name, key := range keys {
			query, args := smtpFindSendsQuery(key)
			plan := []string{}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
				result, err := tx.Query(ctx, "EXPLAIN "+query, args...)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var line string
						server.Raise(result.Scan(&line))
						plan = append(plan, line)
					}
				})
			})
			text := strings.Join(plan, "\n")
			if strings.Contains(text, "Seq Scan") || !strings.Contains(text, "Index") {
				t.Errorf("%s plans without its index:\n%s", name, text)
			}
			if key.Since.IsZero() {
				continue
			}
			// a window reads only its own days' leaves
			windowLeaves := []string{}
			for day := smtpSendPartitionFamily.RangeAt(since).Lower; !day.After(now); day = day.Add(24 * time.Hour) {
				windowLeaves = append(windowLeaves, smtpSendPartitionFamily.LeafName(smtpSendTable, smtpSendPartitionFamily.RangeAt(day)))
			}
			for _, leaf := range smtpTestLeafPattern.FindAllString(text, -1) {
				if !slices.Contains(windowLeaves, leaf) {
					t.Errorf("%s reads %s outside its window:\n%s", name, leaf, text)
				}
			}
		}
	})
}

// A halt holds for SmtpHaltDuration from its start and no longer, a label's
// and a network's alike, and is read back as set.
func TestSmtpHaltsHoldForTheirDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		now := time.Date(2031, time.May, 5, 12, 0, 0, 0, time.UTC)
		networkId := sidecar.NewId()
		ended := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerProRate, StartTime: now.Add(-smtpedgeservice.SmtpHaltDuration)}
		holding := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: networkId, Label: "quiet-otter", Trigger: smtpedgeservice.SmtpHaltTriggerLabelComplaint, StartTime: now.Add(-smtpedgeservice.SmtpHaltDuration + time.Second)}
		for _, halt := range []*smtpedgeservice.SmtpHalt{ended, holding} {
			if err := SetSmtpHalt(ctx, halt); err != nil {
				t.Fatal(err)
			}
		}
		halts, err := GetActiveSmtpHalts(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		own := []*smtpedgeservice.SmtpHalt{}
		for _, halt := range halts {
			if halt.NetworkId == networkId {
				own = append(own, halt)
			}
		}
		if len(own) != 1 || own[0].HaltId != holding.HaltId || own[0].Label != "quiet-otter" || own[0].Trigger != holding.Trigger || !own[0].StartTime.Equal(holding.StartTime) {
			t.Fatalf("active %+v", own)
		}
		if err := SetSmtpHalt(ctx, &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: networkId, StartTime: now}); !errors.Is(err, ErrSmtpRowRefused) {
			t.Fatalf("a halt with no trigger: %v", err)
		}
	})
}

// A key is found by its selector and domain in any case, retired or not, a
// second key under a stored selector is refused, and an Ed25519 key that is
// not 32 octets is no key.
func TestSmtpDkimKeyLooksUpWithoutCase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		publicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key := &smtpedgeservice.SmtpDkimKey{
			Selector:  "AbcdefghijklmnopqrstuvwxyZ",
			Domain:    "QuietNet.Root.Example",
			NetworkId: sidecar.NewId(),
			SidecarId: sidecar.NewId(),
			Algorithm: SmtpDkimAlgorithmEd25519Sha256,
			PublicKey: publicKey,
		}
		publishTime := time.Date(2031, time.May, 5, 12, 0, 0, 0, time.UTC)
		inserted, err := InsertSmtpDkimKey(ctx, key, publishTime)
		if err != nil || !inserted {
			t.Fatalf("%t, %v", inserted, err)
		}
		other := *key
		other.SidecarId = sidecar.NewId()
		if inserted, err := InsertSmtpDkimKey(ctx, &other, publishTime); err != nil || inserted {
			t.Fatalf("a second key under the selector: %t, %v", inserted, err)
		}
		found, err := store.FindDkimKey(ctx, "abcdefghijklmnopqrstuvwxyz", "quietnet.root.example")
		if err != nil || found == nil {
			t.Fatalf("%v, %v", found, err)
		}
		if found.Selector != "abcdefghijklmnopqrstuvwxyz" || found.Domain != "quietnet.root.example" || found.NetworkId != key.NetworkId ||
			found.SidecarId != key.SidecarId || found.Algorithm != key.Algorithm || !bytes.Equal(found.PublicKey, key.PublicKey) || !found.RetireTime.IsZero() {
			t.Fatalf("found %+v", found)
		}
		// a rotated-out key still answers, with its retire time
		retireTime := publishTime.Add(90 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE smtp_dkim_key SET state = $1, retire_time = $2 WHERE selector = $3`, SmtpDkimKeyStateRetired, retireTime, "abcdefghijklmnopqrstuvwxyz"))
		})
		if found, err := store.FindDkimKey(ctx, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "quietnet.root.example"); err != nil || found == nil || !found.RetireTime.Equal(retireTime) {
			t.Fatalf("a retired key: %v, %v", found, err)
		}
		if found, err := store.FindDkimKey(ctx, "abcdefghijklmnopqrstuvwxyz", "othernet.root.example"); err != nil || found != nil {
			t.Fatalf("another domain: %v, %v", found, err)
		}
		short := *key
		short.Selector = "bcdefghijklmnopqrstuvwxyza"
		short.PublicKey = publicKey[:31]
		if _, err := InsertSmtpDkimKey(ctx, &short, publishTime); !errors.Is(err, ErrSmtpRowRefused) {
			t.Fatalf("a 31-octet Ed25519 key: %v", err)
		}
	})
}

// RecipientCount sums the proven recipients of a network's sends from since
// on, inclusive, and no other network's.
func TestSmtpRecipientCountSumsSendsSince(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		now := server.NowUtc()
		networkId := sidecar.NewId()
		sends := []*smtpedgeservice.SmtpSendRecord{}
		for i, recipientCount := range []uint32{1, 10, 100} {
			send := smtpTestSend(networkId, now.Add(-time.Duration(25-i*12)*time.Hour))
			send.RecipientCount = recipientCount
			sends = append(sends, send)
		}
		other := smtpTestSend(sidecar.NewId(), now.Add(-time.Hour))
		other.RecipientCount = 1000
		if err := store.InsertSends(ctx, append(sends, other)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			since time.Time
			want  uint64
		}{
			{since: now.Add(-48 * time.Hour), want: 111},
			{since: sends[1].StartTime, want: 110},
			{since: sends[2].StartTime.Add(time.Microsecond), want: 0},
		} {
			count, err := store.RecipientCount(ctx, networkId, c.since)
			if err != nil || count != c.want {
				t.Errorf("since %v: %d, %v; want %d", c.since, count, err, c.want)
			}
		}
	})
}

// Each MG9 column, with the row of §2.8's list that shows it and words of that
// row naming it (I-18).
var smtpEdgeColumnRows = map[string][2]string{
	"smtp_send.send_day":           {"smtp-3", "times"},
	"smtp_send.send_id":            {"smtp-3", "send_id"},
	"smtp_send.network_id":         {"smtp-3", "network"},
	"smtp_send.client_id":          {"smtp-3", "client"},
	"smtp_send.sending_address":    {"smtp-3", "sending IP"},
	"smtp_send.recipient_domain":   {"smtp-3", "recipient domain"},
	"smtp_send.mx_host":            {"smtp-3", "MX host"},
	"smtp_send.start_time":         {"smtp-3", "times"},
	"smtp_send.end_time":           {"smtp-3", "times"},
	"smtp_send.byte_count":         {"smtp-3", "bytes"},
	"smtp_send.message_id_hash":    {"smtp-3", "Message-ID hash"},
	"smtp_send.x_ur_ref_key_id":    {"smtp-3", "X-UR-Ref id"},
	"smtp_send.recipient_count":    {"smtp-3", "proven counts"},
	"smtp_send.message_byte_count": {"smtp-3", "proven counts"},
	"smtp_send.tls_version":        {"smtp-3", "TLS version and level"},
	"smtp_send.tls_level":          {"smtp-3", "TLS version and level"},
	"smtp_send.result":             {"smtp-3", "result"},
	"smtp_send.retry_of_send_id":   {"smtp-3", "retry-of"},

	"smtp_halt.halt_id":    {"smtp-4", "smtp_halt rows"},
	"smtp_halt.network_id": {"smtp-4", "network or burner label"},
	"smtp_halt.label":      {"smtp-4", "network or burner label"},
	"smtp_halt.trigger":    {"smtp-4", "trigger"},
	"smtp_halt.start_time": {"smtp-4", "start"},

	"smtp_dkim_key.selector":     {"smtp-6", "opaque selectors"},
	"smtp_dkim_key.domain":       {"smtp-6", "publish DKIM"},
	"smtp_dkim_key.network_id":   {"smtp-6", "publish DKIM"},
	"smtp_dkim_key.sidecar_id":   {"smtp-6", "per sending sidecar"},
	"smtp_dkim_key.algorithm":    {"smtp-6", "DKIM public keys"},
	"smtp_dkim_key.public_key":   {"smtp-6", "DKIM public keys"},
	"smtp_dkim_key.state":        {"smtp-6", "after the selector leaves DNS"},
	"smtp_dkim_key.publish_time": {"smtp-6", "publish DKIM"},
	"smtp_dkim_key.retire_time":  {"smtp-6", "7 d after rotation"},
	"smtp_dkim_key.remove_time":  {"smtp-6", "after the selector leaves DNS"},
}

// Every column of MG9's tables maps to its row, whose store names the table
// and whose words name the column, and no mapped column is missing.
func TestSmtpEdgeColumnsMapToMetadataRows(t *testing.T) {
	listPath := "../../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg9Linked() {
			return
		}
		smtpCheckColumnRows(t, listBytes, smtpEdgeTables, smtpEdgeColumnRows)
	})
}
