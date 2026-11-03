package model

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	// the zones of the child processes, where a system has no zone database
	_ "time/tzdata"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Names the zone a child process of TestAuthCodeEndsAtTheSessionHorizonInAnyZone
// checks in.
const authCodeHorizonZoneEnvName = "URNETWORK_TEST_AUTH_CODE_HORIZON_ZONE"

// A session's accept horizon caps an auth code that would outlive it. The
// end_time column holds UTC without a zone, and pgx writes a time's wall clock,
// so the code ends at the horizon only when the capped end is a UTC time: in a
// process west of UTC the code was over before it was made, and east of UTC it
// outlived its session by the zone's offset. A process takes its zone when it
// starts, so the check runs in a child process in each zone.
func TestAuthCodeEndsAtTheSessionHorizonInAnyZone(t *testing.T) {
	if zone := os.Getenv(authCodeHorizonZoneEnvName); zone != "" {
		testAuthCodeEndsAtTheSessionHorizon(t, zone)
		return
	}
	for _, zone := range []string{"America/Chicago", "Asia/Tokyo"} {
		command := exec.CommandContext(
			t.Context(),
			os.Args[0],
			"-test.run=^TestAuthCodeEndsAtTheSessionHorizonInAnyZone$",
			"-test.count=1",
			"-test.v",
		)
		command.Env = append(os.Environ(), "TZ="+zone, authCodeHorizonZoneEnvName+"="+zone)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestAuthCodeEndsAtTheSessionHorizonInAnyZone") {
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
func testAuthCodeEndsAtTheSessionHorizon(t *testing.T, zone string) {
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
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		ctx := t.Context()

		networkId, userId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, "auth-code-horizon-"+networkId.String(), userId)
		claims := session.NewByJwt(networkId, userId, "auth-code-horizon", false, false)
		// the session is accepted for an hour more, so a two-hour code ends at
		// its horizon
		claims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour - session.SessionGrace))
		if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
			t.Fatal(err)
		}
		creator := session.NewLocalClientSession(ctx, "192.0.2.1:1", claims)
		defer creator.Cancel()

		created, err := AuthCodeCreate(&AuthCodeCreateArgs{DurationMinutes: 120, Uses: 1}, creator)
		if err != nil || created == nil || created.Error != nil || created.AuthCode == "" {
			t.Fatal("code creation failed", created, err)
		}
		horizon := time.UnixMilli(session.DeadlineMillis(claims.AcceptUntil()))
		var endTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT end_time FROM auth_code WHERE auth_code = $1`, created.AuthCode).Scan(&endTime))
		})
		if !endTime.Equal(horizon) {
			t.Fatalf("in %s the code was stored to end at %s, want the session's horizon %s", zone, endTime.UTC(), horizon.UTC())
		}

		// and it is redeemed until then
		requestId := server.NewId()
		login := session.NewLocalClientSession(ctx, "192.0.2.2:1", nil)
		defer login.Cancel()
		redeemed, err := AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: created.AuthCode, RequestId: &requestId}, login)
		if err != nil || redeemed == nil || redeemed.Error != nil || redeemed.ByJwt == "" {
			t.Fatalf("in %s a code inside its session's horizon was not redeemed: %v, %v", zone, redeemed, err)
		}
	})
}
