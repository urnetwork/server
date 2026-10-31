package monitor

import (
	"strings"
	"testing"
	"time"
)

// A physical PostgreSQL generation is one directory of encrypted base-backup
// pieces and WAL chunks whose SHA-256 manifest Planetoid validates in full.
// Its integrity report uses the closed pg-basebackup-sha256 format.
const backupArchivePhysicalPGGeneration = "main-pgbase-2026-10-31-04-00-00"

func TestBackupArchiveIntegrityContractAcceptsPhysicalPGFormat(t *testing.T) {
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	checkedAt := float64(now.Add(-time.Hour).Unix())
	tests := []struct {
		archive string
		format  string
		result  string
		accept  bool
	}{
		{archive: "pg", format: "pg-basebackup-sha256", result: "verified", accept: true},
		{archive: "pg", format: "pg-basebackup-sha256", result: "invalid", accept: true},
		// The physical format has no legacy or missing form: an unverifiable
		// generation is invalid, and an absent one is format=none.
		{archive: "pg", format: "pg-basebackup-sha256", result: "legacy-unverified"},
		{archive: "pg", format: "pg-basebackup-sha256", result: "missing"},
		// It describes only PostgreSQL recovery points.
		{archive: "redis", format: "pg-basebackup-sha256", result: "verified"},
		{archive: "github-urnetwork", format: "pg-basebackup-sha256", result: "verified"},
		{archive: "github-urfoundation", format: "pg-basebackup-sha256", result: "verified"},
		// The logical formats keep their existing contract.
		{archive: "pg", format: "pg-gpg-sha256", result: "verified", accept: true},
		{archive: "pg", format: "pg-gpg-md5-legacy", result: "legacy-unverified", accept: true},
	}
	for _, test := range tests {
		labels := map[string]string{
			"format":     test.format,
			"result":     test.result,
			"generation": backupArchivePhysicalPGGeneration,
		}
		sample, err := parseBackupArchiveIntegritySample(test.archive, labels, checkedAt, now)
		if test.accept {
			if err != nil {
				t.Fatalf("%s %s/%s rejected: %v", test.archive, test.format, test.result, err)
			}
			if sample.format != test.format || sample.result != test.result ||
				sample.generation != backupArchivePhysicalPGGeneration {
				t.Fatalf("%s %s/%s parsed as %+v", test.archive, test.format, test.result, sample)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s %s/%s accepted outside the closed contract: %+v", test.archive, test.format, test.result, sample)
		}
	}
}

func TestBackupArchivesSignalSyntheticPhysicalPGGenerationIsHealthy(t *testing.T) {
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	fixtures[0].generation = backupArchivePhysicalPGGeneration
	fixtures[0].integrityFormat = "pg-basebackup-sha256"
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	if len(alerts) != 0 {
		t.Fatalf("verified physical pg generation alerted: %+v", alerts)
	}
}

// The first physical generation lands beside the logical dumps. During the
// Mimir staleness overlap both structural rows and both integrity reports are
// fresh; the newer physical generation and its newer check must be selected
// and joined without a mismatch.
func TestBackupArchivesSignalSyntheticPhysicalPGReplacesLogicalDuringOverlap(t *testing.T) {
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	logicalCreated := now.Add(-3 * time.Hour)
	logicalChecked := now.Add(-2 * time.Hour)
	fixtures[0].generation = "main-pg-2026-11-03-15-00-00.sql.zst"
	fixtures[0].createdAt = &logicalCreated
	fixtures[0].integrityCheckedAt = &logicalChecked
	physicalCreated := now.Add(-time.Hour)
	physicalChecked := now.Add(-10 * time.Minute)
	fixtures = append(fixtures, backupArchiveFixture{
		archive:            "pg",
		generation:         backupArchivePhysicalPGGeneration,
		createdAt:          &physicalCreated,
		integrityFormat:    "pg-basebackup-sha256",
		integrityCheckedAt: &physicalChecked,
	})
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	if len(alerts) != 0 {
		t.Fatalf("logical to physical overlap alerted: %+v", alerts)
	}
}

func TestBackupArchivesSignalSyntheticPhysicalPGInvalidIntegrityPages(t *testing.T) {
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	fixtures := healthyBackupArchiveFixtures(now)
	fixtures[0].generation = backupArchivePhysicalPGGeneration
	fixtures[0].integrityFormat = "pg-basebackup-sha256"
	fixtures[0].integrityResult = "invalid"
	alerts := runBackupArchiveFixtures(t, now, fixtures...)
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-integrity-invalid", "backup-1/pg")
	if alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatalf("physical integrity urgency=%s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"format=pg-basebackup-sha256",
		"result=invalid",
		"generation=" + backupArchivePhysicalPGGeneration,
		"every base-backup piece and WAL chunk",
		"not a failed decryption",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("physical integrity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if unexpected := findBackupArchiveAlert(alerts, "backup-archive-integrity-unobservable", "backup-1/pg"); unexpected != nil {
		t.Fatalf("physical invalid report was reduced to unobservable: %+v", *unexpected)
	}
}

// Outside PostgreSQL, or with a result the physical format does not define,
// the report stays outside the closed contract and is unobservable.
func TestBackupArchivesSignalSyntheticPhysicalFormatStaysClosed(t *testing.T) {
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		index  int
		target string
		modify func(*backupArchiveFixture)
	}{
		{
			name: "redis", index: 1, target: "backup-1/redis",
			modify: func(fixture *backupArchiveFixture) {
				fixture.integrityFormat = "pg-basebackup-sha256"
			},
		},
		{
			name: "legacy result", index: 0, target: "backup-1/pg",
			modify: func(fixture *backupArchiveFixture) {
				fixture.generation = backupArchivePhysicalPGGeneration
				fixture.integrityFormat = "pg-basebackup-sha256"
				fixture.integrityResult = "legacy-unverified"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixtures := healthyBackupArchiveFixtures(now)
			if "backup-1/"+fixtures[test.index].archive != test.target {
				t.Fatalf("fixture %d is %s, not %s", test.index, fixtures[test.index].archive, test.target)
			}
			test.modify(&fixtures[test.index])
			alerts := runBackupArchiveFixtures(t, now, fixtures...)
			alert := requireBackupArchiveAlert(t, alerts, "backup-archive-integrity-unobservable", test.target)
			if !strings.Contains(alert.Markdown(), "labels are outside the bounded archive integrity contract") {
				t.Fatalf("closed-contract rejection missing:\n%s", alert.Markdown())
			}
		})
	}
}

// Planetoid's physical generation pull (pgbase-archive.service) is a third
// archive writer. A pull that runs while the mounted archive lineage has an
// unverified fault and no valid clearance is the same unsafe concurrency as
// the data and GitHub writers.
func TestBackupArchivesSignalSyntheticActivePhysicalPullDuringUnverifiedRecovery(t *testing.T) {
	now := time.Date(2026, 11, 3, 13, 20, 0, 0, time.UTC)
	alerts := runBackupArchiveFixturesWithWriter(t, now, backupArchiveWriterFixture{
		unitState:          "inactive",
		remoteUnitState:    "inactive",
		remoteUnitSubstate: "dead",
		pgbaseUnitState:    "activating",
		pgbaseMainPID:      187654,
		clearanceState:     "missing",
		storageEvents: []backupArchiveStorageEventFixture{
			{epoch: now.Add(-time.Hour).Unix(), kind: "block-io", device: "sda"},
		},
	})
	alert := requireBackupArchiveAlert(t, alerts, "backup-archive-writer-active-during-recovery", "backup-1/archive-volume")
	if alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatalf("unsafe physical pull urgency = %s/%d, want page/1", alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"active_writers=physical-pg",
		"pgbase_unit_state=activating",
		"pgbase_main_pid_present=true",
		"clearance_state=missing",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("unsafe physical pull alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "187654") {
		t.Fatalf("unsafe physical pull alert leaked its raw PID:\n%s", alert.Markdown())
	}
}

// With a valid clearance, or with the pull stopped, the physical writer is
// not unsafe; an idle pull reports inactive with no PID.
func TestBackupArchivesSignalSyntheticPhysicalPullSafeStates(t *testing.T) {
	now := time.Date(2026, 11, 3, 13, 20, 0, 0, time.UTC)
	for _, fixture := range []backupArchiveWriterFixture{
		{remoteUnitState: "inactive", remoteUnitSubstate: "dead", pgbaseUnitState: "activating", pgbaseMainPID: 187654, clearanceState: "valid"},
		{remoteUnitState: "inactive", remoteUnitSubstate: "dead", pgbaseUnitState: "failed", clearanceState: "missing"},
		{remoteUnitState: "inactive", remoteUnitSubstate: "dead", pgbaseUnitState: "activating", clearanceState: "missing"},
	} {
		fixture.storageEvents = []backupArchiveStorageEventFixture{
			{epoch: now.Add(-time.Hour).Unix(), kind: "block-io", device: "sda"},
		}
		alerts := runBackupArchiveFixturesWithWriter(t, now, fixture)
		if alert := findBackupArchiveAlert(alerts, "backup-archive-writer-active-during-recovery", "backup-1/archive-volume"); alert != nil {
			t.Fatalf("%s/%d with clearance %s was marked unsafe: %+v",
				fixture.pgbaseUnitState, fixture.pgbaseMainPID, fixture.clearanceState, *alert)
		}
	}
}

func TestBackupArchivesWriterObservationRejectsMalformedPhysicalPullState(t *testing.T) {
	valid := backupArchiveWriterFixtureText(backupArchiveWriterFixture{})
	if _, err := parseBackupArchiveWriterObservation("backup-1", valid); err != nil {
		t.Fatalf("valid writer observation rejected: %v", err)
	}
	for _, replacement := range []string{
		"pgbase_unit_state=ACTIVE\n",
		"pgbase_unit_state=active;rm\n",
	} {
		output := strings.Replace(valid, "pgbase_unit_state=inactive\n", replacement, 1)
		if _, err := parseBackupArchiveWriterObservation("backup-1", output); err == nil {
			t.Fatalf("malformed physical pull state %q accepted", strings.TrimSpace(replacement))
		}
	}
	output := strings.Replace(valid, "pgbase_main_pid=0\n", "pgbase_main_pid=-4\n", 1)
	if _, err := parseBackupArchiveWriterObservation("backup-1", output); err == nil {
		t.Fatal("negative physical pull PID accepted")
	}
	output = strings.Replace(valid, "pgbase_main_pid=0\n", "", 1)
	if _, err := parseBackupArchiveWriterObservation("backup-1", output); err == nil {
		t.Fatal("writer observation without the physical pull PID accepted")
	}
}
