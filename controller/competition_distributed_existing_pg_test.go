// Disposable PostgreSQL regressions keep the real published job/slot/source
// guards enabled while testing witness-held exact attachment. Never run live.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The store-only legacy fixture has no real workload bytes. Genuine intake
// retains them in an exact synthetic version, which this fixture hydrates before
// the first claim without changing the already-published round or archive API.
func newCompetitionExistingPgFixture(t *testing.T, tb testing.TB) *competitionDistributedPgFixture {
	tb.Helper()
	fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
	if err := hydrateCompetitionExistingPgWorkload(tb.Context(), fixture.settings, fixture.round); err != nil {
		tb.Fatal("genuine existing-run fixture could not hydrate its retained workload", err)
	}
	return fixture
}

// The runtime-envelope store deliberately omits mutable retained objects. Only
// one exact retained workload version may supply its immutable local file.
func hydrateCompetitionExistingPgWorkload(ctx context.Context, settings *Settings, round *roundRecord) error {
	if settings == nil || round == nil || !sha256Pattern.MatchString(round.ProvidersSha256) ||
		round.ProvidersPath != filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String(), "providers.yml") {
		return errors.New("existing fixture workload has a noncanonical identity")
	}
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil {
		return errors.New("existing fixture lacks its byte-backed retained archive")
	}
	store, ok := archive.store.(*competitionRuntimeBlobTestStore)
	if !ok || store == nil {
		return errors.New("existing fixture lacks its synthetic retained-version store")
	}
	key := archive.roundWorkloadKey(settings, round, round.ProvidersSha256)
	prefix, version, matches := key+"\x00", "", 0
	func() {
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		for retainedKey := range store.versions {
			if strings.HasPrefix(retainedKey, prefix) {
				version = strings.TrimPrefix(retainedKey, prefix)
				matches++
			}
		}
	}()
	if matches != 1 || version == "" {
		return errors.New("existing fixture retained workload version is missing or ambiguous")
	}
	reader, err := store.GetVersion(ctx, key, version)
	if err != nil {
		return err
	}
	content, readErr := io.ReadAll(io.LimitReader(reader, maxProvidersFileSize+1))
	closeErr := reader.Close()
	defer clear(content)
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if len(content) == 0 || maxProvidersFileSize < int64(len(content)) || hex.EncodeToString(digest[:]) != round.ProvidersSha256 {
		return errors.New("existing fixture retained workload bytes differ from the published round")
	}
	if err := os.MkdirAll(filepath.Dir(round.ProvidersPath), 0700); err != nil {
		return err
	}
	return writeExclusiveFile(round.ProvidersPath, content, 0400)
}

// A missing current object reproduces the original fixture failure. Exact
// retained bytes repair it; absent, wrong-key, ambiguous and corrupt versions
// cannot create the local input or rewrite its frozen round identity.
func TestCompetitionExistingPgFixtureHydratesOnlyExactRetainedWorkload(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*roundRecord, *competitionRuntimeBlobTestStore, string)
		want   string
	}{
		{name: "exact"},
		{name: "missing", mutate: func(_ *roundRecord, store *competitionRuntimeBlobTestStore, key string) {
			delete(store.versions, key+"\x00synthetic-workload-version")
		}, want: "version is missing or ambiguous"},
		{name: "wrong-key", mutate: func(_ *roundRecord, store *competitionRuntimeBlobTestStore, key string) {
			store.versions[key+"-wrong\x00synthetic-workload-version"] = store.versions[key+"\x00synthetic-workload-version"]
			delete(store.versions, key+"\x00synthetic-workload-version")
		}, want: "version is missing or ambiguous"},
		{name: "ambiguous", mutate: func(_ *roundRecord, store *competitionRuntimeBlobTestStore, key string) {
			store.versions[key+"\x00synthetic-second-version"] = bytes.Clone(store.versions[key+"\x00synthetic-workload-version"])
		}, want: "version is missing or ambiguous"},
		{name: "corrupt", mutate: func(_ *roundRecord, store *competitionRuntimeBlobTestStore, _ string) {
			store.corruptReadback = true
		}, want: "bytes differ from the published round"},
		{name: "wrong-path", mutate: func(round *roundRecord, _ *competitionRuntimeBlobTestStore, _ string) {
			round.ProvidersPath += ".wrong"
		}, want: "noncanonical identity"},
	} {
		settings := automationApiSettings()
		settings.ArtifactRoot = t.TempDir()
		content := []byte("synthetic-existing-retained-workload\n")
		digest := sha256.Sum256(content)
		round := &roundRecord{RoundResult: RoundResult{RoundId: server.NewId(), ProvidersSha256: hex.EncodeToString(digest[:])}}
		round.ProvidersPath = filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String(), "providers.yml")
		store := &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}
		archive := &blobArtifactArchive{store: store}
		settings.artifactArchive = archive
		key := archive.roundWorkloadKey(settings, round, round.ProvidersSha256)
		store.versions[key+"\x00synthetic-workload-version"] = bytes.Clone(content)
		if _, err := readRoundWorkload(t.Context(), settings, round); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("root-cause control did not reproduce the absent current workload", test.name, err)
		}
		if test.mutate != nil {
			test.mutate(round, store, key)
		}
		beforeDigest, beforePath := round.ProvidersSha256, round.ProvidersPath
		err := hydrateCompetitionExistingPgWorkload(t.Context(), settings, round)
		if test.want != "" {
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatal("invalid retained workload passed the exact fixture boundary", test.name, err)
			}
			if _, err := os.Lstat(round.ProvidersPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected workload created a local input", test.name, err)
			}
		} else {
			if err != nil {
				t.Fatal("exact retained workload could not hydrate", err)
			}
			actual, err := readRoundWorkload(t.Context(), settings, round)
			if err != nil || !bytes.Equal(actual, content) {
				t.Fatal("real manifest reader lost the retained workload identity", err)
			}
			clear(actual)
			info, err := os.Lstat(round.ProvidersPath)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
				t.Fatal("hydrated workload is not an immutable regular fixture input", err)
			}
			if err := hydrateCompetitionExistingPgWorkload(t.Context(), settings, round); !errors.Is(err, os.ErrExist) {
				t.Fatal("fixture hydration overwrote an already-existing local input", err)
			}
		}
		if round.ProvidersSha256 != beforeDigest || round.ProvidersPath != beforePath {
			t.Fatal("hydration rewrote frozen round metadata", test.name)
		}
	}
}

// Native status alone is replaced; manifest, frozen baseline, typed witness,
// current supervisor checks and the exact SQL claim transaction remain real.
func competitionExistingPgWitness(t *testing.T, tb testing.TB, fixture *competitionDistributedPgFixture, settings *Settings) (*competitionExistingRunWitness, *queuedJob) {
	t.Helper()
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	connection := competitionExecutorConnection{HostId: host.HostId, AgentConfigSha256: strings.Repeat("d", 64), ExistingControl: &competitionExistingControlConnection{ApprovalSha256: strings.Repeat("e", 64), ResourceBoundarySha256: strings.Repeat("f", 64)}}
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings(), Hosts: []competitionExecutorConnection{connection}}}
	job, err := fixture.store.existingCompetitionJob(tb.Context(), settings)
	if err != nil || job == nil {
		tb.Fatal("real store did not snapshot exact occupied custody", err)
	}
	manifest, err := competitionExistingManifest(tb.Context(), settings, job, host)
	if err != nil {
		tb.Fatal(err)
	}
	transport := &competitionExistingTestTransport{t: tb, manifest: manifest, connection: connection, firstFence: job.SlotFencingToken, keySeed: 1}
	evaluator := &competitionAgentEvaluator{host: host, transport: transport, now: fixture.clock.Now}
	witness, err := evaluator.observeExisting(tb.Context(), settings, job)
	if err != nil {
		tb.Fatal("real immutable manifest did not mint a fresh synthetic native observation", err)
	}
	tb.Cleanup(func() { clear(witness.manifest.EvaluatorRequest) })
	return witness, job
}

// The same hold which refuses every fresh FIFO claim does not strand a proven
// original native attempt. All independent static host/baseline guards remain.
func TestCompetitionExistingPgClaimHoldAllowsOnlyExactExistingWitness(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionExistingPgFixture(t, tb)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-original-worker", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(2 * time.Minute)
		witness, snapshot := competitionExistingPgWitness(t, tb, fixture, settings)
		hold := model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-existing-claim-hold", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic existing-run regression.", EvidenceSha256: strings.Repeat("a", 64)}
		if _, err := fixture.store.SetAutomationHold(tb.Context(), settings, "synthetic-operator", hold); err != nil {
			tb.Fatal(err)
		}
		var gradingReady bool
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT competition_v2_intake_grading_ready($1)`, first.RoundId).Scan(&gradingReady))
		})
		if gradingReady {
			tb.Fatal("real intake grading gate did not refuse the claim-only hold")
		}
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(1), "synthetic-refused-fresh", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrAutomationHeld) {
			tb.Fatal("claim-only hold admitted fresh work", err)
		}
		selected := *settings
		selected.executorReconcileOnly, selected.executorExistingWitness = true, witness
		attached, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-existing-successor", testWorkerImageDigest())
		if err != nil || attached == nil || attached.JobId != first.JobId || attached.AttemptCount != first.AttemptCount || attached.SlotFencingToken != first.SlotFencingToken+1 ||
			!attached.StartedAt.Equal(*snapshot.StartedAt) || !attached.ExecutionDeadline.Equal(*snapshot.ExecutionDeadline) || attached.WorkerImageDigest != snapshot.WorkerImageDigest {
			tb.Fatal("held typed attachment changed execution or failed its real claim guards", err)
		}
		if _, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-replayed-witness", testWorkerImageDigest()); !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("single-use native witness was replayed", err)
		}
		if err := fixture.store.Heartbeat(withCompetitionJobAuthority(tb.Context(), first), settings, "synthetic-original-worker", first.JobId); !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("obsolete fence renewed attached custody", err)
		}
	})
}

// A valid observation is not a lease: a committed competing reconciliation
// between observation and claim makes the old witness fail the exact SQL CAS.
func TestCompetitionExistingPgStaleCasNeverStealsNewCustody(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionExistingPgFixture(t, tb)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-first-worker", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(2 * time.Minute)
		witness, _ := competitionExistingPgWitness(t, tb, fixture, settings)
		newer, err := fixture.store.Claim(tb.Context(), settings, "synthetic-committed-successor", testWorkerImageDigest())
		if err != nil || newer == nil {
			tb.Fatal(err)
		}
		selected := *settings
		selected.executorReconcileOnly, selected.executorExistingWitness = true, witness
		job, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-stale-observer", testWorkerImageDigest())
		if err != nil && !errors.Is(err, ErrLeaseLost) || job != nil {
			tb.Fatal("stale observation stole current custody", err)
		}
		stored, err := fixture.store.GetJob(tb.Context(), settings, first.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
		if err != nil || stored.SlotFencingToken != newer.SlotFencingToken || stored.LeaseOwner != newer.LeaseOwner || stored.AttemptCount != first.AttemptCount || !stored.ExecutionDeadline.Equal(*first.ExecutionDeadline) {
			tb.Fatal("failed CAS changed existing execution", err)
		}
	})
}

// An expired observation or manufactured empty capability cannot waive hold
// or any exact original-run check, even when all normal static gates pass.
func TestCompetitionExistingPgExpiredAndArbitraryWitnessFailClosed(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionExistingPgFixture(t, tb)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-first-worker", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(2 * time.Minute)
		witness, _ := competitionExistingPgWitness(t, tb, fixture, settings)
		fixture.clock.Advance(settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge)
		for _, proof := range []*competitionExistingRunWitness{witness, {}} {
			selected := *settings
			selected.executorReconcileOnly, selected.executorExistingWitness = true, proof
			if job, err := fixture.store.Claim(context.Background(), &selected, "synthetic-refused-observer", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrLeaseLost) {
				tb.Fatal("expired/arbitrary native witness gained a lease", err)
			}
		}
	})
}
