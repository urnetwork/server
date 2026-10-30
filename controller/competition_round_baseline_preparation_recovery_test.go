// Recovery tests use only synthetic owned files and isolated native PostgreSQL.
// They are not a measured-control, production-authorization or live DB receipt.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Creates the actual six-leaf format while inputs remain opaque synthetic bytes.
func newRoundBaselineRecoveryFilesTest(t testing.TB) (*Settings, roundBaselineRecoveryAuthorization, string) {
	t.Helper()
	settings, preparation, _, _ := newRoundBaselinePreparationTest(t)
	settings.ArtifactRoot = t.TempDir()
	directory, err := createRoundBaselinePreparationDirectory(settings.ArtifactRoot, preparation.RoundId.String(), preparation.Owner)
	if err != nil {
		t.Fatal(err)
	}
	authorization := roundBaselineRecoveryAuthorization{Schema: 1, Kind: "sim-latency-round-baseline-zero-sample-recovery",
		RoundId: preparation.RoundId, JobId: preparation.JobId, Owner: preparation.Owner,
		PreviousContextSha256: preparation.ContextSha256}
	hold, err := json.Marshal(map[string]any{"schema": 1, "kind": "sim-latency-round-baseline-preparation-infrastructure-hold",
		"owner": preparation.Owner, "context_sha256": preparation.ContextSha256, "job_id": preparation.JobId, "round_id": preparation.RoundId,
		"submission_started": false, "candidate_executed": false})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"worker-request.json": []byte("opaque synthetic workload input\n"), "providers.yml": []byte("opaque synthetic provider bytes\n"),
		"canonical.patch": []byte("opaque synthetic patch bytes\n"), "round-baseline-preparation.hold.json": hold,
		"baseline-preparation.stderr.log":          {},
		"baseline-preparation.stderr-summary.json": []byte(`{"schema":1,"kind":"sim-latency-round-baseline-preparation-log","limit_bytes":4096,"observed_bytes":0,"retained_bytes":0,"truncated":false}`),
	}
	for name, content := range files {
		if err := writeExclusiveFile(filepath.Join(directory, name), content, 0400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		switch name {
		case "worker-request.json":
			authorization.RequestSha256 = hex.EncodeToString(digest[:])
		case "round-baseline-preparation.hold.json":
			authorization.HoldSha256 = hex.EncodeToString(digest[:])
		case "baseline-preparation.stderr-summary.json":
			authorization.SummarySha256 = hex.EncodeToString(digest[:])
		}
	}
	return settings, authorization, directory
}

// Metadata edits are confined to a fresh synthetic leaf and its new digest.
func replaceRoundBaselineRecoveryTestFile(t testing.TB, path string, content []byte) string {
	t.Helper()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Inputs are hashed without JSON/YAML parsing and no measured artifacts exist.
func TestCompetitionRoundBaselineRecoveryAuthenticatesExactZeroSampleFiles(t *testing.T) {
	settings, authorization, _ := newRoundBaselineRecoveryFilesTest(t)
	patch, providers, err := authenticateRoundBaselineRecoveryFiles(settings, authorization)
	patchDigest, providerDigest := sha256.Sum256([]byte("opaque synthetic patch bytes\n")), sha256.Sum256([]byte("opaque synthetic provider bytes\n"))
	if err != nil || patch != hex.EncodeToString(patchDigest[:]) || providers != hex.EncodeToString(providerDigest[:]) {
		t.Fatal("exact zero-sample census failed", err)
	}
}

// Even one partial control, native completion or missing old leaf refuses resume.
func TestCompetitionRoundBaselineRecoveryRejectsMeasuredOrMissingFiles(t *testing.T) {
	for _, name := range []string{"baseline.json", "evidence", "round-baseline-preparation.complete.json"} {
		settings, authorization, directory := newRoundBaselineRecoveryFilesTest(t)
		if err := writeExclusiveFile(filepath.Join(directory, name), []byte("synthetic partial output"), 0400); err != nil {
			t.Fatal(err)
		}
		if _, _, err := authenticateRoundBaselineRecoveryFiles(settings, authorization); err == nil {
			t.Fatal("measured/unknown output admitted", name)
		}
	}
	for _, name := range []string{"worker-request.json", "providers.yml", "canonical.patch", "round-baseline-preparation.hold.json", "baseline-preparation.stderr.log", "baseline-preparation.stderr-summary.json"} {
		settings, authorization, directory := newRoundBaselineRecoveryFilesTest(t)
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := authenticateRoundBaselineRecoveryFiles(settings, authorization); err == nil {
			t.Fatal("missing input admitted", name)
		}
	}
}

// Authenticated hashes alone cannot turn absent/false zero-sample facts green.
func TestCompetitionRoundBaselineRecoveryRejectsIncompleteZeroMetadata(t *testing.T) {
	for _, mutation := range []string{"submission_started", "candidate_executed", "context_sha256"} {
		settings, authorization, directory := newRoundBaselineRecoveryFilesTest(t)
		path := filepath.Join(directory, "round-baseline-preparation.hold.json")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if json.Unmarshal(content, &document) != nil {
			t.Fatal("synthetic metadata")
		}
		delete(document, mutation)
		content, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		authorization.HoldSha256 = replaceRoundBaselineRecoveryTestFile(t, path, content)
		if _, _, err := authenticateRoundBaselineRecoveryFiles(settings, authorization); err == nil {
			t.Fatal("missing metadata admitted", mutation)
		}
	}
	settings, authorization, directory := newRoundBaselineRecoveryFilesTest(t)
	replaceRoundBaselineRecoveryTestFile(t, filepath.Join(directory, "baseline-preparation.stderr.log"), []byte("synthetic diagnostic"))
	if _, _, err := authenticateRoundBaselineRecoveryFiles(settings, authorization); err == nil {
		t.Fatal("nonempty stderr admitted")
	}
}

// The private runtime fixture represents an already authenticated operator pin.
// Actual Root file loading is separate; no live identities or authorization used.
func newRoundBaselineRecoveryStoreTest(t testing.TB, suffix string) (*Settings, PostgresStore, *roundBaselinePreparation, *time.Time) {
	t.Helper()
	ctx := context.Background()
	settings := validSettings()
	settings.CompetitionId += suffix
	settings.EvaluationPolicy.Replicates = 9
	settings.ArtifactRoot = t.TempDir()
	settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
	now := server.NowUtc().Truncate(time.Second)
	settings.SeasonEndsAt = now.Add(60 * 24 * time.Hour)
	settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
	store := PostgresStore{now: func() time.Time { return now }}
	round, err := store.CreateStagingRound(ctx, settings, GenerateRoundArgs{OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), RevealAt: now.Add(time.Hour)}, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, patchErr := ValidateAndCanonicalizePatch(testPatch("synthetic-zero-recovery"), settings.PatchPolicy)
	if patchErr != nil {
		t.Fatal(patchErr)
	}
	if _, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "synthetic-source", testApiImageDigest()); err != nil {
		t.Fatal(err)
	}
	reservation, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-original-worker", testWorkerImageDigest())
	if err != nil || reservation == nil {
		t.Fatal("original reservation", err)
	}
	if err := store.HoldRoundBaselinePreparation(ctx, settings, reservation, roundBaselinePreparationHold{Stage: "producer_or_archive"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	settings.roundBaselinePreparation.Recovery = &roundBaselineRecoveryCapability{
		Authorization: roundBaselineRecoveryAuthorization{Owner: reservation.Owner, RoundId: reservation.RoundId, JobId: reservation.JobId,
			PreviousContextSha256: reservation.ContextSha256, PreviousWorkerImage: reservation.WorkerImage,
			ReplacementWorkerImage: "sha256:" + strings.Repeat("9", 64), ApprovedAt: now,
			Approval: roundBaselinePreparationPin{Sha256: strings.Repeat("a", 64)}, Evidence: roundBaselinePreparationPin{Sha256: strings.Repeat("b", 64)}},
		AuthorizationSha256: strings.Repeat("c", 64), PatchSha256: reservation.PatchSha256, ProvidersSha256: reservation.SourceJob.Round.ProvidersSha256}
	return settings, store, reservation, &now
}

// Native BIGSERIAL events, exact old/new contexts and FK publication all execute.
func TestCompetitionRoundBaselineRecoverySameOwnerKeepsClockAndPublishes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings, store, original, _ := newRoundBaselineRecoveryStoreTest(t, "-recovery-publish")
		recovered, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-new-worker", settings.roundBaselinePreparation.Recovery.Authorization.ReplacementWorkerImage)
		if err != nil || recovered == nil || recovered.Recovery == nil || recovered.Recovery.EventId <= 0 || recovered.Recovery.PreviousHoldId <= 0 {
			t.Fatal("recovery append/read", err)
		}
		if recovered.Owner != original.Owner || !recovered.StartedAt.Equal(original.StartedAt) || !recovered.Deadline.Equal(original.Deadline) ||
			recovered.ContextSha256 == original.ContextSha256 || recovered.WorkerImage == original.WorkerImage ||
			recovered.SourceJob.StartedAt != nil || recovered.SourceJob.AttemptCount != 0 {
			t.Fatal("recovery reset time, started work, or hid control change")
		}
		if err := store.ReleaseRoundBaselinePreparation(ctx, original); err != nil {
			t.Fatal(err)
		}
		if err := store.CheckRoundBaselinePreparation(ctx, settings, recovered); err != nil {
			t.Fatal("obsolete cleanup erased resumed slot", err)
		}
		if err := store.HoldRoundBaselinePreparation(ctx, settings, original, roundBaselinePreparationHold{Stage: "producer_or_archive"}); err == nil {
			t.Fatal("obsolete hold poisoned recovery")
		}
		obsoleteToken := *recovered
		obsoleteToken.Recovery = nil
		if err := store.HoldRoundBaselinePreparation(ctx, settings, &obsoleteToken, roundBaselinePreparationHold{Stage: "producer_or_archive"}); err == nil {
			t.Fatal("current context with obsolete token poisoned recovery")
		}
		prepared := preparedRoundBaselineTest(t, settings, recovered)
		oldPrefix := "/baseline-preparation/" + recovered.Owner + "/"
		newPrefix := "/baseline-preparation/" + roundBaselinePreparationArtifactOwner(recovered) + "/"
		prepared.ArtifactManifest = []byte(strings.ReplaceAll(string(prepared.ArtifactManifest), oldPrefix, newPrefix))
		digest := sha256.Sum256(prepared.ArtifactManifest)
		prepared.ArtifactManifestSha256 = hex.EncodeToString(digest[:])
		job, err := store.ClaimPreparedRoundBaseline(ctx, settings, recovered, prepared)
		if err != nil || job == nil || job.JobId != original.JobId || job.StartedAt == nil || job.AttemptCount != 1 {
			t.Fatal("native real claim/publication", err)
		}
		if !recovered.Deadline.Equal(original.Deadline) {
			t.Fatal("original control deadline changed")
		}
	})
}

// A second failure or expired original clock never creates another chance.
func TestCompetitionRoundBaselineRecoverySecondFailureAndExpiryStayHeld(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings, store, original, now := newRoundBaselineRecoveryStoreTest(t, "-recovery-second")
		recovered, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-new-worker", settings.roundBaselinePreparation.Recovery.Authorization.ReplacementWorkerImage)
		if err != nil || recovered == nil {
			t.Fatal(err)
		}
		if err := store.HoldRoundBaselinePreparation(ctx, settings, recovered, roundBaselinePreparationHold{Stage: "producer_or_archive"}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReleaseRoundBaselinePreparation(ctx, recovered); err != nil {
			t.Fatal(err)
		}
		if again, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-third-worker", recovered.WorkerImage); !errors.Is(err, errRoundBaselinePreparationHeld) || again != nil {
			t.Fatal("second recovery admitted", err)
		}
		*now = original.Deadline
		if again, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-late-worker", recovered.WorkerImage); !errors.Is(err, errRoundBaselinePreparationHeld) || again != nil {
			t.Fatal("expired recovery received fresh clock", err)
		}
		var reservations, recoveries, holds, attempts int
		var started *time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER(WHERE event_type='round_baseline_preparation_reserved'),
 count(*) FILTER(WHERE event_type='round_baseline_preparation_zero_sample_recovered'),
 count(*) FILTER(WHERE event_type='round_baseline_preparation_held') FROM competition_job_event WHERE job_id=$1`, original.JobId).Scan(&reservations, &recoveries, &holds))
			server.Raise(conn.QueryRow(ctx, `SELECT attempt_count,started_at FROM competition_job WHERE job_id=$1`, original.JobId).Scan(&attempts, &started))
		})
		if reservations != 1 || recoveries != 1 || holds != 2 || attempts != 0 || started != nil {
			t.Fatal("append-only/no-start census changed")
		}
		expiredSettings, expiredStore, expiredOriginal, expiredNow := newRoundBaselineRecoveryStoreTest(t, "-recovery-original-expired")
		*expiredNow = expiredOriginal.Deadline
		if again, err := expiredStore.ReserveRoundBaseline(ctx, expiredSettings, "synthetic-expired-worker", expiredSettings.roundBaselinePreparation.Recovery.Authorization.ReplacementWorkerImage); !errors.Is(err, errRoundBaselinePreparationHeld) || again != nil {
			t.Fatal("first recovery reset an expired original deadline", err)
		}
	})
}

// An ambiguous committed publication is permanently held, not a lost lease.
// Absent, unrelated or already-ineligible capabilities cannot change that error.
func TestCompetitionRoundBaselineRecoveryPreservesCommittedHoldClassification(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, capability := range []string{"absent", "unrelated", "started"} {
			settings := validSettings()
			settings.CompetitionId += "-recovery-committed-hold-" + capability
			settings.EvaluationPolicy.Replicates = 9
			settings.ArtifactRoot = t.TempDir()
			settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
			now := server.NowUtc().Truncate(time.Second)
			settings.SeasonEndsAt = now.Add(60 * 24 * time.Hour)
			settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
			store := PostgresStore{now: func() time.Time { return now }}
			round, err := store.CreateStagingRound(ctx, settings, GenerateRoundArgs{OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), RevealAt: now.Add(time.Hour)}, false)
			if err != nil {
				t.Fatal(err)
			}
			patch, patchErr := ValidateAndCanonicalizePatch(testPatch("synthetic-committed-hold"), settings.PatchPolicy)
			if patchErr != nil {
				t.Fatal(patchErr)
			}
			if _, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "synthetic-source", testApiImageDigest()); err != nil {
				t.Fatal(err)
			}
			original, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-running-worker", testWorkerImageDigest())
			if err != nil || original == nil {
				t.Fatal("original reservation", err)
			}
			prepared := preparedRoundBaselineTest(t, settings, original)
			job, err := store.ClaimPreparedRoundBaseline(ctx, settings, original, prepared)
			if err != nil || job == nil || job.StartedAt == nil || job.AttemptCount != 1 {
				t.Fatal("real committed publication", err)
			}
			started := *job.StartedAt
			if err := store.HoldRoundBaselinePreparation(ctx, settings, original, roundBaselinePreparationHold{Stage: "synthetic_ambiguous_commit"}); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Duration(settings.WorkerLeaseSeconds+1) * time.Second)
			image := "sha256:" + strings.Repeat("9", 64)
			if capability != "absent" {
				settings.roundBaselinePreparation.Recovery = &roundBaselineRecoveryCapability{
					Authorization: roundBaselineRecoveryAuthorization{Owner: original.Owner, RoundId: original.RoundId, JobId: original.JobId,
						PreviousContextSha256: original.ContextSha256, PreviousWorkerImage: original.WorkerImage,
						ReplacementWorkerImage: image, ApprovedAt: now},
					PatchSha256: original.PatchSha256, ProvidersSha256: original.SourceJob.Round.ProvidersSha256}
				if capability == "unrelated" {
					settings.roundBaselinePreparation.Recovery.Authorization.Owner = "baseline.synthetic-unrelated"
				}
			}
			if again, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-replacement-worker", image); !errors.Is(err, errRoundBaselinePreparationHeld) || again != nil {
				t.Fatalf("%s capability reclassified committed hold: preparation=%v err=%v", capability, again, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var owner string
				var slotJob server.Id
				server.Raise(conn.QueryRow(ctx, `SELECT worker_id,job_id FROM competition_worker_slot WHERE slot_id=1`).Scan(&owner, &slotJob))
				if owner != original.WorkerId || slotJob != original.JobId {
					t.Fatal("committed candidate slot changed")
				}
				var attempts, recoveries int
				var actualStarted time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT attempt_count,started_at FROM competition_job WHERE job_id=$1`, original.JobId).Scan(&attempts, &actualStarted))
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_zero_sample_recovered'`, original.JobId).Scan(&recoveries))
				if attempts != 1 || !actualStarted.Equal(started) || recoveries != 0 {
					t.Fatal("committed hold changed original clock, attempts or recovery history")
				}
			})
			// Assertions above retain the genuine candidate's ownership. Normal
			// test cleanup frees the global singleton before the next case.
			if err := store.HandBack(ctx, original.WorkerId, original.JobId, "synthetic-test-cleanup"); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A positive native lock-wait observation forces the READ COMMITTED race:
// old cleanup starts while recovery holds the slot, then sees its committed token.
func TestCompetitionRoundBaselineRecoveryWaitingCleanupCannotClearSlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		settings, store, original, now := newRoundBaselineRecoveryStoreTest(t, "-recovery-release-race")
		childPid := make(chan int32, 1)
		childDone := make(chan error, 1)
		childStarted, childJoined := false, false
		// This runs after an unwinding parent transaction has released its lock.
		defer func() {
			cancel()
			if childStarted && !childJoined {
				joinCtx, joinCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer joinCancel()
				select {
				case <-childDone:
				case <-joinCtx.Done():
					t.Error("cleanup child did not join after parent rollback")
				}
			}
		}()
		var recovered *roundBaselinePreparation
		server.Tx(ctx, func(tx server.PgTx) {
			store.roundBaselinePreparationFenceInTx(ctx, tx, settings)
			var slotId, parentPid int32
			server.Raise(tx.QueryRow(ctx, `SELECT slot_id,pg_backend_pid() FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&slotId, &parentPid))
			childStarted = true
			go func() {
				childDone <- captureDatabaseError(func() {
					server.Tx(ctx, func(child server.PgTx) {
						var pid int32
						server.Raise(child.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
						childPid <- pid
						server.Raise(releaseRoundBaselinePreparationInTx(ctx, child, original, *now))
					}, server.OptReadWrite(), server.TxReadCommitted)
				})
			}()
			var pid int32
			select {
			case pid = <-childPid:
			case <-ctx.Done():
				panic(ctx.Err())
			}
			for {
				var blocked bool
				server.Raise(tx.QueryRow(ctx, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, parentPid, pid).Scan(&blocked))
				if blocked {
					break
				}
				if ctx.Err() != nil {
					panic(ctx.Err())
				}
				runtime.Gosched()
			}
			var err error
			recovered, err = recoverRoundBaselinePreparationInTx(ctx, tx, settings, original, original.SourceJob, "synthetic-new-worker", settings.roundBaselinePreparation.Recovery.Authorization.ReplacementWorkerImage, *now)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=$1,job_id=NULL,lease_expires_at=$2 WHERE slot_id=1`, recovered.Owner, recovered.Deadline.Add(settings.roundBaselinePreparation.Settings.CleanupTimeout)))
		}, server.OptReadWrite())
		select {
		case err := <-childDone:
			childJoined = true
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("old cleanup did not join")
		}
		if recovered == nil {
			t.Fatal("recovery absent")
		}
		if err := store.CheckRoundBaselinePreparation(ctx, settings, recovered); err != nil {
			t.Fatal("waiting old snapshot cleared resumed lease", err)
		}
	})
}
