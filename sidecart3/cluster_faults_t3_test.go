//go:build sidecar_filecluster_t3

// T3's cluster faults on the layouts server/local/file-cluster-t3 starts (t3.sh build, then up): QuorumLoss and
// DriveMiss through a T3 Net's plans, and HostDown and HostUp, on the minio fork's settle_testhooks build. These
// tests fail rather than skip when the layouts are down, carry sidecar_filecluster_t3 beside the file-cluster suite's
// own, and run nightly and at Gn-out, never in the whole-suite scripts: they kill and restart the layouts' hosts and
// wait on MinIO's drive timeout in real time. They run in a TestEnv for the private Redis a T3 Net's host needs.

package sidecart3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The private stores, a database of the test's own, and no migrations: a layout Net reads no server schema.
func layoutTestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: false}
}

// A key no other run shares.
func layoutKey(prefix string) string {
	return prefix + strings.ToLower(sidecar.NewId().String())
}

// A Net on the layouts, closed at the test's end, and the actor ctx its requests run under.
func newLayoutNet(t *testing.T) (*sidecartest.Net, context.Context) {
	t.Helper()
	net := NewNet(t, LayoutSettings(t))
	t.Cleanup(net.Close)
	return net, net.Actors().Actor(net.Context(), "writerA").Context()
}

func layoutCluster(t *testing.T, net *sidecartest.Net, clusterId string) sidecar.BlobCluster {
	t.Helper()
	cluster, err := net.Host().Blobs.Cluster(clusterId)
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

// key's body through host hostIndex alone, or the error that host gives.
func readThroughHost(t *testing.T, net *sidecartest.Net, clusterId string, hostIndex int, key string) ([]byte, error) {
	t.Helper()
	cluster, closeCluster, err := HostCluster(t.Context(), net, clusterId, hostIndex)
	if err != nil {
		t.Fatal(err)
	}
	defer closeCluster()
	object, err := cluster.Get(t.Context(), key)
	if err != nil {
		return nil, err
	}
	defer object.Body.Close()
	return io.ReadAll(object.Body)
}

// Whether the drive volume of a layout host holds key in the Net's bucket on clusterId: what MinIO's own reads do
// not show, a shard missing from one drive. Read by a throwaway alpine container of the image already on this host.
func driveHolds(t *testing.T, net *sidecartest.Net, clusterId string, hostIndex int, drive string, key string) bool {
	t.Helper()
	faults := clusterFaultsOf(net)
	cluster, err := faults.layoutCluster(clusterId, hostIndex)
	if err != nil {
		t.Fatal(err)
	}
	bucket := ""
	for _, config := range faults.configs {
		if config.ClusterId == clusterId {
			bucket = config.Bucket
		}
	}
	volume := cluster.Hosts.Containers[hostIndex] + "-" + drive
	name := "sidecar-test-fc-drive-probe-" + strings.ToLower(sidecar.NewId().String())
	output, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--name", name, "--network", "none", "-v", volume+":/d:ro", "alpine:3.22",
		"sh", "-c", fmt.Sprintf("test -e /d/%s/%s/xl.meta && echo held || echo missing", bucket, key)).CombinedOutput()
	if err != nil {
		t.Fatalf("reading %s: %v: %s", volume, err, output)
	}
	return strings.TrimSpace(string(output)) == "held"
}

// A Net on the layouts runs on its fake clock, months behind the fork's own: every request carries its deadline in
// the store's time, so the fork, which refuses a deadline already past by its clock, takes each one. The same
// clusters on the same fake clock with no store time are refused, which is what the translation is for.
func TestT3LayoutTakesRequestsOnAFakeClock(t *testing.T) {
	layoutTestEnv().Run(t, func(tb testing.TB) {
		net, ctx := newLayoutNet(t)
		clusterSettings := server.DefaultFileClusterSettings()
		clusterSettings.Clock = net.Clock()
		untranslated, err := server.NewFileClusters(ctx, clusterFaultsOf(net).configs, clusterSettings)
		if err != nil {
			t.Fatal(err)
		}
		defer untranslated.Close()
		untranslated.Check(ctx)
		untranslatedCluster, err := untranslated.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := untranslatedCluster.Head(ctx, layoutKey("layout/untranslated/")+"/_"); err == nil || errors.Is(err, sidecar.ErrBlobNotFound) {
			t.Errorf("a request on the fake clock with no store time: %v", err)
		}
		for _, clusterId := range []string{metadataClusterId, segmentClusterId} {
			cluster := layoutCluster(t, net, clusterId)
			key := layoutKey("layout/") + "/_"
			etag, err := cluster.PutIfNoneMatch(ctx, key, []byte("v1"), nil)
			if err != nil {
				t.Fatalf("%s: a PUT on a fake clock: %v", clusterId, err)
			}
			if _, err := cluster.PutIfMatch(ctx, key, etag, []byte("v2"), nil); err != nil {
				t.Errorf("%s: an If-Match PUT: %v", clusterId, err)
			}
			if _, err := cluster.Head(ctx, key); err != nil {
				t.Errorf("%s: a HEAD: %v", clusterId, err)
			}
			if err := cluster.Delete(ctx, key); err != nil {
				t.Errorf("%s: a DELETE: %v", clusterId, err)
			}
		}
	})
}

// QuorumLoss gives 503 and an unknown outcome: a PUT whose plan takes the write quorum away holds an unknown
// outcome, a read whose plan takes the read quorum fails, the hosts come back after, the PUT never lands and the
// object standing before reads as it was. The store itself answers 503 through a host still up.
func TestT3QuorumLossGives503AndAnUnknownOutcome(t *testing.T) {
	layoutTestEnv().Run(t, func(tb testing.TB) {
		net, ctx := newLayoutNet(t)
		for _, clusterId := range []string{metadataClusterId, segmentClusterId} {
			cluster := layoutCluster(t, net, clusterId)
			standing := layoutKey("quorum/standing/") + "/_"
			if _, err := cluster.PutIfNoneMatch(ctx, standing, []byte("standing"), nil); err != nil {
				t.Fatal(err)
			}
			lost := layoutKey("quorum/lost/") + "/_"
			if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: lost, Result: sidecartest.QuorumLoss, ClusterId: clusterId}); err != nil {
				t.Fatal(err)
			}
			var unknown *sidecar.BlobUnknownError
			if _, err := cluster.PutIfNoneMatch(ctx, lost, []byte("lost"), nil); !errors.As(err, &unknown) {
				t.Errorf("%s: a PUT with its write quorum lost: %v", clusterId, err)
			}
			if down := clusterFaultsOf(net).down(clusterId); len(down) != 0 {
				t.Errorf("%s: hosts %v stayed down after the plan's last match", clusterId, down)
			}
			if _, err := readThroughHost(t, net, clusterId, 0, lost); !errors.Is(err, sidecar.ErrBlobNotFound) {
				t.Errorf("%s: the PUT that lost its quorum: %v", clusterId, err)
			}
			if clusterId != metadataClusterId {
				continue
			}
			if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: standing, Result: sidecartest.QuorumLoss, ClusterId: clusterId}); err != nil {
				t.Fatal(err)
			}
			if _, err := cluster.Get(ctx, standing); err == nil || errors.Is(err, sidecar.ErrBlobNotFound) {
				t.Errorf("%s: a GET with its read quorum lost: %v", clusterId, err)
			}
			if body, err := readThroughHost(t, net, clusterId, 1, standing); err != nil || string(body) != "standing" {
				t.Errorf("%s: the standing object after: %q, %v", clusterId, body, err)
			}
		}
		if unfired := net.Faults().Unfired(); len(unfired) != 0 {
			t.Errorf("plans never fired: %v", unfired)
		}

		// the store's own answer, through the first host, with the last two down
		layout := LayoutSettings(t).Layout.Metadata
		for _, hostIndex := range []int{3, 2} {
			if err := HostDown(ctx, net, metadataClusterId, hostIndex); err != nil {
				t.Fatal(err)
			}
		}
		if err := clusterFaultsOf(net).awaitQuorumLost(ctx, metadataClusterId, "cluster"); err != nil {
			t.Fatal(err)
		}
		client, err := minio.New(layout.Config.Endpoints[0], &minio.Options{
			Creds:  credentials.NewStaticV4(layout.Config.AccessKey, layout.Config.SecretKey, ""),
			Region: layout.Config.Region,
		})
		if err != nil {
			t.Fatal(err)
		}
		bucket := clusterFaultsOf(net).configs[0].Bucket
		_, err = client.PutObject(ctx, bucket, layoutKey("quorum/direct/")+"/_", bytes.NewReader([]byte("direct")), 6, minio.PutObjectOptions{})
		if status := minio.ToErrorResponse(err).StatusCode; status != 503 {
			t.Errorf("the store answered a PUT with two hosts down %d: %v", status, err)
		}
		for _, hostIndex := range []int{2, 3} {
			if err := HostUp(ctx, net, metadataClusterId, hostIndex); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// DriveMiss leaves the object readable until a second host goes down: a PUT that misses one drive of the second
// host stands on the other drives, reads with the third host down, and is unreadable with the fourth down too,
// though objects with every shard still read; a host returning ends the miss and the cluster heals the drive, so the
// object then reads with those two hosts down again. On both layouts.
func TestT3DriveMissLeavesTheObjectReadableUntilASecondHostGoesDown(t *testing.T) {
	layoutTestEnv().Run(t, func(tb testing.TB) {
		net, ctx := newLayoutNet(t)
		faults := clusterFaultsOf(net)
		for _, clusterId := range []string{metadataClusterId, segmentClusterId} {
			cluster := layoutCluster(t, net, clusterId)
			whole := layoutKey("drive/whole/") + "/_"
			if _, err := cluster.PutIfNoneMatch(ctx, whole, []byte("whole"), nil); err != nil {
				t.Fatal(err)
			}
			missed := layoutKey("drive/missed/") + "/_"
			if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: missed, Result: sidecartest.DriveMiss, ClusterId: clusterId}); err != nil {
				t.Fatal(err)
			}
			if _, err := cluster.PutIfNoneMatch(ctx, missed, []byte("missed"), nil); err != nil {
				t.Fatalf("%s: the PUT that misses a drive: %v", clusterId, err)
			}
			if driveHolds(t, net, clusterId, 1, "d1", missed) || !driveHolds(t, net, clusterId, 1, "d2", missed) {
				t.Errorf("%s: the second host's first drive took the PUT, or its second did not", clusterId)
			}
			if err := HostDown(ctx, net, clusterId, 2); err != nil {
				t.Fatal(err)
			}
			if body, err := readThroughHost(t, net, clusterId, 0, missed); err != nil || string(body) != "missed" {
				t.Errorf("%s: with one host down the object reads %q, %v", clusterId, body, err)
			}
			if err := HostDown(ctx, net, clusterId, 3); err != nil {
				t.Fatal(err)
			}
			if body, err := readThroughHost(t, net, clusterId, 0, missed); err == nil {
				t.Errorf("%s: with a second host down the object one shard short reads %q", clusterId, body)
			}
			if body, err := readThroughHost(t, net, clusterId, 0, whole); err != nil || string(body) != "whole" {
				t.Errorf("%s: an object with every shard reads %q, %v", clusterId, body, err)
			}
			// a host returning ends the miss: the PUT's own commit is refused past its deadline, and a heal fills the
			// drive. MinIO's data path reconnects some seconds after its health answers, and until then a read through
			// a host may answer 404 for the object (questions/test-m2-07-minio-404-under-lost-quorum.md), so each host
			// is read until it serves the object, which is also what starts MinIO's heal of the drive that misses it
			for _, hostIndex := range []int{3, 2} {
				if err := HostUp(ctx, net, clusterId, hostIndex); err != nil {
					t.Fatal(err)
				}
			}
			for hostIndex := range 4 {
				if err := faults.waitFor(ctx, fmt.Sprintf("host %d to serve the object again", hostIndex), func() (bool, error) {
					body, err := readThroughHost(t, net, clusterId, hostIndex, missed)
					return err == nil && string(body) == "missed", fmt.Errorf("it reads %q, %v", body, err)
				}); err != nil {
					t.Errorf("%s: %v", clusterId, err)
				}
			}
			if err := faults.waitFor(ctx, "the missed drive to heal", func() (bool, error) {
				return driveHolds(t, net, clusterId, 1, "d1", missed), errors.New("the drive still misses the object")
			}); err != nil {
				t.Errorf("%s: %v", clusterId, err)
				continue
			}
			for _, hostIndex := range []int{2, 3} {
				if err := HostDown(ctx, net, clusterId, hostIndex); err != nil {
					t.Fatal(err)
				}
			}
			if body, err := readThroughHost(t, net, clusterId, 0, missed); err != nil || string(body) != "missed" {
				t.Errorf("%s: healed, the object reads %q, %v with two hosts down", clusterId, body, err)
			}
			for _, hostIndex := range []int{2, 3} {
				if err := HostUp(ctx, net, clusterId, hostIndex); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

// A host returning heals: a host taken down and brought back serves again, every host answers the cluster's write
// quorum, and an object written while it was down reads through it once it is back.
func TestT3AHostReturningHeals(t *testing.T) {
	layoutTestEnv().Run(t, func(tb testing.TB) {
		net, ctx := newLayoutNet(t)
		cluster := layoutCluster(t, net, metadataClusterId)
		if err := HostDown(ctx, net, metadataClusterId, 3); err != nil {
			t.Fatal(err)
		}
		written := layoutKey("heal/written-while-down/") + "/_"
		if _, err := cluster.PutIfNoneMatch(ctx, written, []byte("written"), nil); err != nil {
			t.Fatalf("a PUT with one host down: %v", err)
		}
		if err := HostUp(ctx, net, metadataClusterId, 3); err != nil {
			t.Fatal(err)
		}
		if body, err := readThroughHost(t, net, metadataClusterId, 3, written); err != nil || string(body) != "written" {
			t.Errorf("through the host that returned: %q, %v", body, err)
		}
		if err := HostUp(ctx, net, metadataClusterId, 3); err == nil {
			t.Error("a host up already was started again")
		}
	})
}
