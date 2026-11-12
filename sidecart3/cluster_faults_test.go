package sidecart3

// T3's cluster faults (cluster_faults.go) without the layouts running: the layout files, and the refusals that keep
// QuorumLoss and DriveMiss off a store that cannot carry them out. cluster_faults_t3_test.go runs them on the
// layouts.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
)

// T3's layout reads from server/local/file-cluster-t3 as EVSB-M2-17 wrote it: meta-1 is 4 hosts of 2 drives, seg-1 4
// of 4, each host with its control port and its sidecar-test- container.
func TestLoadClusterLayoutReadsT3sFiles(t *testing.T) {
	layout, err := LoadClusterLayout(layoutDirectory, metadataClusterId, segmentClusterId)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cluster       *LayoutCluster
		drivesPerHost int
	}{
		{cluster: layout.Metadata, drivesPerHost: 2},
		{cluster: layout.Segment, drivesPerHost: 4},
	} {
		if len(c.cluster.Config.Endpoints) != 4 || c.cluster.Hosts.DrivesPerHost != c.drivesPerHost {
			t.Errorf("%s: %d hosts of %d drives", c.cluster.Config.ClusterId, len(c.cluster.Config.Endpoints), c.cluster.Hosts.DrivesPerHost)
		}
		perHost, total := c.cluster.driveCounts()
		if perHost != c.drivesPerHost || total != 4*c.drivesPerHost {
			t.Errorf("%s counts %d of %d drives", c.cluster.Config.ClusterId, perHost, total)
		}
		for _, container := range c.cluster.Hosts.Containers {
			if !strings.HasPrefix(container, "sidecar-test-fc-") {
				t.Errorf("%s names container %s", c.cluster.Config.ClusterId, container)
			}
		}
	}
	if _, err := LoadClusterLayout(layoutDirectory, segmentClusterId, metadataClusterId); err == nil {
		t.Error("the segment cluster was taken as the metadata one")
	}
}

// A layout whose host is off loopback, or whose hosts name no control port, is refused: its faults could not be
// carried out, or would reach past this machine.
func TestLoadClusterLayoutRefusesAHostItCannotDrive(t *testing.T) {
	clusters, err := os.ReadFile(filepath.Join(layoutDirectory, "clusters.yml"))
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := os.ReadFile(filepath.Join(layoutDirectory, "hosts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(clustersText string, hostsText string) string {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "clusters.yml"), []byte(clustersText), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "hosts.yml"), []byte(hostsText), 0o600); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	offLoopback := write(strings.Replace(string(clusters), "127.0.0.1:52101", "192.0.2.10:52101", 1), string(hosts))
	if _, err := LoadClusterLayout(offLoopback, metadataClusterId, segmentClusterId); err == nil {
		t.Error("a host off loopback was taken")
	}
	noHooks := write(string(clusters), strings.Replace(string(hosts), "    hooks: [127.0.0.1:52111, 127.0.0.1:52112, 127.0.0.1:52113, 127.0.0.1:52114]\n", "", 1))
	if _, err := LoadClusterLayout(noHooks, metadataClusterId, segmentClusterId); err == nil || !strings.Contains(err.Error(), "control port") {
		t.Errorf("hosts with no control port: %v", err)
	}
}

// QuorumLoss and DriveMiss need a layout, and a plan that names a cluster of it: a Net on the local stack's one
// process refuses both where the schedule armed them, as T2's fakes do, and so do HostDown, HostUp and HostCluster.
func TestAClusterFaultNeedsALayoutAndItsCluster(t *testing.T) {
	quorumLoss := &sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, Result: sidecartest.QuorumLoss, ClusterId: metadataClusterId}
	if err := admitClusterFault(nil)(quorumLoss); !errors.Is(err, errNoLayout) {
		t.Errorf("on the local stack: %v", err)
	}
	queue := sidecartest.NewFaultQueue()
	queue.SetRealStore(admitClusterFault(nil))
	if _, err := queue.Add(quorumLoss); !errors.Is(err, errNoLayout) {
		t.Errorf("armed on the local stack: %v", err)
	}
	layout, err := LoadClusterLayout(layoutDirectory, metadataClusterId, segmentClusterId)
	if err != nil {
		t.Fatal(err)
	}
	admit := admitClusterFault(newClusterFaults(nil, layout, DefaultClusterFaultSettings()))
	for _, plan := range []*sidecartest.FaultPlan{
		{Op: sidecartest.FaultOpPut, Result: sidecartest.QuorumLoss},
		{Op: sidecartest.FaultOpPut, Result: sidecartest.DriveMiss, ClusterId: "meta-2"},
	} {
		if err := admit(plan); err == nil || !strings.Contains(err.Error(), "names its cluster") {
			t.Errorf("%s: %v", plan, err)
		}
	}
	for _, plan := range []*sidecartest.FaultPlan{
		quorumLoss,
		{Op: sidecartest.FaultOpGet, Result: sidecartest.QuorumLoss, ClusterId: segmentClusterId},
		{Op: sidecartest.FaultOpPut, Result: sidecartest.DriveMiss, ClusterId: segmentClusterId},
	} {
		if err := admit(plan); err != nil {
			t.Errorf("%s: %v", plan, err)
		}
	}
	net := sidecartest.NewNet(t.Context(), nil)
	defer net.Close()
	ctx := context.Background()
	if err := HostDown(ctx, net, metadataClusterId, 0); !errors.Is(err, errNoLayout) {
		t.Errorf("HostDown: %v", err)
	}
	if err := HostUp(ctx, net, metadataClusterId, 0); !errors.Is(err, errNoLayout) {
		t.Errorf("HostUp: %v", err)
	}
	if _, _, err := HostCluster(ctx, net, metadataClusterId, 0); !errors.Is(err, errNoLayout) {
		t.Errorf("HostCluster: %v", err)
	}
}

// Which quorum health a cluster's hosts answer whole as hosts go down: the write quorum's with one down, the read
// quorum's alone with two, and neither with three.
func TestStandingHealthFollowsTheHostsDown(t *testing.T) {
	layout, err := LoadClusterLayout(layoutDirectory, metadataClusterId, segmentClusterId)
	if err != nil {
		t.Fatal(err)
	}
	faults := newClusterFaults(nil, layout, DefaultClusterFaultSettings())
	for down, want := range []string{"cluster", "cluster", "cluster/read", "live"} {
		faults.downHosts[metadataClusterId] = []int{0, 1, 2, 3}[:down]
		if got := faults.standingHealth(metadataClusterId); got != want {
			t.Errorf("%d down: %s, want %s", down, got, want)
		}
	}
}
