package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// The connect.day writer's per-process memory: one write per client per UTC
// day, reset when the day changes, retried after a failure.
func TestConnectDayCache(t *testing.T) {
	day1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	a, b := server.NewId(), server.NewId()
	c := &connectDayCache{}

	connect.AssertEqual(t, true, c.remember(a, day1))
	connect.AssertEqual(t, false, c.remember(a, day1))
	connect.AssertEqual(t, true, c.remember(b, day1))
	// a failed write is retried on the next connect
	c.forget(a, day1)
	connect.AssertEqual(t, true, c.remember(a, day1))
	// a new UTC day starts over for every client
	connect.AssertEqual(t, true, c.remember(a, day2))
	connect.AssertEqual(t, true, c.remember(b, day2))
	connect.AssertEqual(t, false, c.remember(b, day2))
	// forgetting a stale day changes nothing
	c.forget(a, day1)
	connect.AssertEqual(t, false, c.remember(a, day2))
}

func TestConnectDayStart(t *testing.T) {
	loc := time.FixedZone("west", -7*60*60)
	at := time.Date(2026, 9, 9, 20, 30, 0, 0, loc) // 03:30 UTC on the 10th
	connect.AssertEqual(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), ConnectDayStart(at))
	connect.AssertEqual(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), ConnectDayStart(time.Date(2026, 9, 10, 23, 59, 59, 0, time.UTC)))
}
