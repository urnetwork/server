package server

// ServiceHost.Blobs on the file clusters (§19.6, X13; EVSB-M2-02): sidecar.BlobStore over D-OPS-20's
// MinIO build, one bucket per cluster, each cluster with its own endpoints, credentials and
// unversioned bucket. It keeps host_blobs.go's promises: every write is conditional, and a PUT or
// DELETE that ends in anything but a definite answer reports sidecar.BlobUnknownError.
//
// This is a surface apart from blob.go's BlobStore, which keeps its overwriting Put and its
// "deliberately no Delete" for stats retention (§11.3). What lives where:
//
//   - file_cluster.go, the vault file, FileClusters and each cluster's operations;
//   - file_cluster_transport.go, the deadline header, the cancel at a request's own deadline, and
//     the hosts a request picks among;
//   - file_cluster_admin.go, admin info, which refuses a cluster with more than one pool before any
//     write (owner decision 145), says whether its build passed the settle suite (SettlePassed), and
//     feeds the sentinel readiness §19.4's evs and gkm blocks gate on.
//
// The clusters are LAN-only, with no presigned URLs (D-OPS-4), which TestServerCallsNoPresignedMethod
// holds for all of server. No secret is in code or fixtures: credentials come only from vault's
// file-clusters-<service>.yml (plan-OPS-small-readings #7). Each names the block's own MinIO user on
// each cluster it reaches, which xops makes with the policy templates/file-clusters/policy-<service>.json.j2
// renders for that cluster (OPS-M2-04, xops's FILE-CLUSTERS.md): api reaches only proj/, projrows/, rev/
// and hold/ on the first cluster, gkm the metadata clusters, evs and taskworker every cluster, and evs
// deletes nothing on the segment cluster until §11.13's purge turns that on. Every such user holds
// admin:ServerInfo, which admin info below needs.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
)

// The vault file each block reads, by service, one entry per cluster the block reaches
// (plan-OPS-small-readings #7). It is a basename, since a block's secret_files mount by basename and a
// block mounts only its own; xops's playbook reads the same file to make the users it names:
//
//	clusters:
//	  meta-1:
//	    kind: metadata        # or segment
//	    first: true           # the one metadata cluster holding place/, proj/, rev/ and hold/
//	    endpoints: [<host>:<port>, ...]
//	    access_key: <this block's MinIO user on this cluster>
//	    secret_key: <its secret>
//	    bucket: <the cluster's bucket>
//	    tls: false
//	    region: us-east-1
const fileClustersResourceNameFormat = "file-clusters-%s.yml"

// §11.8's kinds of cluster.
const (
	FileClusterKindMetadata = "metadata"
	FileClusterKindSegment  = "segment"
)

// §19.6: objects carry this content type and no other.
const fileClusterContentType = "application/octet-stream"

// The header a checksum rides, through PutObjectOptions.UserMetadata, which passes an amz header as
// it is (minio-go's PutObjectOptions.Header).
const fileClusterChecksumHeader = "x-amz-checksum-sha256"

// S3's ceiling on one LIST's keys. A list asks one more than its limit, to learn whether more follow,
// so a limit stays below this and a LIST stays one request.
const fileClusterListMaxKeyCount = 1000

// What the store answers when a body does not match the checksum it named. Nothing was written.
var fileClusterChecksumErrorCodes = []string{"BadDigest", "XAmzContentChecksumMismatch", "XAmzContentSHA256Mismatch"}

var (
	// A write on a cluster whose admin info has not shown exactly one pool (owner decision 145): sent
	// nowhere, so nothing was written.
	ErrFileClusterPools = errors.New("file cluster has not shown exactly one pool")
	// The vault file's text breaks a rule. Nothing is built.
	ErrFileClusterConfig = errors.New("file cluster config invalid")
)

type FileClusterSettings struct {
	// §19.6's store request deadline, 10 s on both clusters: what each request carries as its deadline
	// and what the client waits for a response after the request's last byte.
	RequestTimeout time.Duration
	// How long a dial may take before the request fails. 5 s, since the clusters are on the LAN and
	// answer at once, so this bounds only a dead host.
	DialTimeout time.Duration
	// A host whose request failed is passed over this long while another host is not, so a request
	// after a failure moves hosts. 30 s.
	HostFailureHoldoff time.Duration
	// How often each cluster's admin info and sentinel are read. 30 s, well inside §19.11's 60 s
	// config reread, so a recorded build reads as passed within a minute.
	CheckInterval time.Duration
	// How old the last good check may be before readiness, writes' pool check and SettlePassed fail
	// closed. 90 s, three checks.
	CheckStaleAfter time.Duration
	// The most of an admin info answer read, past which the read fails. 16 MiB: a cluster's answer
	// is tens of KiB, and a host that sends more is not answering admin info.
	AdminInfoMaxByteCount int64
	// The clock deadlines and checks read. nil is real time.
	Clock sidecar.Clock
}

func DefaultFileClusterSettings() *FileClusterSettings {
	return &FileClusterSettings{
		RequestTimeout:        10 * time.Second,
		DialTimeout:           5 * time.Second,
		HostFailureHoldoff:    30 * time.Second,
		CheckInterval:         30 * time.Second,
		CheckStaleAfter:       90 * time.Second,
		AdminInfoMaxByteCount: 16 * 1024 * 1024,
	}
}

// One cluster, as the vault file names it.
type FileClusterConfig struct {
	ClusterId string
	// FileClusterKindMetadata or FileClusterKindSegment.
	Kind string
	// The metadata cluster holding place/, proj/, projrows/, rev/, hold/ and per-sidecar gkm/ (§11.8).
	First bool
	// host:port of each MinIO host, at least one.
	Endpoints []string
	AccessKey string
	SecretKey string
	Bucket    string
	Tls       bool
	Region    string
}

type fileClustersYaml struct {
	Clusters map[string]*struct {
		Kind      string   `yaml:"kind"`
		First     bool     `yaml:"first"`
		Endpoints []string `yaml:"endpoints"`
		AccessKey string   `yaml:"access_key"`
		SecretKey string   `yaml:"secret_key"`
		Bucket    string   `yaml:"bucket"`
		Tls       bool     `yaml:"tls"`
		Region    string   `yaml:"region"`
	} `yaml:"clusters"`
}

// The clusters a vault file's yaml names, checked: at least one metadata cluster, exactly one of them
// first, at most one segment cluster, and every cluster with endpoints, credentials, a bucket and a
// region. The first comes first, then the other metadata clusters by id, then the segment cluster.
func parseFileClusterConfigs(y *fileClustersYaml) ([]*FileClusterConfig, error) {
	configs := []*FileClusterConfig{}
	for clusterId, cluster := range y.Clusters {
		if clusterId == "" || cluster == nil {
			return nil, fmt.Errorf("%w: an empty cluster", ErrFileClusterConfig)
		}
		config := &FileClusterConfig{
			ClusterId: clusterId,
			Kind:      cluster.Kind,
			First:     cluster.First,
			Endpoints: slices.Clone(cluster.Endpoints),
			AccessKey: cluster.AccessKey,
			SecretKey: cluster.SecretKey,
			Bucket:    cluster.Bucket,
			Tls:       cluster.Tls,
			Region:    cluster.Region,
		}
		switch {
		case config.Kind != FileClusterKindMetadata && config.Kind != FileClusterKindSegment:
			return nil, fmt.Errorf("%w: %s has kind %q", ErrFileClusterConfig, clusterId, config.Kind)
		case config.First && config.Kind != FileClusterKindMetadata:
			return nil, fmt.Errorf("%w: %s is first and not a metadata cluster", ErrFileClusterConfig, clusterId)
		case len(config.Endpoints) == 0 || slices.Contains(config.Endpoints, ""):
			return nil, fmt.Errorf("%w: %s has no endpoint", ErrFileClusterConfig, clusterId)
		case config.AccessKey == "" || config.SecretKey == "":
			return nil, fmt.Errorf("%w: %s has no credentials", ErrFileClusterConfig, clusterId)
		case config.Bucket == "":
			return nil, fmt.Errorf("%w: %s has no bucket", ErrFileClusterConfig, clusterId)
		case config.Region == "":
			// with no region the SDK looks the bucket's up first, a hidden round trip
			return nil, fmt.Errorf("%w: %s has no region", ErrFileClusterConfig, clusterId)
		}
		configs = append(configs, config)
	}
	rank := func(config *FileClusterConfig) int {
		switch {
		case config.First:
			return 0
		case config.Kind == FileClusterKindMetadata:
			return 1
		}
		return 2
	}
	slices.SortFunc(configs, func(a *FileClusterConfig, b *FileClusterConfig) int {
		if c := rank(a) - rank(b); c != 0 {
			return c
		}
		return strings.Compare(a.ClusterId, b.ClusterId)
	})
	firstCount := 0
	segmentCount := 0
	for _, config := range configs {
		if config.First {
			firstCount += 1
		}
		if config.Kind == FileClusterKindSegment {
			segmentCount += 1
		}
	}
	if firstCount != 1 {
		return nil, fmt.Errorf("%w: %d first clusters", ErrFileClusterConfig, firstCount)
	}
	if 1 < segmentCount {
		return nil, fmt.Errorf("%w: %d segment clusters", ErrFileClusterConfig, segmentCount)
	}
	return configs, nil
}

// Reads vault's file-clusters-<service>.yml and builds the clusters it names, starting each one's
// checks. settings may be nil for the defaults. The caller closes what it returns.
func LoadFileClusters(ctx context.Context, service string, settings *FileClusterSettings) (*FileClusters, error) {
	resource, err := Vault.SimpleResource(fmt.Sprintf(fileClustersResourceNameFormat, service))
	if err != nil {
		return nil, err
	}
	y := &fileClustersYaml{}
	if err := resource.UnmarshalYamlE(y); err != nil {
		return nil, err
	}
	configs, err := parseFileClusterConfigs(y)
	if err != nil {
		return nil, err
	}
	return NewFileClusters(ctx, configs, settings)
}

// sidecar.BlobStore over the clusters a block holds credentials for (§19.4): gkm has no segment
// cluster, and api only the first. Each cluster's checks run on a goroutine of their own from
// construction until Close. Safe for concurrent use.
type FileClusters struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *FileClusterSettings
	clock    sidecar.Clock
	// what place_new and each cluster's settle_passed_build are read from
	sidecarConfig func() *SidecarConfig

	clusterIdClusters map[string]*FileCluster
	// first first
	metadataClusterIds []string
	segmentClusterId   string
}

// Builds a client per host of every cluster, and starts the checks. settings may be nil for the
// defaults. Fails only on configs a client cannot be built from: a store that does not answer is
// what readiness reports.
func NewFileClusters(ctx context.Context, configs []*FileClusterConfig, settings *FileClusterSettings) (*FileClusters, error) {
	if settings == nil {
		settings = DefaultFileClusterSettings()
	}
	return newFileClusters(ctx, configs, settings, newFileClusterBaseTransport(settings), CurrentSidecarConfig)
}

func newFileClusters(
	ctx context.Context,
	configs []*FileClusterConfig,
	settings *FileClusterSettings,
	newBaseTransport fileClusterBaseTransportFunction,
	sidecarConfig func() *SidecarConfig,
) (*FileClusters, error) {
	clock := settings.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &FileClusters{
		ctx:               cancelCtx,
		cancel:            cancel,
		settings:          settings,
		clock:             clock,
		sidecarConfig:     sidecarConfig,
		clusterIdClusters: map[string]*FileCluster{},
	}
	for _, config := range configs {
		if _, exists := self.clusterIdClusters[config.ClusterId]; exists {
			cancel()
			return nil, fmt.Errorf("%w: %s twice", ErrFileClusterConfig, config.ClusterId)
		}
		hosts := []*fileClusterHost{}
		for _, endpoint := range config.Endpoints {
			host, err := newFileClusterHost(endpoint, config, settings, clock, newBaseTransport)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("cluster %s host %s: %w", config.ClusterId, endpoint, err)
			}
			hosts = append(hosts, host)
		}
		cluster := &FileCluster{
			clusters: self,
			config:   config,
			picker:   &fileClusterHostPicker{hosts: hosts, holdoff: settings.HostFailureHoldoff, clock: clock},
		}
		self.clusterIdClusters[config.ClusterId] = cluster
		if config.Kind == FileClusterKindMetadata {
			if config.First {
				self.metadataClusterIds = slices.Insert(self.metadataClusterIds, 0, config.ClusterId)
			} else {
				self.metadataClusterIds = append(self.metadataClusterIds, config.ClusterId)
			}
		} else {
			self.segmentClusterId = config.ClusterId
		}
	}
	if len(self.metadataClusterIds) == 0 || !self.clusterIdClusters[self.metadataClusterIds[0]].config.First {
		cancel()
		return nil, fmt.Errorf("%w: no first metadata cluster", ErrFileClusterConfig)
	}
	go self.run()
	return self, nil
}

// Checks every cluster at once, then each CheckInterval until Close.
func (self *FileClusters) run() {
	for {
		self.Check(self.ctx)
		select {
		case <-self.ctx.Done():
			return
		case <-self.clock.After(self.settings.CheckInterval):
		}
	}
}

// Ends the checks. Requests in flight run on under their own ctx.
func (self *FileClusters) Close() {
	self.cancel()
}

func (self *FileClusters) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	cluster, ok := self.clusterIdClusters[clusterId]
	if !ok {
		return nil, fmt.Errorf("%w: %s", sidecar.ErrBlobClusterUnknown, clusterId)
	}
	return cluster, nil
}

func (self *FileClusters) FirstCluster() (sidecar.BlobCluster, error) {
	return self.Cluster(self.metadataClusterIds[0])
}

func (self *FileClusters) SegmentCluster() (sidecar.BlobCluster, error) {
	if self.segmentClusterId == "" {
		return nil, fmt.Errorf("%w: no segment cluster", sidecar.ErrBlobClusterUnknown)
	}
	return self.Cluster(self.segmentClusterId)
}

func (self *FileClusters) MetadataClusterIds() []string {
	return slices.Clone(self.metadataClusterIds)
}

// sidecar.yml's place_new, the first when it names none (§19.9). It may name a cluster this block
// holds no credentials for, which Cluster refuses and evs answers 503.
func (self *FileClusters) NewRepoClusterId() string {
	if placeNew := self.sidecarConfig().PlaceNew(); placeNew != "" {
		return placeNew
	}
	return self.metadataClusterIds[0]
}

// Whether every cluster this block holds is ready: §19.4's evs and gkm readiness.
func (self *FileClusters) Ready() bool {
	for _, cluster := range self.clusterIdClusters {
		if !cluster.Ready() {
			return false
		}
	}
	return true
}

// One cluster's operations: sidecar.BlobCluster. Safe for concurrent use.
type FileCluster struct {
	clusters *FileClusters
	config   *FileClusterConfig
	picker   *fileClusterHostPicker

	stateLock sync.Mutex
	// the last check that read admin info, and the last sentinel read that found it
	adminInfo    *FileClusterAdminInfo
	sentinelTime time.Time
}

func (self *FileCluster) ClusterId() string {
	return self.config.ClusterId
}

func (self *FileCluster) RequestTimeout() time.Duration {
	return self.clusters.settings.RequestTimeout
}

// The failure a client cannot tell from a lost answer marks its host, so the next request moves. A
// request its own caller ended says nothing about the host.
func (self *FileCluster) noteFailure(ctx context.Context, host *fileClusterHost, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	response, ok := fileClusterErrorResponse(err)
	if !ok || http.StatusInternalServerError <= response.StatusCode {
		self.picker.failed(host)
	}
}

// What the SDK's error says, when the store answered one.
func fileClusterErrorResponse(err error) (minio.ErrorResponse, bool) {
	var response minio.ErrorResponse
	if errors.As(err, &response) && response.StatusCode != 0 {
		return response, true
	}
	var pointer *minio.ErrorResponse
	if errors.As(err, &pointer) && pointer != nil && pointer.StatusCode != 0 {
		return *pointer, true
	}
	return minio.ErrorResponse{}, false
}

// What a PUT's failure means. Precondition and checksum are definite, nothing was written, and
// everything else leaves the outcome unknown however it reads (host_blobs.go). MinIO answers an If-Match
// on a missing key NoSuchKey, not 412, and host_blobs.go calls both a failed condition.
func fileClusterPutError(err error) error {
	if response, ok := fileClusterErrorResponse(err); ok {
		switch {
		case response.StatusCode == http.StatusPreconditionFailed, response.Code == minio.NoSuchKey:
			return sidecar.ErrBlobPrecondition
		case slices.Contains(fileClusterChecksumErrorCodes, response.Code):
			return sidecar.ErrBlobChecksum
		}
	}
	return &sidecar.BlobUnknownError{Err: err}
}

// What a read's failure means. Nothing about a read is unknown: it failed.
func fileClusterReadError(err error) error {
	if response, ok := fileClusterErrorResponse(err); ok {
		switch response.StatusCode {
		case http.StatusNotFound:
			return sidecar.ErrBlobNotFound
		case http.StatusRequestedRangeNotSatisfiable:
			return sidecar.ErrBlobRange
		}
	}
	return fmt.Errorf("blob read failed: %w", err)
}

// One PUT of body, conditional on header. sha256Hex signs the payload, so the store checks the bytes
// it got are the bytes signed, and checksum, when set, rides as x-amz-checksum-sha256.
func (self *FileCluster) put(
	ctx context.Context,
	key string,
	body io.Reader,
	byteCount int64,
	sha256Hex string,
	match func(options *minio.PutObjectOptions),
	options *sidecar.BlobPutOptions,
	deadline bool,
) (string, error) {
	if err := self.checkPools(); err != nil {
		return "", err
	}
	putOptions := minio.PutObjectOptions{
		ContentType:          fileClusterContentType,
		DisableMultipart:     true,
		DisableContentSha256: true,
		UserMetadata:         map[string]string{},
	}
	match(&putOptions)
	if options != nil {
		if 0 < len(options.Sha256) {
			putOptions.UserMetadata[fileClusterChecksumHeader] = base64.StdEncoding.EncodeToString(options.Sha256)
		}
		for name, value := range options.Metadata {
			putOptions.UserMetadata[name] = value
		}
	}
	requestCtx := ctx
	if deadline {
		requestCtx = withFileClusterDeadline(ctx, self.RequestTimeout())
	}
	host := self.picker.pick()
	info, err := host.core.PutObject(requestCtx, self.config.Bucket, key, body, byteCount, "", sha256Hex, putOptions)
	if err != nil {
		self.noteFailure(ctx, host, err)
		return "", fileClusterPutError(err)
	}
	return info.ETag, nil
}

func (self *FileCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if err := options.Validate(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return self.put(ctx, key, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]), func(putOptions *minio.PutObjectOptions) {
		putOptions.SetMatchETagExcept("*")
	}, options, true)
}

func (self *FileCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if etag == "" {
		return "", fmt.Errorf("%w: an If-Match with no etag", sidecar.ErrBlobPutOptions)
	}
	if err := options.Validate(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return self.put(ctx, key, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]), func(putOptions *minio.PutObjectOptions) {
		putOptions.SetMatchETag(etag)
	}, options, true)
}

// Ends a read with an error at the first byte past byteCount, so a body longer than declared fails
// rather than go out cut short.
type fileClusterExactReader struct {
	source    io.Reader
	remaining int64
}

func (self *fileClusterExactReader) Read(buffer []byte) (int, error) {
	if self.remaining == 0 {
		var extra [1]byte
		count, err := self.source.Read(extra[:])
		if 0 < count {
			return 0, errors.New("a body longer than declared")
		}
		return 0, err
	}
	count, err := self.source.Read(buffer[:min(int64(len(buffer)), self.remaining)])
	self.remaining -= int64(count)
	return count, err
}

// The segment's id is its SHA-256, so it signs the payload too, and the store refuses other bytes
// before the object exists. No deadline header: the uploader sets the pace, and the store bounds it
// 40 s after its last byte (§19.6).
func (self *FileCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	if len(checksumSha256) != 32 {
		return "", fmt.Errorf("%w: a segment names its 32 byte checksum", sidecar.ErrBlobPutOptions)
	}
	if byteCount < 0 {
		return "", fmt.Errorf("%w: a byte count of %d", sidecar.ErrBlobPutOptions, byteCount)
	}
	return self.put(ctx, key, &fileClusterExactReader{source: body, remaining: byteCount}, byteCount, hex.EncodeToString(checksumSha256), func(putOptions *minio.PutObjectOptions) {
		putOptions.SetMatchETagExcept("*")
	}, &sidecar.BlobPutOptions{Sha256: checksumSha256}, false)
}

// The object headers describe: ETag, LastModified and metadata. byteCount is the whole object's.
func fileClusterBlobInfo(key string, info minio.ObjectInfo, byteCount int64) sidecar.BlobInfo {
	blobInfo := sidecar.BlobInfo{
		Key:          key,
		ByteCount:    byteCount,
		Etag:         info.ETag,
		LastModified: info.LastModified,
	}
	for name, value := range info.UserMetadata {
		if blobInfo.Metadata == nil {
			blobInfo.Metadata = map[string]string{}
		}
		blobInfo.Metadata[strings.ToLower(name)] = value
	}
	return blobInfo
}

func (self *FileCluster) get(ctx context.Context, key string, getOptions minio.GetObjectOptions) (io.ReadCloser, minio.ObjectInfo, http.Header, error) {
	host := self.picker.pick()
	body, info, header, err := host.core.GetObject(withFileClusterDeadline(ctx, self.RequestTimeout()), self.config.Bucket, key, getOptions)
	if err != nil {
		self.noteFailure(ctx, host, err)
		return nil, minio.ObjectInfo{}, nil, fileClusterReadError(err)
	}
	return body, info, header, nil
}

// One round trip: Core.GetObject sends its GET at once, where Client.GetObject is lazy.
func (self *FileCluster) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	body, info, _, err := self.get(ctx, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return &sidecar.BlobObject{BlobInfo: fileClusterBlobInfo(key, info, info.Size), Body: body}, nil
}

func (self *FileCluster) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	if offset < 0 || byteCount <= 0 {
		return nil, fmt.Errorf("a range of %d bytes at %d", byteCount, offset)
	}
	getOptions := minio.GetObjectOptions{}
	if err := getOptions.SetRange(offset, offset+byteCount-1); err != nil {
		return nil, err
	}
	body, info, header, err := self.get(ctx, key, getOptions)
	if err != nil {
		return nil, err
	}
	// Content-Range: bytes first-last/whole, the only place the whole object's size is
	_, wholeText, found := strings.Cut(header.Get("Content-Range"), "/")
	whole, err := strconv.ParseInt(wholeText, 10, 64)
	if !found || err != nil {
		body.Close()
		return nil, fmt.Errorf("a Content-Range of %q", header.Get("Content-Range"))
	}
	return &sidecar.BlobObject{BlobInfo: fileClusterBlobInfo(key, info, whole), Body: body}, nil
}

func (self *FileCluster) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	host := self.picker.pick()
	info, err := host.core.StatObject(withFileClusterDeadline(ctx, self.RequestTimeout()), self.config.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		self.noteFailure(ctx, host, err)
		return nil, fileClusterReadError(err)
	}
	blobInfo := fileClusterBlobInfo(key, info, info.Size)
	return &blobInfo, nil
}

// One LIST: it asks for one key past limit to learn whether more follow, and stops there, so the
// listing never pages behind the caller. A limit at or past S3's 1,000 answers at most 999, with more
// set, which "at most limit" allows.
func (self *FileCluster) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	if limit <= 0 {
		return nil, false, fmt.Errorf("a list limit of %d", limit)
	}
	limit = min(limit, fileClusterListMaxKeyCount-1)
	host := self.picker.pick()
	listCtx, cancel := context.WithCancel(withFileClusterDeadline(ctx, self.RequestTimeout()))
	defer cancel()
	objects := []*sidecar.BlobInfo{}
	more := false
	var listErr error
	for object := range host.core.ListObjectsIter(listCtx, self.config.Bucket, minio.ListObjectsOptions{
		Prefix:     keyPrefix,
		StartAfter: startAfter,
		MaxKeys:    limit + 1,
		Recursive:  true,
	}) {
		if object.Err != nil {
			listErr = object.Err
			break
		}
		if len(objects) == limit {
			more = true
			break
		}
		objects = append(objects, &sidecar.BlobInfo{
			Key:          object.Key,
			ByteCount:    object.Size,
			Etag:         object.ETag,
			LastModified: object.LastModified,
		})
	}
	if listErr != nil {
		self.noteFailure(ctx, host, listErr)
		return nil, false, fmt.Errorf("blob list failed: %w", listErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return objects, more, nil
}

// Unconditional, since the store evaluates no precondition on a DELETE (§11.3), and nil for a key
// already gone, which MinIO answers 204 too.
func (self *FileCluster) Delete(ctx context.Context, key string) error {
	if err := self.checkPools(); err != nil {
		return err
	}
	host := self.picker.pick()
	if err := host.core.RemoveObject(withFileClusterDeadline(ctx, self.RequestTimeout()), self.config.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		self.noteFailure(ctx, host, err)
		return &sidecar.BlobUnknownError{Err: err}
	}
	return nil
}

// The sentinel xops writes on every cluster, read for readiness.
const fileClusterSentinelKey = "sentinel/_"

// Reads the sentinel: the cluster answers, with this block's credentials, in its bucket.
func (self *FileCluster) checkSentinel(ctx context.Context) {
	if _, err := self.Head(ctx, fileClusterSentinelKey); err != nil {
		glog.Infof("[filecluster]%s: sentinel not read: %v\n", self.config.ClusterId, err)
		return
	}
	now := self.clusters.clock.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sentinelTime = now
}

// Whether this cluster serves: its sentinel and its admin info were read within CheckStaleAfter, and
// admin info shows exactly one pool. Anything unread is not ready.
func (self *FileCluster) Ready() bool {
	if self.checkPools() != nil {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.sentinelTime.IsZero() && self.clusters.clock.Now().Sub(self.sentinelTime) < self.clusters.settings.CheckStaleAfter
}

var _ sidecar.BlobStore = (*FileClusters)(nil)
var _ sidecar.BlobCluster = (*FileCluster)(nil)
