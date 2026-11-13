package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	// the zones of the child processes, where a system has no zone database
	_ "time/tzdata"

	"github.com/urnetwork/server/v2026"
)

// Names the zone a child process of
// TestSessionIndexReviewIsTheSessionHorizonInAnyZone checks in.
const sessionIndexHorizonZoneEnvName = "URNETWORK_TEST_SESSION_INDEX_HORIZON_ZONE"

// A mint queues its network's session index for review at the session's
// accept horizon. The outbox's next_review_time column holds UTC without a
// zone, and pgx writes a time's wall clock, so the review is queued at the
// horizon only when that time is a UTC one: in a process west of UTC it was
// queued early by the zone's offset, and east of UTC late. A process takes its
// zone when it starts, so the check runs in a child process in each zone.
func TestSessionIndexReviewIsTheSessionHorizonInAnyZone(t *testing.T) {
	if zone := os.Getenv(sessionIndexHorizonZoneEnvName); zone != "" {
		testSessionIndexReviewIsTheSessionHorizon(t, zone)
		return
	}
	for _, zone := range []string{"America/Chicago", "Asia/Tokyo"} {
		command := exec.CommandContext(
			t.Context(),
			os.Args[0],
			"-test.run=^TestSessionIndexReviewIsTheSessionHorizonInAnyZone$",
			"-test.count=1",
			"-test.v",
		)
		command.Env = append(os.Environ(), "TZ="+zone, sessionIndexHorizonZoneEnvName+"="+zone)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestSessionIndexReviewIsTheSessionHorizonInAnyZone") {
			// the check's own lines, without the child's logs
			checkLines := []string{}
			for _, line := range strings.Split(string(output), "\n") {
				line = strings.TrimSpace(line)
				if strings.Contains(line, "_test.go:") || strings.Contains(line, "test_util.go:") ||
					strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "panic:") {
					checkLines = append(checkLines, line)
				}
			}
			t.Errorf("in %s: %v\n%s", zone, err, strings.Join(checkLines, "\n"))
		}
	}
}

// The check, in a process whose zone is zone.
func testSessionIndexReviewIsTheSessionHorizon(t *testing.T, zone string) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, processOffset := now.Zone()
	_, zoneOffset := now.In(location).Zone()
	if processOffset == 0 || processOffset != zoneOffset {
		t.Fatalf("the process's zone is not %s", zone)
	}

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		actor := recoverySessionActor(t)
		claims := NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "session-index-horizon", false, false)
		sessionId := server.NewId()
		claims.SessionId = &sessionId
		if _, err := MintNetworkSession(ctx, claims, "password"); err != nil {
			t.Fatal(err)
		}

		horizon := time.UnixMilli(DeadlineMillis(claims.AcceptUntil()))
		var nextReviewTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT next_review_time FROM network_session_index_outbox WHERE network_id = $1`,
				claims.NetworkId,
			).Scan(&nextReviewTime))
		})
		if !nextReviewTime.Equal(horizon) {
			t.Fatalf("in %s the network's index was queued for review at %s, want the session's horizon %s", zone, nextReviewTime.UTC(), horizon.UTC())
		}
	})
}
