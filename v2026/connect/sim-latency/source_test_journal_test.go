// Explicit reservation barriers and synthetic retained logs prove retries
// replay facts instead of resampling tests or guessing that an owner stopped.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sourceTestJournalFixture struct {
	Directory string
	LogPath   string
	Plan      sourceTestPlan
	Execution sourceTestExecution
	Settings  sourceTestJournalSettings
	Now       time.Time
}

func newSourceTestJournalFixture(t *testing.T) sourceTestJournalFixture {
	t.Helper()
	plan := sourceTestPlanFixture()
	parent := filepath.Join(t.TempDir(), plan.Owner)
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(parent, "synthetic-test-logs.json")
	logs := []byte(`{"synthetic_observed_tests":7}`)
	if err := writePrivateReviewFile(logPath, logs); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(logs)
	execution := sourceTestExecutionFixture(plan)
	execution.TestLogsSha256 = hex.EncodeToString(digest[:])
	settings := defaultSourceTestJournalSettings()
	now := time.Now().UTC()
	settings.Now = func() time.Time { return now }
	return sourceTestJournalFixture{Directory: filepath.Join(parent, "independent-test-journal"), LogPath: logPath, Plan: plan, Execution: execution, Settings: settings, Now: now}
}

func (self sourceTestJournalFixture) verify(execution *sourceTestExecution) error {
	return verifySourceTestLogBundle(self.LogPath, execution.TestLogsSha256, self.Settings.MaximumEvidenceBytes)
}

func TestSourceTestJournalReplaysCompletedObservationWithoutRerunning(t *testing.T) {
	fixture := newSourceTestJournalFixture(t)
	count := 0
	run := func(context.Context) (*sourceTestExecution, error) {
		count++
		result := fixture.Execution
		return &result, nil
	}
	first, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.Directory, "observation.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Completed facts remain replayable after the old execution budget; no new
	// work or broader deadline is granted by the replacement caller.
	fixture.Settings.Now = func() time.Time { return fixture.Now.Add(2 * time.Hour) }
	second, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(8*time.Hour), fixture.Settings, run, fixture.verify)
	after, statErr := os.Stat(path)
	if err != nil || statErr != nil || count != 1 || !reflect.DeepEqual(first, second) || !os.SameFile(before, after) || after.Mode().Perm() != 0o400 {
		t.Fatal("completed tests were rerun or their evidence replaced", count, err, statErr)
	}
}

func TestSourceTestJournalRetainsFailuresAndChangedContextBlocks(t *testing.T) {
	for _, observedFailure := range []bool{false, true} {
		fixture := newSourceTestJournalFixture(t)
		count := 0
		run := func(context.Context) (*sourceTestExecution, error) {
			count++
			if !observedFailure {
				return nil, errors.New("synthetic test infrastructure failed")
			}
			result := fixture.Execution
			result.ExitCode = 1
			return &result, nil
		}
		for range 2 {
			result, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify)
			if observedFailure {
				if err != nil || result == nil || result.ExitCode != 1 {
					t.Fatal("observed test failure was lost or relabeled", result, err)
				}
			} else if err == nil || result != nil {
				t.Fatal("failed infrastructure authorized a result", result, err)
			}
		}
		changed := fixture.Plan
		changed.SuiteSha256 = strings.Repeat("1", 64)
		if _, err := executeJournaledSourceTest(context.Background(), fixture.Directory, changed, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify); err == nil || count != 1 {
			t.Fatal("changed plan resampled an existing execution", count, err)
		}
	}
}

func TestSourceTestJournalReservationExcludesConcurrentExecution(t *testing.T) {
	fixture := newSourceTestJournalFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var count atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	run := func(ctx context.Context) (*sourceTestExecution, error) {
		if count.Add(1) != 1 {
			return nil, errors.New("duplicate synthetic execution")
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		result := fixture.Execution
		return &result, nil
	}
	go func() {
		_, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("owner did not enter the prescribed execution", err)
	}
	result, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify)
	if err == nil || result != nil || count.Load() != 1 {
		unblock()
		<-done
		t.Fatal("live reservation admitted another executor", result, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify); err != nil || count.Load() != 1 {
		t.Fatal("completed concurrent owner did not replay", err)
	}
}

func TestSourceTestJournalInterruptedReservationNeverRestarts(t *testing.T) {
	fixture := newSourceTestJournalFixture(t)
	if err := os.Mkdir(fixture.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	planBytes, err := json.Marshal(fixture.Plan)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(planBytes)
	content, err := json.Marshal(sourceTestReservation{Schema: 1, PlanSha256: hex.EncodeToString(digest[:]), StartedAt: fixture.Now, Deadline: fixture.Now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := publishSourceTestRecord(filepath.Join(fixture.Directory, "reservation.json"), content); err != nil {
		t.Fatal(err)
	}
	fixture.Settings.Now = func() time.Time { return fixture.Now.Add(2 * time.Hour) }
	if result, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(8*time.Hour), fixture.Settings, func(context.Context) (*sourceTestExecution, error) {
		t.Fatal("interrupted/unknown owner was restarted based on a file")
		return nil, nil
	}, fixture.verify); err == nil || result != nil {
		t.Fatal("unknown reservation produced authority", result, err)
	}
}

func TestSourceTestJournalMissingChangedOrRedirectedEvidenceCannotReplay(t *testing.T) {
	for _, mutation := range []string{"missing", "changed", "symlink"} {
		fixture := newSourceTestJournalFixture(t)
		count := 0
		run := func(context.Context) (*sourceTestExecution, error) {
			count++
			result := fixture.Execution
			return &result, nil
		}
		if _, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify); err != nil {
			t.Fatal(err)
		}
		if mutation == "changed" {
			if err := os.Chmod(fixture.LogPath, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.LogPath, []byte("synthetic corruption"), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(fixture.LogPath, 0o400); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Remove(fixture.LogPath); err != nil {
				t.Fatal(err)
			}
			if mutation == "symlink" {
				if err := os.Symlink(filepath.Join(fixture.Directory, "observation.json"), fixture.LogPath); err != nil {
					t.Fatal(err)
				}
			}
		}
		if result, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify); err == nil || result != nil || count != 1 {
			t.Fatal("lost/corrupt evidence reran tests or authorized replay", mutation, result, err)
		}
	}
}

func TestSourceTestJournalCorruptRecordsAndCanceledCallersCannotResample(t *testing.T) {
	for _, mutation := range []string{"reservation", "observation", "canceled"} {
		fixture := newSourceTestJournalFixture(t)
		count := 0
		run := func(context.Context) (*sourceTestExecution, error) {
			count++
			result := fixture.Execution
			return &result, nil
		}
		if _, err := executeJournaledSourceTest(context.Background(), fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if mutation == "canceled" {
			cancel()
		} else {
			path := filepath.Join(fixture.Directory, mutation+".json")
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("synthetic truncated record"), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
		}
		result, err := executeJournaledSourceTest(ctx, fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings, run, fixture.verify)
		cancel()
		if err == nil || result != nil || count != 1 {
			t.Fatal("corrupt journal or retired caller acquired authority", mutation, result, err)
		}
	}
}
