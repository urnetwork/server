package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// §13.9's kill switch on a private database and Redis (TLS-M5-13): a set
// writes the row and, in one MULTI on one slot, the key's field and its
// trigger; a lift by the case and a lift by until remove both; a missing row
// deletes no key; and a trigger's channel parses back to its network.

// The network's {tes_} hash as its readers decode it, and its trigger's count.
func tlsSuspensionTestKey(t testing.TB, ctx context.Context, networkId server.Id) (map[string]*sidecar.TlsKillEntry, int64) {
	t.Helper()
	entries := map[string]*sidecar.TlsKillEntry{}
	var triggerCount int64
	server.Redis(ctx, func(r server.RedisClient) {
		fieldValues, err := r.HGetAll(ctx, sidecar.TlsKillKey(sidecar.Id(networkId))).Result()
		if err != nil {
			t.Fatal(err)
		}
		for field, value := range fieldValues {
			entry := &sidecar.TlsKillEntry{}
			if err := json.Unmarshal([]byte(value), entry); err != nil {
				t.Fatal(err)
			}
			entries[field] = entry
		}
		triggerCount, err = r.Get(ctx, sidecar.TlsKillTriggerKey(sidecar.Id(networkId))).Int64()
		if err != nil && !errors.Is(err, server.RedisNil) {
			t.Fatal(err)
		}
	})
	return entries, triggerCount
}

// The network's rows, by host.
func tlsSuspensionTestRows(t testing.TB, ctx context.Context, networkId server.Id) map[string]*sidecar.TlsSuspendArgs {
	t.Helper()
	suspensions, err := GetTlsSuspensions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]*sidecar.TlsSuspendArgs{}
	for _, suspension := range suspensions {
		if suspension.NetworkId == sidecar.Id(networkId) {
			rows[suspension.Host] = suspension
		}
	}
	return rows
}

func TestTlsSuspensionUnlinkedWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(networkId), Host: "blog", Reason: "abuse:case"})
		if !errors.Is(err, ErrTlsSuspensionUnavailable) {
			t.Fatalf("a set without MG5 answered %v", err)
		}
		if err := LiftTlsSuspension(ctx, networkId, "blog"); !errors.Is(err, ErrTlsSuspensionUnavailable) {
			t.Fatalf("a lift without MG5 answered %v", err)
		}
		if _, err := GetTlsSuspensions(ctx); !errors.Is(err, ErrTlsSuspensionUnavailable) {
			t.Fatalf("a read without MG5 answered %v", err)
		}
		if entries, triggerCount := tlsSuspensionTestKey(t, ctx, networkId); len(entries) != 0 || triggerCount != 0 {
			t.Fatalf("a set without MG5 wrote %v, trigger %d", entries, triggerCount)
		}
	})
}

// Both keys of a network share one hash tag, so the MULTI is one slot's.
func TestTlsSuspensionKeysShareOneSlot(t *testing.T) {
	for range 64 {
		networkId := sidecar.NewId()
		keys := []string{sidecar.TlsKillKey(networkId), sidecar.TlsKillTriggerKey(networkId)}
		if !sidecar.RedisKeysShareTag(keys) {
			t.Fatalf("%v do not share a tag", keys)
		}
		if server.RedisHashSlot(keys[0]) != server.RedisHashSlot(keys[1]) {
			t.Fatalf("%v are on two slots", keys)
		}
	}
}

func TestTlsSuspensionSetWritesRowKeyAndOneTrigger(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		until := server.NowUtc().Add(time.Hour)

		if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(networkId), Host: "blog", Reason: "abuse:case-1", Until: until}); err != nil {
			t.Fatal(err)
		}
		rows := tlsSuspensionTestRows(t, ctx, networkId)
		if len(rows) != 1 || rows["blog"] == nil || rows["blog"].Reason != "abuse:case-1" || !rows["blog"].Until.Equal(until.Truncate(time.Microsecond)) {
			t.Fatalf("rows %+v", rows["blog"])
		}
		entries, triggerCount := tlsSuspensionTestKey(t, ctx, networkId)
		if len(entries) != 1 || triggerCount != 1 {
			t.Fatalf("%d fields and %d triggers after one write", len(entries), triggerCount)
		}
		entry := entries["blog"]
		if entry == nil || entry.Host != "blog" || entry.Reason != "abuse:case-1" || !entry.Until.Equal(rows["blog"].Until) {
			t.Fatalf("the key's entry is %+v", entry)
		}
		now := server.NowUtc()
		if !entry.Kills("blog", now) || entry.Kills("shop", now) || entry.Kills("blog", until.Add(time.Second)) {
			t.Fatal("the entry does not kill its host until its until")
		}

		// every host, with no until, beside the first; a second write of one kill replaces it
		if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(networkId), Host: sidecar.TlsKillAllHosts, Reason: "abuse:case-2"}); err != nil {
			t.Fatal(err)
		}
		if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(networkId), Host: "blog", Reason: "abuse:case-3"}); err != nil {
			t.Fatal(err)
		}
		rows = tlsSuspensionTestRows(t, ctx, networkId)
		entries, triggerCount = tlsSuspensionTestKey(t, ctx, networkId)
		if len(rows) != 2 || len(entries) != 2 || triggerCount != 3 {
			t.Fatalf("%d rows, %d fields and %d triggers after three writes", len(rows), len(entries), triggerCount)
		}
		if !rows["blog"].Until.IsZero() || rows["blog"].Reason != "abuse:case-3" || !entries["blog"].Until.IsZero() {
			t.Fatalf("the replaced kill is %+v and %+v", rows["blog"], entries["blog"])
		}
		if !entries[sidecar.TlsKillAllHosts].Kills("anything", now.Add(1000*time.Hour)) {
			t.Fatal("a kill of every host with no until does not hold")
		}
		if entries, triggerCount := tlsSuspensionTestKey(t, ctx, otherNetworkId); len(entries) != 0 || triggerCount != 0 {
			t.Fatal("another network's key was written")
		}

		// what a kill cannot name is refused before anything is written
		for _, args := range []*sidecar.TlsSuspendArgs{
			nil,
			{Host: "blog", Reason: "abuse:case"},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "", Reason: "abuse:case"},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "Blog", Reason: "abuse:case"},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "blog.", Reason: "abuse:case"},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "-blog", Reason: "abuse:case"},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "*.blog", Reason: "abuse:case"},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "blog", Reason: ""},
			{NetworkId: sidecar.Id(otherNetworkId), Host: "blog", Reason: string(make([]byte, 257))},
		} {
			if err := SetTlsSuspension(ctx, args); !errors.Is(err, ErrTlsSuspensionInvalid) {
				t.Fatalf("%+v answered %v", args, err)
			}
		}
		if rows := tlsSuspensionTestRows(t, ctx, otherNetworkId); len(rows) != 0 {
			t.Fatalf("a refused kill wrote %v", rows)
		}
	})
}

func TestTlsSuspensionLiftByCase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		for _, host := range []string{"blog", "@"} {
			if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(networkId), Host: host, Reason: "abuse:case"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := LiftTlsSuspension(ctx, networkId, "blog"); err != nil {
			t.Fatal(err)
		}
		rows := tlsSuspensionTestRows(t, ctx, networkId)
		entries, triggerCount := tlsSuspensionTestKey(t, ctx, networkId)
		if len(rows) != 1 || rows["@"] == nil || len(entries) != 1 || entries["@"] == nil || triggerCount != 3 {
			t.Fatalf("after one lift: rows %v, fields %v, %d triggers", rows, entries, triggerCount)
		}

		// a restore lost the row and kept the key: nothing but the case's lift removes the key
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_tls_suspension WHERE network_id = $1`, networkId))
		})
		if liftedCount, err := LiftExpiredTlsSuspensions(ctx, server.NowUtc().Add(1000*time.Hour)); err != nil || liftedCount != 0 {
			t.Fatalf("a sweep with no row lifted %d, %v", liftedCount, err)
		}
		if entries, _ := tlsSuspensionTestKey(t, ctx, networkId); entries["@"] == nil {
			t.Fatal("a missing row deleted its key")
		}
		if err := LiftTlsSuspension(ctx, networkId, "@"); err != nil {
			t.Fatal(err)
		}
		if entries, triggerCount := tlsSuspensionTestKey(t, ctx, networkId); len(entries) != 0 || triggerCount != 4 {
			t.Fatalf("after the case's lift: fields %v, %d triggers", entries, triggerCount)
		}
	})
}

func TestTlsSuspensionLiftByUntil(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		set := func(host string, until time.Time) {
			t.Helper()
			if err := SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: sidecar.Id(networkId), Host: host, Reason: "abuse:case", Until: until}); err != nil {
				t.Fatal(err)
			}
		}
		set("past", now.Add(-time.Minute))
		set("ahead", now.Add(time.Hour))
		set("standing", time.Time{})
		// set again with a later until after its row expired: the row's entry is no longer the key's
		set("renewed", now.Add(-time.Minute))
		value, err := tlsSuspensionEntryValue("renewed", "abuse:case", now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.HSet(ctx, sidecar.TlsKillKey(sidecar.Id(networkId)), "renewed", value).Err(); err != nil {
				t.Fatal(err)
			}
		})
		_, triggerCount := tlsSuspensionTestKey(t, ctx, networkId)

		liftedCount, err := LiftExpiredTlsSuspensions(ctx, now)
		if err != nil || liftedCount != 2 {
			t.Fatalf("the sweep lifted %d, %v", liftedCount, err)
		}
		rows := tlsSuspensionTestRows(t, ctx, networkId)
		if len(rows) != 2 || rows["ahead"] == nil || rows["standing"] == nil {
			t.Fatalf("rows after the sweep: %v", rows)
		}
		entries, triggerCountAfter := tlsSuspensionTestKey(t, ctx, networkId)
		if len(entries) != 3 || entries["past"] != nil || entries["renewed"] == nil {
			t.Fatalf("fields after the sweep: %v", entries)
		}
		// one trigger for the one field that went
		if triggerCountAfter != triggerCount+1 {
			t.Fatalf("%d triggers for one lifted field", triggerCountAfter-triggerCount)
		}
		if liftedCount, err := LiftExpiredTlsSuspensions(ctx, now); err != nil || liftedCount != 0 {
			t.Fatalf("a second sweep lifted %d, %v", liftedCount, err)
		}
	})
}

func TestTlsSuspensionKeyEventParsesBackToItsNetwork(t *testing.T) {
	networkId := server.NewId()
	pattern := TlsSuspensionKeyEventPattern(3)
	if pattern != "__keyspace@3__:{tes_*}n" {
		t.Fatalf("the pattern is %q", pattern)
	}
	channel := fmt.Sprintf("__keyspace@3__:%s", sidecar.TlsKillTriggerKey(sidecar.Id(networkId)))
	if parsed, ok := ParseTlsSuspensionKeyEvent(channel); !ok || parsed != networkId {
		t.Fatalf("the trigger's channel parsed to %v, %v", parsed, ok)
	}
	for _, channel := range []string{
		fmt.Sprintf("__keyspace@3__:%s", sidecar.TlsKillKey(sidecar.Id(networkId))),
		"__keyspace@3__:{tes_not-an-id}n",
		"__keyspace@3__:{scp_" + networkId.String() + "}n",
		sidecar.TlsKillTriggerKey(sidecar.Id(networkId)),
		"",
	} {
		if _, ok := ParseTlsSuspensionKeyEvent(channel); ok {
			t.Fatalf("%q parsed as a trigger", channel)
		}
	}
}
