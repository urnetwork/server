package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"
)

// The scrub migration rewrites raw ip:port persisted before the hashing
// change: pending_task/finished_task client_address, and the top-level
// client_address key inside network-create audit blobs. It must hash what it
// can, blank what it cannot, terminate on rows it declines to touch, and
// leave the nested audit payload intact.
func TestMigrationScrubTaskAndAuditClientAddresses(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientAddress := "11.22.33.44:5566"
		expectedHash, err := ClientIpHash("11.22.33.44")
		assert.Equal(t, err, nil)

		// legacy-shaped task rows: raw address, no hash
		pendingTaskId := NewId()
		finishedTaskId := NewId()
		// a row whose stored address cannot be parsed: must be blanked anyway
		malformedTaskId := NewId()
		Tx(ctx, func(tx PgTx) {
			for _, ins := range []struct {
				taskId  Id
				address string
			}{
				{pendingTaskId, clientAddress},
				{malformedTaskId, "not an address"},
			} {
				RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO pending_task (
							task_id, function_name, args_json, client_address,
							run_at, run_priority, run_max_time_seconds,
							claim_time, release_time
						) VALUES ($1, 'test', '{}', $2, now(), 0, 60, 'epoch', 'epoch')
					`,
					ins.taskId,
					ins.address,
				))
			}
			RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO finished_task (
						task_id, function_name, args_json, client_address,
						run_at, run_priority, run_max_time_seconds,
						run_start_time, run_end_time, result_json
					) VALUES ($1, 'test', '{}', $2, now(), 0, 60, now(), now(), '{}')
				`,
				finishedTaskId,
				clientAddress,
			))
		})

		// legacy-shaped audit rows
		legacyEventId := NewId()
		legacyDetails := fmt.Sprintf(
			`{"network_create":{"network_name":"scrubtest","terms":true},"client_address":%q}`,
			clientAddress,
		)
		// not-json row that still matches the selection predicate: the
		// migration must pass over it exactly once, not spin
		brokenEventId := NewId()
		brokenDetails := `this is not json but mentions "client_address" anyway`
		Tx(ctx, func(tx PgTx) {
			for _, ins := range []struct {
				eventId Id
				details string
			}{
				{legacyEventId, legacyDetails},
				{brokenEventId, brokenDetails},
			} {
				RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO audit_network_event (
							event_id, network_id, event_type, event_details
						) VALUES ($1, $2, 'network_created', $3)
					`,
					ins.eventId,
					NewId(),
					ins.details,
				))
			}
		})

		// the migration must terminate despite the un-parseable rows
		migration_20260807_ScrubTaskAndAuditClientAddresses(ctx)

		// task rows: hashed where parseable, blanked regardless
		for _, check := range []struct {
			table      string
			taskId     Id
			expectHash []byte
			expectPort int
		}{
			{"pending_task", pendingTaskId, expectedHash[:], 5566},
			{"finished_task", finishedTaskId, expectedHash[:], 5566},
			{"pending_task", malformedTaskId, nil, 0},
		} {
			var address string
			var hash []byte
			var port int
			Tx(ctx, func(tx PgTx) {
				result, err := tx.Query(
					ctx,
					`
						SELECT client_address, client_address_hash, client_address_port
						FROM `+check.table+`
						WHERE task_id = $1
					`,
					check.taskId,
				)
				WithPgResult(result, err, func() {
					assert.Equal(t, result.Next(), true)
					Raise(result.Scan(&address, &hash, &port))
				})
			})
			assert.Equal(t, address, "")
			assert.Equal(t, hash, check.expectHash)
			assert.Equal(t, port, check.expectPort)
		}

		// the legacy audit blob: raw key gone, hash present, payload intact
		var details string
		Tx(ctx, func(tx PgTx) {
			result, err := tx.Query(
				ctx,
				`SELECT event_details FROM audit_network_event WHERE event_id = $1`,
				legacyEventId,
			)
			WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				Raise(result.Scan(&details))
			})
		})
		if strings.Contains(details, "11.22.33.44") {
			t.Fatalf("scrubbed audit blob still contains the raw ip: %s", details)
		}
		parsed := map[string]any{}
		assert.Equal(t, json.Unmarshal([]byte(details), &parsed), nil)
		_, hasRaw := parsed["client_address"]
		assert.Equal(t, hasRaw, false)
		assert.Equal(t, parsed["client_address_hash"], hex.EncodeToString(expectedHash[:]))
		assert.Equal(t, parsed["client_port"], float64(5566))
		networkCreate, ok := parsed["network_create"].(map[string]any)
		assert.Equal(t, ok, true)
		assert.Equal(t, networkCreate["network_name"], "scrubtest")

		// the broken row is preserved untouched, not destroyed
		var brokenAfter string
		Tx(ctx, func(tx PgTx) {
			result, err := tx.Query(
				ctx,
				`SELECT event_details FROM audit_network_event WHERE event_id = $1`,
				brokenEventId,
			)
			WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				Raise(result.Scan(&brokenAfter))
			})
		})
		assert.Equal(t, brokenAfter, brokenDetails)

		// idempotence: a second run changes nothing and still terminates
		migration_20260807_ScrubTaskAndAuditClientAddresses(ctx)
	})
}
