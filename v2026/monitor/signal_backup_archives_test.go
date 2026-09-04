package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type backupArchiveFixture struct {
	archive    string
	generation string
	createdAt  *time.Time
	progress   *float64
	heartbeat  *time.Time
	sampleAt   time.Time
}

type backupArchiveWriterFixture struct {
	unitState          string
	mainPID            int64
	remoteUnitState    string
	remoteUnitSubstate string
	remoteMainPID      int64
	remoteResult       string
	remoteRestart      string
	remoteRestartDelay string
	remoteExitStatus   int64
	remoteInvocationID string
	remoteExecStart    int64
	remoteTimerState   string
	remoteTimerNext    int64
	remoteBoot         int64
	remotePGSource     string
	remotePGPort       int64
	remoteRedisSource  string
	remoteRedisPort    int64
	remoteMount        string
	remoteMountPresent *bool
	remoteMountSource  string
	remoteMountFSType  string
	remoteMountOptions string
	remoteMountLineage string
	clearanceState     string
	storageReadable    *bool
	storageEvents      []backupArchiveStorageEventFixture
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

func TestBackupArchivesSignalSyntheticQuotedCollectorOmission(t *testing.T) {
	now := time.Date(2026, 9, 1, 18, 1, 0, 0, time.UTC)
	alerts := runBackupArchiveFixtures(t, now)
	if len(alerts) != len(backupArchiveNames)*2 {
		t.Fatalf("collector omission alerts=%d, want %d: %+v", len(alerts), len(backupArchiveNames)*2, alerts)
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
		!strings.Contains(missing.Markdown(), "no observable completed archive generation") ||
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
		remoteInvocationID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
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
	requireBackupArchiveAlert(t, alerts, "backup-archive-volume-recovery-unverified", "backup-1/archive-volume")
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
		remoteInvocationID: "cccccccccccccccccccccccccccccccc",
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
		{name: "missing", output: "github_unit_state=activating", want: "expected 26 properties"},
		{name: "state", output: strings.Replace(valid, "github_unit_state=inactive", "github_unit_state=ACTIVE", 1), want: "invalid github_unit_state"},
		{name: "pid", output: strings.Replace(valid, "github_main_pid=0", "github_main_pid=nope", 1), want: "invalid main PID"},
		{name: "delay", output: strings.Replace(valid, "remote_restart_delay=30min", "remote_restart_delay=immediate!", 1), want: "invalid remote_restart_delay"},
		{name: "invocation", output: strings.Replace(valid, "remote_invocation_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "remote_invocation_id=not-a-uuid", 1), want: "invalid remote_invocation_id"},
		{name: "timer epoch", output: strings.Replace(valid, "remote_timer_next_epoch=2000000000", "remote_timer_next_epoch=tomorrow", 1), want: "invalid remote_timer_next_epoch"},
		{name: "mount present", output: strings.Replace(valid, "remote_mount_present=true", "remote_mount_present=maybe", 1), want: "invalid remote_mount_present"},
		{name: "mount options", output: strings.Replace(valid, "remote_mount_options=rw,nosuid,nodev,relatime,errors=remount-ro", "remote_mount_options=rw secret", 1), want: "invalid remote_mount_options"},
		{name: "mount lineage", output: strings.Replace(valid, "remote_mount_lineage=dm-2,sda1,sda", "remote_mount_lineage=dm-2,sda1,sda;bad", 1), want: "invalid remote_mount_lineage"},
		{name: "clearance state", output: strings.Replace(valid, "remote_clearance_state=unobservable", "remote_clearance_state=raw helper error", 1), want: "invalid remote_clearance_state"},
		{name: "journal readable", output: strings.Replace(valid, "remote_storage_journal_readable=true", "remote_storage_journal_readable=maybe", 1), want: "invalid remote_storage_journal_readable"},
		{name: "event kind", output: valid + "remote_storage_event=1788450000,other,sda\n", want: "invalid remote_storage_event kind"},
		{name: "event device", output: valid + "remote_storage_event=1788450000,transport,sda;bad\n", want: "invalid remote_storage_event device"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseBackupArchiveWriterObservation("backup-1", testCase.output)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("parse error=%v, want substring %q", err, testCase.want)
			}
		})
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
	if !strings.Contains(missing.Observed, "stale_scrape_samples=2") {
		t.Fatalf("stale samples were not kept as visibility evidence: %s", missing.Observed)
	}
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
			!strings.Contains(command, "urnetwork_backup_archive_%28latest_timestamp_seconds%7Cin_progress%7Cheartbeat_timestamp_seconds%29") ||
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
		fixture.remoteInvocationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if fixture.remoteExecStart == 0 {
			fixture.remoteExecStart = 1
		}
	}
	if fixture.remoteTimerState == "" {
		fixture.remoteTimerState = "active"
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
			"github_main_pid=%d\n"+
			"remote_unit_state=%s\n"+
			"remote_unit_substate=%s\n"+
			"remote_main_pid=%d\n"+
			"remote_result=%s\n"+
			"remote_restart=%s\n"+
			"remote_restart_delay=%s\n"+
			"remote_exit_status=%d\n"+
			"remote_invocation_id=%s\n"+
			"remote_exec_start_monotonic=%d\n"+
			"remote_timer_state=%s\n"+
			"remote_timer_next_epoch=%d\n"+
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
		fixture.mainPID,
		fixture.remoteUnitState,
		fixture.remoteUnitSubstate,
		fixture.remoteMainPID,
		fixture.remoteResult,
		fixture.remoteRestart,
		fixture.remoteRestartDelay,
		fixture.remoteExitStatus,
		fixture.remoteInvocationID,
		fixture.remoteExecStart,
		fixture.remoteTimerState,
		fixture.remoteTimerNext,
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
