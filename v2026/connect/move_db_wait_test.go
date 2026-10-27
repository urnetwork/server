package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// A pool reading the test sets between guard readings.
type moveDbPoolWaitFixture struct {
	stat server.DbPoolAcquireStat
	ok   bool
	now  time.Time
}

func (self *moveDbPoolWaitFixture) wait() *moveDbPoolWait {
	return &moveDbPoolWait{
		acquireStat: func() (server.DbPoolAcquireStat, bool) {
			return self.stat, self.ok
		},
		now: func() time.Time {
			return self.now
		},
	}
}

// The guard reads the mean wait of the acquires between its readings.
func TestMoveDbPoolWaitIsTheMeanSinceTheLastReading(t *testing.T) {
	fixture := &moveDbPoolWaitFixture{
		stat: server.DbPoolAcquireStat{AcquireCount: 100, AcquireDuration: 2 * time.Second, MaxConns: 8},
		ok:   true,
		now:  time.Unix(1000, 0),
	}
	wait := fixture.wait()
	// the first reading has no interval
	connect.AssertEqual(t, time.Duration(0), wait.PoolAcquireWait())

	fixture.stat.AcquireCount += 4
	fixture.stat.AcquireDuration += 400 * time.Millisecond
	fixture.now = fixture.now.Add(10 * time.Second)
	connect.AssertEqual(t, 100*time.Millisecond, wait.PoolAcquireWait())

	// nothing acquired and the pool not full: nothing waited
	fixture.now = fixture.now.Add(10 * time.Second)
	connect.AssertEqual(t, time.Duration(0), wait.PoolAcquireWait())
}

// A pool with every connection checked out and no acquire completing reads as
// having waited the whole interval: its waiters are still queued.
func TestMoveDbPoolWaitCountsAStuckPool(t *testing.T) {
	fixture := &moveDbPoolWaitFixture{
		stat: server.DbPoolAcquireStat{AcquireCount: 100, AcquireDuration: time.Second, AcquiredConns: 8, MaxConns: 8},
		ok:   true,
		now:  time.Unix(1000, 0),
	}
	wait := fixture.wait()
	wait.PoolAcquireWait()

	fixture.now = fixture.now.Add(10 * time.Second)
	connect.AssertEqual(t, 10*time.Second, wait.PoolAcquireWait())
}

// An unopened pool waits nothing, and a reset that restarted the counts gives
// no interval rather than a negative one.
func TestMoveDbPoolWaitWithoutAnInterval(t *testing.T) {
	fixture := &moveDbPoolWaitFixture{
		ok:  false,
		now: time.Unix(1000, 0),
	}
	wait := fixture.wait()
	connect.AssertEqual(t, time.Duration(0), wait.PoolAcquireWait())

	fixture.ok = true
	fixture.stat = server.DbPoolAcquireStat{AcquireCount: 50, AcquireDuration: 5 * time.Second, MaxConns: 8}
	fixture.now = fixture.now.Add(10 * time.Second)
	// the pool opened since the last reading: no interval yet
	connect.AssertEqual(t, time.Duration(0), wait.PoolAcquireWait())

	// a reset restarted the counts below the last reading
	fixture.stat = server.DbPoolAcquireStat{AcquireCount: 10, AcquireDuration: time.Second, MaxConns: 8}
	fixture.now = fixture.now.Add(10 * time.Second)
	connect.AssertEqual(t, time.Duration(0), wait.PoolAcquireWait())

	fixture.stat.AcquireCount += 2
	fixture.stat.AcquireDuration += 60 * time.Millisecond
	fixture.now = fixture.now.Add(10 * time.Second)
	connect.AssertEqual(t, 30*time.Millisecond, wait.PoolAcquireWait())
}
