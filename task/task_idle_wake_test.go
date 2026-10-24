// Idle chain marks and producer wakes against the real queue row and Redis.
package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Every mark is in one cluster slot, so a producer clears several in one
// pipeline, and each run-once key has its own mark.
func TestIdleWakeMarksShareOneHashTag(t *testing.T) {
	first, second := idleWakeMarkKey("flush_transfer_debits_0"), idleWakeMarkKey("flush_transfer_debits_15")
	tag := func(key string) string {
		start := strings.Index(key, "{")
		end := strings.Index(key[start+1:], "}")
		if start < 0 || end <= 0 {
			return ""
		}
		return key[start+1 : start+1+end]
	}
	if first == second || tag(first) != "task_idle_wake" || tag(second) != "task_idle_wake" {
		t.Fatal("idle marks do not share one slot under distinct keys", first, second)
	}
}

// A wake kicks a row only while its chain is marked, consumes the mark doing
// so, and writes nothing otherwise; a cleared mark is not kicked.
func TestIdleWakeKicksOnlyAMarkedChain(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		rawKey := "synthetic_idle_wake_" + server.NewId().String()
		later := server.NowUtc().Add(time.Hour)
		taskId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunOnce(rawKey), RunAt(later))
		settings := DefaultIdleWakeSettings()
		read := func() (runAt time.Time, version string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT run_at,xmin::text FROM pending_task WHERE task_id=$1`, taskId).Scan(&runAt, &version))
			})
			return
		}
		_, unmarked := read()
		if kicked, err := WakeIdleRunOnce(ctx, settings, rawKey); err != nil || len(kicked) != 0 {
			t.Fatal("an unmarked chain was kicked", kicked, err)
		}
		if _, version := read(); version != unmarked {
			t.Fatal("a wake wrote an unmarked chain's row")
		}
		server.Raise(MarkIdleRunOnce(ctx, rawKey, settings))
		server.Raise(ClearIdleRunOnce(ctx, rawKey, settings))
		if kicked, err := WakeIdleRunOnce(ctx, settings, rawKey); err != nil || len(kicked) != 0 {
			t.Fatal("a cleared mark was kicked", kicked, err)
		}
		server.Raise(MarkIdleRunOnce(ctx, rawKey, settings))
		kicked, err := WakeIdleRunOnce(ctx, settings, rawKey, rawKey)
		if err != nil || len(kicked) != 1 || kicked[0] != rawKey {
			t.Fatal("a marked chain was not kicked once", kicked, err)
		}
		runAt, kickedVersion := read()
		if runAt.After(server.NowUtc()) {
			t.Fatal("the kick did not pull the chain to now", runAt)
		}
		if kicked, err := WakeIdleRunOnce(ctx, settings, rawKey); err != nil || len(kicked) != 0 {
			t.Fatal("a consumed mark was kicked again", kicked, err)
		}
		if _, version := read(); version != kickedVersion {
			t.Fatal("a wake without a mark wrote the kicked row")
		}
	})
}
