package competition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestPostgresStoreQueueCacheFailoverAndImmutability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := validSettings()
		store := PostgresStore{}
		apiImageDigest := testApiImageDigest()
		workerAImageDigest := testWorkerImageDigest()
		workerBImageDigest := "sha256:" + strings.Repeat("9", 64)
		now := server.NowUtc()
		round, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(500 * time.Millisecond), RevealAt: now.Add(500 * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("CreateRound: %s", err)
		}
		if round.Status != "open" || round.WorkloadCommitment == "" || len(round.SeedCiphertext) <= 32 {
			t.Fatalf("unexpected round: %#v", round)
		}
		if _, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt: now, ClosesAt: now.Add(30 * time.Minute), RevealAt: now.Add(time.Hour),
		}); !errors.Is(err, ErrPreviousEpochOpen) {
			t.Fatalf("overlapping round error = %v", err)
		}

		patch1, patchErr := ValidateAndCanonicalizePatch(testPatch("first"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job1, hit, err := store.Enqueue(ctx, settings, round.RoundId, patch1, "miner-a", apiImageDigest)
		if err != nil || hit {
			t.Fatalf("first enqueue = hit %v, err %v", hit, err)
		}
		cached, hit, err := store.Enqueue(ctx, settings, round.RoundId, patch1, "miner-b", apiImageDigest)
		if err != nil || !hit || cached.JobId != job1.JobId {
			t.Fatalf("cached enqueue = %#v, hit %v, err %v", cached, hit, err)
		}
		if _, err := store.GetJob(ctx, settings, job1.JobId, &Principal{Id: "miner-b", Role: "submitter"}); err != nil {
			t.Fatalf("cache-hit principal cannot poll: %s", err)
		}
		if _, err := store.GetJob(ctx, settings, job1.JobId, &Principal{Id: "miner-c", Role: "submitter"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unlisted principal poll error = %v", err)
		}

		patch2, patchErr := ValidateAndCanonicalizePatch(testPatch("second"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job2, _, err := store.Enqueue(ctx, settings, round.RoundId, patch2, "miner-a", apiImageDigest)
		if err != nil {
			t.Fatalf("second enqueue: %s", err)
		}
		patch3, patchErr := ValidateAndCanonicalizePatch(testPatch("third"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job3, _, err := store.Enqueue(ctx, settings, round.RoundId, patch3, "miner-a", apiImageDigest)
		if err != nil {
			t.Fatalf("third enqueue: %s", err)
		}
		if early, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest); err != nil || early != nil {
			t.Fatalf("batch job was claimable before closes_at: %#v, %v", early, err)
		}
		time.Sleep(600 * time.Millisecond)
		claimed1, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest)
		if err != nil || claimed1 == nil || claimed1.JobId != job1.JobId || claimed1.AttemptCount != 1 {
			t.Fatalf("first claim = %#v, %v", claimed1, err)
		}
		if blocked, err := store.Claim(ctx, settings, "worker-b", workerBImageDigest); err != nil || blocked != nil {
			t.Fatalf("singleton slot allowed concurrent claim: %#v, %v", blocked, err)
		}
		if blocked, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest); err != nil || blocked != nil {
			t.Fatalf("duplicate worker id bypassed singleton slot: %#v, %v", blocked, err)
		}
		if err := store.Heartbeat(ctx, settings, "worker-a", claimed1.JobId); err != nil {
			t.Fatalf("heartbeat: %s", err)
		}
		raw, normalized := 100.0, 100.0
		_, err = store.Complete(ctx, settings, "worker-a", claimed1.JobId, EvaluationOutcome{
			Score:            &ScoreResult{ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, TakeoverEligible: true, Gates: map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}}},
			ArtifactManifest: []byte(`{"schema":1,"test":true}`),
		})
		if err != nil {
			t.Fatalf("complete first: %s", err)
		}
		claimed2, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest)
		if err != nil || claimed2 == nil || claimed2.JobId != job2.JobId {
			t.Fatalf("second claim = %#v, %v", claimed2, err)
		}
		// Simulate an abrupt host loss. A second box may take the expired job,
		// but it keeps the same job/cache identity and increments only attempt.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_job SET lease_expires_at = $1 WHERE job_id = $2
			`, now.Add(-time.Minute), job2.JobId))
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET lease_expires_at = $1 WHERE slot_id = 1
			`, now.Add(-time.Minute)))
		})
		failedOver, err := store.Claim(ctx, settings, "worker-b", workerBImageDigest)
		if err != nil || failedOver == nil || failedOver.JobId != job2.JobId || failedOver.AttemptCount != 2 {
			t.Fatalf("failover claim = %#v, %v", failedOver, err)
		}
		_, err = store.Complete(ctx, settings, "worker-b", failedOver.JobId, EvaluationOutcome{
			Error:            &CompetitionError{Kind: "submission", Code: "build_failed", Message: "candidate did not build", Retriable: false},
			ArtifactManifest: []byte(`{"schema":1,"test":true}`),
		})
		if err != nil {
			t.Fatalf("complete failed submission: %s", err)
		}

		claimed3, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest)
		if err != nil || claimed3 == nil || claimed3.JobId != job3.JobId {
			t.Fatalf("third claim = %#v, %v", claimed3, err)
		}
		retry, err := store.Complete(ctx, settings, "worker-a", claimed3.JobId, EvaluationOutcome{
			Error:            infrastructureError("host_transient", "transient host fault"),
			ArtifactManifest: []byte(`{"schema":1,"attempt":1}`),
			Infrastructure:   true,
		})
		if err != nil || !retry {
			t.Fatalf("infrastructure retry = %v, %v", retry, err)
		}
		var retryError []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT eval_error_json FROM competition_job WHERE job_id = $1`, job3.JobId).Scan(&retryError))
		})
		if len(retryError) != 0 {
			t.Fatalf("transient retry error was stored in mutable terminal field: %s", retryError)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE competition_job SET available_at = $1 WHERE job_id = $2`, now.Add(-time.Minute), job3.JobId))
		}, server.OptReadWrite())
		retried3, err := store.Claim(ctx, settings, "worker-b", workerBImageDigest)
		if err != nil || retried3 == nil || retried3.JobId != job3.JobId || retried3.AttemptCount != 2 {
			t.Fatalf("third retry claim = %#v, %v", retried3, err)
		}
		_, err = store.Complete(ctx, settings, "worker-b", retried3.JobId, EvaluationOutcome{
			Error:            &CompetitionError{Kind: "submission", Code: "build_failed", Message: "candidate did not build", Retriable: false},
			ArtifactManifest: []byte(`{"schema":1,"attempt":2}`),
		})
		if err != nil {
			t.Fatalf("complete retried submission: %s", err)
		}
		finalizedRound, finalized, err := store.FinalizeEligibleRound(ctx, settings)
		if err != nil || !finalized || finalizedRound == nil ||
			finalizedRound.WinnerJobId == nil || *finalizedRound.WinnerJobId != job1.JobId {
			t.Fatalf("round finalization = %#v, %v, %v", finalizedRound, finalized, err)
		}
		if _, finalized, err := store.FinalizeEligibleRound(ctx, settings); err != nil || finalized {
			t.Fatalf("finalized round was published twice: %v, %v", finalized, err)
		}
		leaderboards, err := store.Leaderboards(ctx, settings)
		if err != nil || len(leaderboards.Epochs) != 1 ||
			len(leaderboards.Epochs[0].Entries) != 1 ||
			!leaderboards.Epochs[0].Entries[0].Winner {
			t.Fatalf("leaderboard = %#v, %v", leaderboards, err)
		}
		nextRound, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt: now.Add(time.Hour), ClosesAt: now.Add(2 * time.Hour), RevealAt: now.Add(2 * time.Hour),
		})
		if err != nil || nextRound.Epoch != 2 {
			t.Fatalf("next epoch = %#v, %v", nextRound, err)
		}

		checkA := passingHostCheck(settings)
		checkA.RebaselineRoundId = &nextRound.RoundId
		if err := store.RegisterHost(ctx, settings, checkA); err != nil {
			t.Fatalf("register host: %s", err)
		}
		if err := refreshOperationalMetrics(ctx, settings); err != nil {
			t.Fatalf("refresh competition metrics: %s", err)
		}
		checks, err := store.Readiness(ctx, settings)
		if err != nil || !allChecks(checks) {
			t.Fatalf("readiness = %#v, %v", checks, err)
		}

		var immutableErr error
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job SET patch_bytes = 'tampered'::bytea WHERE job_id = $1`, job1.JobId)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "immutable") {
			t.Fatalf("patch immutability update error = %v", immutableErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job SET api_image_digest = $2 WHERE job_id = $1`, job1.JobId, workerBImageDigest)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "immutable") {
			t.Fatalf("API image identity immutability update error = %v", immutableErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job SET eval_error_json = '{"tampered":true}'::jsonb WHERE job_id = $1`, job2.JobId)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "immutable") {
			t.Fatalf("terminal result immutability update error = %v", immutableErr)
		}
		var events int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_event`).Scan(&events))
		})
		if events < 8 {
			t.Fatalf("only %d durable queue events recorded", events)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job_event SET actor_id = 'tampered' WHERE event_id = (SELECT min(event_id) FROM competition_job_event)`)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "append-only") {
			t.Fatalf("event append-only update error = %v", immutableErr)
		}
	})
}

func testPatch(value string) string {
	return "diff --git a/connect/example.go b/connect/example.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/connect/example.go\n" +
		"+++ b/connect/example.go\n" +
		"@@ -1 +1 @@\n-old\n+" + value + "\n"
}
