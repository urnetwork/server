package perf

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/session"
)

// The harness's own helpers, without any service. The slots are Redis
// CLUSTER KEYSLOT answers (Redis 8.10).
func TestKeySlotMatchesRedis(t *testing.T) {
	for key, slot := range map[string]uint16{
		"foo":                  12182,
		"bar":                  5061,
		"hello":                866,
		"{user1000}.following": 3443,
		"{ns_0192f7a4-1c9e-7c3a-8f0e-2b1d3c4e5f60}z":   12638,
		"{rv_0192f7a4-1c9e-7c3a-8f0e-2b1d3c4e5f60_17}": 4562,
		"foo{}{bar}": 8363,
		"{nsi_07}z":  14228,
	} {
		if got := keySlot(key); got != slot {
			t.Errorf("keySlot(%q) = %d, want %d", key, got, slot)
		}
	}
}

func TestRefuseTunnelEndpoint(t *testing.T) {
	for _, authority := range []string{
		"127.0.0.1:26379",
		"127.0.0.1:6379",
		"10.213.0.1:6379",
		"10.213.0.1:6427",
		"10.213.0.1:5432",
		"10.213.0.1:6432",
		"[::1]:26379",
		"0.0.0.0:26379",
		"localhost:26379",
		"10.213.0.1",
	} {
		if refuseTunnelEndpoint(authority) == nil {
			t.Errorf("%s was admitted", authority)
		}
	}
	for _, authority := range []string{"10.213.0.1:26379", "10.213.0.1:26381", "10.213.0.1:6378", "10.213.0.1:6428"} {
		if err := refuseTunnelEndpoint(authority); err != nil {
			t.Errorf("%s was refused: %v", authority, err)
		}
	}
}

func TestScriptNameByKeyCount(t *testing.T) {
	prelude := "\nlocal now=tonumber(ARGV[1])\nlocal incarnation=ARGV[2]\n"
	network := "0192f7a4-1c9e-7c3a-8f0e-2b1d3c4e5f60"
	metadata := "{ns_" + network + "}s:0192f7a4-0000-7c3a-8f0e-2b1d3c4e5f60"
	receipt := "{ns_" + network + "}op:0192f7a4-0000-7c3a-8f0e-2b1d3c4e5f60"
	for _, item := range []struct {
		text     string
		keys     int
		fifthKey string
		want     string
	}{
		{prelude, 8, metadata, "mint"},
		{prelude, 4, "", "inventory_or_review"},
		{prelude, 4 + 3*5000, metadata, "list"},
		{prelude, 5 + 3*5000, receipt, "revoke"},
		{prelude, 5 + 3, receipt, "revoke"},
		{"\nlocal now=tonumber(ARGV[1]);local sid=ARGV[2]", 3, "", "last_use_sample"},
		{"\nfor i=3,#ARGV do redis.call('HSET',KEYS[1],ARGV[i],'x') end", 1, "", "record_tombstone"},
		{"\nlocal t=redis.call('TIME')", 1, "", "record_projection"},
		{"\nif ARGV[1]=='del' then", 1, "", "record_network"},
		{"return 1", 1, "", "other"},
	} {
		if got := scriptName(item.text, item.keys, item.fifthKey); got != item.want {
			t.Errorf("scriptName(%q, %d) = %s, want %s", item.text[:min(20, len(item.text))], item.keys, got, item.want)
		}
	}
}

func TestOpRecorderQuantilesAndClasses(t *testing.T) {
	recorder := newOpRecorder()
	base := time.Unix(1_800_000_000, 0)
	for index := 1; index <= 1000; index++ {
		recorder.observeTagged("op", "", base.Add(time.Duration(index)*time.Millisecond), time.Duration(index)*time.Microsecond, "", nil)
	}
	recorder.observeTagged("op", "", base, 0, classify(&session.SessionError{Code: "session_limit_reached", Status: 409}), nil)
	recorder.observeTagged("op", "", base, 0, classify(errors.Join(session.ErrAuthUnavailable, errors.New("down"))), nil)
	summary := recorder.summary("op", time.Time{}, time.Time{}, nil)
	if summary.Count != 1000 || summary.P50Ms != 0.5 || summary.P99Ms != 0.99 || summary.MaxMs != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Errors["refused:session_limit_reached"] != 1 || summary.Errors["unavailable"] != 1 {
		t.Fatalf("errors = %v", summary.Errors)
	}
	if summary.failureCount() != 1 {
		t.Fatalf("a designed refusal counted as a failure: %v", summary.Errors)
	}
	window := recorder.summary("op", base.Add(501*time.Millisecond), base.Add(601*time.Millisecond), nil)
	if window.Count != 100 || window.MaxMs != 0.6 {
		t.Fatalf("window = %+v", window)
	}
	if !strings.HasPrefix(classify(errors.New("409 session_retention_limit_reached")), "refused:") {
		t.Fatal("a wrapped capacity refusal was not classified")
	}
}
