// The connect load table (connect_move_load.go): records stamped with the
// Redis node's clock and read back with it, the stored JSON shape MOVE.md
// §5.4 names, lenient reads that skip what they cannot parse, and the stale
// cleanup's compare-and-delete. The Redis tests run on the local test env.
package model

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A bounded context for one test's Redis calls, which need a deadline.
func connectMoveTestContext(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The Redis node's clock.
func connectMoveTestNodeTime(ctx context.Context, t testing.TB) time.Time {
	t.Helper()
	var nodeTime time.Time
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		nodeTime, err = r.Time(ctx).Result()
		if err != nil {
			t.Fatal(err)
		}
	})
	return nodeTime
}

// Writes a raw table field, as another build or a stalled process left it.
func connectMoveTestSetLoadField(ctx context.Context, t testing.TB, field string, value string) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		if err := r.HSet(ctx, connectMoveLoadKey, field, value).Err(); err != nil {
			t.Fatal(err)
		}
	})
}

// The raw table.
func connectMoveTestLoadFields(ctx context.Context, t testing.TB) map[string]string {
	t.Helper()
	var fields map[string]string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		fields, err = r.HGetAll(ctx, connectMoveLoadKey).Result()
		if err != nil {
			t.Fatal(err)
		}
	})
	return fields
}

// The read record of one exchange, or nil.
func connectMoveTestFindLoad(loads []*ConnectMoveLoad, exchangeId server.Id) *ConnectMoveLoad {
	for _, load := range loads {
		if load.ExchangeId == exchangeId {
			return load
		}
	}
	return nil
}

// A stored record as another process wrote it at writeTime.
func connectMoveTestLoadValue(t testing.TB, writeTime time.Time, residents int) string {
	t.Helper()
	value, err := json.Marshal(&connectMoveLoadValue{
		Time: writeTime.UnixMilli(),
		connectMoveLoadBody: connectMoveLoadBody{
			Residents:   residents,
			HostCpuBusy: 0.5,
			HostCores:   8,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// The table is one key whose braces are the whole key: one cluster slot,
// read with one HGETALL.
func TestConnectMoveLoadKeyIsOneSlot(t *testing.T) {
	if connectMoveLoadKey != "{connect_load}" {
		t.Fatalf("load table key = %q", connectMoveLoadKey)
	}
	field, err := connectMoveLoadField("edge-a.example", "g1", server.Id{})
	if err != nil {
		t.Fatal(err)
	}
	if field != "edge-a.example/g1/"+(server.Id{}).String() {
		t.Fatalf("field = %q", field)
	}
}

// A host or block with a slash would make a field that does not parse back,
// and an empty one names no process: publishing either is refused before
// Redis.
func TestConnectMoveLoadRefusesUnparseableIdentity(t *testing.T) {
	ctx := connectMoveTestContext(t)
	for _, identity := range [][2]string{
		{"edge/a.example", "g1"},
		{"edge-a.example", "g/1"},
		{"", "g1"},
		{"edge-a.example", ""},
	} {
		_, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:       identity[0],
			Block:      identity[1],
			ExchangeId: server.NewId(),
		})
		if err == nil {
			t.Errorf("published identity %q/%q", identity[0], identity[1])
		}
	}
}

// A published record reads back with every value, stamped with the node's
// clock, and is stored as the JSON object MOVE.md §5.4 names, `time` first.
// Publishing again replaces the process's field.
func TestConnectMoveLoadPublishReadsBack(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)

		exchangeId := server.NewId()
		otherExchangeId := server.NewId()
		before := connectMoveTestNodeTime(ctx, t)
		publishTime, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:              "edge-a.example",
			Block:             "g1",
			ExchangeId:        exchangeId,
			Time:              time.Unix(1, 0),
			Residents:         1200,
			ProviderResidents: 900,
			Transports:        1500,
			ProcessCpu:        2.5,
			HostCpuBusy:       0.42,
			HostCores:         72,
			Draining:          false,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:        "edge-b.example",
			Block:       "g2",
			ExchangeId:  otherExchangeId,
			Residents:   10,
			HostCpuBusy: ConnectMoveLoadCpuUnknown,
			HostCores:   96,
			Draining:    true,
		}); err != nil {
			t.Fatal(err)
		}
		after := connectMoveTestNodeTime(ctx, t)

		// the record's time is the node's clock, not the caller's
		if publishTime.Before(before.Truncate(time.Millisecond)) || after.Before(publishTime) {
			t.Fatalf("publish time %s outside the node's clock [%s, %s]", publishTime, before, after)
		}

		loads, nodeTime, err := GetConnectMoveLoads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if nodeTime.Before(publishTime) {
			t.Fatalf("read clock %s before the publish %s", nodeTime, publishTime)
		}
		if len(loads) != 2 {
			t.Fatalf("read %d records, want 2", len(loads))
		}
		load := connectMoveTestFindLoad(loads, exchangeId)
		if load == nil {
			t.Fatal("published record is missing")
		}
		if load.Host != "edge-a.example" || load.Block != "g1" ||
			!load.Time.Equal(publishTime) ||
			load.Residents != 1200 || load.ProviderResidents != 900 || load.Transports != 1500 ||
			load.ProcessCpu != 2.5 || load.HostCpuBusy != 0.42 || load.HostCores != 72 || load.Draining {
			t.Fatalf("read back %+v", load)
		}
		other := connectMoveTestFindLoad(loads, otherExchangeId)
		if other == nil || !other.Draining || other.HostCpuBusy != ConnectMoveLoadCpuUnknown || other.HostCores != 96 {
			t.Fatalf("read back %+v", other)
		}

		field, _ := connectMoveLoadField("edge-a.example", "g1", exchangeId)
		raw := connectMoveTestLoadFields(ctx, t)[field]
		if !strings.HasPrefix(raw, `{"time":`) {
			t.Fatalf("stored record does not start with its time: %s", raw)
		}
		var stored map[string]any
		if err := json.Unmarshal([]byte(raw), &stored); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"time", "residents", "providerResidents", "transports", "processCpu", "hostCpuBusy", "hostCores", "draining"} {
			if _, ok := stored[name]; !ok {
				t.Errorf("stored record lacks %q: %s", name, raw)
			}
		}
		if len(stored) != 8 {
			t.Errorf("stored record has %d values: %s", len(stored), raw)
		}

		// a second publish replaces the field
		if _, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:       "edge-a.example",
			Block:      "g1",
			ExchangeId: exchangeId,
			Residents:  1300,
			HostCores:  72,
		}); err != nil {
			t.Fatal(err)
		}
		loads, _, err = GetConnectMoveLoads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(loads) != 2 || connectMoveTestFindLoad(loads, exchangeId).Residents != 1300 {
			t.Fatalf("second publish did not replace the field: %d records", len(loads))
		}

		// a stopping publisher removes its field
		if err := RemoveConnectMoveLoad(ctx, "edge-a.example", "g1", exchangeId); err != nil {
			t.Fatal(err)
		}
		loads, _, err = GetConnectMoveLoads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(loads) != 1 || loads[0].ExchangeId != otherExchangeId {
			t.Fatalf("remove left %d records", len(loads))
		}
	})
}

// JSON cannot carry NaN or an infinity: a CPU measurement that is not finite
// is stored as unknown, and process CPU as zero, so the record still
// publishes.
func TestConnectMoveLoadPublishesNonFiniteAsUnknown(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		exchangeId := server.NewId()
		if _, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:        "edge-a.example",
			Block:       "g1",
			ExchangeId:  exchangeId,
			ProcessCpu:  math.Inf(1),
			HostCpuBusy: math.NaN(),
			HostCores:   8,
		}); err != nil {
			t.Fatal(err)
		}
		loads, _, err := GetConnectMoveLoads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		load := connectMoveTestFindLoad(loads, exchangeId)
		if load == nil || load.HostCpuBusy != ConnectMoveLoadCpuUnknown || load.ProcessCpu != 0 {
			t.Fatalf("read back %+v", load)
		}
	})
}

// A field this build does not parse, its name or its value, is skipped by
// the read and never deleted by the cleanup: a newer build may write a shape
// this one does not know. Unknown JSON values in a record are allowed.
func TestConnectMoveLoadReadSkipsWhatItCannotParse(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		nodeTime := connectMoveTestNodeTime(ctx, t)
		old := nodeTime.Add(-time.Hour)

		goodExchangeId := server.NewId()
		goodField, _ := connectMoveLoadField("edge-a.example", "g1", goodExchangeId)
		connectMoveTestSetLoadField(ctx, t, goodField, `{"time":`+
			strings.TrimSuffix(strings.TrimPrefix(connectMoveTestLoadValue(t, nodeTime, 5), `{"time":`), "}")+
			`,"futureValue":[1,2]}`)
		malformedFields := map[string]string{
			"edge-a.example/g1":                              connectMoveTestLoadValue(t, old, 1),
			"edge-a.example/g1/not-an-id":                    connectMoveTestLoadValue(t, old, 1),
			"/g1/" + server.NewId().String():                 connectMoveTestLoadValue(t, old, 1),
			"edge-a.example/g1/" + server.NewId().String():   `not json`,
			"edge-a.example/g1/x/" + server.NewId().String(): connectMoveTestLoadValue(t, old, 1),
			"edge-a.example/g2/" + server.NewId().String():   `{"time":"yesterday"}`,
		}
		for field, value := range malformedFields {
			connectMoveTestSetLoadField(ctx, t, field, value)
		}

		loads, readTime, err := GetConnectMoveLoads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(loads) != 1 || loads[0].ExchangeId != goodExchangeId || loads[0].Residents != 5 {
			t.Fatalf("read %d records, want only the parseable one", len(loads))
		}

		removedCount, err := RemoveStaleConnectMoveLoads(ctx, loads, readTime.Add(time.Hour), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if removedCount != 1 {
			t.Fatalf("cleanup removed %d fields, want the 1 parseable stale one", removedCount)
		}
		fields := connectMoveTestLoadFields(ctx, t)
		for field := range malformedFields {
			if _, ok := fields[field]; !ok {
				t.Errorf("cleanup deleted the unparseable field %q", field)
			}
		}
		if _, ok := fields[goodField]; ok {
			t.Error("cleanup kept the stale parseable field")
		}
	})
}

// The read returns every record with the node's clock, so a reader judges
// staleness on the clock that stamped them. The cleanup deletes only records
// older than its timeout at the read, and only while they still hold the
// value read: a process that wrote its field again after the read keeps it.
func TestConnectMoveLoadStaleCleanup(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		nodeTime := connectMoveTestNodeTime(ctx, t)

		staleExchangeId := server.NewId()
		rewrittenExchangeId := server.NewId()
		recentExchangeId := server.NewId()
		freshExchangeId := server.NewId()
		staleField, _ := connectMoveLoadField("edge-a.example", "g1", staleExchangeId)
		rewrittenField, _ := connectMoveLoadField("edge-a.example", "g2", rewrittenExchangeId)
		recentField, _ := connectMoveLoadField("edge-b.example", "g1", recentExchangeId)
		connectMoveTestSetLoadField(ctx, t, staleField, connectMoveTestLoadValue(t, nodeTime.Add(-11*time.Minute), 1))
		connectMoveTestSetLoadField(ctx, t, rewrittenField, connectMoveTestLoadValue(t, nodeTime.Add(-20*time.Minute), 2))
		// stale for the mover (over 60 s) but not yet for the cleanup
		connectMoveTestSetLoadField(ctx, t, recentField, connectMoveTestLoadValue(t, nodeTime.Add(-5*time.Minute), 3))
		if _, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:       "edge-b.example",
			Block:      "g2",
			ExchangeId: freshExchangeId,
			Residents:  4,
			HostCores:  8,
		}); err != nil {
			t.Fatal(err)
		}

		loads, readTime, err := GetConnectMoveLoads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(loads) != 4 {
			t.Fatalf("read %d records, want every record, fresh or not", len(loads))
		}
		recent := connectMoveTestFindLoad(loads, recentExchangeId)
		if age := readTime.Sub(recent.Time); age < 5*time.Minute || 5*time.Minute+30*time.Second < age {
			t.Fatalf("a record written 5 min before the read is %s old on the read's clock", age)
		}

		// the process behind rewrittenField publishes again after the read
		if _, err := PublishConnectMoveLoad(ctx, &ConnectMoveLoad{
			Host:       "edge-a.example",
			Block:      "g2",
			ExchangeId: rewrittenExchangeId,
			Residents:  20,
			HostCores:  8,
		}); err != nil {
			t.Fatal(err)
		}

		// a non-positive timeout deletes nothing
		if removedCount, err := RemoveStaleConnectMoveLoads(ctx, loads, readTime, 0); err != nil || removedCount != 0 {
			t.Fatalf("a zero cleanup timeout removed %d (%v)", removedCount, err)
		}

		removedCount, err := RemoveStaleConnectMoveLoads(ctx, loads, readTime, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if removedCount != 1 {
			t.Fatalf("cleanup removed %d fields, want 1", removedCount)
		}
		fields := connectMoveTestLoadFields(ctx, t)
		if _, ok := fields[staleField]; ok {
			t.Error("cleanup kept the record stale for 11 min")
		}
		if _, ok := fields[rewrittenField]; !ok {
			t.Error("cleanup deleted a record its process wrote again after the read")
		}
		if _, ok := fields[recentField]; !ok {
			t.Error("cleanup deleted a record stale for 5 min, under its timeout")
		}
		freshField, _ := connectMoveLoadField("edge-b.example", "g2", freshExchangeId)
		if _, ok := fields[freshField]; !ok {
			t.Error("cleanup deleted the fresh record")
		}
	})
}
