package monitor

// SIGNALS.md §29.2 on fixture admin info: a set short of its write quorum and a cluster showing other than one
// pool refuse writes, the first cluster's page says every cluster's writes stop, and an unread cluster is blind.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func checkFileClusterWrites(t *testing.T, source *fakeFileClusterAdminSource) []finding {
	t.Helper()
	findings, err := fileClusterWritesProbe{}.check(t.Context(), sidecarStorageTestEnv(sidecarStorageTestSettings(source, nil), func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// One host down leaves the 8-drive set 6 of 8, over its quorum of 5; a host and a drive leave it 5, still
// writing; two hosts leave it 4 and refuse. The 16-drive set needs 9.
func TestFileClusterWritesRefusedBelowTheWriteQuorum(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	meta := source.info("meta-1")
	setFileClusterTestHost(meta, 0, "offline")
	requireSidecarStorageRaised(t, checkFileClusterWrites(t, source))
	setFileClusterTestDrive(meta, 2, "offline", false)
	requireSidecarStorageRaised(t, checkFileClusterWrites(t, source))
	// a rebuilding drive still takes writes
	setFileClusterTestDrive(meta, 4, "ok", true)
	requireSidecarStorageRaised(t, checkFileClusterWrites(t, source))
	setFileClusterTestDrive(meta, 6, "faulty", false)
	findings := checkFileClusterWrites(t, source)
	requireSidecarStorageRaised(t, findings, "filecluster-refusing-writes meta-1")
	for _, finding := range findings {
		if finding.class == fileClusterWritesClassRefusing && !finding.healthy {
			if finding.tier != tierPage || !strings.Contains(finding.observed, "4 of 8 online, quorum 5") || !strings.Contains(finding.symptom, "every cluster's writes stop") {
				t.Errorf("finding %+v", finding)
			}
		}
	}

	segment := source.info("seg-1")
	for _, hostIndex := range []int{0, 1} {
		setFileClusterTestHost(segment, hostIndex, "offline")
	}
	requireSidecarStorageRaised(t, checkFileClusterWrites(t, source), "filecluster-refusing-writes meta-1", "filecluster-refusing-writes seg-1")
}

// A second pool refuses every write and warns; no pool counted refuses too.
func TestFileClusterWritesRefusedOnOtherThanOnePool(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	source.info("seg-1").PoolCount = 2
	findings := checkFileClusterWrites(t, source)
	requireSidecarStorageRaised(t, findings, "filecluster-multi-pool seg-1", "filecluster-refusing-writes seg-1")
	for _, finding := range findings {
		if finding.class == fileClusterWritesClassRefusing && !finding.healthy && strings.Contains(finding.symptom, "every cluster") {
			t.Errorf("a cluster other than the first claims every cluster's writes: %+v", finding)
		}
	}
	source.info("seg-1").PoolCount = 0
	requireSidecarStorageRaised(t, checkFileClusterWrites(t, source), "filecluster-refusing-writes seg-1")
}

// A cluster whose admin info does not read is blind; the other is still judged.
func TestFileClusterWritesUnreadAdminInfoIsBlind(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	source.errs["seg-1"] = errors.New("connection refused")
	findings := checkFileClusterWrites(t, source)
	requireSidecarStorageRaised(t, findings, "filecluster-writes-unobservable seg-1")
	healthy := 0
	for _, finding := range findings {
		if finding.healthy && finding.target == "meta-1" {
			healthy += 1
		}
		if !finding.healthy && !finding.isUnobservable() {
			t.Errorf("finding %+v", finding)
		}
	}
	if healthy != 2 {
		t.Errorf("meta-1 judged %d times", healthy)
	}
}
