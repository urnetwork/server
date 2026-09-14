package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backupArchiveFixture struct {
	archive             string
	generation          string
	createdAt           *time.Time
	progress            *float64
	heartbeat           *time.Time
	sampleAt            time.Time
	integrityFormat     string
	integrityResult     string
	integrityGeneration string
	integrityCheckedAt  *time.Time
	integritySampleAt   time.Time
	omitIntegrity       bool
}

type backupArchiveWriterFixture struct {
	unitState                     string
	unitSubstate                  string
	mainPID                       int64
	result                        string
	exitStatus                    int64
	invocationID                  string
	execStart                     int64
	execStartEpoch                int64
	timerState                    string
	timerUnitFileState            string
	timerNext                     *int64
	timerLast                     int64
	githubGitTransferAttempts     int64
	githubGitTransferRetrySeconds int64
	githubFailureStatus           string
	githubFirstFailureBoundary    string
	githubFailureJournalLines     int64
	githubFailureBoundaryLines    map[string]int64
	archiveRootObservation        string
	githubArchivePathState        string
	remoteArchivePathState        string
	archivePathsMatch             *bool
	archiveMountsMatch            *bool
	archivePathsOnMount           *bool
	archivePathPermissionsSecure  *bool
	remoteUnitState               string
	remoteUnitSubstate            string
	remoteMainPID                 int64
	remoteResult                  string
	remoteRestart                 string
	remoteRestartDelay            string
	remoteExitStatus              int64
	remoteInvocationID            string
	remoteExecStart               int64
	remoteExecStartEpoch          int64
	remoteTimerState              string
	remoteTimerUnitFileState      string
	remoteTimerNext               int64
	remoteTimerLast               int64
	remoteBoot                    int64
	remotePGSource                string
	remotePGPort                  int64
	remoteRedisSource             string
	remoteRedisPort               int64
	remoteMount                   string
	remoteMountPresent            *bool
	remoteMountSource             string
	remoteMountFSType             string
	remoteMountOptions            string
	remoteMountLineage            string
	clearanceState                string
	storageReadable               *bool
	storageEvents                 []backupArchiveStorageEventFixture
}

type backupArchiveStorageEventFixture struct {
	epoch  int64
	kind   string
	device string
}

func TestBackupArchivesSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for index, archive := range backupArchiveNames {
		createdAt := now.Add(-time.Duration(index+1) * 12 * time.Hour)
		progress := float64(0)
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &progress,
		})
	}
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	if len(alerts) != 0 {
		t.Fatalf("healthy backup archives alerted: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticIntegrityClassifications(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		modify     func(*backupArchiveFixture)
		class      string
		severity   Severity
		sustain    int
		frame      string
		want       []string
		notClasses []string
	}{
		{
			name: "invalid",
			modify: func(fixture *backupArchiveFixture) {
				fixture.integrityResult = "invalid"
			},
			class:    "backup-archive-integrity-invalid",
			severity: SeverityPage,
			sustain:  1,
			want: []string{
				"result=invalid",
				"neither a fresh scrape nor a young artifact timestamp overrides it",
				"not a failed decryption",
				"separately authorized restore drill",
			},
			notClasses: []string{"backup-archive-integrity-legacy-unverified"},
		},
		{
			name: "missing",
			modify: func(fixture *backupArchiveFixture) {
				fixture.createdAt = nil
				fixture.generation = ""
				fixture.integrityResult = "missing"
			},
			class:    "backup-archive-integrity-missing",
			severity: SeverityPage,
			sustain:  2,
			want: []string{
				"result=missing",
				"distinct from losing the integrity metric",
				"does not prove why media is absent",
			},
			notClasses: []string{"backup-archive-integrity-unobservable"},
		},
		{
			name: "unobservable",
			modify: func(fixture *backupArchiveFixture) {
				fixture.omitIntegrity = true
			},
			class:    "backup-archive-integrity-unobservable",
			severity: SeverityWarn,
			sustain:  2,
			want: []string{
				"fresh_samples=0",
				"UNKNOWN integrity is neither verified nor invalid media",
				"explicit bounded revalidation mode",
			},
			notClasses: []string{"backup-archive-integrity-stale"},
		},
		{
			name: "stale check",
			modify: func(fixture *backupArchiveFixture) {
				checkedAt := now.Add(-49 * time.Hour)
				fixture.integrityCheckedAt = &checkedAt
			},
			class:    "backup-archive-integrity-stale",
			severity: SeverityWarn,
			sustain:  2,
			frame:    "check-time",
			want: []string{
				"check_age=2 days 1h0m0s",
				"metrics refreshes deliberately preserve the last check time",
				"Scrape time, integrity check time, and structural artifact time",
			},
			notClasses: []string{"backup-archive-integrity-generation-mismatch"},
		},
		{
			name: "generation mismatch",
			modify: func(fixture *backupArchiveFixture) {
				fixture.integrityGeneration = "synthetic-other-generation"
			},
			class:    "backup-archive-integrity-generation-mismatch",
			severity: SeverityPage,
			sustain:  2,
			want: []string{
				"integrity_generation=synthetic-other-generation",
				"latest_generation=pg-complete",
				"No integrity result counts as current health across a generation mismatch",
			},
			notClasses: []string{"backup-archive-integrity-invalid"},
		},
		{
			name: "legacy unverified",
			modify: func(fixture *backupArchiveFixture) {
				fixture.integrityResult = "legacy-unverified"
			},
			class:    "backup-archive-integrity-legacy-unverified",
			severity: SeverityWarn,
			sustain:  1,
			want: []string{
				"format=pg-gpg-md5-legacy",
				"result=legacy-unverified",
				"does not mean corrupt",
				"would not be a decrypt or restore drill",
			},
			notClasses: []string{"backup-archive-integrity-invalid"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixtures := healthyBackupArchiveFixtures(now)
			test.modify(&fixtures[0])
			alerts := runBackupArchiveFixtures(t, now, fixtures...)
			alert := requireBackupArchiveAlert(t, alerts, test.class, "backup-1/pg")
			if alert.Severity != test.severity || alert.Sustain != test.sustain {
				t.Fatalf("integrity urgency=%s/%d, want %s/%d", alert.Severity, alert.Sustain, test.severity, test.sustain)
			}
			if test.frame != "" && alert.Frame != test.frame {
				t.Fatalf("integrity frame=%q, want %q", alert.Frame, test.frame)
			}
			for _, want := range test.want {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("integrity alert missing %q:\n%s", want, alert.Markdown())
				}
			}
			for _, class := range test.notClasses {
				if unexpected := findBackupArchiveAlert(alerts, class, "backup-1/pg"); unexpected != nil {
					t.Fatalf("integrity state also emitted %s: %+v", class, *unexpected)
				}
			}
		})
	}
}

func TestBackupArchivesSignalSyntheticIntegrityScrapeStalenessIsDistinct(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	fixtures[0].integritySampleAt = now.Add(-2 * time.Minute)
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	stale := requireBackupArchiveAlert(t, alerts, "backup-archive-integrity-stale", "backup-1/pg")
	if stale.Frame != "scrape-time" {
		t.Fatalf("stale integrity scrape frame=%q, want scrape-time", stale.Frame)
	}
	for _, want := range []string{
		"fresh_integrity_samples=0",
		"stale_integrity_scrape_samples=1",
	} {
		if !strings.Contains(stale.Markdown(), want) {
			t.Fatalf("stale integrity scrape alert missing %q:\n%s", want, stale.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(alerts, "backup-archive-integrity-unobservable", "backup-1/pg"); unexpected != nil {
		t.Fatalf("stale integrity scrape was reduced to generic unobservable: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticIntegrityOverlapSelectsNewestCheck(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	olderCheck := now.Add(-time.Minute)
	fixtures = append(fixtures, backupArchiveFixture{
		archive:             "pg",
		integrityResult:     "invalid",
		integrityGeneration: "synthetic-older-generation",
		integrityCheckedAt:  &olderCheck,
	})
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	for _, class := range []string{
		"backup-archive-integrity-invalid",
		"backup-archive-integrity-unobservable",
		"backup-archive-integrity-generation-mismatch",
	} {
		if unexpected := findBackupArchiveAlert(alerts, class, "backup-1/pg"); unexpected != nil {
			t.Fatalf("older overlap emitted %s: %+v", class, *unexpected)
		}
	}
}

func TestBackupArchivesSignalSyntheticIntegrityEqualCheckIsUnobservable(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	fixtures = append(fixtures, backupArchiveFixture{
		archive:             "pg",
		integrityResult:     "invalid",
		integrityGeneration: "synthetic-other-generation",
		integrityCheckedAt:  &now,
	})
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-integrity-unobservable", "backup-1/pg")
	if !strings.Contains(alert.Observed, "ambiguous_current_samples=2") {
		t.Fatalf("equal-check ambiguity was not bounded: %s", alert.Observed)
	}
	if unexpected := findBackupArchiveAlert(alerts, "backup-archive-integrity-invalid", "backup-1/pg"); unexpected != nil {
		t.Fatalf("ambiguous report was attributed as artifact invalid: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticIntegrityParserRedactsInvalidGeneration(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	const privateSyntheticLabel = "synthetic/private-label"
	fixtures[0].integrityGeneration = privateSyntheticLabel
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-integrity-unobservable", "backup-1/pg")
	if strings.Contains(alert.Markdown(), privateSyntheticLabel) {
		t.Fatalf("invalid integrity label leaked into alert:\n%s", alert.Markdown())
	}
	for _, want := range []string{
		"generation is outside the bounded archive integrity contract",
		"raw labels, paths, checksums, credentials, and archive contents are not emitted",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("redacted integrity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticIntegrityJoinRedactsInvalidLatestGeneration(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	const privateSyntheticLabel = "synthetic/private-latest"
	fixtures[0].generation = privateSyntheticLabel
	fixtures[0].integrityGeneration = "synthetic-safe-generation"
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-invalid", "backup-1/pg")
	if strings.Contains(alert.Markdown(), privateSyntheticLabel) {
		t.Fatalf("invalid latest label leaked into alert:\n%s", alert.Markdown())
	}
	if !strings.Contains(alert.Observed, "generation-outside-bounded-contract") {
		t.Fatalf("invalid latest label was not reduced: %s", alert.Observed)
	}
	if unexpected := findBackupArchiveAlert(alerts, "backup-archive-integrity-generation-mismatch", "backup-1/pg"); unexpected != nil {
		t.Fatalf("invalid latest label entered integrity join: %+v", *unexpected)
	}
}

// A valid previous tarball does not make the writer healthy. This reproduces
// the gap where the last archive remained inside five days while systemd had
// already recorded a failed oneshot.
func TestBackupArchivesSignalSyntheticDetectsFailedGitHubWriterBeforeFreshnessBreach(t *testing.T) {
	now := time.Date(2026, 9, 8, 17, 30, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-4 * 24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:                  "failed",
		unitSubstate:               "failed",
		result:                     "exit-code",
		exitStatus:                 1,
		invocationID:               "present",
		execStart:                  123456,
		githubFailureStatus:        "complete",
		githubFirstFailureBoundary: "storage-eio",
		githubFailureJournalLines:  11,
		githubFailureBoundaryLines: map[string]int64{
			"storage-eio":  10,
			"unclassified": 1,
		},
		clearanceState:    "valid",
		remoteMount:       "/synthetic/archive",
		remoteMountSource: "/dev/mapper/synthetic-archive",
	}, fixtures...)
	alert := requireBackupArchiveAlert(
		t,
		alerts,
		"backup-archive-writer-failed",
		"backup-1/github",
	)
	if alert.Sustain != 1 || alert.Severity != SeverityPage {
		t.Fatalf("GitHub writer failure urgency=%s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"unit_state=failed",
		"unit_substate=failed",
		"result=exit-code",
		"exit_status=1",
		"invocation_id_present=true",
		"failure_journal_status=complete",
		"first_failure_boundary=storage-eio",
		"failure_journal_lines=11",
		"storage_eio_lines=10",
		"unclassified_lines=1",
		"current_mount_state=read-write",
		"current_clearance_state=valid",
		"later, independent observations",
		"never leave the host",
		"still-young previous tarball",
		"single-writer boundary",
		"operator authorization",
		"SIGNALS.md §11.22",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("GitHub writer failure missing %q:\n%s", want, alert.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(
		alerts,
		"backup-archive-stale",
		"backup-1/github-urnetwork",
	); unexpected != nil {
		t.Fatalf("fresh archive was misclassified as stale: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticKeepsClippedGitHubFailureBoundaryAmbiguous(t *testing.T) {
	now := time.Date(2026, 9, 8, 17, 30, 30, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	alert := requireBackupArchiveAlert(t, runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:                  "failed",
		unitSubstate:               "failed",
		result:                     "exit-code",
		exitStatus:                 1,
		invocationID:               "present",
		execStart:                  654321,
		githubFailureStatus:        "ambiguous",
		githubFirstFailureBoundary: "unclassified",
		githubFailureJournalLines:  backupArchiveGitHubJournalMaxLines + 1,
		githubFailureBoundaryLines: map[string]int64{
			"unclassified": backupArchiveGitHubJournalMaxLines + 1,
		},
		remoteMount:       "/synthetic/archive",
		remoteMountSource: "/dev/mapper/synthetic-archive",
	}, fixtures...), "backup-archive-writer-failed", "backup-1/github")
	for _, want := range []string{
		"failure_journal_status=ambiguous",
		"first_failure_boundary=unclassified",
		"failure_journal_lines=513",
		"exceeded the 512-line classification bound",
		"cannot prove the invocation's first failed boundary",
		"do not infer a first boundary from the clipped tail",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("ambiguous GitHub writer alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

// Storage-repair tooling may deliberately stop timers. A successful previous
// service result cannot hide that no later calendar owner was restored.
func TestBackupArchivesSignalSyntheticDetectsUnscheduledGitHubTimer(t *testing.T) {
	now := time.Date(2026, 9, 8, 17, 31, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	missingNext := int64(0)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		timerState:         "inactive",
		timerUnitFileState: "enabled",
		timerNext:          &missingNext,
	}, fixtures...)
	alert := requireBackupArchiveAlert(
		t,
		alerts,
		"backup-archive-timer-unscheduled",
		"backup-1/github",
	)
	if alert.Sustain != 1 || alert.Severity != SeverityPage {
		t.Fatalf("GitHub timer urgency=%s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"timer_state=inactive",
		"timer_unit_file_state=enabled",
		"timer_next=missing",
		"no proven durable future code-backup trigger",
		"does not schedule itself",
		"offline archive-repair workflow",
		"explicit operator authorization",
		"persistent missed trigger may immediately create",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("GitHub timer alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(
		alerts,
		"backup-archive-writer-failed",
		"backup-1/github",
	); unexpected != nil {
		t.Fatalf("successful idle writer was misclassified as failed: %+v", *unexpected)
	}
}

// The data writer uses a separate persistent calendar owner. Existing valid
// PostgreSQL and Redis generations must not hide a disabled future schedule.
func TestBackupArchivesSignalSyntheticDetectsUnscheduledDataTimer(t *testing.T) {
	now := time.Date(2026, 9, 8, 17, 31, 30, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteTimerState:         "inactive",
		remoteTimerUnitFileState: "disabled",
	}, fixtures...)
	alert := requireBackupArchiveAlert(
		t,
		alerts,
		"backup-archive-timer-unscheduled",
		"backup-1/remote",
	)
	if alert.Sustain != 1 || alert.Severity != SeverityPage {
		t.Fatalf("data timer urgency=%s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"timer_state=inactive",
		"timer_unit_file_state=disabled",
		"future PostgreSQL/Redis backup trigger",
		"latest/weekly/monthly",
		"dedicated direct SSH endpoints",
		"explicit operator authorization",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("data timer alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(
		alerts,
		"backup-archive-timer-unscheduled",
		"backup-1/github",
	); unexpected != nil {
		t.Fatalf("healthy GitHub timer was misclassified as unscheduled: %+v", *unexpected)
	}
}

// A persistent timer can retain its elapsed NextElapse while the oneshot it
// triggered is still running. Exact LastTrigger/start ownership keeps that
// active invocation from becoming a false scheduling page.
func TestBackupArchivesSignalSyntheticRunningTimerOwnedGitHubWriterIsScheduled(t *testing.T) {
	now := time.Date(2026, 9, 8, 21, 0, 0, 0, time.UTC)
	pastNext := now.Add(-14 * time.Hour).Unix()
	start := now.Add(-30 * time.Minute).Unix()
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:      "activating",
		mainPID:        231,
		execStartEpoch: start,
		timerNext:      &pastNext,
		timerLast:      start,
	})
	if alert := findBackupArchiveAlert(alerts, "backup-archive-timer-unscheduled", "backup-1/github"); alert != nil {
		t.Fatalf("timer-owned running GitHub writer was marked unscheduled: %+v", *alert)
	}
}

func TestBackupArchivesSignalSyntheticManualGitHubWriterDoesNotHideUnscheduledTimer(t *testing.T) {
	now := time.Date(2026, 9, 8, 21, 1, 0, 0, time.UTC)
	pastNext := now.Add(-14 * time.Hour).Unix()
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:      "activating",
		mainPID:        232,
		execStartEpoch: now.Add(-30 * time.Minute).Unix(),
		timerNext:      &pastNext,
		timerLast:      now.Add(-24 * time.Hour).Unix(),
	})
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-timer-unscheduled", "backup-1/github")
	for _, want := range []string{
		"timer_next=2026-09-08T07:01:00Z",
		"timer_last=2026-09-07T21:01:00Z",
		"writer_main_pid_present=true",
		"writer_start=2026-09-08T20:31:00Z",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("manual-writer timer alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticRunningTimerOwnedDataWriterIsScheduled(t *testing.T) {
	now := time.Date(2026, 9, 8, 21, 2, 0, 0, time.UTC)
	start := now.Add(-time.Hour).Unix()
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:      "activating",
		remoteUnitSubstate:   "start",
		remoteMainPID:        233,
		remoteExecStartEpoch: start,
		remoteTimerNext:      now.Add(-10 * time.Hour).Unix(),
		remoteTimerLast:      start,
	})
	if alert := findBackupArchiveAlert(alerts, "backup-archive-timer-unscheduled", "backup-1/remote"); alert != nil {
		t.Fatalf("timer-owned running data writer was marked unscheduled: %+v", *alert)
	}
}

// A repaired and mounted external filesystem can still lack the configured
// root directory. Both writers intentionally refuse that state rather than
// falling through to a similarly named directory on the system disk.
func TestBackupArchivesSignalSyntheticDetectsMissingEffectiveArchiveRoot(t *testing.T) {
	now := time.Date(2026, 9, 8, 17, 32, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		githubArchivePathState:       "missing",
		remoteArchivePathState:       "missing",
		archivePathsOnMount:          boolPointer(false),
		archivePathPermissionsSecure: boolPointer(false),
	}, fixtures...)
	alert := requireBackupArchiveAlert(
		t,
		alerts,
		"backup-archive-root-unavailable",
		"backup-1/archive-root",
	)
	if alert.Sustain != 1 || alert.Severity != SeverityPage {
		t.Fatalf("archive-root urgency=%s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"github_path_state=missing",
		"data_path_state=missing",
		"writer_paths_match=true",
		"writer_mounts_match=true",
		"paths_on_configured_mount=false",
		"root_owner_mode_0700=false",
		"mount_state=read-write",
		"fail closed",
		"system disk",
		"root:root mode 0700",
		"explicit operator authorization",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing archive-root alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(
		alerts,
		"backup-archive-volume-unavailable",
		"backup-1/archive-volume",
	); unexpected != nil {
		t.Fatalf("healthy mounted volume was conflated with its missing root: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticArchiveRootObservationLossIsNotMissingRoot(t *testing.T) {
	now := time.Date(2026, 9, 8, 17, 32, 30, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		archiveRootObservation: "unobservable",
	})
	alert := requireBackupArchiveAlert(
		t,
		alerts,
		"backup-archive-root-unobservable",
		"backup-1/archive-root",
	)
	if alert.Sustain != 2 || alert.Severity != SeverityWarn {
		t.Fatalf("archive-root visibility urgency=%s/%d, want warn/2", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"archive_root_observation=unobservable",
		"permission denial from absence",
		"UNKNOWN root state",
		"exact --root-status sudoers rule",
		"Do not create a directory",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("archive-root visibility alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(
		alerts,
		"backup-archive-root-unavailable",
		"backup-1/archive-root",
	); unexpected != nil {
		t.Fatalf("unobservable root was misclassified as unavailable: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticQuotedCollectorOmission(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 1, 0, 0, time.UTC)
	alerts := runBackupArchiveFixtures(t, now)
	if len(alerts) != len(backupArchiveNames)*3 {
		t.Fatalf("collector omission alerts=%d, want %d: %+v", len(alerts), len(backupArchiveNames)*3, alerts)
	}
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-missing", "backup-1/pg")
	if alert.SignalNumber != "11.22" || alert.SignalKey != "backup-archives" || alert.Sustain != 2 {
		t.Fatalf("wrong backup metrics signal identity: %+v", alert)
	}
	for _, want := range []string{
		"classic-config quotes",
		"textfile\"",
		"node_uname_info",
		"stdout-only textfile collector",
		"wrapping quotes must not be present",
		"SIGNALS.md §11.22",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("backup metrics alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticStaleAndMissingGenerations(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 2, 0, 0, time.UTC)
	zero := float64(0)
	stale := now.Add(-12 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	alerts := runBackupArchiveFixtures(t, now,
		backupArchiveFixture{archive: "pg", generation: "main-pg-old.sql.xz", createdAt: &stale, progress: &zero},
		backupArchiveFixture{archive: "redis", generation: "main-redis-current", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urnetwork", progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", progress: &zero},
	)
	staleAlert := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/pg")
	for _, want := range []string{
		"generation=main-pg-old.sql.xz",
		"12 days",
		"absent udisks mount",
		"Start a catch-up run only with operator authorization",
		"cannot create a new recovery point",
	} {
		if !strings.Contains(staleAlert.Markdown(), want) {
			t.Fatalf("stale archive alert missing %q:\n%s", want, staleAlert.Markdown())
		}
	}
	missing := requireBackupArchiveAlert(t, alerts, "backup-archive-missing", "backup-1/github-urnetwork")
	if !strings.Contains(missing.Markdown(), "in_progress=0") ||
		!strings.Contains(missing.Markdown(), "no observable structurally complete archive generation") ||
		!strings.Contains(missing.Markdown(), "Metric absence alone cannot distinguish") ||
		!strings.Contains(missing.Markdown(), "preserves last-known completion rows") {
		t.Fatalf("missing archive alert lacks completion semantics:\n%s", missing.Markdown())
	}
	if strings.Contains(missing.Markdown(), "there is no recoverable generation") {
		t.Fatalf("missing metric still overclaims physical archive absence:\n%s", missing.Markdown())
	}
	if findBackupArchiveAlert(alerts, "backup-archive-stale", "backup-1/redis") != nil {
		t.Fatalf("fresh Redis archive was marked stale: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticStaleActiveTransferNeedsCapacity(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 30, 0, 0, time.UTC)
	zero := float64(0)
	one := float64(1)
	stale := now.Add(-13 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	alerts := runBackupArchiveFixtures(t, now,
		backupArchiveFixture{archive: "pg", generation: "main-pg-old.sql.xz", createdAt: &stale, progress: &one},
		backupArchiveFixture{archive: "redis", generation: "main-redis-current", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urnetwork", generation: "main-code-urnetwork-current.tar.xz", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", generation: "main-code-urfoundation-current.tar.xz", createdAt: &fresh, progress: &zero},
	)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/pg")
	for _, want := range []string{
		"in_progress=1",
		"Preserve the active writer",
		"dedicated direct SSH path",
		"source backlog with sustained direct-transfer throughput",
		"faster path or an approved offline seed",
		"Software cannot create WAN bandwidth",
		"Do not restart, duplicate, or manually finalize",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("active stale archive alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Action, "Start a catch-up run") {
		t.Fatalf("active transfer retained stale start guidance: %+v", alert)
	}
}

func TestBackupArchivesSignalSyntheticStaleArchiveQueuedBehindActiveDataWriter(t *testing.T) {
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	zero := float64(0)
	one := float64(1)
	stale := now.Add(-13 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "activating",
		remoteUnitSubstate: "start",
		remoteMainPID:      156739,
	},
		backupArchiveFixture{archive: "pg", generation: "main-pg-old.sql.xz", createdAt: &stale, progress: &one},
		backupArchiveFixture{archive: "redis", generation: "main-redis-old.rdb", createdAt: &stale, progress: &zero},
		backupArchiveFixture{archive: "github-urnetwork", generation: "main-code-urnetwork-current.tar.xz", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", generation: "main-code-urfoundation-current.tar.xz", createdAt: &fresh, progress: &zero},
	)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/redis")
	if alert.Frame != "queued-behind=pg" {
		t.Fatalf("queued archive frame=%q, want queued-behind=pg", alert.Frame)
	}
	for _, want := range []string{
		"same single-writer data job",
		"queued_behind=pg",
		"owner_unit=remote-backup-archive.service",
		"owner_unit_state=activating",
		"owner_unit_substate=start",
		"owner_main_pid=156739",
		"Preserve the active pg phase",
		"Do not start, restart, or duplicate",
		"publishes redis in_progress=1 without a second unit generation",
		"Software cannot create WAN bandwidth",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("queued stale archive alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Action, "Start a catch-up run") {
		t.Fatalf("queued transfer retained stale start guidance: %+v", alert)
	}
}

func TestBackupArchivesSignalSyntheticUnavailableVolumeRejectsFalseProgressAndQueue(t *testing.T) {
	now := time.Date(2026, 9, 3, 14, 18, 0, 0, time.UTC)
	zero := float64(0)
	one := float64(1)
	stale := now.Add(-14 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "activating",
		remoteUnitSubstate: "start",
		remoteMainPID:      365957,
		remoteMountPresent: boolPointer(false),
		remoteMountSource:  "unknown",
		remoteMountFSType:  "unknown",
		remoteMountOptions: "unknown",
	},
		backupArchiveFixture{archive: "pg", generation: "main-pg-old.sql.xz", createdAt: &stale, progress: &zero},
		backupArchiveFixture{archive: "redis", generation: "main-redis-old.tar.gpg", createdAt: &stale, progress: &one},
		backupArchiveFixture{archive: "github-urnetwork", generation: "main-code-urnetwork-current.tar.xz", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", generation: "main-code-urfoundation-current.tar.xz", createdAt: &fresh, progress: &zero},
	)

	volume := requireBackupArchiveAlert(t, alerts, "backup-archive-volume-unavailable", "backup-1/archive-volume")
	if volume.Sustain != 1 || volume.Severity != SeverityPage {
		t.Fatalf("volume alert urgency = %s/%d, want page/1: %+v", volume.Severity, volume.Sustain, volume)
	}
	for _, want := range []string{
		"mount_state=missing",
		"main_pid=365957",
		"same physical volume appears under a new /dev name",
		"stable LUKS UUID",
		"run e2fsck offline",
		"bounded write/read/delete check",
		"potentially hardware repair",
		"Do not live-remount",
		"SIGNALS.md §11.22",
	} {
		if !strings.Contains(volume.Markdown(), want) {
			t.Fatalf("unavailable-volume alert missing %q:\n%s", want, volume.Markdown())
		}
	}

	pg := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/pg")
	if pg.Frame == "queued-behind=redis" || strings.Contains(pg.Markdown(), "same single-writer data job") {
		t.Fatalf("missing volume was misclassified as a healthy Redis queue:\n%s", pg.Markdown())
	}
	redis := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/redis")
	for _, want := range []string{
		"archive_volume_state=missing",
		"cannot prove that rsync still has a mounted, writable destination",
		"do not attribute the unchanged completion time to source backlog or WAN capacity",
	} {
		if !strings.Contains(redis.Markdown(), want) {
			t.Fatalf("stale archive did not reject false progress %q:\n%s", want, redis.Markdown())
		}
	}
	if strings.Contains(redis.Action, "Preserve the active writer") {
		t.Fatalf("unavailable volume retained healthy-transfer action: %+v", redis)
	}
}

func TestBackupArchivesSignalSyntheticEmergencyReadOnlyVolume(t *testing.T) {
	now := time.Date(2026, 9, 3, 13, 18, 12, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteMountPresent: boolPointer(true),
		remoteMountSource:  "/dev/mapper/luks-synthetic",
		remoteMountFSType:  "ext4",
		remoteMountOptions: "rw,nosuid,nodev,errors=remount-ro,emergency_ro",
	})
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-volume-unavailable", "backup-1/archive-volume")
	for _, want := range []string{
		"mounted read-only",
		"read-only or ext4 emergency_ro",
		"source=/dev/mapper/luks-synthetic",
		"fstype=ext4",
		"aborted journal",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("read-only volume alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticPostRebootStorageRecoveryRemainsUnverifiedAndIdle(t *testing.T) {
	now := time.Date(2026, 9, 3, 17, 5, 11, 0, time.UTC)
	zero := float64(0)
	fresh := now.Add(-24 * time.Hour)
	boot := time.Date(2026, 9, 3, 14, 23, 45, 0, time.UTC)
	nextTimer := time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	transportFault := time.Date(2026, 9, 3, 13, 17, 58, 0, time.UTC)
	journalFault := time.Date(2026, 9, 3, 13, 18, 12, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "inactive",
		remoteUnitSubstate: "dead",
		remoteResult:       "success",
		remoteExitStatus:   0,
		remoteInvocationID: "none",
		remoteExecStart:    0,
		remoteTimerState:   "active",
		remoteTimerNext:    nextTimer.Unix(),
		remoteBoot:         boot.Unix(),
		remoteMountPresent: boolPointer(true),
		remoteMountSource:  "/dev/mapper/luks-synthetic",
		remoteMountFSType:  "ext4",
		remoteMountOptions: "rw,nosuid,nodev,relatime,errors=remount-ro",
		remoteMountLineage: "dm-2,sda1,sda",
		storageReadable:    boolPointer(true),
		storageEvents: []backupArchiveStorageEventFixture{
			{epoch: transportFault.Unix(), kind: "transport", device: "sda"},
			{epoch: transportFault.Add(time.Second).Unix(), kind: "block-io", device: "sda"},
			{epoch: journalFault.Unix(), kind: "journal", device: "dm-2"},
			// The filter must not attribute an unrelated local disk to archive1.
			{epoch: journalFault.Unix(), kind: "block-io", device: "sdb"},
		},
	},
		backupArchiveFixture{archive: "pg", progress: &zero},
		backupArchiveFixture{archive: "redis", progress: &zero},
		backupArchiveFixture{archive: "github-urnetwork", generation: "main-code-urnetwork-current.tar.xz", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", generation: "main-code-urfoundation-current.tar.xz", createdAt: &fresh, progress: &zero},
	)

	volume := requireBackupArchiveAlert(t, alerts, "backup-archive-volume-recovery-unverified", "backup-1/archive-volume")
	if volume.Sustain != 1 || volume.Severity != SeverityPage {
		t.Fatalf("post-reboot volume alert urgency = %s/%d, want page/1: %+v", volume.Severity, volume.Sustain, volume)
	}
	for _, want := range []string{
		"mount_state=read-write",
		"lineage=dm-2,sda1,sda",
		"matched_devices=dm-2,sda",
		"transport_events=1",
		"block_io_events=1",
		"journal_events=1",
		"latest_event=2026-09-03T13:18:12Z",
		"Journal replay and a fresh read-write mount",
		"do not prove an offline full-filesystem check",
		"kernel names are mutable",
		"full offline e2fsck",
		"bounded write/read/delete check",
		"30 minutes with no new lineage-bound",
		"intentionally remains active for the full 30-day evidence window",
		"not an automated alert-clear condition",
		"neither alert disappearance nor the 30-minute probation alone is closure",
	} {
		if !strings.Contains(volume.Markdown(), want) {
			t.Fatalf("post-reboot volume alert missing %q:\n%s", want, volume.Markdown())
		}
	}
	for _, raw := range []string{"uas_eh_abort_handler", "Remounting filesystem read-only", "sector 123"} {
		if strings.Contains(volume.Markdown(), raw) {
			t.Fatalf("post-reboot volume alert leaked raw kernel text %q:\n%s", raw, volume.Markdown())
		}
	}

	idle := requireBackupArchiveAlert(t, alerts, "backup-archive-recovery-idle", "backup-1/remote")
	for _, want := range []string{
		"recovery_required=pg:missing,redis:missing",
		"unit_state=inactive",
		"unit_substate=dead",
		"main_pid=0",
		"invocation_id=none",
		"exec_start_monotonic=0",
		"result=success",
		"exit_status=0",
		"timer_state=active",
		"timer_next=2026-09-04T11:00:00Z",
		"boot=2026-09-03T14:23:45Z",
		"manager defaults",
		"reboot therefore discarded the pre-reboot retry/backoff",
		"run only the bounded metrics refresh",
		"explicit operator authorization",
		"direct SSH banner and an enp65s0 route",
	} {
		if !strings.Contains(idle.Markdown(), want) {
			t.Fatalf("post-reboot idle alert missing %q:\n%s", want, idle.Markdown())
		}
	}
	for _, archive := range []string{"pg", "redis"} {
		requireBackupArchiveAlert(t, alerts, "backup-archive-missing", "backup-1/"+archive)
	}
}

func TestBackupArchivesSignalSyntheticUnrelatedDeviceFaultDoesNotTaintArchiveVolume(t *testing.T) {
	now := time.Date(2026, 9, 3, 17, 6, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteInvocationID: "present",
		remoteExecStart:    42,
		remoteMountLineage: "dm-2,sda1,sda",
		storageReadable:    boolPointer(true),
		storageEvents: []backupArchiveStorageEventFixture{
			{epoch: now.Add(-time.Minute).Unix(), kind: "transport", device: "sdb"},
			{epoch: now.Add(-time.Minute).Unix(), kind: "block-io", device: "dm-9"},
		},
	}, fixtures...)
	if alert := findBackupArchiveAlert(alerts, "backup-archive-volume-recovery-unverified", "backup-1/archive-volume"); alert != nil {
		t.Fatalf("unrelated block-device fault tainted archive volume: %+v", *alert)
	}
	if alert := findBackupArchiveAlert(alerts, "backup-archive-volume-history-unobservable", "backup-1/archive-volume"); alert != nil {
		t.Fatalf("observable archive lineage was marked unknown: %+v", *alert)
	}
}

func TestBackupArchivesSignalSyntheticActiveWriterDuringUnverifiedRecovery(t *testing.T) {
	now := time.Date(2026, 9, 4, 13, 20, 0, 0, time.UTC)
	for _, test := range []struct {
		name             string
		githubState      string
		githubPID        int64
		remoteState      string
		remoteSubstate   string
		remotePID        int64
		clearanceState   string
		wantActiveWriter string
	}{
		{name: "data-missing", githubState: "inactive", remoteState: "activating", remoteSubstate: "start", remotePID: 153799, clearanceState: "missing", wantActiveWriter: "active_writers=data"},
		{name: "github-invalid", githubState: "active", githubPID: 168693, remoteState: "inactive", remoteSubstate: "dead", clearanceState: "invalid", wantActiveWriter: "active_writers=github"},
		{name: "both-unobservable", githubState: "active", githubPID: 168693, remoteState: "activating", remoteSubstate: "start", remotePID: 153799, clearanceState: "unobservable", wantActiveWriter: "active_writers=data,github"},
	} {
		t.Run(test.name, func(t *testing.T) {
			alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
				unitState:          test.githubState,
				mainPID:            test.githubPID,
				remoteUnitState:    test.remoteState,
				remoteUnitSubstate: test.remoteSubstate,
				remoteMainPID:      test.remotePID,
				clearanceState:     test.clearanceState,
				storageEvents: []backupArchiveStorageEventFixture{
					{epoch: now.Add(-time.Hour).Unix(), kind: "journal", device: "dm-2"},
				},
			})
			alert := requireBackupArchiveAlert(t, alerts, "backup-archive-writer-active-during-recovery", "backup-1/archive-volume")
			if alert.Severity != SeverityPage || alert.Sustain != 1 {
				t.Fatalf("unsafe writer urgency = %s/%d, want page/1: %+v", alert.Severity, alert.Sustain, alert)
			}
			for _, want := range []string{
				test.wantActiveWriter,
				"clearance_state=" + test.clearanceState,
				"data_main_pid_present=",
				"github_main_pid_present=",
				"fault_latest=2026-09-04T12:20:00Z",
				"explicit current-writer operator decision",
				"does not protect an already-running shell",
				"does not return process arguments",
			} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("unsafe writer alert missing %q:\n%s", want, alert.Markdown())
				}
			}
			for _, rawPID := range []string{"153799", "168693"} {
				if strings.Contains(alert.Markdown(), rawPID) {
					t.Fatalf("unsafe writer alert leaked raw PID %s:\n%s", rawPID, alert.Markdown())
				}
			}
			requireBackupArchiveAlert(t, alerts, "backup-archive-volume-recovery-unverified", "backup-1/archive-volume")
		})
	}
}

func TestBackupArchivesSignalSyntheticClearedActiveWriterIsNotUnsafe(t *testing.T) {
	now := time.Date(2026, 9, 4, 13, 20, 30, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "active", mainPID: 168693,
		remoteUnitState: "activating", remoteUnitSubstate: "start", remoteMainPID: 153799,
		clearanceState: "valid",
		storageEvents: []backupArchiveStorageEventFixture{
			{epoch: now.Add(-time.Hour).Unix(), kind: "journal", device: "dm-2"},
		},
	})
	if alert := findBackupArchiveAlert(alerts, "backup-archive-writer-active-during-recovery", "backup-1/archive-volume"); alert != nil {
		t.Fatalf("valid stable-identity clearance was still marked unsafe: %+v", *alert)
	}
	recovery := requireBackupArchiveAlert(t, alerts, "backup-archive-volume-recovery-unverified", "backup-1/archive-volume")
	for _, want := range []string{
		"cleared-recovery-history",
		"clearance_state=valid",
		"Do not stop, restart, or duplicate",
		"retained historical page",
		"repeat hardware isolation or offline e2fsck only if new evidence",
	} {
		if !strings.Contains(recovery.Markdown(), want) {
			t.Fatalf("cleared recovery guidance missing %q:\n%s", want, recovery.Markdown())
		}
	}
	if strings.Contains(recovery.Action, "Keep both archive writers stopped") {
		t.Fatalf("cleared recovery guidance still asks to stop the active writer:\n%s", recovery.Markdown())
	}
}

func TestBackupArchivesSignalSyntheticStoppedWritersAreSafeDuringRecoveryGate(t *testing.T) {
	now := time.Date(2026, 9, 4, 13, 21, 0, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "inactive", remoteUnitState: "inactive", remoteUnitSubstate: "dead",
		storageEvents: []backupArchiveStorageEventFixture{
			{epoch: now.Add(-time.Hour).Unix(), kind: "transport", device: "sda"},
		},
	})
	if alert := findBackupArchiveAlert(alerts, "backup-archive-writer-active-during-recovery", "backup-1/archive-volume"); alert != nil {
		t.Fatalf("stopped writers were marked active during recovery: %+v", *alert)
	}
	requireBackupArchiveAlert(t, alerts, "backup-archive-volume-recovery-unverified", "backup-1/archive-volume")
}

func TestBackupArchivesSignalSyntheticSuccessfulIdleWithRecentRowsIsNotRecoveryIdle(t *testing.T) {
	now := time.Date(2026, 9, 3, 17, 7, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "inactive",
		remoteUnitSubstate: "dead",
		remoteResult:       "success",
		remoteInvocationID: "present",
		remoteExecStart:    123456,
	}, fixtures...)
	if alert := findBackupArchiveAlert(alerts, "backup-archive-recovery-idle", "backup-1/remote"); alert != nil {
		t.Fatalf("normal successful idle unit was marked recovery-idle: %+v", *alert)
	}
}

func TestBackupArchivesSignalSyntheticArchiveStorageHistoryUnknownIsExplicit(t *testing.T) {
	now := time.Date(2026, 9, 3, 17, 8, 0, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		storageReadable:    boolPointer(false),
		remoteMountLineage: "unknown",
	})
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-volume-history-unobservable", "backup-1/archive-volume")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatalf("storage-history visibility alert urgency = %s/%d, want warn/2: %+v", alert.Severity, alert.Sustain, alert)
	}
	for _, want := range []string{
		"journal_readable=false",
		"lineage=unknown",
		"UNKNOWN storage history",
		"must not attribute an unrelated disk",
		"raw kernel text does not leave",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("storage-history visibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchiveMountStatePrioritizesEmergencyReadOnlyOverRW(t *testing.T) {
	tests := []struct {
		name    string
		mount   string
		present bool
		options string
		want    string
	}{
		{name: "healthy", mount: "/run/media/by/archive1", present: true, options: "rw,nosuid,nodev,errors=remount-ro", want: "read-write"},
		{name: "ordinary read only", mount: "/run/media/by/archive1", present: true, options: "ro,nosuid,nodev", want: "read-only"},
		{name: "ext4 emergency overrides rw", mount: "/run/media/by/archive1", present: true, options: "rw,nosuid,nodev,errors=remount-ro,emergency_ro", want: "read-only"},
		{name: "disconnected", mount: "/run/media/by/archive1", present: false, options: "unknown", want: "missing"},
		{name: "unit lacks mount contract", mount: "unknown", present: false, options: "unknown", want: "unknown"},
		{name: "ambiguous options", mount: "/run/media/by/archive1", present: true, options: "relatime", want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := backupArchiveMountState(test.mount, test.present, test.options); got != test.want {
				t.Fatalf("backupArchiveMountState(%q, %t, %q) = %q, want %q", test.mount, test.present, test.options, got, test.want)
			}
		})
	}

	for _, want := range []string{
		"BRINGYOUR_BACKUP_MOUNT=",
		"mountpoint -q --",
		"findmnt -rn -T",
		"remote_mount_options",
		"InvocationID",
		"NextElapseUSecRealtime",
		"LastTriggerUSec",
		"ExecMainStartTimestamp",
		"UnitFileState",
		"github_result",
		"github_failure_journal_status",
		"github_failure_first_boundary",
		"_SYSTEMD_INVOCATION_ID=",
		"_SYSTEMD_UNIT=github-backup-archive.service",
		"-n 513",
		"archive_root_observation",
		"github_archive_path_state",
		"remote_archive_path_state",
		"archive_paths_on_mount",
		"archive_path_permissions_secure",
		"sudo -n /var/bringyour/backup/archive-write-clearance.sh --root-status",
		"remote_timer_unit_file_state",
		"remote_mount_lineage",
		"lsblk -srno KNAME",
		"remote_storage_journal_readable",
		"remote_storage_event=",
		"_TRANSPORT=kernel",
		"-n 512",
	} {
		if !strings.Contains(backupArchiveWriterCommand, want) {
			t.Fatalf("writer observation command missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"printf 'github_archive_path=%s",
		"printf 'remote_archive_path=%s",
		"printf 'github_environment=%s",
		"printf 'remote_environment=%s",
		"stat -c '%u:%g:%a'",
		"test -d \"${github_archive_path}\"",
		"test -d \"${remote_archive_path}\"",
	} {
		if strings.Contains(backupArchiveWriterCommand, forbidden) {
			t.Fatalf("writer observation command emits private effective value %q", forbidden)
		}
	}
}

func TestBackupArchivesWriterCommandReducesInvocationIdentifiersToPresence(t *testing.T) {
	binDir := t.TempDir()
	invocationID := "0123456789abcdef0123456789abcdef"
	systemctl := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'-p InvocationID'*) printf '%s\\n' '" + invocationID + "' ;;\n" +
		"esac\n"
	for _, name := range []string{"systemctl", "date", "sudo", "mountpoint", "journalctl"} {
		body := "#!/bin/sh\nexit 1\n"
		if name == "systemctl" {
			body = systemctl
		}
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatalf("write synthetic %s: %v", name, err)
		}
	}
	command := exec.Command("sh", "-c", backupArchiveWriterCommand)
	command.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	outputBytes, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run writer observation command: %v\n%s", err, outputBytes)
	}
	output := string(outputBytes)
	if strings.Contains(output, invocationID) {
		t.Fatalf("writer observation leaked a full invocation identifier:\n%s", output)
	}
	for _, want := range []string{
		"github_invocation_id=present\n",
		"github_failure_journal_status=unobservable\n",
		"remote_invocation_id=present\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("writer observation missing %q:\n%s", want, output)
		}
	}
}

func TestBackupArchivesWriterCommandTriesDirectFailureJournalBeforeSudo(t *testing.T) {
	directProbe := strings.Index(
		backupArchiveWriterCommand,
		"if command journalctl --quiet --no-pager -n 1 -o cat",
	)
	sudoFallback := strings.Index(
		backupArchiveWriterCommand,
		"elif sudo -n journalctl --quiet --no-pager -n 1 -o cat",
	)
	if directProbe < 0 || sudoFallback < 0 || directProbe >= sudoFallback {
		t.Fatalf("failure journal access order direct=%d sudo=%d, want direct before sudo fallback", directProbe, sudoFallback)
	}
}

func runSyntheticBackupArchiveWriterCommand(
	t testing.TB,
	unitState string,
	result string,
	exitStatus int64,
	journal string,
) string {
	t.Helper()
	binDir := t.TempDir()
	systemctl := `#!/bin/sh
case "$2:$4" in
  github-backup-archive.service:ActiveState) printf '%s\n' "${SYNTHETIC_UNIT_STATE}" ;;
  github-backup-archive.service:SubState) printf '%s\n' "${SYNTHETIC_UNIT_STATE}" ;;
  github-backup-archive.service:Result) printf '%s\n' "${SYNTHETIC_UNIT_RESULT}" ;;
  github-backup-archive.service:ExecMainStatus) printf '%s\n' "${SYNTHETIC_UNIT_EXIT_STATUS}" ;;
  github-backup-archive.service:InvocationID) printf '%s\n' '0123456789abcdef0123456789abcdef' ;;
esac
`
	sudo := `#!/bin/sh
case " $* " in
  *" _SYSTEMD_INVOCATION_ID="*)
    if [ -n "${SYNTHETIC_JOURNAL-}" ]; then
      printf '%s\n' "${SYNTHETIC_JOURNAL}"
    else
      exit 1
    fi
    ;;
  *" archive-write-clearance.sh --root-status ")
    printf '%s\n' \
      'github_archive_path_state=directory' \
      'remote_archive_path_state=directory' \
      'archive_paths_match=true' \
      'archive_mounts_match=true' \
      'archive_paths_on_mount=true' \
      'archive_path_permissions_secure=true'
    ;;
  *" archive-write-clearance.sh --status ") printf '%s\n' valid ;;
  *) exit 1 ;;
esac
`
	journalctl := `#!/bin/sh
case " $* " in
  *" _SYSTEMD_INVOCATION_ID="*)
    if [ -n "${SYNTHETIC_JOURNAL-}" ]; then
      printf '%s\n' "${SYNTHETIC_JOURNAL}"
    else
      exit 1
    fi
    ;;
  *) exit 1 ;;
esac
`
	for _, commandFixture := range []struct {
		name string
		body string
	}{
		{name: "systemctl", body: systemctl},
		{name: "sudo", body: sudo},
		{name: "date", body: "#!/bin/sh\nexit 1\n"},
		{name: "mountpoint", body: "#!/bin/sh\nexit 1\n"},
		{name: "journalctl", body: journalctl},
	} {
		path := filepath.Join(binDir, commandFixture.name)
		if err := os.WriteFile(path, []byte(commandFixture.body), 0o700); err != nil {
			t.Fatalf("write synthetic %s: %v", commandFixture.name, err)
		}
	}
	command := exec.Command("sh", "-c", backupArchiveWriterCommand)
	command.Env = append(
		os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SYNTHETIC_UNIT_STATE="+unitState,
		"SYNTHETIC_UNIT_RESULT="+result,
		fmt.Sprintf("SYNTHETIC_UNIT_EXIT_STATUS=%d", exitStatus),
		"SYNTHETIC_JOURNAL="+journal,
	)
	outputBytes, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run synthetic writer observation command: %v\n%s", err, outputBytes)
	}
	return string(outputBytes)
}

func TestBackupArchivesWriterCommandClassifiesGitHubFailureBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		journal  string
		boundary string
	}{
		{name: "storage EIO", journal: "synthetic writer: Input/output error", boundary: "storage-eio"},
		{name: "storage read only", journal: "synthetic writer: Read-only file system", boundary: "storage-read-only"},
		{name: "clearance mount", journal: "archive write clearance denied: synthetic fixture", boundary: "clearance-mount"},
		{name: "clearance observation unavailable", journal: "archive write clearance cannot be observed: synthetic fixture", boundary: "clearance-mount"},
		{name: "clearance helper unavailable", journal: "archive write-clearance helper is unavailable", boundary: "clearance-mount"},
		{name: "storage metrics clearance", journal: "archive mount identity is not cleared and read-write: mount=/synthetic/mount path=/synthetic/archive", boundary: "clearance-mount"},
		{name: "authentication", journal: "Permission denied (publickey).", boundary: "auth"},
		{name: "missing SSH key", journal: "missing GitHub backup ssh key: /synthetic/fixture-key", boundary: "auth"},
		{name: "missing API token", journal: "missing synthetic-org GitHub API token file: /synthetic/fixture-api-token", boundary: "auth"},
		{name: "malformed API token", journal: "synthetic-org GitHub API token file must contain exactly one line: /synthetic/fixture-api-token", boundary: "auth"},
		{name: "empty API tokens", journal: "GitHub API token files must not be empty", boundary: "auth"},
		{name: "HTTP 401", journal: "curl: (22) The requested URL returned error: 401", boundary: "auth"},
		{name: "API rate", journal: "curl: (22) The requested URL returned error: 429", boundary: "api-rate"},
		{name: "Git transfer", journal: "synthetic transport: Connection reset by peer", boundary: "git-transfer"},
		{name: "Git update wrapper", journal: "failed to update synthetic-org/synthetic-repository; preserving the previous synthetic-org code archive", boundary: "git-transfer"},
		{name: "invalid mirror cache", journal: "cached repository is not a bare mirror: /synthetic/repository.git", boundary: "git-transfer"},
		{name: "capacity", journal: "synthetic writer: No space left on device", boundary: "capacity"},
		{name: "capacity observation", journal: "could not read archive volume capacity for /synthetic/archive", boundary: "capacity"},
		{name: "compression integrity", journal: "new code archive failed its xz/tar integrity check: synthetic.tar.xz", boundary: "compression-integrity"},
		{name: "atomic publication", journal: "mv: cannot move synthetic-input to main-code-synthetic.tar.xz", boundary: "atomic-publication"},
		{name: "storage metrics helper", journal: "archive storage metrics helper is missing or not executable: /synthetic/archive-storage-metrics", boundary: "atomic-publication"},
		{name: "storage metrics initialization", journal: "failed to initialize archive storage metrics", boundary: "atomic-publication"},
		{name: "storage metrics update", journal: "failed to update archive storage metrics", boundary: "atomic-publication"},
		{name: "storage metrics directory", journal: "backup storage metrics directory does not exist: /synthetic/metrics", boundary: "atomic-publication"},
		{name: "progress metrics refresh", journal: "failed to refresh GitHub backup in-progress metrics", boundary: "atomic-publication"},
		{name: "retention source", journal: "no complete synthetic-org code archive in /synthetic/archive", boundary: "atomic-publication"},
		{name: "storage precedence", journal: "tar: synthetic output: Input/output error", boundary: "storage-eio"},
	} {
		output := runSyntheticBackupArchiveWriterCommand(t, "failed", "exit-code", 1, testCase.journal)
		for _, want := range []string{
			"github_failure_journal_status=complete\n",
			"github_failure_first_boundary=" + testCase.boundary + "\n",
			"github_failure_journal_lines=1\n",
			"github_failure_" + strings.ReplaceAll(testCase.boundary, "-", "_") + "_lines=1\n",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("%s: writer observation missing %q:\n%s", testCase.name, want, output)
			}
		}
		if testCase.boundary != "clearance-mount" {
			continue
		}
		writer, err := parseBackupArchiveWriterObservation("backup-writer.example.test", output)
		if err != nil {
			t.Fatalf("%s: parse actual synthetic writer command: %v", testCase.name, err)
		}
		alert := alertFromFinding(SignalSettings{
			Environment: "synthetic",
			Now: func() time.Time {
				return time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
			},
		}, "11.22", "backup-archives", "Synthetic archive writer", evaluateBackupArchiveGitHubRun(writer))
		if alert.Severity != SeverityPage || alert.Sustain != 1 ||
			!strings.Contains(alert.Mechanism, "first observed explicit clearance/mount error") ||
			!strings.Contains(alert.Mechanism, "does not prove which command") ||
			!strings.Contains(alert.Action, "Establish the owning fatal command and generation") ||
			!strings.Contains(alert.Action, "remained unresolved at the terminal boundary") {
			t.Errorf("%s: explicit clearance error lost urgency or fatal-command qualification", testCase.name)
		}
	}
}

// Positive clearance evidence must not replace the later explicit error stage.
func TestBackupArchivesWriterCommandPositiveClearancePreservesCompressionFailure(t *testing.T) {
	const positiveClearance = "archive write clearance is valid; continuing"
	const privateSyntheticArchive = "privacy-marker.example/synthetic.tar.xz"
	output := runSyntheticBackupArchiveWriterCommand(
		t,
		"failed",
		"exit-code",
		1,
		positiveClearance+"\n"+
			"new code archive failed its xz/tar integrity check: "+privateSyntheticArchive,
	)
	writer, err := parseBackupArchiveWriterObservation("backup-writer.example.test", output)
	if err != nil {
		t.Fatalf("parse actual synthetic writer command: %v", err)
	}
	alert := alertFromFinding(SignalSettings{
		Environment: "synthetic",
		Now: func() time.Time {
			return time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
		},
	}, "11.22", "backup-archives", "Synthetic archive writer", evaluateBackupArchiveGitHubRun(writer))
	if alert.Class != "backup-archive-writer-failed" ||
		alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatalf("explicit compression failure lost writer page/1: %s/%s/%d", alert.Class, alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"unit_state=failed",
		"exit_status=1",
		"failure_journal_status=complete",
		"first_failure_boundary=compression-integrity",
		"failure_journal_lines=2",
		"compression_integrity_lines=1",
		"clearance_mount_lines=0",
	} {
		if !strings.Contains(alert.Observed, want) {
			t.Errorf("positive-clearance compression observation missing %q: %s", want, alert.Observed)
		}
	}
	if !strings.Contains(alert.Mechanism, "archive compression") ||
		!strings.Contains(alert.Action, "compression/integrity stage") {
		t.Error("positive clearance replaced the explicit compression mechanism/action")
	}
	if strings.Contains(alert.Mechanism, "first crossed the archive clearance") ||
		strings.Contains(alert.Action, "Repair the privacy-reduced clearance/mount contract") {
		t.Error("positive clearance produced an unsupported interlock claim or repair")
	}
	for _, forbidden := range []string{positiveClearance, privateSyntheticArchive} {
		if strings.Contains(output, forbidden) || strings.Contains(alert.Markdown(), forbidden) {
			t.Errorf("raw synthetic journal content leaked: %q", forbidden)
		}
	}
}

// A failed unit with only positive status evidence retains an unknown cause.
func TestBackupArchivesWriterCommandPositiveOnlyClearanceKeepsFailureCauseUnknown(t *testing.T) {
	for _, positiveClearance := range []string{
		"archive write clearance is valid; continuing",
		"archive write clearance valid",
		"archive write clearance recorded; complete the synthetic fault-free probation before starting a writer",
		"archive write-clearance helper is available; clearance marker present; stable archive synthetic identity verified; clearance probation complete",
	} {
		output := runSyntheticBackupArchiveWriterCommand(t, "failed", "exit-code", 1, positiveClearance)
		writer, err := parseBackupArchiveWriterObservation("backup-writer.example.test", output)
		if err != nil {
			t.Fatalf("parse actual synthetic writer command: %v", err)
		}
		alert := alertFromFinding(SignalSettings{
			Environment: "synthetic",
			Now: func() time.Time {
				return time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
			},
		}, "11.22", "backup-archives", "Synthetic archive writer", evaluateBackupArchiveGitHubRun(writer))
		if alert.Class != "backup-archive-writer-failed" ||
			alert.Severity != SeverityPage || alert.Sustain != 1 {
			t.Fatalf("positive-only journal erased the unsuccessful writer page/1: %s/%s/%d", alert.Class, alert.Severity, alert.Sustain)
		}
		for _, want := range []string{
			"unit_state=failed",
			"exit_status=1",
			"failure_journal_status=complete",
			"first_failure_boundary=unclassified",
			"failure_journal_lines=1",
			"clearance_mount_lines=0",
			"unclassified_lines=1",
		} {
			if !strings.Contains(alert.Observed, want) {
				t.Errorf("positive-only clearance observation missing %q: %s", want, alert.Observed)
			}
		}
		if !strings.Contains(alert.Mechanism, "cause remains unclassified") {
			t.Error("positive-only clearance did not retain the unknown writer failure cause")
		}
		if strings.Contains(alert.Mechanism, "first crossed the archive clearance") ||
			strings.Contains(alert.Action, "Repair the privacy-reduced clearance/mount contract") {
			t.Error("positive-only clearance produced an unsupported interlock claim or repair")
		}
		if strings.Contains(output, positiveClearance) || strings.Contains(alert.Markdown(), positiveClearance) {
			t.Error("positive-only raw synthetic journal content leaked")
		}
	}
}

func TestBackupArchivesWriterCommandDoesNotTreatMountWaitAsFailedBoundary(t *testing.T) {
	output := runSyntheticBackupArchiveWriterCommand(
		t,
		"failed",
		"exit-code",
		1,
		"waiting up to 30s for archive mount /synthetic/mount and /synthetic/archive\n"+
			"synthetic archive write: Input/output error",
	)
	for _, want := range []string{
		"github_failure_journal_status=complete\n",
		"github_failure_first_boundary=storage-eio\n",
		"github_failure_journal_lines=2\n",
		"github_failure_storage_eio_lines=1\n",
		"github_failure_clearance_mount_lines=0\n",
		"github_failure_unclassified_lines=1\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("mount-wait writer observation missing %q:\n%s", want, output)
		}
	}
}

func TestBackupArchivesWriterCommandKeepsUnsupportedStorageMetricTypeUnclassified(t *testing.T) {
	output := runSyntheticBackupArchiveWriterCommand(
		t,
		"failed",
		"exit-code",
		1,
		"unsupported archive type: synthetic",
	)
	for _, want := range []string{
		"github_failure_journal_status=complete\n",
		"github_failure_first_boundary=unclassified\n",
		"github_failure_journal_lines=1\n",
		"github_failure_unclassified_lines=1\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("unsupported storage-metric type observation missing %q:\n%s", want, output)
		}
	}
}

func TestBackupArchivesWriterCommandReducesExactEioInvocationWithoutRawText(t *testing.T) {
	const redactionMarker = "redaction-marker.example/synthetic-repository"
	journalLines := []string{redactionMarker}
	for index := 0; index < 10; index++ {
		journalLines = append(journalLines, "synthetic archive write: Input/output error")
	}
	output := runSyntheticBackupArchiveWriterCommand(
		t,
		"failed",
		"exit-code",
		1,
		strings.Join(journalLines, "\n"),
	)
	for _, want := range []string{
		"github_failure_journal_status=complete\n",
		"github_failure_first_boundary=storage-eio\n",
		"github_failure_journal_lines=11\n",
		"github_failure_storage_eio_lines=10\n",
		"github_failure_unclassified_lines=1\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("EIO writer observation missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{
		redactionMarker,
		"synthetic archive write",
		"0123456789abcdef0123456789abcdef",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("writer observation leaked raw invocation value %q:\n%s", forbidden, output)
		}
	}
}

func TestBackupArchivesWriterCommandSkipsJournalForHealthyGitHubWriter(t *testing.T) {
	const redactionMarker = "healthy-redaction-marker.example/synthetic-repository"
	output := runSyntheticBackupArchiveWriterCommand(t, "inactive", "success", 0, redactionMarker)
	for _, want := range []string{
		"github_failure_journal_status=not-applicable\n",
		"github_failure_first_boundary=none\n",
		"github_failure_journal_lines=0\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("healthy writer observation missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, redactionMarker) {
		t.Fatalf("healthy writer observation read or leaked journal text:\n%s", output)
	}
}

func TestBackupArchivesWriterCommandFailsClosedAndRedactsAmbiguousJournal(t *testing.T) {
	const redactionMarker = "ambiguous-redaction-marker.example/synthetic-repository"
	journalLines := make([]string, 0, backupArchiveGitHubJournalMaxLines+2)
	for index := int64(0); index <= backupArchiveGitHubJournalMaxLines+1; index++ {
		journalLines = append(journalLines, fmt.Sprintf("%s-%d", redactionMarker, index))
	}
	output := runSyntheticBackupArchiveWriterCommand(t, "failed", "exit-code", 1, strings.Join(journalLines, "\n"))
	for _, want := range []string{
		"github_failure_journal_status=ambiguous\n",
		"github_failure_first_boundary=unclassified\n",
		"github_failure_journal_lines=513\n",
		"github_failure_unclassified_lines=513\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("ambiguous writer observation missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, redactionMarker) {
		t.Fatalf("ambiguous writer observation leaked raw journal text:\n%s", output)
	}
}

func TestBackupArchivesWriterCommandFailsClosedOnOverlappingFirstBoundary(t *testing.T) {
	output := runSyntheticBackupArchiveWriterCommand(
		t,
		"failed",
		"exit-code",
		1,
		"curl: synthetic connection reset",
	)
	for _, want := range []string{
		"github_failure_journal_status=ambiguous\n",
		"github_failure_first_boundary=unclassified\n",
		"github_failure_journal_lines=1\n",
		"github_failure_unclassified_lines=1\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("overlapping writer observation missing %q:\n%s", want, output)
		}
	}
}

func TestBackupArchivesWriterCommandAcceptsOnlyReducedPrivilegedRootState(t *testing.T) {
	binDir := t.TempDir()
	sudo := `#!/bin/sh
case " $* " in
  *" --root-status ")
    printf '%s\n' \
      'github_archive_path_state=directory' \
      'remote_archive_path_state=directory' \
      'archive_paths_match=true' \
      'archive_mounts_match=true' \
      'archive_paths_on_mount=true' \
      'archive_path_permissions_secure=true'
    if [ "${FAKE_ROOT_HELPER_EXTRA:-0}" = 1 ]; then
      printf '%s\n' 'effective_path=/synthetic/private/archive'
    fi
    ;;
  *" --status ") printf '%s\n' valid ;;
  *) exit 1 ;;
esac
`
	for _, name := range []string{"systemctl", "date", "sudo", "mountpoint", "journalctl"} {
		body := "#!/bin/sh\nexit 1\n"
		if name == "sudo" {
			body = sudo
		}
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatalf("write synthetic %s: %v", name, err)
		}
	}
	run := func(extra bool) string {
		t.Helper()
		command := exec.Command("sh", "-c", backupArchiveWriterCommand)
		command.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if extra {
			command.Env = append(command.Env, "FAKE_ROOT_HELPER_EXTRA=1")
		}
		outputBytes, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("run writer observation command: %v\n%s", err, outputBytes)
		}
		return string(outputBytes)
	}

	healthy := run(false)
	for _, want := range []string{
		"archive_root_observation=observable\n",
		"github_archive_path_state=directory\n",
		"archive_path_permissions_secure=true\n",
	} {
		if !strings.Contains(healthy, want) {
			t.Fatalf("writer observation missing %q:\n%s", want, healthy)
		}
	}

	malformed := run(true)
	if !strings.Contains(malformed, "archive_root_observation=unobservable\n") {
		t.Fatalf("extra helper field did not fail observation closed:\n%s", malformed)
	}
	if strings.Contains(malformed, "/synthetic/private/archive") {
		t.Fatalf("writer observation leaked rejected helper output:\n%s", malformed)
	}
}

func TestBackupArchivesSignalSyntheticDetectsStaleActiveWriterProgress(t *testing.T) {
	now := time.Date(2026, 9, 1, 23, 56, 0, 0, time.UTC)
	zero := float64(0)
	one := float64(1)
	createdAt := now.Add(-time.Hour)
	staleHeartbeat := now.Add(-2 * time.Hour)
	base := []backupArchiveFixture{
		{archive: "pg", generation: "main-pg-current.sql.xz", createdAt: &createdAt, progress: &zero},
		{archive: "redis", generation: "main-redis-current", createdAt: &createdAt, progress: &zero},
		{archive: "github-urnetwork", generation: "main-code-urnetwork-2026-09-01-22-30-00.tar.xz", createdAt: &createdAt, progress: &zero, heartbeat: &staleHeartbeat},
		{archive: "github-urfoundation", generation: "main-code-urfoundation-2026-09-01-22-30-00.tar.xz", createdAt: &createdAt, progress: &zero, heartbeat: &staleHeartbeat},
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "activating", mainPID: 156738,
	}, base...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-progress-stale", "backup-1/github")
	for _, want := range []string{
		"unit_state=activating",
		"main_pid=156738",
		"heartbeat_age=2h0m0s",
		"published_progress_total=0",
		"metrics-heartbeat-stale",
		"active-unit-progress-total-not-one",
		"Fluent Bit assigns a fresh scrape timestamp",
		"Xops commit 2733b0b",
		"already-running pre-fix shell will not gain that behavior",
		"rather than restarting this one",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stale active-writer alert missing %q:\n%s", want, alert.Markdown())
		}
	}

	healthy := append([]backupArchiveFixture(nil), base...)
	freshHeartbeat := now.Add(-30 * time.Second)
	healthy[2].progress = &one
	healthy[2].heartbeat = &freshHeartbeat
	healthy[3].heartbeat = &freshHeartbeat
	healthyAlerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "activating", mainPID: 156738,
	}, healthy...)
	if unexpected := findBackupArchiveAlert(healthyAlerts, "backup-archive-progress-stale", "backup-1/github"); unexpected != nil {
		t.Fatalf("fresh single-owner active progress alerted: %+v", *unexpected)
	}
}

func TestBackupArchivesSignalSyntheticRejectsMalformedWriterObservation(t *testing.T) {
	valid := backupArchiveWriterFixtureText(backupArchiveWriterFixture{})
	for _, testCase := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "missing", output: "github_unit_state=activating", want: "expected 61 properties"},
		{name: "state", output: strings.Replace(valid, "github_unit_state=inactive", "github_unit_state=ACTIVE", 1), want: "invalid github_unit_state"},
		{name: "pid", output: strings.Replace(valid, "github_main_pid=0", "github_main_pid=nope", 1), want: "invalid main PID"},
		{name: "github result", output: strings.Replace(valid, "github_result=success", "github_result=EXIT CODE", 1), want: "invalid github_result"},
		{name: "github exit", output: strings.Replace(valid, "github_exit_status=0", "github_exit_status=nope", 1), want: "invalid GitHub exit status"},
		{name: "github invocation", output: strings.Replace(valid, "github_invocation_id=present", "github_invocation_id=not-a-state", 1), want: "invalid github_invocation_id"},
		{name: "failure journal status", output: strings.Replace(valid, "github_failure_journal_status=not-applicable", "github_failure_journal_status=invalid.example", 1), want: "invalid GitHub failure journal status"},
		{name: "first failure boundary", output: strings.Replace(valid, "github_failure_first_boundary=none", "github_failure_first_boundary=invalid.example", 1), want: "invalid GitHub first failure boundary"},
		{name: "failure journal lines", output: strings.Replace(valid, "github_failure_journal_lines=0", "github_failure_journal_lines=many", 1), want: "invalid GitHub failure journal line count"},
		{name: "failure count", output: strings.Replace(valid, "github_failure_storage_eio_lines=0", "github_failure_storage_eio_lines=many", 1), want: "invalid GitHub failure boundary count for storage-eio"},
		{name: "failure count coverage", output: strings.Replace(valid, "github_failure_unclassified_lines=0", "github_failure_unclassified_lines=1", 1), want: "boundary counts do not cover"},
		{name: "complete empty journal", output: strings.Replace(valid, "github_failure_journal_status=not-applicable", "github_failure_journal_status=complete", 1), want: "complete GitHub failure journal has invalid cardinality"},
		{name: "healthy journal applicability", output: strings.Replace(valid, "github_failure_journal_status=not-applicable", "github_failure_journal_status=unobservable", 1), want: "applicability disagrees"},
		{name: "github start epoch", output: strings.Replace(valid, "github_exec_start_epoch=0", "github_exec_start_epoch=earlier", 1), want: "invalid github_exec_start_epoch"},
		{name: "github timer epoch", output: strings.Replace(valid, "github_timer_next_epoch=2000000000", "github_timer_next_epoch=tomorrow", 1), want: "invalid github_timer_next_epoch"},
		{name: "root observation", output: strings.Replace(valid, "archive_root_observation=observable", "archive_root_observation=maybe", 1), want: "invalid archive_root_observation"},
		{name: "archive path state", output: strings.Replace(valid, "github_archive_path_state=directory", "github_archive_path_state=unsafe", 1), want: "invalid github_archive_path_state"},
		{name: "observable unknown root", output: strings.Replace(valid, "github_archive_path_state=directory", "github_archive_path_state=unknown", 1), want: "invalid archive path state for observable archive root"},
		{name: "archive path mount", output: strings.Replace(valid, "archive_paths_on_mount=true", "archive_paths_on_mount=maybe", 1), want: "invalid archive_paths_on_mount"},
		{name: "data timer unit file", output: strings.Replace(valid, "remote_timer_unit_file_state=enabled", "remote_timer_unit_file_state=ENABLED", 1), want: "invalid remote_timer_unit_file_state"},
		{name: "delay", output: strings.Replace(valid, "remote_restart_delay=30min", "remote_restart_delay=immediate!", 1), want: "invalid remote_restart_delay"},
		{name: "invocation", output: strings.Replace(valid, "remote_invocation_id=present", "remote_invocation_id=not-a-state", 1), want: "invalid remote_invocation_id"},
		{name: "timer epoch", output: strings.Replace(valid, "remote_timer_next_epoch=2000000000", "remote_timer_next_epoch=tomorrow", 1), want: "invalid remote_timer_next_epoch"},
		{name: "Git transfer attempts", output: strings.Replace(valid, "github_git_transfer_attempts=4", "github_git_transfer_attempts=many", 1), want: "invalid github_git_transfer_attempts"},
		{name: "mount present", output: strings.Replace(valid, "remote_mount_present=true", "remote_mount_present=maybe", 1), want: "invalid remote_mount_present"},
		{name: "mount options", output: strings.Replace(valid, "remote_mount_options=rw,nosuid,nodev,relatime,errors=remount-ro", "remote_mount_options=rw secret", 1), want: "invalid remote_mount_options"},
		{name: "mount lineage", output: strings.Replace(valid, "remote_mount_lineage=dm-2,sda1,sda", "remote_mount_lineage=dm-2,sda1,sda;bad", 1), want: "invalid remote_mount_lineage"},
		{name: "clearance state", output: strings.Replace(valid, "remote_clearance_state=unobservable", "remote_clearance_state=raw helper error", 1), want: "invalid remote_clearance_state"},
		{name: "journal readable", output: strings.Replace(valid, "remote_storage_journal_readable=true", "remote_storage_journal_readable=maybe", 1), want: "invalid remote_storage_journal_readable"},
		{name: "event kind", output: valid + "remote_storage_event=1788450000,other,sda\n", want: "invalid remote_storage_event kind"},
		{name: "event device", output: valid + "remote_storage_event=1788450000,transport,sda;bad\n", want: "invalid remote_storage_event device"},
	} {
		_, err := parseBackupArchiveWriterObservation("backup-1", testCase.output)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: parse error=%v, want substring %q", testCase.name, err, testCase.want)
		}
	}
	const redactionMarker = "parser-redaction-marker.example/synthetic-repository"
	_, err := parseBackupArchiveWriterObservation(
		"backup-1",
		strings.Replace(valid, "github_failure_journal_status=not-applicable", "github_failure_journal_status="+redactionMarker, 1),
	)
	if err == nil || strings.Contains(err.Error(), redactionMarker) {
		t.Fatalf("malformed GitHub failure summary was not safely redacted: %v", err)
	}
}

// Reproduces the September 9 boundary: one repository SSH update reset while
// prior updates and the other organization succeeded. A daily timer does not
// make that transfer resilient inside the current atomic archive invocation.
func TestBackupArchivesSignalSyntheticDetectsDisabledGitTransferRetry(t *testing.T) {
	now := time.Date(2026, 9, 9, 13, 30, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		githubGitTransferAttempts:     1,
		githubGitTransferRetrySeconds: 1,
	}, fixtures...)
	alert := requireBackupArchiveAlert(
		t,
		alerts,
		"backup-archive-git-transfer-retry-disabled",
		"backup-1/github",
	)
	if alert.Sustain != 1 || alert.Severity != SeverityPage {
		t.Fatalf("Git transfer retry urgency=%s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"git_transfer_attempts=1",
		"git_transfer_retry_seconds=1",
		"one transient SSH reset",
		"four attempts with 30 seconds",
		"provider-side connection reset and broken pipe",
		"Preserve existing mirror caches",
		"explicit operator authorization",
		"SIGNALS.md §11.22",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("disabled Git transfer retry alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticDetectsDisabledPullRetry(t *testing.T) {
	now := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:          "inactive",
		remoteUnitState:    "failed",
		remoteResult:       "exit-code",
		remoteRestart:      "no",
		remoteRestartDelay: "100ms",
		remoteExitStatus:   1,
	}, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-retry-disabled", "backup-1/remote")
	if alert.Sustain != 1 {
		t.Fatalf("retry policy sustain=%d, want 1", alert.Sustain)
	}
	for _, want := range []string{
		"result=exit-code",
		"exit_status=1",
		"restart=no",
		"restart_delay=100ms",
		"Xops commit 2311114",
		"cannot unlock LUKS",
		"does not authorize a catch-up pull",
		"run-planetoid.sh",
		"RestartUSec=30min",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("disabled retry alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

// A oneshot with Restart=on-failure enters ActiveState=activating while it
// waits in the auto-restart backoff. Treating every activating state as a live
// transfer hid the failed September 2 direct PostgreSQL pull: ExecStart had
// exited 1, both progress gauges were zero, and no rsync process existed.
func TestBackupArchivesSignalSyntheticDetectsFailedPullInRestartBackoff(t *testing.T) {
	now := time.Date(2026, 9, 2, 20, 49, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "activating",
		remoteUnitSubstate: "auto-restart",
		remoteResult:       "exit-code",
		remoteRestart:      "on-failure",
		remoteRestartDelay: "30min",
		remoteExitStatus:   1,
	}, fixtures...)

	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-run-failed", "backup-1/remote")
	for _, want := range []string{
		"unit_state=activating",
		"unit_substate=auto-restart",
		"main_pid=0",
		"result=exit-code",
		"exit_status=1",
		"restart=on-failure",
		"restart_delay=30min",
		"no archive writer is active during the restart backoff",
		"Preserve the rsync partial",
		"NetworkManager connectivity state",
		"independent public control",
		"Planetoid's router or upstream Internet path",
		"If independent Internet stayed healthy",
		"both source sshd journals",
		"no orderly source close",
		"either before authentication or after authentication without an orderly source close",
		"shared direct-path infrastructure",
		"source-observed public egress identity",
		"Planetoid WAN/NAT evidence",
		"does not distinguish Planetoid gateway policy from the Fremont public-forward edge",
		"carrier-private or ECMP hops",
		"upstream multi-egress NAT a candidate",
		"does not assign reset ownership",
		"authoritative RIR",
		"addresses owned by independent carriers",
		"one carrier and one source daemon are no longer the common fault domain",
		"offsite gateway/conntrack boundary or the destination public-forward gateway",
		"does not choose between those two routers",
		"IPv6 route/DNS reselection is not an IPv4-reset cause by itself",
		"isolated reselection while the same transfer survives is a negative control",
		"reselection bursts bracketing resets",
		"no link-carrier loss",
		"no whole-site Internet transition",
		"narrower router/WAN/NAT/RA lifecycle event",
		"not proof that NetworkManager reset IPv4",
		"paired UDM and destination-forward WAN-event/config/conntrack evidence",
		"carrier NAT/session evidence",
		"stable public/no-CGNAT egress",
		"never the management VPN",
		"router lifecycle/conntrack evidence",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("failed pull alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "sibling forward reset before authentication") {
		t.Fatalf("failed pull alert still requires the obsolete pre-auth-only discriminator:\n%s", alert.Markdown())
	}
}

func TestBackupArchivesSignalSyntheticActivePullHasNoFailureAlert(t *testing.T) {
	now := time.Date(2026, 9, 2, 20, 50, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "activating",
		remoteUnitSubstate: "start",
		remoteMainPID:      4201,
	}, fixtures...)

	if alert := findBackupArchiveAlert(alerts, "backup-archive-run-failed", "backup-1/remote"); alert != nil {
		t.Fatalf("live data pull was misclassified as a failed invocation: %+v", *alert)
	}
}

// A stale progress textfile must not turn an auto-restart backoff into an
// apparent serial queue. MainPID is the discriminator between an executing
// oneshot and systemd merely retaining ActiveState=activating for its timer.
func TestBackupArchivesSignalSyntheticRestartBackoffIsNotActiveQueue(t *testing.T) {
	now := time.Date(2026, 9, 2, 20, 51, 0, 0, time.UTC)
	zero := float64(0)
	one := float64(1)
	stale := now.Add(-13 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remoteUnitState:    "activating",
		remoteUnitSubstate: "auto-restart",
		remoteResult:       "exit-code",
		remoteExitStatus:   1,
	},
		backupArchiveFixture{archive: "pg", generation: "main-pg-old.sql.xz", createdAt: &stale, progress: &one},
		backupArchiveFixture{archive: "redis", generation: "main-redis-old.tar.gpg", createdAt: &stale, progress: &zero},
		backupArchiveFixture{archive: "github-urnetwork", generation: "main-code-urnetwork-current.tar.xz", createdAt: &fresh, progress: &zero},
		backupArchiveFixture{archive: "github-urfoundation", generation: "main-code-urfoundation-current.tar.xz", createdAt: &fresh, progress: &zero},
	)

	redis := requireBackupArchiveAlert(t, alerts, "backup-archive-stale", "backup-1/redis")
	if redis.Frame == "queued-behind=pg" || strings.Contains(redis.Action, "Preserve the active pg phase") {
		t.Fatalf("restart backoff was misclassified as an active PostgreSQL phase: %+v", redis)
	}
	requireBackupArchiveAlert(t, alerts, "backup-archive-run-failed", "backup-1/remote")
}

func TestBackupArchivesSignalSyntheticDetectsManagementVPNSourceRouting(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 45, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-24 * time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &zero,
		})
	}
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		remotePGSource:    "by@172.28.0.2",
		remotePGPort:      22,
		remoteRedisSource: "by@172.28.0.3",
		remoteRedisPort:   22,
	}, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-source-route", "backup-1/remote-sources")
	if alert.Sustain != 1 {
		t.Fatalf("source route sustain=%d, want 1", alert.Sustain)
	}
	for _, want := range []string{
		"pg_source=by@172.28.0.2 pg_port=22",
		"redis_source=by@172.28.0.3 redis_port=22",
		"PostgreSQL by@203.0.113.10:8022",
		"Redis by@203.0.113.11:8023",
		"hundreds of GiB",
		"management OpenVPN tunnel",
		"run-planetoid.sh",
		"Do not restart or interrupt",
		"does not select tun0",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("source route alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestBackupArchivesSignalSyntheticStaleScrapeIsObservationLoss(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 3, 0, 0, time.UTC)
	zero := float64(0)
	createdAt := now.Add(-time.Hour)
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for _, archive := range backupArchiveNames {
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt,
			progress: &zero, sampleAt: now.Add(-2 * time.Minute),
		})
	}
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	missing := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-missing", "backup-1/pg")
	if !strings.Contains(missing.Observed, "stale_scrape_samples=3") {
		t.Fatalf("stale samples were not kept as visibility evidence: %s", missing.Observed)
	}
	requireBackupArchiveAlert(t, alerts, "backup-archive-integrity-stale", "backup-1/pg")
	if findBackupArchiveAlert(alerts, "backup-archive-stale", "backup-1/pg") != nil {
		t.Fatalf("stale scrape was misclassified as stale archive: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticRejectsInvalidMetrics(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 4, 0, 0, time.UTC)
	invalidProgress := float64(2)
	future := now.Add(10 * time.Minute)
	alerts := runBackupArchiveFixtures(t, now,
		backupArchiveFixture{archive: "pg", generation: "future", createdAt: &future, progress: &invalidProgress},
	)
	invalid := requireBackupArchiveAlert(t, alerts, "backup-archive-metrics-invalid", "backup-1/pg")
	for _, want := range []string{"future_timestamp=", "value=2", "single-writer exposition"} {
		if !strings.Contains(invalid.Markdown(), want) {
			t.Fatalf("invalid metric alert missing %q:\n%s", want, invalid.Markdown())
		}
	}
}

func runBackupArchiveFixtures(t testing.TB, now time.Time, fixtures ...backupArchiveFixture) Alerts {
	return runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState: "inactive",
	}, fixtures...)
}

func healthyBackupArchiveFixtures(now time.Time) []backupArchiveFixture {
	fixtures := make([]backupArchiveFixture, 0, len(backupArchiveNames))
	for index, archive := range backupArchiveNames {
		createdAt := now.Add(-time.Duration(index+1) * time.Hour)
		progress := float64(0)
		fixtures = append(fixtures, backupArchiveFixture{
			archive: archive, generation: archive + "-complete", createdAt: &createdAt, progress: &progress,
		})
	}
	return fixtures
}

func runBackupArchiveFixturesWithWriter(
	t testing.TB,
	now time.Time,
	writer backupArchiveWriterFixture,
	fixtures ...backupArchiveFixture,
) Alerts {
	t.Helper()
	payload := backupArchiveFixtureJSON(t, now, fixtures...)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name == "backup-1" && strings.Contains(command, "monitor-signal-11.22-backup-archives") {
			return backupArchiveWriterFixtureText(writer), nil
		}
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_backup_archive_%28latest_timestamp_seconds%7Cin_progress%7Cheartbeat_timestamp_seconds%7Cintegrity_checked_timestamp_seconds%29") ||
			!strings.Contains(command, "host%3D~%22backup-1%22") ||
			!strings.Contains(command, "env%3D%22synthetic%22") {
			return "", fmt.Errorf("unexpected backup Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	for index := range settings.Hosts {
		switch settings.Hosts[index].Name {
		case "pg-1":
			settings.Hosts[index].OverlayAddress = "172.28.0.2"
		case "redis-1":
			settings.Hosts[index].OverlayAddress = "172.28.0.3"
		}
	}
	settings.Hosts = append(settings.Hosts,
		HostSettings{
			Name: "backup-1", Roles: []string{"backup"},
			Backup: &BackupHostSettings{
				PGSource: "by@203.0.113.10", PGPort: 8022,
				RedisSource: "by@203.0.113.11", RedisPort: 8023,
			},
		},
		HostSettings{Name: "metrics-1", Roles: []string{"services"}},
	)
	alerts, err := NewBackupArchivesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func backupArchiveWriterFixtureText(fixture backupArchiveWriterFixture) string {
	if fixture.unitState == "" {
		fixture.unitState = "inactive"
	}
	if fixture.unitSubstate == "" {
		fixture.unitSubstate = "dead"
	}
	if fixture.result == "" {
		fixture.result = "success"
	}
	if fixture.invocationID == "" {
		fixture.invocationID = "present"
		if fixture.execStart == 0 {
			fixture.execStart = 1
		}
	}
	if fixture.timerState == "" {
		fixture.timerState = "active"
	}
	if fixture.timerUnitFileState == "" {
		fixture.timerUnitFileState = "enabled"
	}
	if fixture.timerNext == nil {
		timerNext := int64(2_000_000_000)
		fixture.timerNext = &timerNext
	}
	if fixture.githubGitTransferAttempts == 0 {
		fixture.githubGitTransferAttempts = 4
	}
	if fixture.githubGitTransferRetrySeconds == 0 {
		fixture.githubGitTransferRetrySeconds = 30
	}
	if fixture.githubFailureStatus == "" {
		if backupArchiveGitHubWriterFailed(fixture.unitState, fixture.result, fixture.exitStatus) {
			fixture.githubFailureStatus = "unobservable"
		} else {
			fixture.githubFailureStatus = "not-applicable"
		}
	}
	if fixture.githubFirstFailureBoundary == "" {
		fixture.githubFirstFailureBoundary = "none"
	}
	if fixture.githubFailureBoundaryLines == nil {
		fixture.githubFailureBoundaryLines = map[string]int64{}
	}
	if fixture.archiveRootObservation == "" {
		fixture.archiveRootObservation = "observable"
	}
	if fixture.archiveRootObservation == "unobservable" {
		if fixture.githubArchivePathState == "" {
			fixture.githubArchivePathState = "unknown"
		}
		if fixture.remoteArchivePathState == "" {
			fixture.remoteArchivePathState = "unknown"
		}
		if fixture.archivePathsMatch == nil {
			fixture.archivePathsMatch = boolPointer(false)
		}
		if fixture.archiveMountsMatch == nil {
			fixture.archiveMountsMatch = boolPointer(false)
		}
		if fixture.archivePathsOnMount == nil {
			fixture.archivePathsOnMount = boolPointer(false)
		}
		if fixture.archivePathPermissionsSecure == nil {
			fixture.archivePathPermissionsSecure = boolPointer(false)
		}
	} else {
		if fixture.githubArchivePathState == "" {
			fixture.githubArchivePathState = "directory"
		}
		if fixture.remoteArchivePathState == "" {
			fixture.remoteArchivePathState = "directory"
		}
		if fixture.archivePathsMatch == nil {
			fixture.archivePathsMatch = boolPointer(true)
		}
		if fixture.archiveMountsMatch == nil {
			fixture.archiveMountsMatch = boolPointer(true)
		}
		if fixture.archivePathsOnMount == nil {
			fixture.archivePathsOnMount = boolPointer(true)
		}
		if fixture.archivePathPermissionsSecure == nil {
			fixture.archivePathPermissionsSecure = boolPointer(true)
		}
	}
	if fixture.remoteUnitState == "" {
		fixture.remoteUnitState = "inactive"
	}
	if fixture.remoteUnitSubstate == "" {
		fixture.remoteUnitSubstate = "dead"
	}
	if fixture.remoteResult == "" {
		fixture.remoteResult = "success"
	}
	if fixture.remoteRestart == "" {
		fixture.remoteRestart = "on-failure"
	}
	if fixture.remoteRestartDelay == "" {
		fixture.remoteRestartDelay = "30min"
	}
	if fixture.remoteInvocationID == "" {
		fixture.remoteInvocationID = "present"
		if fixture.remoteExecStart == 0 {
			fixture.remoteExecStart = 1
		}
	}
	if fixture.remoteTimerState == "" {
		fixture.remoteTimerState = "active"
	}
	if fixture.remoteTimerUnitFileState == "" {
		fixture.remoteTimerUnitFileState = "enabled"
	}
	if fixture.remoteTimerNext == 0 {
		fixture.remoteTimerNext = 2_000_000_000
	}
	if fixture.remoteBoot == 0 {
		fixture.remoteBoot = 1_700_000_000
	}
	if fixture.remotePGSource == "" {
		fixture.remotePGSource = "by@203.0.113.10"
	}
	if fixture.remotePGPort == 0 {
		fixture.remotePGPort = 8022
	}
	if fixture.remoteRedisSource == "" {
		fixture.remoteRedisSource = "by@203.0.113.11"
	}
	if fixture.remoteRedisPort == 0 {
		fixture.remoteRedisPort = 8023
	}
	if fixture.remoteMount == "" {
		fixture.remoteMount = "/run/media/by/archive1"
	}
	if fixture.remoteMountPresent == nil {
		fixture.remoteMountPresent = boolPointer(true)
	}
	if fixture.remoteMountSource == "" {
		fixture.remoteMountSource = "/dev/mapper/luks-synthetic"
	}
	if fixture.remoteMountFSType == "" {
		fixture.remoteMountFSType = "ext4"
	}
	if fixture.remoteMountOptions == "" {
		fixture.remoteMountOptions = "rw,nosuid,nodev,relatime,errors=remount-ro"
	}
	if fixture.remoteMountLineage == "" {
		fixture.remoteMountLineage = "dm-2,sda1,sda"
	}
	if fixture.clearanceState == "" {
		fixture.clearanceState = "unobservable"
	}
	if fixture.storageReadable == nil {
		fixture.storageReadable = boolPointer(true)
	}
	output := fmt.Sprintf(
		"github_unit_state=%s\n"+
			"github_unit_substate=%s\n"+
			"github_main_pid=%d\n"+
			"github_result=%s\n"+
			"github_exit_status=%d\n"+
			"github_invocation_id=%s\n"+
			"github_exec_start_monotonic=%d\n"+
			"github_exec_start_epoch=%d\n"+
			"github_timer_state=%s\n"+
			"github_timer_unit_file_state=%s\n"+
			"github_timer_next_epoch=%d\n"+
			"github_timer_last_epoch=%d\n"+
			"github_git_transfer_attempts=%d\n"+
			"github_git_transfer_retry_seconds=%d\n"+
			"archive_root_observation=%s\n"+
			"github_archive_path_state=%s\n"+
			"remote_archive_path_state=%s\n"+
			"archive_paths_match=%t\n"+
			"archive_mounts_match=%t\n"+
			"archive_paths_on_mount=%t\n"+
			"archive_path_permissions_secure=%t\n"+
			"remote_unit_state=%s\n"+
			"remote_unit_substate=%s\n"+
			"remote_main_pid=%d\n"+
			"remote_result=%s\n"+
			"remote_restart=%s\n"+
			"remote_restart_delay=%s\n"+
			"remote_exit_status=%d\n"+
			"remote_invocation_id=%s\n"+
			"remote_exec_start_monotonic=%d\n"+
			"remote_exec_start_epoch=%d\n"+
			"remote_timer_state=%s\n"+
			"remote_timer_unit_file_state=%s\n"+
			"remote_timer_next_epoch=%d\n"+
			"remote_timer_last_epoch=%d\n"+
			"remote_boot_epoch=%d\n"+
			"remote_pg_source=%s\n"+
			"remote_pg_port=%d\n"+
			"remote_redis_source=%s\n"+
			"remote_redis_port=%d\n"+
			"remote_mount=%s\n"+
			"remote_mount_present=%t\n"+
			"remote_mount_source=%s\n"+
			"remote_mount_fstype=%s\n"+
			"remote_mount_options=%s\n"+
			"remote_mount_lineage=%s\n"+
			"remote_clearance_state=%s\n"+
			"remote_storage_journal_readable=%t\n",
		fixture.unitState,
		fixture.unitSubstate,
		fixture.mainPID,
		fixture.result,
		fixture.exitStatus,
		fixture.invocationID,
		fixture.execStart,
		fixture.execStartEpoch,
		fixture.timerState,
		fixture.timerUnitFileState,
		*fixture.timerNext,
		fixture.timerLast,
		fixture.githubGitTransferAttempts,
		fixture.githubGitTransferRetrySeconds,
		fixture.archiveRootObservation,
		fixture.githubArchivePathState,
		fixture.remoteArchivePathState,
		*fixture.archivePathsMatch,
		*fixture.archiveMountsMatch,
		*fixture.archivePathsOnMount,
		*fixture.archivePathPermissionsSecure,
		fixture.remoteUnitState,
		fixture.remoteUnitSubstate,
		fixture.remoteMainPID,
		fixture.remoteResult,
		fixture.remoteRestart,
		fixture.remoteRestartDelay,
		fixture.remoteExitStatus,
		fixture.remoteInvocationID,
		fixture.remoteExecStart,
		fixture.remoteExecStartEpoch,
		fixture.remoteTimerState,
		fixture.remoteTimerUnitFileState,
		fixture.remoteTimerNext,
		fixture.remoteTimerLast,
		fixture.remoteBoot,
		fixture.remotePGSource,
		fixture.remotePGPort,
		fixture.remoteRedisSource,
		fixture.remoteRedisPort,
		fixture.remoteMount,
		*fixture.remoteMountPresent,
		fixture.remoteMountSource,
		fixture.remoteMountFSType,
		fixture.remoteMountOptions,
		fixture.remoteMountLineage,
		fixture.clearanceState,
		*fixture.storageReadable,
	)
	output += fmt.Sprintf(
		"github_failure_journal_status=%s\n"+
			"github_failure_first_boundary=%s\n"+
			"github_failure_journal_lines=%d\n",
		fixture.githubFailureStatus,
		fixture.githubFirstFailureBoundary,
		fixture.githubFailureJournalLines,
	)
	for _, boundaryField := range backupArchiveGitHubFailureBoundaryFields {
		output += fmt.Sprintf(
			"%s=%d\n",
			boundaryField.field,
			fixture.githubFailureBoundaryLines[boundaryField.boundary],
		)
	}
	for _, event := range fixture.storageEvents {
		output += fmt.Sprintf("remote_storage_event=%d,%s,%s\n", event.epoch, event.kind, event.device)
	}
	return output
}

func boolPointer(value bool) *bool { return &value }

func backupArchiveFixtureJSON(t testing.TB, now time.Time, fixtures ...backupArchiveFixture) string {
	t.Helper()
	result := []map[string]any{}
	for _, fixture := range fixtures {
		sampleAt := fixture.sampleAt
		if sampleAt.IsZero() {
			sampleAt = now
		}
		baseLabels := map[string]string{
			"env": "synthetic", "host": "backup-1", "archive": fixture.archive,
		}
		if fixture.createdAt != nil {
			metric := map[string]string{"__name__": "urnetwork_backup_archive_latest_timestamp_seconds"}
			for key, value := range baseLabels {
				metric[key] = value
			}
			metric["generation"] = fixture.generation
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleAt.Unix()), fmt.Sprintf("%d", fixture.createdAt.Unix())},
			})
		}
		if fixture.progress != nil {
			metric := map[string]string{"__name__": "urnetwork_backup_archive_in_progress"}
			for key, value := range baseLabels {
				metric[key] = value
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleAt.Unix()), fmt.Sprintf("%.0f", *fixture.progress)},
			})
		}
		if fixture.heartbeat != nil {
			metric := map[string]string{"__name__": "urnetwork_backup_archive_heartbeat_timestamp_seconds"}
			for key, value := range baseLabels {
				metric[key] = value
			}
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(sampleAt.Unix()), fmt.Sprintf("%d", fixture.heartbeat.Unix())},
			})
		}
		integrityResult := fixture.integrityResult
		if !fixture.omitIntegrity && integrityResult == "" {
			switch {
			case fixture.createdAt != nil:
				integrityResult = "verified"
			case fixture.progress != nil:
				integrityResult = "missing"
			}
		}
		if !fixture.omitIntegrity && integrityResult != "" {
			integrityFormat := fixture.integrityFormat
			if integrityFormat == "" {
				integrityFormat = defaultBackupArchiveIntegrityFormat(fixture.archive, integrityResult)
			}
			integrityGeneration := fixture.integrityGeneration
			if integrityGeneration == "" {
				integrityGeneration = fixture.generation
				if integrityResult == "missing" {
					integrityGeneration = "none"
				}
			}
			checkedAt := sampleAt
			if fixture.integrityCheckedAt != nil {
				checkedAt = *fixture.integrityCheckedAt
			}
			integritySampleAt := sampleAt
			if !fixture.integritySampleAt.IsZero() {
				integritySampleAt = fixture.integritySampleAt
			}
			metric := map[string]string{
				"__name__": "urnetwork_backup_archive_integrity_checked_timestamp_seconds",
				"format":   integrityFormat,
				"result":   integrityResult,
			}
			for key, value := range baseLabels {
				metric[key] = value
			}
			metric["generation"] = integrityGeneration
			result = append(result, map[string]any{
				"metric": metric,
				"value":  []any{float64(integritySampleAt.Unix()), fmt.Sprintf("%d", checkedAt.Unix())},
			})
		}
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func defaultBackupArchiveIntegrityFormat(archive, result string) string {
	if result == "missing" {
		return "none"
	}
	if result == "legacy-unverified" {
		switch archive {
		case "pg":
			return "pg-gpg-md5-legacy"
		case "redis":
			return "redis-pairs-md5-legacy"
		case "github-urnetwork", "github-urfoundation":
			return "github-tar-xz-legacy"
		}
	}
	switch archive {
	case "pg":
		return "pg-gpg-sha256"
	case "redis":
		return "redis-bundle-sha256"
	case "github-urnetwork", "github-urfoundation":
		return "github-tar-xz-sha256"
	}
	return "unknown"
}

func requireBackupArchiveAlert(t testing.TB, alerts Alerts, class, target string) Alert {
	t.Helper()
	alert := findBackupArchiveAlert(alerts, class, target)
	if alert == nil {
		t.Fatalf("no %s alert for %s in %+v", class, target, alerts)
	}
	return *alert
}

func findBackupArchiveAlert(alerts Alerts, class, target string) *Alert {
	for index := range alerts {
		if alerts[index].Class == class && alerts[index].Target == target {
			return &alerts[index]
		}
	}
	return nil
}
