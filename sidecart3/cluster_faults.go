package sidecart3

// T3's cluster faults (TEST-M2-07; §22.4, §22.8, D-OPS-20, FS-1, FS-40): QuorumLoss and DriveMiss, which T2's fakes
// refuse as sidecartest.ErrFaultNeedsRealStore, and HostDown and HostUp, carried out for real on T3's file-cluster
// layouts, EVSB-M2-17's local/file-cluster-t3: meta-1, 4 hosts of 2 drives at EC:4, and seg-1, 4 hosts of 4 drives
// at EC:8, each host a container of the minio fork's settle_testhooks build, its S3 and its barrier control port on
// loopback above port 50000. A Net takes them with Settings.Layout, and its buckets go on them, made for it and
// removed with it.
//
//   - HostDown kills one host's container, as a host that lost power, and returns once its S3 port refuses. MinIO
//     learns of a lost host only from the requests it fails, each waiting out the drive timeout, so nothing waits
//     for the others to notice. HostUp starts it again with its drives, and returns once it serves and every host
//     up answers MinIO's own quorum health as the hosts still down allow. A Net's close starts every host it left
//     down.
//   - QuorumLoss, at the request its plan fires on, takes down the last hosts of the plan's cluster that the
//     request's quorum needs and waits until a host still up answers MinIO's quorum health 503, then sends the
//     request, which the store answers 503 or, for a read, not before the client's own timeout, so a write's
//     caller holds an unknown outcome and a read's a failure; the hosts start again after the plan's last match.
//     On both layouts a write's quorum is lost with 2 of the 4 hosts down (5 of 8 drives at EC:4, 9 of 16 at EC:8,
//     are a write quorum) and a read's with 3 (4 of 8, 8 of 16, still read). What quorum means differs per set,
//     so a plan names its cluster.
//   - DriveMiss, at the PUT its plan fires on, holds every commit of that key to one drive of one host (the second
//     host's first drive, FS-40's "one drive of B") at the fork's commit-arrived barrier, so the PUT completes at
//     its write quorum on the other drives, and MinIO's heal, which would refill the drive within seconds, holds
//     there too. The hold ends at the next HostUp on the cluster or at the Net's close, once the PUT's deadline has
//     passed by the store's clock, so the PUT's own commit never begins (D-OPS-20's deadline rule) and only a heal
//     then restores the shard: until then the object stands one shard short, and the drive serves every other key
//     as before. A held key is the fork's own behaviour, never a fake of this wrapper's.
//
// A layout's clusters are shared by everything on this machine that runs them, so T3 runs never use t.Parallel,
// and a test that takes a host down brings it back. Everything here waits in real time, bounded by
// ClusterFaultSettings.WaitLimit: a host's restart is the store's own.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
)

// The fork's barrier point a DriveMiss holds its commit at (minio/cmd/settle-testhooks.go).
const clusterFaultCommitArrived = "commit-arrived"

var (
	// A plan or a host call this Net's stores cannot carry out.
	errNoLayout = errors.New("this T3 Net runs on the local stack's one MinIO process, which has no host to lose: QuorumLoss, DriveMiss, HostDown and HostUp need Settings.Layout, T3's 4-host file-cluster layouts")
	// A layout this machine is not running.
	errLayoutDown = errors.New("T3's file clusters do not answer (server/local/file-cluster-t3/t3.sh up)")
)

// One cluster of a layout: what clusters.yml and hosts.yml say of it.
type LayoutCluster struct {
	// Its id, kind, endpoints, synthetic root user and region. Bucket is the layout's own, which a Net never writes:
	// each Net makes buckets of its own on these hosts.
	Config *server.FileClusterConfig
	// Its drives per host, drive timeout, and each host's control port and container, in Config.Endpoints' order.
	Hosts *server.FileClusterSuiteHostsEntry
}

// The drives each host of the cluster holds, and the set's whole count (each layout cluster is one set).
func (self *LayoutCluster) driveCounts() (perHost int, total int) {
	return self.Hosts.DrivesPerHost, self.Hosts.DrivesPerHost * len(self.Config.Endpoints)
}

// T3's file-cluster layout: the metadata and the segment cluster a Net's buckets go on.
type ClusterLayout struct {
	Metadata *LayoutCluster
	Segment  *LayoutCluster
}

// The layout cluster of clusterId, or nil.
func (self *ClusterLayout) cluster(clusterId string) *LayoutCluster {
	switch clusterId {
	case self.Metadata.Config.ClusterId:
		return self.Metadata
	case self.Segment.Config.ClusterId:
		return self.Segment
	}
	return nil
}

// Reads directory's clusters.yml and hosts.yml, as local/file-cluster-t3 holds them, taking the metadata cluster
// metadataClusterId and the segment cluster segmentClusterId. Every host is held to loopback above port 50000, and
// a cluster with no control port or container for each host fails: its faults could not be carried out.
func LoadClusterLayout(directory string, metadataClusterId string, segmentClusterId string) (*ClusterLayout, error) {
	clustersText, err := os.ReadFile(filepath.Join(directory, "clusters.yml"))
	if err != nil {
		return nil, err
	}
	configs, err := server.ParseFileClusterConfigs(clustersText)
	if err != nil {
		return nil, err
	}
	hostsText, err := os.ReadFile(filepath.Join(directory, "hosts.yml"))
	if err != nil {
		return nil, err
	}
	hostsFile, err := server.ParseFileClusterSuiteHostsFile(hostsText)
	if err != nil {
		return nil, err
	}
	layoutCluster := func(clusterId string, kind string) (*LayoutCluster, error) {
		index := slices.IndexFunc(configs, func(config *server.FileClusterConfig) bool { return config.ClusterId == clusterId })
		if index < 0 {
			return nil, fmt.Errorf("%s names no cluster %s", directory, clusterId)
		}
		config := configs[index]
		if config.Kind != kind {
			return nil, fmt.Errorf("cluster %s is %s, not %s", clusterId, config.Kind, kind)
		}
		hosts := hostsFile.Clusters[clusterId]
		if hosts == nil || hosts.DrivesPerHost <= 0 {
			return nil, fmt.Errorf("hosts.yml gives cluster %s no drives per host", clusterId)
		}
		if len(hosts.Hooks) != len(config.Endpoints) || len(hosts.Containers) != len(config.Endpoints) {
			return nil, fmt.Errorf("hosts.yml names no control port and container for each of cluster %s's %d hosts", clusterId, len(config.Endpoints))
		}
		for _, address := range slices.Concat(config.Endpoints, hosts.Hooks) {
			if err := localstack.CheckStackAddress(address); err != nil {
				return nil, fmt.Errorf("cluster %s: %w", clusterId, err)
			}
		}
		for _, container := range hosts.Containers {
			if !strings.HasPrefix(container, "sidecar-test-") {
				return nil, fmt.Errorf("cluster %s names container %s, which is no sidecar-test- container", clusterId, container)
			}
		}
		return &LayoutCluster{Config: config, Hosts: hosts}, nil
	}
	metadata, err := layoutCluster(metadataClusterId, server.FileClusterKindMetadata)
	if err != nil {
		return nil, err
	}
	segment, err := layoutCluster(segmentClusterId, server.FileClusterKindSegment)
	if err != nil {
		return nil, err
	}
	return &ClusterLayout{Metadata: metadata, Segment: segment}, nil
}

// How a Net's cluster faults run on its layout.
type ClusterFaultSettings struct {
	// The longest any wait on a layout takes before it fails: 2 min, past a host's restart. Positive.
	WaitLimit time.Duration
	// How often a wait asks again: 50 ms. Positive.
	PollInterval time.Duration
	// The bound on one control-port or health call: 10 s. Positive.
	CallTimeout time.Duration
	// The hosts down for a write's quorum to be lost, and for a read's: 2 and 3 of the layouts' 4, the last hosts
	// first. Positive.
	WriteQuorumLossHostCount int
	ReadQuorumLossHostCount  int
	// The host whose drive a DriveMiss holds, by index, 1 (FS-40's B), and the drive, as its path ends: "/drives/d1".
	DriveMissHostIndex int
	DriveMissDrive     string
	// How far past a DriveMiss PUT's deadline, by the store's clock, its held commit is let go: 1 s, past the fork's
	// clock slack (the layouts' _MINIO_SETTLE_CLOCK_MAX_OFFSET). Positive.
	DeadlineSlack time.Duration
	// The most of a control-port reply read: 64 KiB. Positive.
	ReplyByteCount int64
	// The docker binary: "docker".
	Docker string
}

func DefaultClusterFaultSettings() *ClusterFaultSettings {
	return &ClusterFaultSettings{
		WaitLimit:                2 * time.Minute,
		PollInterval:             50 * time.Millisecond,
		CallTimeout:              10 * time.Second,
		WriteQuorumLossHostCount: 2,
		ReadQuorumLossHostCount:  3,
		DriveMissHostIndex:       1,
		DriveMissDrive:           "/drives/d1",
		DeadlineSlack:            time.Second,
		ReplyByteCount:           64 * 1024,
		Docker:                   "docker",
	}
}

// One Net's cluster faults on its layout. Safe for concurrent use.
type clusterFaults struct {
	net      *sidecartest.Net
	layout   *ClusterLayout
	settings *ClusterFaultSettings
	client   *http.Client
	// the Net's own clusters, its buckets on the layout, which newHost sets once they are made
	configs []*server.FileClusterConfig

	stateLock sync.Mutex
	// by cluster id, the hosts this Net took down, by index
	downHosts map[string][]int
	// by plan, how many matches of a QuorumLoss plan have fired, and the hosts it took down
	planFireCounts map[*sidecartest.FaultPlan]int
	planDownHosts  map[*sidecartest.FaultPlan][]int
	// by cluster id, the DriveMiss holds still standing
	driveMisses map[string][]*clusterFaultDriveMiss
	// what carrying a fault out failed on: the run fails, since the store did not do what the plan said
	errs []error
}

func newClusterFaults(net *sidecartest.Net, layout *ClusterLayout, settings *ClusterFaultSettings) *clusterFaults {
	return &clusterFaults{
		net:            net,
		layout:         layout,
		settings:       settings,
		client:         &http.Client{Timeout: settings.CallTimeout},
		downHosts:      map[string][]int{},
		planFireCounts: map[*sidecartest.FaultPlan]int{},
		planDownHosts:  map[*sidecartest.FaultPlan][]int{},
		driveMisses:    map[string][]*clusterFaultDriveMiss{},
	}
}

// Each open T3 Net's cluster faults, so HostDown and HostUp find them from the Net.
var clusterFaultNets sync.Map

func clusterFaultsOf(net *sidecartest.Net) *clusterFaults {
	if faults, ok := clusterFaultNets.Load(net); ok {
		return faults.(*clusterFaults)
	}
	return nil
}

// Whether a Net on these stores carries out plan, a QuorumLoss or a DriveMiss (FaultQueue.SetRealStore): only on a
// layout, and only for a plan that names a cluster of it.
func admitClusterFault(faults *clusterFaults) func(plan *sidecartest.FaultPlan) error {
	return func(plan *sidecartest.FaultPlan) error {
		if faults == nil {
			return errNoLayout
		}
		if plan.ClusterId == "" || faults.layout.cluster(plan.ClusterId) == nil {
			return fmt.Errorf("a %s names its cluster, %s or %s, since what quorum means differs per set", plan.Result, faults.layout.Metadata.Config.ClusterId, faults.layout.Segment.Config.ClusterId)
		}
		return nil
	}
}

// Takes host hostIndex of net's cluster clusterId down, as a host that lost power, and returns once its S3 port
// refuses. The Net's close starts it again if nothing has.
func HostDown(ctx context.Context, net *sidecartest.Net, clusterId string, hostIndex int) error {
	faults := clusterFaultsOf(net)
	if faults == nil {
		return errNoLayout
	}
	return faults.hostDown(ctx, clusterId, hostIndex)
}

// Starts host hostIndex of net's cluster clusterId again with its drives, and returns once it serves and every host
// up answers MinIO's quorum health as the hosts still down allow. A DriveMiss hold on the cluster ends first.
func HostUp(ctx context.Context, net *sidecartest.Net, clusterId string, hostIndex int) error {
	faults := clusterFaultsOf(net)
	if faults == nil {
		return errNoLayout
	}
	return faults.hostUp(ctx, clusterId, hostIndex)
}

// net's cluster clusterId reached through host hostIndex alone, on the store's own clock and with no plan, for a
// test reading what each host serves ("read through every host"), and what closes it.
func HostCluster(ctx context.Context, net *sidecartest.Net, clusterId string, hostIndex int) (sidecar.BlobCluster, func(), error) {
	faults := clusterFaultsOf(net)
	if faults == nil {
		return nil, nil, errNoLayout
	}
	cluster, err := faults.layoutCluster(clusterId, hostIndex)
	if err != nil {
		return nil, nil, err
	}
	index := slices.IndexFunc(faults.configs, func(config *server.FileClusterConfig) bool { return config.ClusterId == clusterId })
	if index < 0 {
		return nil, nil, fmt.Errorf("the Net has no cluster %s", clusterId)
	}
	config := *faults.configs[index]
	config.Endpoints = []string{cluster.Config.Endpoints[hostIndex]}
	// a store of the one cluster stands as its own first metadata cluster, as the file-cluster suite's does
	config.Kind = server.FileClusterKindMetadata
	config.First = true
	clusters, err := server.NewFileClusters(ctx, []*server.FileClusterConfig{&config}, server.DefaultFileClusterSettings())
	if err != nil {
		return nil, nil, err
	}
	clusters.Check(ctx)
	hostCluster, err := clusters.Cluster(clusterId)
	if err != nil {
		clusters.Close()
		return nil, nil, err
	}
	return hostCluster, clusters.Close, nil
}

// The hosts of clusterId this Net has down.
func (self *clusterFaults) down(clusterId string) []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.downHosts[clusterId])
}

func (self *clusterFaults) layoutCluster(clusterId string, hostIndex int) (*LayoutCluster, error) {
	cluster := self.layout.cluster(clusterId)
	if cluster == nil {
		return nil, fmt.Errorf("the layout has no cluster %s", clusterId)
	}
	if hostIndex < 0 || len(cluster.Config.Endpoints) <= hostIndex {
		return nil, fmt.Errorf("cluster %s has no host %d", clusterId, hostIndex)
	}
	return cluster, nil
}

func (self *clusterFaults) hostDown(ctx context.Context, clusterId string, hostIndex int) error {
	cluster, err := self.layoutCluster(clusterId, hostIndex)
	if err != nil {
		return err
	}
	if slices.Contains(self.down(clusterId), hostIndex) {
		return fmt.Errorf("cluster %s's host %d is down already", clusterId, hostIndex)
	}
	host := &server.FileClusterSuiteDockerHost{Container: cluster.Hosts.Containers[hostIndex], Docker: self.settings.Docker}
	if err := host.Stop(ctx); err != nil {
		return err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.downHosts[clusterId] = append(self.downHosts[clusterId], hostIndex)
	}()
	endpoint := cluster.Config.Endpoints[hostIndex]
	return self.waitFor(ctx, fmt.Sprintf("cluster %s's host %d to stop answering", clusterId, hostIndex), func() (bool, error) {
		status, err := self.health(ctx, endpoint, "live")
		return err != nil, fmt.Errorf("it answers %d", status)
	})
}

func (self *clusterFaults) hostUp(ctx context.Context, clusterId string, hostIndex int) error {
	cluster, err := self.layoutCluster(clusterId, hostIndex)
	if err != nil {
		return err
	}
	if !slices.Contains(self.down(clusterId), hostIndex) {
		return fmt.Errorf("cluster %s's host %d is not down", clusterId, hostIndex)
	}
	// a host returning is where a missed drive heals: its hold ends first
	releaseErr := self.endDriveMisses(ctx, clusterId)
	host := &server.FileClusterSuiteDockerHost{Container: cluster.Hosts.Containers[hostIndex], Docker: self.settings.Docker}
	if err := host.Start(context.WithoutCancel(ctx)); err != nil {
		return errors.Join(releaseErr, err)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.downHosts[clusterId] = slices.DeleteFunc(self.downHosts[clusterId], func(index int) bool { return index == hostIndex })
	}()
	return errors.Join(releaseErr, self.awaitQuorum(ctx, clusterId))
}

// Ends every DriveMiss hold and starts every host this Net has down, for its close: a layout is never left short of
// a host or holding a commit.
func (self *clusterFaults) restoreAll(ctx context.Context) error {
	errs := []error{}
	for _, clusterId := range []string{self.layout.Metadata.Config.ClusterId, self.layout.Segment.Config.ClusterId} {
		errs = append(errs, self.endDriveMisses(ctx, clusterId))
		for _, hostIndex := range self.down(clusterId) {
			errs = append(errs, self.hostUp(ctx, clusterId, hostIndex))
		}
	}
	return errors.Join(errs...)
}

// One call to a host's control port, its reply read whole up to ReplyByteCount.
func (self *clusterFaults) hook(ctx context.Context, address string, action string, values url.Values) (int, []byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, self.settings.CallTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, "http://"+address+"/"+action, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := self.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("the control port %s %s: %w", address, action, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, self.settings.ReplyByteCount))
	return response.StatusCode, body, err
}

// What MinIO's health check of kind answers at endpoint: live, the process; cluster, the set's write quorum as the
// host sees it; cluster/read, its read quorum. An error is a host that does not answer.
func (self *clusterFaults) health(ctx context.Context, endpoint string, kind string) (int, error) {
	callCtx, cancel := context.WithTimeout(ctx, self.settings.CallTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, "http://"+endpoint+"/minio/health/"+kind, nil)
	if err != nil {
		return 0, err
	}
	response, err := self.client.Do(request)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, self.settings.ReplyByteCount))
	response.Body.Close()
	return response.StatusCode, nil
}

// Polls condition until it holds, bounded by WaitLimit in real time.
func (self *clusterFaults) waitFor(ctx context.Context, what string, condition func() (bool, error)) error {
	deadline := time.Now().Add(self.settings.WaitLimit)
	var last error
	for {
		held, err := condition()
		if held {
			return nil
		}
		last = err
		if deadline.Before(time.Now()) {
			return fmt.Errorf("waited %s for %s: %v", self.settings.WaitLimit, what, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// The quorum health kind the hosts down on clusterId leave whole: cluster while a write quorum stands, cluster/read
// while only a read quorum does, and live alone past that.
func (self *clusterFaults) standingHealth(clusterId string) string {
	switch down := len(self.down(clusterId)); {
	case down < self.settings.WriteQuorumLossHostCount:
		return "cluster"
	case down < self.settings.ReadQuorumLossHostCount:
		return "cluster/read"
	}
	return "live"
}

// Returns once every host of clusterId up answers 200 to the quorum health the hosts down leave whole and, while a
// write quorum stands, writes through each of them (awaitWritable): MinIO's health answers before its hosts have
// dialed each other again.
func (self *clusterFaults) awaitQuorum(ctx context.Context, clusterId string) error {
	cluster := self.layout.cluster(clusterId)
	down := self.down(clusterId)
	kind := self.standingHealth(clusterId)
	err := self.waitFor(ctx, fmt.Sprintf("cluster %s's hosts up to answer health %s", clusterId, kind), func() (bool, error) {
		for hostIndex, endpoint := range cluster.Config.Endpoints {
			if slices.Contains(down, hostIndex) {
				continue
			}
			status, err := self.health(ctx, endpoint, kind)
			if err != nil {
				return false, err
			}
			if status != http.StatusOK {
				return false, fmt.Errorf("host %d answers %d", hostIndex, status)
			}
		}
		return true, nil
	})
	if err != nil || kind != "cluster" {
		return err
	}
	return awaitWritable(ctx, cluster, down, self.settings)
}

// Returns once each host of cluster but those down coordinates a write: a probe PUT and its DELETE in the layout's
// own bucket, which no Net writes, so a Net's stores hold nothing of it.
func awaitWritable(ctx context.Context, cluster *LayoutCluster, down []int, settings *ClusterFaultSettings) error {
	config := cluster.Config
	for hostIndex, endpoint := range config.Endpoints {
		if slices.Contains(down, hostIndex) {
			continue
		}
		client, err := minio.New(endpoint, &minio.Options{
			Creds:      credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
			Region:     config.Region,
			MaxRetries: 1,
		})
		if err != nil {
			return err
		}
		probe := "sidecart3-probe/" + strings.ToLower(sidecar.NewId().String())
		deadline := time.Now().Add(settings.WaitLimit)
		for {
			callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
			_, err := client.PutObject(callCtx, config.Bucket, probe, strings.NewReader("probe"), int64(len("probe")), minio.PutObjectOptions{})
			cancel()
			if err == nil {
				break
			}
			if deadline.Before(time.Now()) {
				return fmt.Errorf("cluster %s's host %d took no write in %s: %w", config.ClusterId, hostIndex, settings.WaitLimit, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settings.PollInterval):
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
		client.RemoveObject(callCtx, config.Bucket, probe, minio.RemoveObjectOptions{})
		cancel()
	}
	return nil
}

// Returns once a host of clusterId still up answers kind's quorum health 503: the cluster knows the quorum gone.
func (self *clusterFaults) awaitQuorumLost(ctx context.Context, clusterId string, kind string) error {
	cluster := self.layout.cluster(clusterId)
	down := self.down(clusterId)
	return self.waitFor(ctx, fmt.Sprintf("cluster %s to answer health %s 503", clusterId, kind), func() (bool, error) {
		for hostIndex, endpoint := range cluster.Config.Endpoints {
			if slices.Contains(down, hostIndex) {
				continue
			}
			status, err := self.health(ctx, endpoint, kind)
			if err != nil {
				return false, err
			}
			return status == http.StatusServiceUnavailable, fmt.Errorf("host %d answers %d", hostIndex, status)
		}
		return false, fmt.Errorf("no host of cluster %s is up", clusterId)
	})
}

// A QuorumLoss plan's hosts, taken down before the request it fired on: the last hosts of its cluster, as many as
// the request's quorum needs, beside those already down.
func (self *clusterFaults) loseQuorum(ctx context.Context, plan *sidecartest.FaultPlan, write bool) error {
	cluster := self.layout.cluster(plan.ClusterId)
	if cluster == nil {
		return fmt.Errorf("the layout has no cluster %s", plan.ClusterId)
	}
	count, kind := self.settings.ReadQuorumLossHostCount, "cluster/read"
	if write {
		count, kind = self.settings.WriteQuorumLossHostCount, "cluster"
	}
	for hostIndex := len(cluster.Config.Endpoints) - 1; 0 <= hostIndex && len(self.down(plan.ClusterId)) < count; hostIndex -= 1 {
		if slices.Contains(self.down(plan.ClusterId), hostIndex) {
			continue
		}
		if err := self.hostDown(ctx, plan.ClusterId, hostIndex); err != nil {
			return err
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.planDownHosts[plan] = append(self.planDownHosts[plan], hostIndex)
		}()
	}
	return self.awaitQuorumLost(ctx, plan.ClusterId, kind)
}

// After a QuorumLoss plan's request: at its last match, the hosts it took down start again, and those a test took
// down stay down. A plan repeating to the end keeps them down until the Net closes.
func (self *clusterFaults) afterQuorumLoss(ctx context.Context, plan *sidecartest.FaultPlan) error {
	hostIndexes, last := func() ([]int, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.planFireCounts[plan] += 1
		if plan.Repeat < 0 || self.planFireCounts[plan] < plan.Repeat+1 {
			return nil, false
		}
		hostIndexes := self.planDownHosts[plan]
		delete(self.planDownHosts, plan)
		return hostIndexes, true
	}()
	if !last {
		return nil
	}
	errs := []error{}
	for _, hostIndex := range hostIndexes {
		errs = append(errs, self.hostUp(ctx, plan.ClusterId, hostIndex))
	}
	return errors.Join(errs...)
}

// A DriveMiss's hold: armed on one host's drive for key before its PUT is sent.
type clusterFaultDriveMiss struct {
	// the host's control port
	address string
	// the PUT's deadline, by the store's clock
	storeDeadline time.Time
}

// Holds every commit of key on the DriveMiss drive at commit-arrived, the PUT's and any heal's. deadline is the PUT's
// on the schedule's clock.
func (self *clusterFaults) armDriveMiss(ctx context.Context, plan *sidecartest.FaultPlan, key string, deadline time.Time) (*clusterFaultDriveMiss, error) {
	cluster := self.layout.cluster(plan.ClusterId)
	if cluster == nil {
		return nil, fmt.Errorf("the layout has no cluster %s", plan.ClusterId)
	}
	if self.settings.DriveMissHostIndex < 0 || len(cluster.Hosts.Hooks) <= self.settings.DriveMissHostIndex {
		return nil, fmt.Errorf("cluster %s has no host %d", plan.ClusterId, self.settings.DriveMissHostIndex)
	}
	if slices.Contains(self.down(plan.ClusterId), self.settings.DriveMissHostIndex) {
		return nil, fmt.Errorf("cluster %s's host %d is down, so none of its drives can miss a PUT", plan.ClusterId, self.settings.DriveMissHostIndex)
	}
	address := cluster.Hosts.Hooks[self.settings.DriveMissHostIndex]
	values := url.Values{"point": {clusterFaultCommitArrived}, "object": {key}, "drive": {self.settings.DriveMissDrive}}
	if _, _, err := self.hook(ctx, address, "arm", values); err != nil {
		return nil, err
	}
	hold := &clusterFaultDriveMiss{address: address, storeDeadline: StoreTime(self.net, deadline)}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.driveMisses[plan.ClusterId] = append(self.driveMisses[plan.ClusterId], hold)
	return hold, nil
}

// Ends every DriveMiss hold on clusterId: waits on the store's clock past each PUT's deadline, so its commit is
// refused at its gate, and lets the held commits go, a heal's then landing.
func (self *clusterFaults) endDriveMisses(ctx context.Context, clusterId string) error {
	holds := func() []*clusterFaultDriveMiss {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		holds := self.driveMisses[clusterId]
		delete(self.driveMisses, clusterId)
		return holds
	}()
	ctx = context.WithoutCancel(ctx)
	errs := []error{}
	for _, hold := range holds {
		if wait := time.Until(hold.storeDeadline.Add(self.settings.DeadlineSlack)); 0 < wait {
			<-time.After(wait)
		}
		if _, _, err := self.hook(ctx, hold.address, "release", url.Values{"point": {clusterFaultCommitArrived}}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Keeps what carrying a fault out failed on, and returns the error the caller sees: a transport error, since the
// store never got the request the plan meant.
func (self *clusterFaults) failed(err error, write bool) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errs = append(self.errs, err)
	return blobTransportError(write)
}

// What carrying out this Net's cluster faults failed on.
func (self *clusterFaults) problems() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	problems := []string{}
	for _, err := range self.errs {
		problems = append(problems, fmt.Sprintf("a cluster fault was not carried out: %v", err))
	}
	return problems
}

// Carries plan out around serve, the request it fired on: QuorumLoss and DriveMiss, which only reach here on a
// layout (admitClusterFault). key is the request's, and write whether it writes.
func (self *clusterFaults) carryOut(ctx context.Context, plan *sidecartest.FaultPlan, cluster sidecar.BlobCluster, key string, write bool, serve func() error) error {
	switch plan.Result {
	case sidecartest.QuorumLoss:
		if err := self.loseQuorum(ctx, plan, write); err != nil {
			return self.failed(err, write)
		}
		err := serve()
		if restoreErr := self.afterQuorumLoss(ctx, plan); restoreErr != nil {
			self.failed(restoreErr, write)
		}
		if err == nil {
			// the store answered as if its quorum stood: the plan was not carried out
			return self.failed(fmt.Errorf("a %s %s on cluster %s answered with its quorum lost", plan.Op, key, plan.ClusterId), write)
		}
		return err
	case sidecartest.DriveMiss:
		deadline := sidecar.BlobRequestDeadline(ctx, self.net.Clock().Now(), cluster.RequestTimeout())
		if _, err := self.armDriveMiss(ctx, plan, key, deadline); err != nil {
			return self.failed(err, write)
		}
		// the hold stands until a host returns or the Net closes (endDriveMisses)
		return serve()
	}
	return serve()
}

// Checks that the layout's S3 answers on every host and that every host coordinates a write: what a Net on a layout
// checks first, failing rather than skipping when the clusters are down, and never starting on a cluster a fault
// before it left short of its quorum.
func checkLayoutUp(ctx context.Context, layout *ClusterLayout, settings *ClusterFaultSettings) error {
	client := &http.Client{Timeout: settings.CallTimeout}
	for _, cluster := range []*LayoutCluster{layout.Metadata, layout.Segment} {
		for _, endpoint := range cluster.Config.Endpoints {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint+"/minio/health/live", nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				return fmt.Errorf("%w: %s: %v", errLayoutDown, endpoint, err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("%w: %s answers %d", errLayoutDown, endpoint, response.StatusCode)
			}
		}
		if err := awaitWritable(ctx, cluster, nil, settings); err != nil {
			return fmt.Errorf("%w: %v", errLayoutDown, err)
		}
	}
	return nil
}
