package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// One request the fake store saw.
type fakeS3Request struct {
	host     string
	method   string
	path     string
	query    string
	deadline string
	header   http.Header
}

type fakeS3Object struct {
	body []byte
	etag string
}

// A MinIO in memory, as a host's base RoundTripper sees it: enough S3 for one cluster's operations,
// and admin info. Every request is recorded. Safe for concurrent use.
type fakeS3 struct {
	stateLock sync.Mutex
	requests  []*fakeS3Request
	objects   map[string]*fakeS3Object
	etagCount int
	// answers every request but admin info with this status when set
	failStatus int
	// hosts whose requests fail at the transport
	downHosts map[string]bool
	// the admin info answer
	adminInfo []byte
	// requests whose path has this prefix wait for their ctx to end
	holdPrefix       string
	held             chan struct{}
	lastModifiedTime time.Time
}

func newFakeS3(poolCount int, commitId string) *fakeS3 {
	return &fakeS3{
		objects:          map[string]*fakeS3Object{},
		downHosts:        map[string]bool{},
		adminInfo:        fakeAdminInfo(poolCount, commitId),
		held:             make(chan struct{}, 16),
		lastModifiedTime: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	}
}

// Admin info of one host per pool, each with two drives, in madmin's JSON as MinIO fills it: a host's
// pool numbers count from 1 and a drive's pool_index from 0.
func fakeAdminInfo(poolCount int, commitId string) []byte {
	servers := []map[string]any{}
	totalSets := []int{}
	pools := map[string]any{}
	for pool := range poolCount {
		totalSets = append(totalSets, 1)
		pools[strconv.Itoa(pool)] = map[string]any{"0": map[string]any{"rawUsage": 100, "rawCapacity": 1000}}
		servers = append(servers, map[string]any{
			"state":       "online",
			"endpoint":    fmt.Sprintf("host-%d.example:9000", pool),
			"commitID":    commitId,
			"poolNumber":  pool + 1,
			"poolNumbers": []int{pool + 1},
			"drives": []map[string]any{
				{"endpoint": "d1", "state": "ok", "pool_index": pool, "used_inodes": 10, "free_inodes": 90},
				{"endpoint": "d2", "state": "ok", "healing": true, "pool_index": pool, "used_inodes": 10, "free_inodes": 90},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"mode":    "online",
		"backend": map[string]any{"backendType": "Erasure", "totalSets": totalSets},
		"servers": servers,
		"pools":   pools,
	})
	if err != nil {
		panic(err)
	}
	return body
}

func (self *fakeS3) transport(host string) http.RoundTripper {
	return fakeS3Transport(func(request *http.Request) (*http.Response, error) {
		return self.serve(host, request)
	})
}

type fakeS3Transport func(request *http.Request) (*http.Response, error)

func (self fakeS3Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

func fakeS3Response(request *http.Request, status int, header http.Header, body []byte) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}

func fakeS3Error(request *http.Request, status int, code string) *http.Response {
	body := fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
	return fakeS3Response(request, status, http.Header{"Content-Type": {"application/xml"}}, body)
}

func (self *fakeS3) objectHeader(object *fakeS3Object) http.Header {
	return http.Header{
		"Etag":           {`"` + object.etag + `"`},
		"Last-Modified":  {self.lastModifiedTime.Format(http.TimeFormat)},
		"Content-Type":   {"application/octet-stream"},
		"Content-Length": {strconv.Itoa(len(object.body))},
	}
}

func (self *fakeS3) serve(host string, request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		var err error
		if body, err = io.ReadAll(request.Body); err != nil {
			return nil, err
		}
	}
	self.stateLock.Lock()
	self.requests = append(self.requests, &fakeS3Request{
		host:     host,
		method:   request.Method,
		path:     request.URL.Path,
		query:    request.URL.RawQuery,
		deadline: request.Header.Get(fileClusterDeadlineHeader),
		header:   request.Header.Clone(),
	})
	hold := self.holdPrefix != "" && strings.HasPrefix(request.URL.Path, self.holdPrefix)
	down := self.downHosts[host]
	self.stateLock.Unlock()

	if hold {
		self.held <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	if down {
		return nil, errors.New("connection refused")
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if request.URL.Path == fileClusterAdminInfoPath {
		return fakeS3Response(request, http.StatusOK, http.Header{"Content-Type": {"application/json"}}, self.adminInfo), nil
	}
	if self.failStatus != 0 {
		return fakeS3Error(request, self.failStatus, "ServiceUnavailable"), nil
	}
	// bucket/key, so each cluster's bucket holds its own keys
	key := strings.TrimPrefix(request.URL.Path, "/")
	switch request.Method {
	case http.MethodPut:
		object, exists := self.objects[key]
		if request.Header.Get("If-None-Match") == "*" && exists {
			return fakeS3Error(request, http.StatusPreconditionFailed, "PreconditionFailed"), nil
		}
		if match := request.Header.Get("If-Match"); match != "" {
			if !exists {
				return fakeS3Error(request, http.StatusNotFound, "NoSuchKey"), nil
			}
			if match != `"`+object.etag+`"` {
				return fakeS3Error(request, http.StatusPreconditionFailed, "PreconditionFailed"), nil
			}
		}
		if payloadSha256 := request.Header.Get("X-Amz-Content-Sha256"); payloadSha256 != "" && payloadSha256 != "UNSIGNED-PAYLOAD" {
			sum := sha256.Sum256(body)
			if payloadSha256 != fmt.Sprintf("%x", sum) {
				return fakeS3Error(request, http.StatusBadRequest, "XAmzContentSHA256Mismatch"), nil
			}
		}
		self.etagCount += 1
		object = &fakeS3Object{body: body, etag: fmt.Sprintf("etag-%d", self.etagCount)}
		self.objects[key] = object
		return fakeS3Response(request, http.StatusOK, http.Header{"Etag": {`"` + object.etag + `"`}}, nil), nil
	case http.MethodGet:
		if strings.Contains(request.URL.RawQuery, "list-type=2") {
			return self.list(request), nil
		}
		object, exists := self.objects[key]
		if !exists {
			return fakeS3Error(request, http.StatusNotFound, "NoSuchKey"), nil
		}
		header := self.objectHeader(object)
		if byteRange := request.Header.Get("Range"); byteRange != "" {
			var first, last int
			fmt.Sscanf(byteRange, "bytes=%d-%d", &first, &last)
			last = min(last, len(object.body)-1)
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(object.body)))
			header.Set("Content-Length", strconv.Itoa(last-first+1))
			return fakeS3Response(request, http.StatusPartialContent, header, object.body[first:last+1]), nil
		}
		return fakeS3Response(request, http.StatusOK, header, object.body), nil
	case http.MethodHead:
		object, exists := self.objects[key]
		if !exists {
			return fakeS3Response(request, http.StatusNotFound, nil, nil), nil
		}
		response := fakeS3Response(request, http.StatusOK, self.objectHeader(object), nil)
		response.ContentLength = int64(len(object.body))
		return response, nil
	case http.MethodDelete:
		delete(self.objects, key)
		return fakeS3Response(request, http.StatusNoContent, nil, nil), nil
	}
	return fakeS3Error(request, http.StatusMethodNotAllowed, "MethodNotAllowed"), nil
}

func (self *fakeS3) list(request *http.Request) *http.Response {
	query := request.URL.Query()
	bucket := strings.Trim(request.URL.Path, "/") + "/"
	prefix := bucket + query.Get("prefix")
	startAfter := ""
	if query.Get("start-after") != "" {
		startAfter = bucket + query.Get("start-after")
	}
	maxKeys, _ := strconv.Atoi(query.Get("max-keys"))
	keys := []string{}
	for key := range self.objects {
		if strings.HasPrefix(key, prefix) && startAfter < key {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	type content struct {
		Key          string `xml:"Key"`
		Size         int    `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	}
	document := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}{}
	if maxKeys < len(keys) {
		document.IsTruncated = true
		document.NextContinuationToken = keys[maxKeys-1]
		keys = keys[:maxKeys]
	}
	for _, key := range keys {
		object := self.objects[key]
		document.Contents = append(document.Contents, content{
			Key:          strings.TrimPrefix(key, bucket),
			Size:         len(object.body),
			ETag:         `"` + object.etag + `"`,
			LastModified: self.lastModifiedTime.Format(time.RFC3339),
		})
	}
	body, _ := xml.Marshal(document)
	return fakeS3Response(request, http.StatusOK, http.Header{"Content-Type": {"application/xml"}}, body)
}

// The requests seen so far, and forgets them.
func (self *fakeS3) takeRequests() []*fakeS3Request {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	requests := self.requests
	self.requests = nil
	return requests
}

type fileClusterFixture struct {
	clock    *sidecartest.FakeClock
	store    *fakeS3
	clusters *FileClusters
	cluster  *FileCluster
	config   *SidecarConfig
}

func fileClusterTestConfigs(endpoints ...string) []*FileClusterConfig {
	return []*FileClusterConfig{
		{
			ClusterId: "meta-1",
			Kind:      FileClusterKindMetadata,
			First:     true,
			Endpoints: endpoints,
			AccessKey: "synthetic-access",
			SecretKey: "synthetic-secret",
			Bucket:    "meta-bucket",
			Region:    "us-east-1",
		},
		{
			ClusterId: "seg-1",
			Kind:      FileClusterKindSegment,
			Endpoints: endpoints,
			AccessKey: "synthetic-access",
			SecretKey: "synthetic-secret",
			Bucket:    "seg-bucket",
			Region:    "us-east-1",
		},
	}
}

// Settings on clock whose checks run once, at construction, and never again within a test, so the
// test decides when a check happens and every request it counts is its own.
func fileClusterTestSettings(clock *sidecartest.FakeClock) *FileClusterSettings {
	settings := DefaultFileClusterSettings()
	settings.Clock = clock
	settings.CheckInterval = 24 * time.Hour
	return settings
}

// Waits for the constructor's first check: done once the check loop waits on the clock.
func awaitFirstCheck(t *testing.T, clock *sidecartest.FakeClock) {
	t.Helper()
	if err := clock.AwaitTimers(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
}

// Clusters over store on a fake clock, checked once, with sidecarYaml as sidecar.yml.
func newFileClusterFixture(t *testing.T, store *fakeS3, sidecarYaml string, endpoints ...string) *fileClusterFixture {
	t.Helper()
	if len(endpoints) == 0 {
		endpoints = []string{"host-a.example:9000"}
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	config, err := parseSidecarConfig([]byte(sidecarYaml))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &fileClusterFixture{clock: clock, store: store, config: config}
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoints...), fileClusterTestSettings(clock), func(tls bool) (http.RoundTripper, error) {
		return store.transport("any"), nil
	}, func() *SidecarConfig { return fixture.config })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	awaitFirstCheck(t, clock)
	fixture.clusters = clusters
	fixture.cluster = clusters.clusterIdClusters["meta-1"]
	store.takeRequests()
	return fixture
}

// Each operation is the one request it names.
func TestFileClusterOneRoundTripPerOperation(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	ctx := t.Context()
	cluster := fixture.cluster
	sum := sha256.Sum256([]byte("segment"))
	segmentCluster := fixture.clusters.clusterIdClusters["seg-1"]
	for _, c := range []struct {
		name   string
		method string
		run    func() error
	}{
		{name: "put if none match", method: http.MethodPut, run: func() error {
			_, err := cluster.PutIfNoneMatch(ctx, "head/a/_", []byte("v1"), nil)
			return err
		}},
		{name: "put if match", method: http.MethodPut, run: func() error {
			info, err := cluster.Head(ctx, "head/a/_")
			if err != nil {
				return err
			}
			fixture.store.takeRequests()
			_, err = cluster.PutIfMatch(ctx, "head/a/_", info.Etag, []byte("v2"), nil)
			return err
		}},
		{name: "put stream", method: http.MethodPut, run: func() error {
			_, err := segmentCluster.PutStream(ctx, "seg/r/id", bytes.NewReader([]byte("segment")), 7, sum[:])
			return err
		}},
		{name: "get", method: http.MethodGet, run: func() error {
			object, err := cluster.Get(ctx, "head/a/_")
			if err == nil {
				object.Body.Close()
			}
			return err
		}},
		{name: "get range", method: http.MethodGet, run: func() error {
			object, err := cluster.GetRange(ctx, "head/a/_", 0, 1)
			if err == nil {
				object.Body.Close()
			}
			return err
		}},
		{name: "head", method: http.MethodHead, run: func() error {
			_, err := cluster.Head(ctx, "head/a/_")
			return err
		}},
		{name: "list", method: http.MethodGet, run: func() error {
			_, _, err := cluster.List(ctx, "head/", "", 10)
			return err
		}},
		{name: "delete", method: http.MethodDelete, run: func() error {
			return cluster.Delete(ctx, "head/a/_")
		}},
	} {
		if err := c.run(); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		requests := fixture.store.takeRequests()
		if len(requests) != 1 || requests[0].method != c.method {
			t.Errorf("%s: %d requests", c.name, len(requests))
		}
	}
}

// A LIST stops at the key past its limit, so a page with more after it is still one request.
func TestFileClusterListPagesInOneRequestEach(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	ctx := t.Context()
	for i := range 5 {
		if _, err := fixture.cluster.PutIfNoneMatch(ctx, fmt.Sprintf("seg/r/%d/_", i), []byte("x"), nil); err != nil {
			t.Fatal(err)
		}
	}
	fixture.store.takeRequests()
	keys := []string{}
	startAfter := ""
	for {
		objects, more, err := fixture.cluster.List(ctx, "seg/r/", startAfter, 2)
		if err != nil {
			t.Fatal(err)
		}
		if requests := fixture.store.takeRequests(); len(requests) != 1 {
			t.Fatalf("%d requests for one page", len(requests))
		}
		for _, object := range objects {
			keys = append(keys, object.Key)
			startAfter = object.Key
		}
		if !more {
			break
		}
	}
	if len(keys) != 5 {
		t.Errorf("listed %q", keys)
	}
}

// §19.6: the header is the request's deadline, RequestTimeout after it is issued by this process's
// clock, on every request but a streamed PUT.
func TestFileClusterDeadlineHeaderRidesEveryRequestButPutStream(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	ctx := t.Context()
	cluster := fixture.cluster
	want := strconv.FormatInt(fixture.clock.Now().Add(cluster.RequestTimeout()).UnixMilli(), 10)

	if _, err := cluster.PutIfNoneMatch(ctx, "head/a/_", []byte("v1"), nil); err != nil {
		t.Fatal(err)
	}
	object, err := cluster.Get(ctx, "head/a/_")
	if err != nil {
		t.Fatal(err)
	}
	object.Body.Close()
	if _, err := cluster.Head(ctx, "head/a/_"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cluster.List(ctx, "head/", "", 10); err != nil {
		t.Fatal(err)
	}
	if err := cluster.Delete(ctx, "head/a/_"); err != nil {
		t.Fatal(err)
	}
	for _, request := range fixture.store.takeRequests() {
		if request.deadline != want {
			t.Errorf("%s %s: deadline %q, want %q", request.method, request.path, request.deadline, want)
		}
	}

	sum := sha256.Sum256([]byte("segment"))
	segmentCluster := fixture.clusters.clusterIdClusters["seg-1"]
	if _, err := segmentCluster.PutStream(ctx, "seg/r/id", bytes.NewReader([]byte("segment")), 7, sum[:]); err != nil {
		t.Fatal(err)
	}
	for _, request := range fixture.store.takeRequests() {
		if request.deadline != "" {
			t.Errorf("a streamed PUT carried deadline %q", request.deadline)
		}
	}
}

// A request's own earlier deadline is the header's value: a collection DELETE's 50 ms before until_ms.
func TestFileClusterDeadlineHeaderTakesTheOwnDeadline(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	own := fixture.clock.Now().Add(2 * time.Second)
	if err := fixture.cluster.Delete(sidecar.WithBlobDeadline(t.Context(), own), "idx/r/f/1/t.e"); err != nil {
		t.Fatal(err)
	}
	requests := fixture.store.takeRequests()
	if len(requests) != 1 || requests[0].deadline != strconv.FormatInt(own.UnixMilli(), 10) {
		t.Errorf("requests %+v", requests)
	}
}

// The client cancels at the request's own deadline when no response has started, and the outcome is
// unknown: a cancel tells the store nothing.
func TestFileClusterCancelsAtTheOwnDeadline(t *testing.T) {
	store := newFakeS3(1, "build-1")
	fixture := newFileClusterFixture(t, store, "")
	store.holdPrefix = "/meta-bucket/idx/"
	done := make(chan error, 1)
	go func() {
		own := fixture.clock.Now().Add(2 * time.Second)
		done <- fixture.cluster.Delete(sidecar.WithBlobDeadline(t.Context(), own), "idx/r/f/1/t.e")
	}()
	<-store.held
	fixture.clock.Advance(2 * time.Second)
	err := <-done
	var unknown *sidecar.BlobUnknownError
	if !errors.As(err, &unknown) {
		t.Errorf("err %v", err)
	}
}

// The SDK sends once: a 503 is surfaced at once, a write's as unknown, and nothing is sent again.
func TestFileClusterNoRetryOn503(t *testing.T) {
	store := newFakeS3(1, "build-1")
	fixture := newFileClusterFixture(t, store, "")
	store.failStatus = http.StatusServiceUnavailable
	_, err := fixture.cluster.PutIfNoneMatch(t.Context(), "head/a/_", []byte("v1"), nil)
	var unknown *sidecar.BlobUnknownError
	if !errors.As(err, &unknown) {
		t.Errorf("put: %v", err)
	}
	if requests := store.takeRequests(); len(requests) != 1 {
		t.Errorf("put: %d requests", len(requests))
	}
	if _, err := fixture.cluster.Head(t.Context(), "head/a/_"); err == nil || errors.Is(err, sidecar.ErrBlobNotFound) {
		t.Errorf("head: %v", err)
	}
	if requests := store.takeRequests(); len(requests) != 1 {
		t.Errorf("head: %d requests", len(requests))
	}
	if err := fixture.cluster.Delete(t.Context(), "head/a/_"); !errors.As(err, &unknown) {
		t.Errorf("delete: %v", err)
	}
	if requests := store.takeRequests(); len(requests) != 1 {
		t.Errorf("delete: %d requests", len(requests))
	}
}

// A request that failed at a host is surfaced, and the next goes to another host.
func TestFileClusterNextRequestMovesHosts(t *testing.T) {
	store := newFakeS3(1, "build-1")
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fileClusterTestSettings(clock)
	endpoints := []string{"host-a.example:9000", "host-b.example:9000", "host-c.example:9000"}
	hostIndex := 0
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoints...), settings, func(tls bool) (http.RoundTripper, error) {
		host := endpoints[hostIndex%len(endpoints)]
		hostIndex += 1
		return store.transport(host), nil
	}, func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	awaitFirstCheck(t, clock)
	cluster := clusters.clusterIdClusters["meta-1"]
	store.takeRequests()

	store.downHosts["host-a.example:9000"] = true
	failedHost := ""
	for range len(endpoints) {
		_, err := cluster.Head(t.Context(), "head/a/_")
		requests := store.takeRequests()
		if len(requests) != 1 {
			t.Fatalf("%d requests", len(requests))
		}
		if requests[0].host == "host-a.example:9000" {
			if err == nil || errors.Is(err, sidecar.ErrBlobNotFound) {
				t.Errorf("a down host answered: %v", err)
			}
			failedHost = requests[0].host
		}
	}
	if failedHost == "" {
		t.Fatal("no request went to the down host")
	}
	// within the holdoff the failed host gets nothing
	for range 2 * len(endpoints) {
		cluster.Head(t.Context(), "head/a/_")
		for _, request := range store.takeRequests() {
			if request.host == failedHost {
				t.Errorf("a request went to %s within its holdoff", failedHost)
			}
		}
	}
	// past it, it is tried again
	clock.Advance(settings.HostFailureHoldoff)
	store.downHosts["host-a.example:9000"] = false
	seen := false
	for range len(endpoints) {
		cluster.Head(t.Context(), "head/a/_")
		for _, request := range store.takeRequests() {
			seen = seen || request.host == failedHost
		}
	}
	if !seen {
		t.Errorf("%s was never tried again", failedHost)
	}
}

// A checksum rides as x-amz-checksum-sha256, and other bytes than the signed ones write nothing.
func TestFileClusterChecksumRidesThePut(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	sum := sha256.Sum256([]byte("batch"))
	if _, err := fixture.cluster.PutIfNoneMatch(t.Context(), "pb/h/m/a-b", []byte("batch"), &sidecar.BlobPutOptions{Sha256: sum[:]}); err != nil {
		t.Fatal(err)
	}
	requests := fixture.store.takeRequests()
	if len(requests) != 1 || requests[0].header.Get("X-Amz-Checksum-Sha256") == "" {
		t.Errorf("no checksum header: %+v", requests)
	}
	other := sha256.Sum256([]byte("other"))
	segmentCluster := fixture.clusters.clusterIdClusters["seg-1"]
	_, err := segmentCluster.PutStream(t.Context(), "seg/r/x", bytes.NewReader([]byte("segment")), 7, other[:])
	if !errors.Is(err, sidecar.ErrBlobChecksum) {
		t.Errorf("a lying segment: %v", err)
	}
}
