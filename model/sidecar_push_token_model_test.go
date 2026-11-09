//go:build sidecar_pending

package model

// sidecar_push_token's writers (sidecar_push_token_model.go; §18.7's step 0, SDK-M8-09) on Postgres with
// MG8 and the test stack's Redis, through the Lua: a change raises rev in its own statement, a new row's
// rev is its insert's millisecond, an equal POST commits nothing and applies the row as it stands, an
// empty POST tombstones, every removal path tombstones in its own transaction, a vendor's rejection
// tombstones only at the revision it read, an expired apply retries, and §19.10's raise lifts every row.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const testPushTokenValue = "push-token-canary-model-1"

// A new active client of a new network, with no row.
func newTestPushTokenClient(t testing.TB, ctx context.Context) (networkId server.Id, clientId server.Id) {
	t.Helper()
	networkId = server.NewId()
	clientId = server.NewId()
	Testing_CreateNetwork(ctx, networkId, testPushTokenNetworkName(networkId), server.NewId())
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	return networkId, clientId
}

// A network name no other network of the test takes.
func testPushTokenNetworkName(networkId server.Id) string {
	return "pushtoken" + strings.ReplaceAll(networkId.String(), "-", "")[:16]
}

// A network whose one client AuthNetworkClient minted, as newTestRemovalNetwork makes one, under a name
// of its own, so a test can make several.
func newTestPushTokenRemovalNetwork(t testing.TB, ctx context.Context) *testRemovalNetwork {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	networkName := testPushTokenNetworkName(networkId)
	Testing_CreateNetwork(ctx, networkId, networkName, userId)
	networkSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: networkName})
	authResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, networkSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return &testRemovalNetwork{
		networkId:      networkId,
		networkSession: networkSession,
		clientId:       *byJwt.ClientId,
		sidecarId:      server.NewId(),
	}
}

// The client's row, failing on an error.
func testPushTokenRow(t testing.TB, ctx context.Context, clientId server.Id) *SidecarPushTokenRow {
	t.Helper()
	row, err := GetPushToken(ctx, clientId)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// What the client's tok and tokrev hold on the test Redis, "" for absent.
func testPushTokenKeys(t testing.TB, ctx context.Context, clientId server.Id) (tok string, tokrev string) {
	t.Helper()
	redis := NewSidecarPushRedis(&SidecarPushRedisSettings{CallTimeout: 10 * time.Second})
	tokValue, found, err := redis.Get(ctx, pushservice.HintTokenKey(connect.Id(clientId)))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		tok = string(tokValue)
	}
	revValue, found, err := redis.Get(ctx, pushservice.WakeTokenRevisionKey(connect.Id(clientId)))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		tokrev = string(revValue)
	}
	return tok, tokrev
}

// Fails unless tok and tokrev project row: its token at its rev, or no tok for a tombstone.
func expectTestPushTokenProjected(t testing.TB, ctx context.Context, row *SidecarPushTokenRow) {
	t.Helper()
	tok, tokrev := testPushTokenKeys(t, ctx, row.ClientId)
	wantTok := ""
	if !row.Tombstone() {
		wantTok = string(pushservice.WakeTokenValue(row.Platform, row.Token))
	}
	if tok != wantTok || tokrev != strconv.FormatInt(row.Rev, 10) {
		t.Fatalf("tok %q tokrev %q, want %q at %d", tok, tokrev, wantTok, row.Rev)
	}
}

func setTestPushToken(t testing.TB, ctx context.Context, clientId server.Id, platform string, token string) *SetPushTokenResult {
	t.Helper()
	result, err := SetPushToken(ctx, clientId, platform, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A new row's rev is its insert's time in milliseconds, as update_time, and its slot its tag's.
func TestNewRowRevIsInsertMillis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, clientId := newTestPushTokenClient(t, ctx)
		before := time.Now()
		result := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformApns, testPushTokenValue)
		after := time.Now()
		if result.Outcome != PushTokenInserted || result.Applied != pushservice.WakeTokenApplied {
			t.Fatalf("%s %s", result.Outcome, result.Applied)
		}
		row := testPushTokenRow(t, ctx, clientId)
		if row.Rev != row.UpdateTime.UnixMilli() {
			t.Fatalf("rev %d, update_time %d ms", row.Rev, row.UpdateTime.UnixMilli())
		}
		// the primary's clock, here the same host's
		if row.Rev < before.Add(-5*time.Second).UnixMilli() || after.Add(5*time.Second).UnixMilli() < row.Rev {
			t.Fatalf("rev %d is not the insert's millisecond (%d to %d)", row.Rev, before.UnixMilli(), after.UnixMilli())
		}
		if row.Slot != server.RedisHashSlot(pushservice.HintTokenKey(connect.Id(clientId))) || row.Slot != PushTokenSlot(clientId) {
			t.Fatalf("slot %d", row.Slot)
		}
		if row.Platform != sidecar.PushTokenPlatformApns || row.Token != testPushTokenValue || row.RemovedTime != nil {
			t.Fatalf("row %s", row)
		}
		expectTestPushTokenProjected(t, ctx, row)
	})
}

// Each change raises rev by one in the statement that makes it: a change that rolls back raises nothing,
// and the projection follows the committed rows.
func TestRevisionRisesInTheStatement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, clientId := newTestPushTokenClient(t, ctx)
		first := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformApns, "first-token")
		second := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformFcm, "second-token")
		if second.Outcome != PushTokenChanged || second.Row.Rev != first.Row.Rev+1 {
			t.Fatalf("%s at %d after %d", second.Outcome, second.Row.Rev, first.Row.Rev)
		}
		expectTestPushTokenProjected(t, ctx, second.Row)

		// a change whose transaction rolls back leaves rev where it was
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the transaction did not fail")
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				changed := setPushTokenInTx(ctx, tx, clientId, sidecar.PushTokenPlatformApns, "rolled-back")
				if changed.Row.Rev != second.Row.Rev+1 {
					t.Errorf("inside the statement: %d", changed.Row.Rev)
				}
				panic(errors.New("roll back"))
			}, server.OptNoRetry())
		}()
		row := testPushTokenRow(t, ctx, clientId)
		if row.Rev != second.Row.Rev || row.Token != "second-token" {
			t.Fatalf("after a rollback: %s", row)
		}
		third := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformFcm, "third-token")
		if third.Row.Rev != second.Row.Rev+1 {
			t.Fatalf("rev %d after %d", third.Row.Rev, second.Row.Rev)
		}
		expectTestPushTokenProjected(t, ctx, third.Row)
	})
}

// A POST equal to the row commits nothing, and applies the row as it stands, which lands a lost apply.
func TestEqualPostCommitsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, clientId := newTestPushTokenClient(t, ctx)
		first := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformFcm, testPushTokenValue)

		// the first apply was lost: tok and tokrev never landed
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Del(ctx, pushservice.HintTokenKey(connect.Id(clientId)), pushservice.WakeTokenRevisionKey(connect.Id(clientId))).Err())
		})
		again := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformFcm, testPushTokenValue)
		if again.Outcome != PushTokenUnchanged || again.Applied != pushservice.WakeTokenApplied {
			t.Fatalf("%s %s", again.Outcome, again.Applied)
		}
		row := testPushTokenRow(t, ctx, clientId)
		if row.Rev != first.Row.Rev || !row.UpdateTime.Equal(first.Row.UpdateTime) {
			t.Fatalf("an equal POST wrote the row: %s, then %s", first.Row, row)
		}
		expectTestPushTokenProjected(t, ctx, row)

		// and once landed, an equal POST's apply is current
		third := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformFcm, testPushTokenValue)
		if third.Outcome != PushTokenUnchanged || third.Applied != pushservice.WakeTokenCurrent {
			t.Fatalf("%s %s", third.Outcome, third.Applied)
		}
	})
}

// An empty POST tombstones the row, raising rev and deleting tok; another is equal; and a client with no
// row gets a tombstone row, so a token a lost row projected is deleted too.
func TestEmptyPostTombstones(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, clientId := newTestPushTokenClient(t, ctx)
		first := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformApns, testPushTokenValue)
		deleted := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformApns, "")
		if deleted.Outcome != PushTokenChanged || deleted.Row.Rev != first.Row.Rev+1 || !deleted.Row.Tombstone() || deleted.Row.Platform != "" || deleted.Row.RemovedTime != nil {
			t.Fatalf("%s %s", deleted.Outcome, deleted.Row)
		}
		expectTestPushTokenProjected(t, ctx, deleted.Row)
		again := setTestPushToken(t, ctx, clientId, "", "")
		if again.Outcome != PushTokenUnchanged || again.Row.Rev != deleted.Row.Rev {
			t.Fatalf("%s %s", again.Outcome, again.Row)
		}

		_, otherClientId := newTestPushTokenClient(t, ctx)
		empty := setTestPushToken(t, ctx, otherClientId, "", "")
		if empty.Outcome != PushTokenInserted || !empty.Row.Tombstone() || empty.Row.Rev != empty.Row.UpdateTime.UnixMilli() {
			t.Fatalf("%s %s", empty.Outcome, empty.Row)
		}
		expectTestPushTokenProjected(t, ctx, empty.Row)

		for _, c := range []struct {
			platform string
			token    string
		}{
			{platform: "gcm", token: ""},
			{platform: "", token: testPushTokenValue},
			{platform: sidecar.PushTokenPlatformApns, token: "a b"},
		} {
			if _, err := SetPushToken(ctx, clientId, c.platform, c.token, nil); !errors.Is(err, ErrPushTokenInvalid) {
				t.Fatalf("%q %q: %v", c.platform, c.token, err)
			}
		}
	})
}

// Every path that removes a client tombstones its row in that transaction, with removed_time, and the
// tombstone lands in Redis after the commit: RemoveNetworkClient's deactivation (and so every caller of
// deactivateLockedNetworkClientsInTx: a user's removal, the descendants, a session's end, a proxy trim and
// a prober shard's drain), the mark of abandoned top-level clients, and the child reap, which deletes
// active children without the chain. A removal that rolls back tombstones nothing, one applied twice has
// one result, and a removed client's POST is refused.
//
// The other deletions remove only clients already inactive, whose rows their deactivation tombstoned: the
// inactive reap (active = false), the hosted proxy reap and the retired hosted reap (NOT active), and the
// prober shard reap, whose clients its drain deactivated.
func TestRemovalTombstonesInTheTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		expectRemoved := func(clientId server.Id, before *SidecarPushTokenRow) *SidecarPushTokenRow {
			t.Helper()
			row := testPushTokenRow(t, ctx, clientId)
			if row == nil || !row.Tombstone() || row.RemovedTime == nil || row.Rev != before.Rev+1 {
				t.Fatalf("after its removal: %v", row)
			}
			if !row.UpdateTime.Equal(*row.RemovedTime) {
				t.Fatalf("update_time %v, removed_time %v", row.UpdateTime, *row.RemovedTime)
			}
			expectTestPushTokenProjected(t, ctx, row)
			return row
		}

		// RemoveNetworkClient
		network := newTestPushTokenRemovalNetwork(t, ctx)
		before := setTestPushToken(t, ctx, network.clientId, sidecar.PushTokenPlatformApns, testPushTokenValue).Row
		removeTestClient(t, network)
		removed := expectRemoved(network.clientId, before)
		if _, err := SetPushToken(ctx, network.clientId, sidecar.PushTokenPlatformApns, testPushTokenValue, nil); !errors.Is(err, ErrPushTokenClientRemoved) {
			t.Fatalf("a removed client's POST: %v", err)
		}
		// §19.10 applies the removal again: nothing changes
		server.Tx(ctx, func(tx server.PgTx) {
			removal := NewClientRemoval(network.networkId, []server.Id{network.clientId}, sidecar.RevocationReasonRemoved, server.NowUtc())
			server.Raise(ApplyRemovalEffectsInTx(ctx, tx, removal))
		})
		if row := testPushTokenRow(t, ctx, network.clientId); row.Rev != removed.Rev || !row.RemovedTime.Equal(*removed.RemovedTime) {
			t.Fatalf("applied twice: %s then %s", removed, row)
		}

		// a removal that rolls back
		rolledBack := newTestPushTokenRemovalNetwork(t, ctx)
		kept := setTestPushToken(t, ctx, rolledBack.clientId, sidecar.PushTokenPlatformFcm, testPushTokenValue).Row
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the transaction did not fail")
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				removal := NewClientRemoval(rolledBack.networkId, []server.Id{rolledBack.clientId}, sidecar.RevocationReasonRemoved, server.NowUtc())
				server.Raise(ApplyRemovalEffectsInTx(ctx, tx, removal))
				panic(errors.New("the removal's transaction fails after its effects"))
			}, server.OptNoRetry())
		}()
		if row := testPushTokenRow(t, ctx, rolledBack.clientId); row.Rev != kept.Rev || row.Tombstone() {
			t.Fatalf("a rolled back removal changed the row: %s", row)
		}
		expectTestPushTokenProjected(t, ctx, kept)

		// the abandoned top-level mark, and the child reap of an active child
		networkId, topLevelClientId := newTestPushTokenClient(t, ctx)
		childClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), childClientId, "", "")
		freshClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), freshClientId, "", "")
		topLevelBefore := setTestPushToken(t, ctx, topLevelClientId, sidecar.PushTokenPlatformApns, testPushTokenValue).Row
		childBefore := setTestPushToken(t, ctx, childClientId, sidecar.PushTokenPlatformFcm, testPushTokenValue).Row
		now := server.NowUtc()
		idleAuthTime := now.Add(-TopLevelClientIdleExpiration - 24*time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, childClientId, freshClientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time = $2 WHERE client_id = ANY($1)`, []server.Id{topLevelClientId, childClientId}, idleAuthTime))
		})
		RemoveDisconnectedNetworkClients(ctx, now.Add(-8*time.Hour), now.Add(-NetworkClientReapAfterDeactivate), now.Add(-TopLevelClientIdleExpiration))
		expectRemoved(topLevelClientId, topLevelBefore)
		expectRemoved(childClientId, childBefore)
		var childExists bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM network_client WHERE client_id = $1)`, childClientId).Scan(&childExists))
		})
		if childExists {
			t.Fatal("the child reap kept the child")
		}
		if _, err := SetPushToken(ctx, childClientId, sidecar.PushTokenPlatformFcm, testPushTokenValue, nil); !errors.Is(err, ErrPushTokenClientRemoved) {
			t.Fatalf("a reaped client's POST: %v", err)
		}
	})
}

// A vendor's rejection tombstones the row only at the revision the sender read with the token, so a newer
// registration survives it, and applies the tombstone.
func TestTombstoneOnlyAtTheReadRevision(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, clientId := newTestPushTokenClient(t, ctx)
		rejected := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformApns, "rejected-token").Row
		newer := setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformApns, "newer-token").Row
		// the sender read the rejected token at its rev; the newer one survives
		tombstoned, err := TombstonePushTokenIfRevision(ctx, clientId, rejected.Rev, nil)
		if err != nil || tombstoned {
			t.Fatalf("%t %v", tombstoned, err)
		}
		if row := testPushTokenRow(t, ctx, clientId); row.Rev != newer.Rev || row.Token != "newer-token" {
			t.Fatalf("the newer registration changed: %s", row)
		}
		// the newer one rejected at its own rev goes
		tombstoned, err = TombstonePushTokenIfRevision(ctx, clientId, newer.Rev, nil)
		if err != nil || !tombstoned {
			t.Fatalf("%t %v", tombstoned, err)
		}
		row := testPushTokenRow(t, ctx, clientId)
		if !row.Tombstone() || row.Rev != newer.Rev+1 || row.RemovedTime != nil {
			t.Fatalf("after the rejection: %s", row)
		}
		expectTestPushTokenProjected(t, ctx, row)
		// a second rejection of the same read changes nothing
		if tombstoned, err := TombstonePushTokenIfRevision(ctx, clientId, newer.Rev, nil); err != nil || tombstoned {
			t.Fatalf("%t %v", tombstoned, err)
		}
	})
}

// An apply past its window is expired and writes nothing; its retry rereads t0 and the row and lands. With
// no retry the row stands, unprojected, for the sweep.
func TestExpiredApplyRereadsAndRetries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultSidecarPushTokenSettings()
		_, clientId := newTestPushTokenClient(t, ctx)
		var row *SetPushTokenResult
		server.Tx(ctx, func(tx server.PgTx) {
			row = setPushTokenInTx(ctx, tx, clientId, sidecar.PushTokenPlatformFcm, testPushTokenValue)
		})
		t0, err := readPushTokenTime(ctx, clientId, settings)
		if err != nil {
			t.Fatal(err)
		}
		staleT0 := t0 - settings.ApplyWindow.Milliseconds() - 1000

		noRetry := *settings
		noRetry.ApplyRetryCount = 0
		if applied := applyPushTokenWithRetries(ctx, row.Row, &staleT0, &noRetry); applied != pushservice.WakeTokenExpired {
			t.Fatalf("no retry: %s", applied)
		}
		if tok, tokrev := testPushTokenKeys(t, ctx, clientId); tok != "" || tokrev != "" {
			t.Fatalf("an expired apply wrote %q %q", tok, tokrev)
		}
		if applied := applyPushTokenWithRetries(ctx, row.Row, &staleT0, settings); applied != pushservice.WakeTokenApplied {
			t.Fatalf("with retries: %s", applied)
		}
		expectTestPushTokenProjected(t, ctx, row.Row)
	})
}

// §19.10's raise lifts every row by 2^32 with update_time now, in batches, and each row's next apply lands
// above whatever the lost primary applied.
func TestRaisePushTokenRevisions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		before := map[server.Id]*SidecarPushTokenRow{}
		for i := range 3 {
			_, clientId := newTestPushTokenClient(t, ctx)
			token := ""
			if i != 1 {
				token = testPushTokenValue
			}
			before[clientId] = setTestPushToken(t, ctx, clientId, sidecar.PushTokenPlatformFcm, token).Row
		}
		raiseTime := server.NowUtc().Add(-time.Second)
		settings := DefaultSidecarPushTokenSettings()
		settings.RaiseBatchCount = 2
		raised, err := RaisePushTokenRevisions(ctx, settings)
		if err != nil || raised != 3 {
			t.Fatalf("raised %d: %v", raised, err)
		}
		for clientId, row := range before {
			after := testPushTokenRow(t, ctx, clientId)
			if after.Rev != row.Rev+PushTokenRestoreRevisionRaise || after.UpdateTime.Before(raiseTime) || after.Token != row.Token {
				t.Fatalf("%s raised to %s", row, after)
			}
			if _, applied := ApplyPushToken(ctx, clientId, nil); applied != pushservice.WakeTokenApplied {
				t.Fatalf("%s: %s", after, applied)
			}
			expectTestPushTokenProjected(t, ctx, after)
		}
	})
}

// Two first POSTs at once insert one row: one inserts, the other changes or equals it, and rev is the
// insert's plus at most one.
func TestConcurrentFirstPostsInsertOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, clientId := newTestPushTokenClient(t, ctx)
		results := make(chan *SetPushTokenResult, 2)
		for _, token := range []string{"concurrent-a", "concurrent-b"} {
			go func() {
				var result *SetPushTokenResult
				defer func() {
					results <- result
				}()
				server.HandleError(func() {
					var err error
					result, err = SetPushToken(ctx, clientId, sidecar.PushTokenPlatformFcm, token, nil)
					if err != nil {
						t.Error(err)
					}
				})
			}()
		}
		outcomes := map[PushTokenOutcome]int{}
		var insertedRev int64
		for range 2 {
			result := <-results
			if result == nil {
				t.FailNow()
			}
			outcomes[result.Outcome] += 1
			if result.Outcome == PushTokenInserted {
				insertedRev = result.Row.Rev
			}
		}
		if outcomes[PushTokenInserted] != 1 || outcomes[PushTokenChanged] != 1 {
			t.Fatalf("outcomes %v", outcomes)
		}
		row := testPushTokenRow(t, ctx, clientId)
		if row.Rev != insertedRev+1 {
			t.Fatalf("rev %d after an insert at %d", row.Rev, insertedRev)
		}
		expectTestPushTokenProjected(t, ctx, row)
	})
}
