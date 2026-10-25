// The public network health status: the reason rules, the stored record, the
// cached read GET /health serves, and the Redis round trip of the operator
// commands.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestValidateNetworkHealthReasonAcceptsPublicText(t *testing.T) {
	for _, reason := range []string{
		"Some connections are slow to start. We're working on it.",
		"Connections in Europe have been failing since 08:10 UTC.",
		"Updated at 08:10:00 UTC; a fix is rolling out.",
		"Sign-in at ur.io is failing. The apps keep working.",
		"The API at api.bringyour.com returns errors for about 30% of requests.",
		"IPv6 connections fail on some networks, e.g. public Wi-Fi.",
		"Card payments are delayed. Nothing is charged twice.",
		"See /outages for earlier incidents.",
		// the examples in monitor/RUN-MAIN.md, "Public network status"
		"Some connections fail to start. We're working on a fix.",
		"Some connections are slow or fail to start. We're working on a fix.",
		"Most new connections fail. We're working on a fix.",
		"Sign-in is failing. Apps that are already connected keep working.",
		"A fix is rolling out. Some connections may still be slow.",
		"  Leading and trailing space is trimmed.  ",
		// at most NetworkHealthReasonMaxLength characters
		"abcd" + strings.Repeat(" abc", (NetworkHealthReasonMaxLength-4)/4),
	} {
		if err := ValidateNetworkHealthReason(reason); err != nil {
			t.Errorf("%q: %v", reason, err)
		}
	}
}

func TestValidateNetworkHealthReasonRefusesWhatMustNotBePublished(t *testing.T) {
	for _, test := range []struct {
		reason string
		names  string
	}{
		{"", "empty"},
		{"   ", "empty"},
		{strings.Repeat("x", NetworkHealthReasonMaxLength+1), "characters"},
		{"Line one\nline two", "one line"},
		{"Tab\tseparated", "one line"},
		{"edge-3 is down", "a host name"},
		{"Fireside ran out of disk", "a host name"},
		{"The xops deploy failed", "an internal tool"},
		{"A bringyourctl command is running", "an internal tool"},
		{"See SIGNALS for the details", "an internal document"},
		{"T0123 tracks this", "an internal document"},
		{"The owner is looking into it", "a person or role"},
		{"Claude is fixing it", "an AI agent"},
		{"Refunds of $5 are queued", "an amount of money"},
		{"A 25 USDC payment is stuck", "an amount of money"},
		{"Circle transfers are slow", "a payment provider"},
		{"The JWT signing key rotated", "a credential"},
		{"Payouts are delayed", "provider earnings"},
		{"See https://status.example.com", "a URL"},
		{"Write to ops@example.com", "an email address"},
		{"10.0.0.1 is unreachable", "an IP address"},
		{"fe80::1 refuses connections", "an IP address"},
		{"2001:db8:0:0:0:0:2:1 refuses connections", "an IP address"},
		{"Check degraded.yml first", "a file name"},
		{"The disk under /var/lib is full", "a file path"},
		{"transfer_contract is slow", "an internal identifier"},
		{"Request 123e4567-e89b-12d3-a456-426614174000 failed", "an identifier"},
		{"Token dGhpc2lzYXZlcnlsb25ndG9rZW52YWx1ZQ leaked", "a token"},
		{"db.internal.example is down", "a host name"},
		{"redis:6379 refuses connections", "a host and port"},
		{"Fixed in 5ff19aebb", "a commit or hash"},
	} {
		err := ValidateNetworkHealthReason(test.reason)
		if err == nil {
			t.Errorf("%q: accepted", test.reason)
		} else if !strings.Contains(err.Error(), test.names) {
			t.Errorf("%q: %v, want it to name %q", test.reason, err, test.names)
		}
	}
}

func TestParseNetworkHealthLevel(t *testing.T) {
	for text, want := range map[string]NetworkHealthStatus{
		"degraded":  NetworkHealthDegraded,
		" Partial ": NetworkHealthPartial,
		"MAJOR":     NetworkHealthMajor,
		"full":      NetworkHealthFull,
	} {
		if level, err := ParseNetworkHealthLevel(text); err != nil || level != want {
			t.Errorf("%q: %q, %v", text, level, err)
		}
	}
	for _, text := range []string{"", "healthy", "unknown", "down"} {
		if _, err := ParseNetworkHealthLevel(text); err == nil {
			t.Errorf("%q: accepted", text)
		}
	}
}

func TestParseNetworkHealthRecord(t *testing.T) {
	valid := `{"status":"partial","reason":"Some connections fail to start.","since":"2026-10-25T08:00:00Z","updated":"2026-10-25T08:10:00Z","source":"operator"}`
	record, err := parseNetworkHealthRecord(valid)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != NetworkHealthPartial || record.Source != NetworkHealthSourceOperator || !record.Since.Equal(time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", record)
	}
	// a newer writer's field is not a reason to stop serving the status
	if _, err := parseNetworkHealthRecord(strings.Replace(valid, `"source"`, `"expires":"2026-10-26T00:00:00Z","source"`, 1)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`not json`,
		`{}`,
		strings.Replace(valid, `"partial"`, `"healthy"`, 1),
		strings.Replace(valid, `"partial"`, `"unknown"`, 1),
		strings.Replace(valid, `"since":"2026-10-25T08:00:00Z",`, "", 1),
		strings.Replace(valid, `Some connections fail to start.`, `edge-3 is down`, 1),
		strings.Replace(valid, `"reason":"Some connections fail to start.",`, "", 1),
	} {
		if _, err := parseNetworkHealthRecord(raw); !errors.Is(err, ErrNetworkHealthMalformed) {
			t.Errorf("%s: %v, want ErrNetworkHealthMalformed", raw, err)
		}
	}
}

func TestNetworkHealthResultOf(t *testing.T) {
	if result := NetworkHealthResultOf(nil); *result != (NetworkHealthResult{Status: NetworkHealthHealthy}) {
		t.Fatalf("%+v", result)
	}
	central := time.FixedZone("CDT", -5*60*60)
	result := NetworkHealthResultOf(&NetworkHealthRecord{
		Status:  NetworkHealthMajor,
		Reason:  "Most connections fail to start.",
		Since:   time.Date(2026, 10, 25, 3, 0, 0, 0, central),
		Updated: time.Date(2026, 10, 25, 3, 30, 0, 0, central),
		Source:  NetworkHealthSourceOperator,
	})
	want := NetworkHealthResult{
		Status:  NetworkHealthMajor,
		Reason:  "Most connections fail to start.",
		Since:   "2026-10-25T08:00:00Z",
		Updated: "2026-10-25T08:30:00Z",
	}
	if *result != want {
		t.Fatalf("%+v, want %+v", result, want)
	}
}

// Replaces the raw read and the clock for one test, with an empty cache.
func stubNetworkHealthRead(t *testing.T, read func() (string, bool, error), now *time.Time) *int {
	t.Helper()
	reads := 0
	previousRead, previousNow := readNetworkHealthRaw, networkHealthNow
	readNetworkHealthRaw = func(context.Context) (string, bool, error) {
		reads += 1
		return read()
	}
	networkHealthNow = func() time.Time { return *now }
	Testing_ResetNetworkHealthCache()
	t.Cleanup(func() {
		readNetworkHealthRaw, networkHealthNow = previousRead, previousNow
		Testing_ResetNetworkHealthCache()
	})
	return &reads
}

func TestGetNetworkHealthReportsTheStoredStatusThroughTheCache(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	raw, present := "", false
	reads := stubNetworkHealthRead(t, func() (string, bool, error) { return raw, present, nil }, &now)

	if result := GetNetworkHealth(ctx); result.Status != NetworkHealthHealthy || result.Reason != "" {
		t.Fatalf("no record: %+v", result)
	}
	raw, present = `{"status":"degraded","reason":"Connections are slow.","since":"2026-10-25T08:00:00Z","updated":"2026-10-25T08:00:00Z","source":"operator"}`, true
	// within the cache period the process keeps its read
	now = now.Add(networkHealthCacheTtl - time.Second)
	if result := GetNetworkHealth(ctx); result.Status != NetworkHealthHealthy || *reads != 1 {
		t.Fatalf("cached: %+v after %d reads", result, *reads)
	}
	now = now.Add(time.Second)
	result := GetNetworkHealth(ctx)
	if result.Status != NetworkHealthDegraded || result.Reason != "Connections are slow." || result.Since != "2026-10-25T08:00:00Z" || *reads != 2 {
		t.Fatalf("refreshed: %+v after %d reads", result, *reads)
	}
	// a clock that moved behind the read refreshes rather than trusting it
	now = now.Add(-time.Minute)
	GetNetworkHealth(ctx)
	if *reads != 3 {
		t.Fatalf("%d reads after the clock moved back", *reads)
	}
}

func TestGetNetworkHealthIsUnknownForWhatItCannotTrust(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	var readErr error
	raw := ""
	reads := stubNetworkHealthRead(t, func() (string, bool, error) { return raw, readErr == nil, readErr }, &now)

	readErr = errors.New("redis is down")
	if result := GetNetworkHealth(ctx); *result != (NetworkHealthResult{Status: NetworkHealthUnknown}) {
		t.Fatalf("read error: %+v", result)
	}
	// a failed read is retried soon, not after the whole cache period
	now = now.Add(networkHealthErrorRetry)
	readErr = nil
	raw = `{"status":"full","reason":"edge-3 is down","since":"2026-10-25T08:00:00Z","updated":"2026-10-25T08:00:00Z","source":"operator"}`
	if result := GetNetworkHealth(ctx); *result != (NetworkHealthResult{Status: NetworkHealthUnknown}) || *reads != 2 {
		t.Fatalf("a stored reason that fails the rules is never served: %+v after %d reads", result, *reads)
	}
	// a malformed record is kept for the whole cache period
	now = now.Add(networkHealthErrorRetry)
	GetNetworkHealth(ctx)
	if *reads != 2 {
		t.Fatalf("%d reads within the cache period of a malformed record", *reads)
	}
}

func TestSetNetworkHealthRefusesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	reads := stubNetworkHealthRead(t, func() (string, bool, error) { return "", false, nil }, &now)
	for _, test := range []struct {
		level  NetworkHealthStatus
		reason string
		source NetworkHealthSource
	}{
		{NetworkHealthHealthy, "Connections are slow.", NetworkHealthSourceOperator},
		{NetworkHealthUnknown, "Connections are slow.", NetworkHealthSourceOperator},
		{NetworkHealthDegraded, "edge-3 is slow", NetworkHealthSourceOperator},
		{NetworkHealthDegraded, "", NetworkHealthSourceOperator},
		{NetworkHealthDegraded, "Connections are slow.", ""},
	} {
		if _, err := SetNetworkHealth(ctx, test.level, test.reason, test.source); err == nil {
			t.Errorf("%+v: accepted", test)
		}
	}
	if *reads != 0 {
		t.Fatalf("a refused status read Redis %d times", *reads)
	}
}

func TestNetworkHealthRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
		previousNow := networkHealthNow
		networkHealthNow = func() time.Time { return now }
		Testing_ResetNetworkHealthCache()
		defer func() {
			networkHealthNow = previousNow
			Testing_ResetNetworkHealthCache()
		}()
		clear := func() {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Del(ctx, networkHealthKey).Err())
			})
		}
		clear()
		defer clear()

		if record, err := ReadNetworkHealth(ctx); err != nil || record != nil {
			t.Fatalf("no record: %+v, %v", record, err)
		}
		if previous, cleared, err := ClearNetworkHealth(ctx); err != nil || cleared || previous != nil {
			t.Fatalf("clearing a healthy network: %+v, %t, %v", previous, cleared, err)
		}

		record, err := SetNetworkHealth(ctx, NetworkHealthPartial, "  Some connections fail to start.  ", NetworkHealthSourceOperator)
		if err != nil {
			t.Fatal(err)
		}
		if record.Reason != "Some connections fail to start." || !record.Since.Equal(now) || !record.Updated.Equal(now) {
			t.Fatalf("%+v", record)
		}
		// the writer's process sees its own change at once
		if result := GetNetworkHealth(ctx); result.Status != NetworkHealthPartial || result.Since != "2026-10-25T08:00:00Z" {
			t.Fatalf("%+v", result)
		}

		// a change of level or reason keeps the time the network left healthy
		now = now.Add(30 * time.Minute)
		record, err = SetNetworkHealth(ctx, NetworkHealthMajor, "Most connections fail to start.", NetworkHealthSourceOperator)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := ReadNetworkHealth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != NetworkHealthMajor || !stored.Since.Equal(now.Add(-30*time.Minute)) || !stored.Updated.Equal(now) || *stored != *record {
			t.Fatalf("stored %+v, set %+v", stored, record)
		}

		previous, cleared, err := ClearNetworkHealth(ctx)
		if err != nil || !cleared || previous == nil || previous.Status != NetworkHealthMajor {
			t.Fatalf("recover: %+v, %t, %v", previous, cleared, err)
		}
		if result := GetNetworkHealth(ctx); *result != (NetworkHealthResult{Status: NetworkHealthHealthy}) {
			t.Fatalf("after recover: %+v", result)
		}

		// a malformed record reads as an error, is replaced by a new status, and can be cleared
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, networkHealthKey, "not json", 0).Err())
		})
		if _, err := ReadNetworkHealth(ctx); !errors.Is(err, ErrNetworkHealthMalformed) {
			t.Fatalf("malformed: %v", err)
		}
		if record, err := SetNetworkHealth(ctx, NetworkHealthDegraded, "Connections are slow.", NetworkHealthSourceOperator); err != nil || !record.Since.Equal(now) {
			t.Fatalf("replacing a malformed record: %+v, %v", record, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, networkHealthKey, "not json", 0).Err())
		})
		if previous, cleared, err := ClearNetworkHealth(ctx); err != nil || !cleared || previous != nil {
			t.Fatalf("clearing a malformed record: %+v, %t, %v", previous, cleared, err)
		}
	})
}
