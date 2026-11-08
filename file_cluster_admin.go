package server

// Each cluster's admin info (§19.6, FS-63, plan-EVSB-settle-gate): a SigV4-signed GET of
// /minio/admin/v3/info through minio-go's pkg/signer, parsed here for the fields this needs. madmin-go
// is not linked: it is AGPL-3.0 (minio/CREDITS), which §4.7's license rule refuses, and its
// InfoMessage only names the fields read below.
//
// What it decides:
//
//   - a cluster that has not shown exactly one pool takes no write (owner decision 145): a PUT can
//     pass its precondition in a pool its key is not in (§11.3), so the check comes before any write,
//     on metadata and segment clusters both, and an unread or stale admin info refuses too;
//   - SettlePassed is true only while every host's build equals sidecar.yml's settle_passed_build for
//     the cluster, so an absent entry, an unreadable admin info or any mismatch reads false, and an
//     upgrade reads false until the operator records the new build after filecluster-suite passes;
//   - raw usage and inodes, which the budget loop turns into {nvb_<set>}'s at_limit flag at 80% and
//     70% (plan-EVSB-nvb-fields, §19.9).
//
// The block's MinIO user needs admin:ServerInfo in its policy for any of this, and without it the
// cluster refuses writes and reads not passed, which fails closed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7/pkg/signer"

	"github.com/urnetwork/glog"
)

// MinIO's admin route for server info.
const fileClusterAdminInfoPath = "/minio/admin/v3/info"

// The SHA-256 of an empty payload, which a GET signs.
const fileClusterEmptyPayloadSha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// The fields of madmin's InfoMessage this reads, by their JSON names.
type fileClusterAdminInfoJson struct {
	Mode    string `json:"mode"`
	Backend struct {
		BackendType  string `json:"backendType"`
		TotalSets    []int  `json:"totalSets"`
		DrivesPerSet []int  `json:"totalDrivesPerSet"`
		StandardSc   int    `json:"standardSCParity"`
	} `json:"backend"`
	Servers []struct {
		State       string `json:"state"`
		Endpoint    string `json:"endpoint"`
		Version     string `json:"version"`
		CommitId    string `json:"commitID"`
		PoolNumber  int    `json:"poolNumber"`
		PoolNumbers []int  `json:"poolNumbers"`
		Drives      []struct {
			Endpoint   string `json:"endpoint"`
			State      string `json:"state"`
			Healing    bool   `json:"healing"`
			TotalSpace uint64 `json:"totalspace"`
			UsedSpace  uint64 `json:"usedspace"`
			UsedInodes uint64 `json:"used_inodes"`
			FreeInodes uint64 `json:"free_inodes"`
			PoolIndex  int    `json:"pool_index"`
			SetIndex   int    `json:"set_index"`
			DiskIndex  int    `json:"disk_index"`
		} `json:"drives"`
	} `json:"servers"`
	Pools map[string]map[string]struct {
		RawUsage    uint64 `json:"rawUsage"`
		RawCapacity uint64 `json:"rawCapacity"`
		HealDisks   int    `json:"healDisks"`
	} `json:"pools"`
}

// One host as admin info reports it.
type FileClusterHostInfo struct {
	Endpoint string
	// "online" while the host answers.
	State string
	// The build's commit, what settle_passed_build names.
	CommitId string
	Version  string
}

// One drive as admin info reports it: what §19.6's protection states and the budget loop's
// busiest-drive-not-healing rule read.
type FileClusterDriveInfo struct {
	Endpoint  string
	State     string
	Healing   bool
	PoolIndex int
	SetIndex  int
	DiskIndex int
	// Bytes and inodes, for §19.9's raw and inode lines.
	TotalByteCount uint64
	UsedByteCount  uint64
	UsedInodeCount uint64
	FreeInodeCount uint64
}

// One cluster's admin info, as read at ReadTime. Immutable once read.
type FileClusterAdminInfo struct {
	// When it was read, by this process's clock.
	ReadTime time.Time
	// The pools it reports, by every field that counts them, the most of them: one is all owner
	// decision 145 allows, and zero means it said nothing that counts.
	PoolCount int
	Hosts     []*FileClusterHostInfo
	Drives    []*FileClusterDriveInfo
	// Summed over its sets, and over its drives for inodes.
	RawUsageByteCount    uint64
	RawCapacityByteCount uint64
	UsedInodeCount       uint64
	FreeInodeCount       uint64
}

// Parses an admin info answer.
func parseFileClusterAdminInfo(body []byte, readTime time.Time) (*FileClusterAdminInfo, error) {
	y := &fileClusterAdminInfoJson{}
	if err := json.Unmarshal(body, y); err != nil {
		return nil, err
	}
	info := &FileClusterAdminInfo{ReadTime: readTime}
	// pool indexes from 0, as a drive's pool_index and the pools map count them
	poolIndexes := map[int]bool{}
	for _, server := range y.Servers {
		info.Hosts = append(info.Hosts, &FileClusterHostInfo{
			Endpoint: server.Endpoint,
			State:    server.State,
			CommitId: server.CommitId,
			Version:  server.Version,
		})
		// A host's pool numbers count from 1 (MinIO's getLocalServerProperty adds 1 to each local
		// endpoint's pool index), and poolNumber repeats poolNumbers' entry when there is exactly one,
		// else reads math.MaxInt for unset.
		poolNumbers := server.PoolNumbers
		if len(poolNumbers) == 0 && server.PoolNumber != 0 && server.PoolNumber != math.MaxInt {
			poolNumbers = []int{server.PoolNumber}
		}
		for _, poolNumber := range poolNumbers {
			if poolNumber < 1 {
				return nil, fmt.Errorf("a host reports pool number %d", poolNumber)
			}
			poolIndexes[poolNumber-1] = true
		}
		for _, drive := range server.Drives {
			info.Drives = append(info.Drives, &FileClusterDriveInfo{
				Endpoint:       drive.Endpoint,
				State:          drive.State,
				Healing:        drive.Healing,
				PoolIndex:      drive.PoolIndex,
				SetIndex:       drive.SetIndex,
				DiskIndex:      drive.DiskIndex,
				TotalByteCount: drive.TotalSpace,
				UsedByteCount:  drive.UsedSpace,
				UsedInodeCount: drive.UsedInodes,
				FreeInodeCount: drive.FreeInodes,
			})
			if drive.PoolIndex < 0 {
				return nil, fmt.Errorf("a drive reports pool index %d", drive.PoolIndex)
			}
			poolIndexes[drive.PoolIndex] = true
			info.UsedInodeCount += drive.UsedInodes
			info.FreeInodeCount += drive.FreeInodes
		}
	}
	for _, sets := range y.Pools {
		for _, set := range sets {
			info.RawUsageByteCount += set.RawUsage
			info.RawCapacityByteCount += set.RawCapacity
		}
	}
	info.PoolCount = max(len(y.Backend.TotalSets), len(y.Pools), len(poolIndexes))
	if len(y.Servers) == 0 {
		// no server, so the pool indexes counted nothing real
		info.PoolCount = max(len(y.Backend.TotalSets), len(y.Pools))
	}
	return info, nil
}

// Reads the cluster's admin info from one host, in one signed request bounded by RequestTimeout.
func (self *FileCluster) AdminInfo(ctx context.Context) (*FileClusterAdminInfo, error) {
	host := self.picker.pick()
	scheme := "http"
	if self.config.Tls {
		scheme = "https"
	}
	endpoint := url.URL{Scheme: scheme, Host: host.endpoint, Path: fileClusterAdminInfoPath, RawQuery: "metrics=false"}
	requestCtx, cancel := context.WithTimeout(ctx, self.RequestTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Amz-Content-Sha256", fileClusterEmptyPayloadSha256)
	request = signer.SignV4(*request, self.config.AccessKey, self.config.SecretKey, "", self.config.Region)
	httpResponse, err := self.adminClient(host).Do(request)
	if err != nil {
		self.noteFailure(ctx, host, err)
		return nil, err
	}
	defer httpResponse.Body.Close()
	maxByteCount := self.clusters.settings.AdminInfoMaxByteCount
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxByteCount+1))
	if err != nil {
		return nil, err
	}
	if maxByteCount < int64(len(body)) {
		return nil, fmt.Errorf("admin info past %d bytes", maxByteCount)
	}
	if httpResponse.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin info answered %d", httpResponse.StatusCode)
	}
	return parseFileClusterAdminInfo(body, self.clusters.clock.Now())
}

// The admin route goes through the host's own transport, without the deadline header.
func (self *FileCluster) adminClient(host *fileClusterHost) *http.Client {
	return &http.Client{
		Transport: host.transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The admin info last read, nil when none was or it is past CheckStaleAfter.
func (self *FileCluster) freshAdminInfo() *FileClusterAdminInfo {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.adminInfo == nil || self.clusters.settings.CheckStaleAfter <= self.clusters.clock.Now().Sub(self.adminInfo.ReadTime) {
		return nil
	}
	return self.adminInfo
}

// ErrFileClusterPools unless fresh admin info shows exactly one pool.
func (self *FileCluster) checkPools() error {
	info := self.freshAdminInfo()
	switch {
	case info == nil:
		return fmt.Errorf("%w: %s's admin info is unread or stale", ErrFileClusterPools, self.config.ClusterId)
	case info.PoolCount != 1:
		return fmt.Errorf("%w: %s reports %d", ErrFileClusterPools, self.config.ClusterId, info.PoolCount)
	}
	return nil
}

// FS-114's per-cluster flag: true only while fresh admin info shows every host at sidecar.yml's
// settle_passed_build for this cluster. See the file header.
func (self *FileCluster) SettlePassed() bool {
	entry, ok := self.clusters.sidecarConfig().FileCluster(self.config.ClusterId)
	if !ok || entry.SettlePassedBuild == "" {
		return false
	}
	info := self.freshAdminInfo()
	if info == nil || len(info.Hosts) == 0 || info.PoolCount != 1 {
		return false
	}
	for _, host := range info.Hosts {
		if host.CommitId != entry.SettlePassedBuild {
			return false
		}
	}
	return true
}

// The latest admin info, nil when none was read within CheckStaleAfter.
func (self *FileCluster) LastAdminInfo() *FileClusterAdminInfo {
	return self.freshAdminInfo()
}

// One pass of every cluster's checks: admin info, then the sentinel. A failed read keeps the last good
// one, which goes stale in CheckStaleAfter and then fails closed.
func (self *FileClusters) Check(ctx context.Context) {
	for _, cluster := range self.clusterIdClusters {
		info, err := cluster.AdminInfo(ctx)
		if err != nil {
			glog.Infof("[filecluster]%s: admin info not read: %v\n", cluster.config.ClusterId, err)
		} else {
			if info.PoolCount != 1 {
				glog.Errorf("[filecluster]%s reports %d pools, so it takes no write (owner decision 145)\n", cluster.config.ClusterId, info.PoolCount)
			}
			func() {
				cluster.stateLock.Lock()
				defer cluster.stateLock.Unlock()
				cluster.adminInfo = info
			}()
		}
		cluster.checkSentinel(ctx)
	}
}

// Whether admin info reads the cluster at §19.9's limit: 80% of raw capacity used, or 70% of inodes.
// ok is false while it is unread or stale.
func (self *FileCluster) AtLimit(rawUsedPercent int, inodeUsedPercent int) (atLimit bool, ok bool) {
	info := self.freshAdminInfo()
	if info == nil {
		return false, false
	}
	if 0 < info.RawCapacityByteCount && uint64(rawUsedPercent)*info.RawCapacityByteCount <= 100*info.RawUsageByteCount {
		return true, true
	}
	inodes := info.UsedInodeCount + info.FreeInodeCount
	if 0 < inodes && uint64(inodeUsedPercent)*inodes <= 100*info.UsedInodeCount {
		return true, true
	}
	return false, true
}
