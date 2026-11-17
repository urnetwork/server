package model

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	// the zones of the child processes, where a system has no zone database
	_ "time/tzdata"

	"github.com/urnetwork/server"
)

// Names the zone a child process of
// TestExpiredOpenContractsCutOffAtTheTimeoutInAnyZone checks in.
const expiredContractZoneEnvName = "URNETWORK_TEST_EXPIRED_CONTRACT_ZONE"

// An open contract with a party's close and a checkpoint is expired once both
// are older than the close timeout. close_time holds UTC without a zone, and
// pgx writes a time's wall clock, so the cutoff is the timeout ago only when
// it is a UTC time: in a process west of UTC it fell earlier by the zone's
// offset and missed expired contracts, and east of UTC later and named fresh
// ones. A process takes its zone when it starts, so the check runs in a child
// process in each zone.
func TestExpiredOpenContractsCutOffAtTheTimeoutInAnyZone(t *testing.T) {
	if zone := os.Getenv(expiredContractZoneEnvName); zone != "" {
		testExpiredOpenContractsCutOffAtTheTimeout(t, zone)
		return
	}
	for _, zone := range []string{"America/Chicago", "Asia/Tokyo"} {
		command := exec.CommandContext(
			t.Context(),
			os.Args[0],
			"-test.run=^TestExpiredOpenContractsCutOffAtTheTimeoutInAnyZone$",
			"-test.count=1",
			"-test.v",
		)
		command.Env = append(os.Environ(), "TZ="+zone, expiredContractZoneEnvName+"="+zone)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestExpiredOpenContractsCutOffAtTheTimeoutInAnyZone") {
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
func testExpiredOpenContractsCutOffAtTheTimeout(t *testing.T, zone string) {
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
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: networkId, destinationId: networkId})
		timeout := time.Hour
		closedAt := func(closeTime time.Time) server.Id {
			contractId, err := CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 1000)
			if err != nil {
				t.Fatal(err)
			}
			// the source's close and the destination's checkpoint
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO contract_close (contract_id, party, used_transfer_byte_count, close_time, checkpoint)
					VALUES ($1, 'source', 100, $2, false), ($1, 'destination', 100, $2, true)`,
					contractId,
					closeTime,
				))
			})
			return contractId
		}
		now := server.NowUtc()
		expired := closedAt(now.Add(-timeout - 10*time.Minute))
		fresh := closedAt(now.Add(-timeout + 10*time.Minute))

		expiredContractIds := GetExpiredOpenContractIds(ctx, timeout)
		if !expiredContractIds[expired] || expiredContractIds[fresh] {
			t.Fatalf(
				"in %s a contract closed %s ago read expired=%t and one closed %s ago expired=%t; want only the first",
				zone, timeout+10*time.Minute, expiredContractIds[expired], timeout-10*time.Minute, expiredContractIds[fresh],
			)
		}
	})
}
