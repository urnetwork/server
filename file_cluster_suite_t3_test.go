//go:build sidecar_filecluster_t3

// §22.9's file-cluster suite on T3 (EVSB-M2-17; FS-114, FS-63): every case on the clusters local/file-cluster-t3
// starts, the minio fork's settle_testhooks build of one commit, with blobsuite's store cases, as
// server/cli/filecluster-suite runs them. It needs those containers up (local/file-cluster-t3/t3.sh build, then up),
// carries the sidecar_filecluster_t3 tag, and fails, rather than skips, when they are down. It runs nightly and at
// Gn-out, never in the whole-suite scripts: its FS-114 cases hold drive commits past the clusters' drive timeout in
// real time.

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest/blobsuite"
)

// blobsuite's cases as the suite's, as filecluster-suite's main adds them.
func fileClusterSuiteT3StoreCases(ctx context.Context, target *FileClusterSuiteStoreTarget) []*FileClusterSuiteCase {
	cases := []*FileClusterSuiteCase{}
	for _, result := range blobsuite.RunCases(ctx, &blobsuite.Target{
		Cluster:        target.Cluster,
		Now:            target.Now,
		ClockTolerance: 2 * time.Second,
		Settle:         &blobsuite.Settle{Now: target.Now, Delayed: target.Delayed},
	}) {
		c := &FileClusterSuiteCase{Name: "store/" + result.Name, Outcome: FileClusterSuitePass, Elapsed: result.Elapsed}
		switch {
		case result.Waits:
			c.Outcome = FileClusterSuiteNotRun
		case result.Err != nil:
			c.Outcome = FileClusterSuiteFail
			c.Detail = result.Err.Error()
		}
		cases = append(cases, c)
	}
	return cases
}

// T3's clusters, by id, as the suite reaches them.
func fileClusterSuiteT3Targets(t *testing.T) map[string]*FileClusterSuiteTarget {
	t.Helper()
	dir := filepath.Join("local", "file-cluster-t3")
	clustersText, err := os.ReadFile(filepath.Join(dir, "clusters.yml"))
	if err != nil {
		t.Fatal(err)
	}
	configs, err := ParseFileClusterConfigs(clustersText)
	if err != nil {
		t.Fatal(err)
	}
	hostsText, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	hostsFile, err := ParseFileClusterSuiteHostsFile(hostsText)
	if err != nil {
		t.Fatal(err)
	}
	driveTimeout := os.Getenv("FILECLUSTER_T3_DRIVE_TIMEOUT")
	targets := map[string]*FileClusterSuiteTarget{}
	for _, config := range configs {
		entry := hostsFile.Clusters[config.ClusterId]
		if driveTimeout != "" {
			entry.DriveTimeout = driveTimeout
		}
		target, err := NewFileClusterSuiteTarget(config, entry, nil)
		if err != nil {
			t.Fatal(err)
		}
		target.StoreCases = fileClusterSuiteT3StoreCases
		targets[config.ClusterId] = target
	}
	return targets
}

// FS-114's head and marker cases and its twin, the conditional DELETE's three store cases, a LIST whole or 503, and
// stale conditions with a host lost, on the first metadata cluster (4 x 2 at EC:4) and the segment cluster (4 x 4 at
// EC:8): every case passes and the report names the build every host runs.
func TestFileClusterSuitePassesOnT3(t *testing.T) {
	targets := fileClusterSuiteT3Targets(t)
	for _, clusterId := range []string{"meta-1", "seg-1"} {
		report, err := RunFileClusterSuite(t.Context(), targets[clusterId])
		if err != nil {
			t.Fatalf("%s: %v (is T3 up? local/file-cluster-t3/t3.sh up)", clusterId, err)
		}
		t.Logf("%s", report)
		if build, ok := report.SettlePassedBuild(); !ok {
			for _, c := range report.Cases {
				if c.Outcome != FileClusterSuitePass {
					t.Errorf("%s: %s %s: %s", clusterId, c.Name, c.Outcome, c.Detail)
				}
			}
		} else if len(build) != 40 {
			t.Errorf("%s: the build %q", clusterId, build)
		}
	}
}

// The one-process cluster, a second metadata cluster: the suite runs every case one host allows and never names a
// build while any case failed or did not run, which with one host its FS-114 cases cannot.
func TestFileClusterSuiteOnAOneProcessClusterNamesNoBuild(t *testing.T) {
	report, err := RunFileClusterSuite(t.Context(), fileClusterSuiteT3Targets(t)["meta-2"])
	if err != nil {
		t.Fatalf("meta-2: %v (is T3 up? local/file-cluster-t3/t3.sh up)", err)
	}
	t.Logf("%s", report)
	if _, ok := report.SettlePassedBuild(); ok {
		t.Error("a one-process cluster named a build with its FS-114 cases not run")
	}
}
