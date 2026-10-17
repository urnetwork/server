// Seed recovery retains its scoring epoch across interrupted publication and
// later hot-state loss, without replacing any authenticated response bytes.
package controller

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Drive the real seed/extend boundary with a synthetic epoch mirror. The crash
// hook ends the seed immediately after its durable original commits; otherwise
// removing only the window models a lost window with an intact trail.
func testVerifyOriginalWindowRecovery(t *testing.T, crashAfterCommit bool, replaySeed bool) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		priorSettings, priorKeys := verifySettingsInstance, verifyServerKeysInstance
		t.Cleanup(func() { SetVerifySettings(priorSettings); SetVerifyServerKeys(priorKeys) })
		settings := model.DefaultVerifySettings()
		settings.StepTimeout = 5 * time.Minute
		settings.CohortDeploymentKey = model.StDeploymentKey("101:synthetic-verify-window")
		SetVerifySettings(settings)
		publicKey := testVerifyInstallServerKey()
		model.SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &model.StEpochSummary{Epoch: 41}, time.Hour)
		ips := map[server.Id]string{}
		seedIp := "192.0.2.10"
		for index := 0; index < connect.VerifyMMin; index++ {
			ip := fmt.Sprintf("192.0.2.%d", 10+index)
			clientId := testVerifyProvider(ctx, netip.MustParseAddr(ip), settings)
			ips[clientId] = ip
		}
		clientId, vpk, key := testVerifyValidator(ctx)
		request := testVerifySeedArgs(t, clientId, vpk, key, connect.VerifyMMin)
		message, err := connect.BuildVerifySeedMessage(vpk, request.ClientNonce, byte(request.M))
		if err != nil {
			t.Fatal(err)
		}
		crash := errors.New("synthetic seed original committed before publication")
		if crashAfterCommit {
			verifyOriginalAfterCommit = func() { panic(crash) }
		}
		defer func() { verifyOriginalAfterCommit = nil }()
		recovered := server.HandleError(func() {
			_, err := Verify(request, testVerifySession(ctx, seedIp))
			server.Raise(err)
		})
		verifyOriginalAfterCommit = nil
		if crashAfterCommit {
			cause, ok := recovered.(error)
			if !ok || !errors.Is(cause, crash) {
				t.Fatalf("seed did not reach the original commit barrier: %v", recovered)
			}
		} else if recovered != nil {
			t.Fatalf("seed failed before the window-loss fixture: %v", recovered)
		}
		locator := model.VerifyOriginalRequest{Scope: verifyCurrentOriginalScope(), ClientId: clientId, Message: message, Signature: request.SeedSig}
		original := model.GetVerifyOriginalRequest(ctx, locator)
		if original == nil {
			t.Fatal("seed original was not retained")
		}
		body := verifyDecodeOriginal(original)
		trailId := body.Trail.TrailId
		if _, err := protocol.ValidateProviderAttemptReceipt(&protocol.ProviderAttemptReceipt{Body: original.Body, Signature: original.Signature}, publicKey); err != nil {
			t.Fatal("seed recovery metadata changed the existing SN receipt grammar", err)
		}
		envelope, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(envelope, &fields); err != nil || len(fields) != 2 || fields["body"] == nil || fields["signature"] == nil {
			t.Fatal("internal recovery metadata escaped into the public receipt envelope", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT head_window FROM verify_original_transition WHERE trail_id=$1 AND previous_depth=0`, trailId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("seed recovery metadata was not committed with the original")
				}
				var window *string
				server.Raise(rows.Scan(&window))
				if window == nil || *window != "e41" {
					t.Errorf("seed window was not committed before publication: %v", window)
				}
			})
		})
		if mutation := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE verify_original_transition SET head_window='e99' WHERE trail_id=$1 AND previous_depth=0`, trailId))
			})
		}); mutation == nil {
			t.Fatal("retained window could be detached from its immutable original")
		}
		// Model an older writer appending a valid extension without the new
		// optional column. The seed row must remain the recovery authority.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION verify_window_legacy_writer() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.previous_depth > 0 THEN NEW.head_window = NULL; END IF; RETURN NEW; END $$;
 CREATE TRIGGER verify_window_legacy_writer BEFORE INSERT ON verify_original_transition
 FOR EACH ROW EXECUTE FUNCTION verify_window_legacy_writer()`))
		})
		defer server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER verify_window_legacy_writer ON verify_original_transition; DROP FUNCTION verify_window_legacy_writer()`))
		})
		if hot := model.GetVerifyTrail(ctx, trailId); (hot == nil) != crashAfterCommit {
			t.Fatalf("seed hot projection did not match the injected boundary: %+v", hot)
		}
		response, err := verifyDecodeCachedResponse(body.ResponseJson)
		if err != nil {
			t.Fatal(err)
		}
		assign := response.(*connect.VerifyAssignResult)
		seedPin, laterPin := [32]byte{0x41}, [32]byte{0x42}
		windowKey := fmt.Sprintf("{vtr_%s}window", trailId)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, windowKey).Err())
			for providerId := range ips {
				server.Raise(r.Set(ctx, fmt.Sprintf("{vhead_%s}e41", providerId), hex.EncodeToString(seedPin[:]), time.Hour).Err())
				server.Raise(r.Set(ctx, fmt.Sprintf("{vhead_%s}e42", providerId), hex.EncodeToString(laterPin[:]), time.Hour).Err())
			}
		})
		model.SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &model.StEpochSummary{Epoch: 42}, time.Hour)
		if replaySeed {
			result, err := Verify(request, testVerifySession(ctx, seedIp))
			retry, ok := result.(*connect.VerifyAssignResult)
			if err != nil || !ok || retry.TrailId != assign.TrailId || !bytes.Equal(retry.AssignSig, assign.AssignSig) {
				t.Fatalf("seed replay changed the original assignment: %T, %v", result, err)
			}
			if window := model.GetVerifyTrailHeadWindow(ctx, trailId, settings); window != "e41" {
				t.Errorf("seed replay restored window %q, want its retained e41", window)
			}
		}
		result, err := Verify(testVerifyExtendArgs(t, clientId, vpk, key, assign), testVerifySession(ctx, ips[server.Id(assign.NextHop)]))
		if err != nil {
			t.Fatal(err)
		}
		trail := model.GetVerifyTrail(ctx, trailId)
		if trail == nil || len(trail.Hops) != 2 || trail.Hops[1].EgressIpHash != seedPin {
			t.Fatalf("recovered confirmation did not use its seed epoch's provider pin: %+v", trail)
		}
		retained := model.GetVerifyOriginalRequest(ctx, locator)
		if retained == nil || !bytes.Equal(retained.Body, original.Body) || !bytes.Equal(retained.Signature, original.Signature) {
			t.Fatal("recovery rewrote the authenticated seed original")
		}

		// Losing the later projection must recover the same window from its
		// latest original, even though the seed is no longer the latest row.
		next, ok := result.(*connect.VerifyAssignResult)
		if !ok {
			t.Fatalf("fixture needs another pending hop, got %T", result)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			for _, suffix := range []string{"h", "hops", "pending", "resp", "window"} {
				server.Raise(r.Del(ctx, fmt.Sprintf("{vtr_%s}%s", trailId, suffix)).Err())
			}
		})
		model.SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &model.StEpochSummary{Epoch: 43}, time.Hour)
		if _, err := Verify(testVerifyExtendArgs(t, clientId, vpk, key, next), testVerifySession(ctx, ips[server.Id(next.NextHop)])); err != nil {
			t.Fatal(err)
		}
		trail = model.GetVerifyTrail(ctx, trailId)
		if trail == nil || len(trail.Hops) != 3 || trail.Hops[2].EgressIpHash != seedPin {
			t.Fatalf("later original lost the seed window: %+v", trail)
		}
	})
}

// An exact seed retry repairs a commit that never reached Redis publication.
func TestVerifyOriginalSeedWindowCommitRecovery(t *testing.T) {
	testVerifyOriginalWindowRecovery(t, true, true)
}

// An exact seed retry repairs a missing window beside an intact seed trail.
func TestVerifyOriginalSeedWindowProjectionRepair(t *testing.T) {
	testVerifyOriginalWindowRecovery(t, false, true)
}

// An extension can recover a committed seed without a preceding seed retry.
func TestVerifyOriginalSeedWindowExtendRecovery(t *testing.T) {
	testVerifyOriginalWindowRecovery(t, true, false)
}

// Equal-depth recovery also repairs a missing window; depth alone is not a
// complete test of whether every projection was published.
func TestVerifyOriginalSeedWindowSameDepthRecovery(t *testing.T) {
	testVerifyOriginalWindowRecovery(t, false, false)
}
