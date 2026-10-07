package model

// These tests use the leased private database from DefaultTestEnv. Every
// replica has its own ledger; PostgreSQL owns the shared admission boundary.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Holds every request at the transaction entrance before any count is read.
type testReleaseReplicaLedger struct {
	*NetworkExtenderReleaseLedger
	arrived *sync.WaitGroup
	proceed <-chan struct{}
}

// The wrapper does not supply synchronization inside the real database ledger.
func (self *testReleaseReplicaLedger) Transact(identity []byte, vantage string, country string, keys []string, apply func(connect.ExtenderReleaseLedgerTx)) {
	self.arrived.Done()
	<-self.proceed
	self.NetworkExtenderReleaseLedger.Transact(identity, vantage, country, keys, apply)
}

// Independent replicas cannot exceed any of the three shared admission caps.
func TestExtenderReleaseReplicasShareAtomicBudgets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := time.Unix(1700000000, 0)
		publicKey := []byte("synthetic-release-extender-key-01")
		sign, _ := testRecordSigner()
		activation := ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 4, "192.0.2.1"), sign)
		key := hex.EncodeToString(publicKey)
		keys := []string{key}
		for _, budget := range []string{"country", "identity", "vantage"} {
			settings := connect.DefaultExtenderReleaseSettings()
			settings.Now = func() time.Time { return now }
			settings.IdentityRequestLimit = 0
			settings.VantageRequestLimit = 0
			settings.MaxClientsPerExtenderPerCountry = 0
			var wantErr error
			switch budget {
			case "country":
				settings.MaxClientsPerExtenderPerCountry = 10
			case "identity":
				settings.IdentityRequestLimit = 10
				wantErr = connect.ErrExtenderReleaseIdentityLimited
			case "vantage":
				settings.VantageRequestLimit = 10
				wantErr = connect.ErrExtenderReleaseVantageLimited
			}
			const requestCount = 11
			var arrived sync.WaitGroup
			arrived.Add(requestCount)
			proceed := make(chan struct{})
			type outcome struct {
				result *connect.ExtenderReleaseResult
				err    error
			}
			outcomes := make(chan outcome, requestCount)
			for i := range requestCount {
				go func() {
					identity := []byte(fmt.Sprintf("synthetic-%s-%d", budget, i))
					vantage := fmt.Sprintf("synthetic-%s-%d", budget, i)
					if budget == "identity" {
						identity = []byte("synthetic-shared-identity")
					}
					if budget == "vantage" {
						vantage = "192.0.2.0/24"
					}
					ledger := &testReleaseReplicaLedger{
						NetworkExtenderReleaseLedger: NewNetworkExtenderReleaseLedger(ctx, map[string]server.Id{key: activation.Extender.ExtenderId}),
						arrived:                      &arrived, proceed: proceed,
					}
					policy := connect.NewExtenderReleasePolicy([]byte("synthetic-release-secret"), ledger, nil, settings)
					result, err := policy.Release(&connect.ExtenderReleaseRequest{Identity: identity, Vantage: vantage, CountryCode: budget}, keys)
					outcomes <- outcome{result: result, err: err}
				}()
			}
			arrived.Wait()
			close(proceed)
			admitted := 0
			for range requestCount {
				outcome := <-outcomes
				if outcome.err != nil {
					if !errors.Is(outcome.err, wantErr) {
						t.Fatalf("%s: %v", budget, outcome.err)
					}
				} else if len(outcome.result.KeyHexes) != 0 {
					admitted++
				}
			}
			if admitted != 10 {
				t.Fatalf("%s admitted %d; want 10", budget, admitted)
			}
		}
	})
}

// The exact read/decision gap retains PostgreSQL locks, and a waiting replica
// reads a new statement snapshot after it acquires those locks.
func TestExtenderReleaseTransactionOwnsDatabaseLocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ledger := NewNetworkExtenderReleaseLedger(ctx, map[string]server.Id{"synthetic-key": server.NewId()})
		ledger.Transact([]byte("synthetic-identity"), "192.0.2.0/24", "us", []string{"synthetic-key"}, func(view connect.ExtenderReleaseLedgerTx) {
			view.RequestCounts([]byte("synthetic-identity"), "192.0.2.0/24", time.Unix(1700000000, 0))
			view.ClientCount("synthetic-key", "us", []byte("synthetic-identity"), time.Unix(1700000000, 0))
			tx := view.(*networkExtenderReleaseTx).tx
			var isolation string
			result, err := tx.Query(ctx, "SHOW transaction_isolation")
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&isolation))
				}
			})
			if isolation != "read committed" {
				t.Fatalf("waiting admission reads stale %s snapshot", isolation)
			}
			lockIds := []int64{}
			result, err = tx.Query(ctx, "SELECT ((classid::bigint << 32) | objid::bigint) FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'advisory'")
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var lockId int64
					server.Raise(result.Scan(&lockId))
					lockIds = append(lockIds, lockId)
				}
			})
			if len(lockIds) != 3 {
				t.Fatalf("admission owns %d locks; want identity, vantage and country/key", len(lockIds))
			}
			server.Db(ctx, func(conn server.PgConn) {
				for _, lockId := range lockIds {
					available := true
					result, err := conn.Query(ctx, "SELECT pg_try_advisory_xact_lock($1)", lockId)
					server.WithPgResult(result, err, func() {
						if result.Next() {
							server.Raise(result.Scan(&available))
						}
					})
					if available {
						t.Error("admission lock was released between count and reservation")
					}
				}
			})
		})
	})
}

// A different replica and eligibility filter still see the original disclosure.
func TestExtenderReleaseReplicaRetainsEpochDisclosures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := time.Unix(1700000000, 0)
		settings := connect.DefaultExtenderReleaseSettings()
		settings.Now = func() time.Time { return now }
		keyIds := map[string]server.Id{}
		keys := []string{}
		for i := range 2 {
			key := []byte(fmt.Sprintf("synthetic-disclosure-key-%d", i))
			sign, _ := testRecordSigner()
			activation := ActivateNetworkExtender(ctx, testExtenderActivation(key, 4, fmt.Sprintf("192.0.2.%d", i+1)), sign)
			keyHex := hex.EncodeToString(key)
			keys = append(keys, keyHex)
			keyIds[keyHex] = activation.Extender.ExtenderId
		}
		probeTime := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "UPDATE network_extender_address SET last_probe_success_time = $2 WHERE extender_id = $1", keyIds[keys[0]], probeTime))
		})
		for range connect.DefaultExtenderBlockedStateSettings().ReportThreshold {
			RecordNetworkExtenderBlockReport(ctx, keyIds[keys[0]], server.NewId(), "us", probeTime)
		}
		blocked := NewNetworkExtenderBlockedSource(ctx, keyIds, nil)
		flags := blocked.BlockedKeys(keys, " US ")
		if !flags[keys[0]] || flags[keys[1]] || len(blocked.BlockedKeys(keys, "ca")) != 0 {
			t.Fatalf("batched country evidence: %v", flags)
		}
		for _, key := range keys {
			if flags[key] != blocked.Blocked(key, "us") {
				t.Fatal("batch and scalar blocked evidence disagree")
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "UPDATE network_extender_address SET last_probe_success_time = $2 WHERE extender_id = $1", keyIds[keys[0]], probeTime.Add(-2*blocked.settings.ProbeWindow)))
		})
		if flags := blocked.BlockedKeys(keys, "us"); len(flags) != 0 {
			t.Fatalf("expired probe treated as a block: %v", flags)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "UPDATE network_extender_address SET last_probe_success_time = $2, active = false WHERE extender_id = $1", keyIds[keys[0]], probeTime))
		})
		if flags := blocked.BlockedKeys(keys, "us"); len(flags) != 0 {
			t.Fatalf("inactive address treated as a block: %v", flags)
		}
		request := &connect.ExtenderReleaseRequest{Identity: []byte("synthetic-epoch-identity"), Vantage: "192.0.2.0/24"}
		release := func(eligible string) []string {
			request.Eligible = func(key string) bool { return key == eligible }
			policy := connect.NewExtenderReleasePolicy([]byte("synthetic-secret"), NewNetworkExtenderReleaseLedger(ctx, keyIds), nil, settings)
			result, err := policy.Release(request, keys)
			if err != nil {
				t.Fatal(err)
			}
			return result.KeyHexes
		}
		if got := release(keys[0]); !slices.Equal(got, keys[:1]) {
			t.Fatalf("first release: %v", got)
		}
		if got := release(keys[1]); len(got) != 0 {
			t.Fatalf("filter change disclosed another key: %v", got)
		}
		if got := release(keys[0]); !slices.Equal(got, keys[:1]) {
			t.Fatalf("original disclosure unavailable: %v", got)
		}
		// Removing a fleet row leaves a disclosure tombstone. Its address
		// cannot be exchanged for a fresh secret within the same epoch.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "DELETE FROM network_extender WHERE extender_id = $1", keyIds[keys[0]]))
		})
		if got := release(keys[1]); len(got) != 0 {
			t.Fatalf("deleting an issued fleet row refunded its disclosure: %v", got)
		}
		now = now.Add(settings.EpochTimeout)
		if got := release(keys[1]); !slices.Equal(got, keys[1:]) {
			t.Fatalf("new epoch did not renew budget: %v", got)
		}
	})
}
