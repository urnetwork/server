//go:build sidecar_localstack

// Each block's MinIO user on the local stack's MinIO (OPS-M2-04, §19.4's secret_files column): xops's
// templates/file-clusters/policy-<block>.json.j2, rendered as the playbook renders them and applied
// through MinIO's admin API as `mc admin` applies them, each block then doing through FileClusters what
// its row allows and failing one thing it must not. The templates are read from the xops checkout beside
// this one, so this needs xops as well as the stack; it carries the sidecar_localstack tag and fails,
// rather than skips, when either is missing. It works in buckets, users and policies of its own, each name
// carrying a fresh suffix, and removes its users and policies when it ends.
//
// One MinIO stands in for each cluster's own: in production every cluster is its own MinIO with its own
// users, so a block with no user on a cluster is refused there outright. Here a block's user is made per
// cluster it reaches, from that cluster's rendering, and a cluster it does not reach is shown refused by
// its users' policies.

package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"
	"golang.org/x/crypto/argon2"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/localstack"
)

// The xops templates the playbook renders, beside this checkout.
func fileClusterPolicyTemplateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(fileClusterPolicyServerRoot(t), "..", "xops", "main", "ansible", "templates", "file-clusters")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("xops's policy templates are not beside this checkout: %v", err)
	}
	return dir
}

// madmin's EncryptData, which the admin API's add-user takes: salt, the AEAD's id, a nonce, then the
// body as one final sio-go fragment under an Argon2id key of the root secret. The format is MinIO's
// (madmin-go's encrypt.go and secure-io/sio-go); madmin-go is not linked here, since §4.7's license rule
// refuses it.
func fileClusterAdminEncrypt(password string, data []byte) ([]byte, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// sio-go's nonce is the AEAD's less 4 bytes, which carry a little-endian fragment number
	nonce := make([]byte, aead.NonceSize()-4)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	fragmentNonce := func(sequence uint32) []byte {
		full := make([]byte, aead.NonceSize())
		copy(full, nonce)
		binary.LittleEndian.PutUint32(full[len(nonce):], sequence)
		return full
	}
	// the stream's associated data: a final flag, then fragment 0's seal of the caller's (empty) data
	associatedData := append([]byte{0x80}, aead.Seal(nil, fragmentNonce(0), nil, nil)...)
	out := bytes.NewBuffer(nil)
	out.Write(salt)
	// argon2id with AES-GCM
	out.WriteByte(0x00)
	out.Write(nonce)
	out.Write(aead.Seal(nil, fragmentNonce(1), data, associatedData))
	return out.Bytes(), nil
}

// The stack's MinIO through its admin API, as its root user.
type fileClusterStackAdmin struct {
	endpoint  string
	accessKey string
	secretKey string
}

func (self *fileClusterStackAdmin) do(ctx context.Context, method string, path string, query url.Values, body []byte) error {
	endpoint := url.URL{Scheme: "http", Host: self.endpoint, Path: "/minio/admin/v3/" + path, RawQuery: query.Encode()}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	request.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	request = signer.SignV4(*request, self.accessKey, self.secretKey, "", "us-east-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("admin %s %s answered %d: %s", method, path, response.StatusCode, answer)
	}
	return nil
}

// One block's user on one cluster, with the policy its template renders for that cluster, removed when the
// test ends: `mc admin user add`, `policy create` and `policy attach`, as the playbook runs them.
func (self *fileClusterStackAdmin) makeBlockUser(t *testing.T, block string, cluster *FileClusterConfig, extraVars map[string]any) *FileClusterConfig {
	t.Helper()
	ctx := t.Context()
	templateText, err := os.ReadFile(filepath.Join(fileClusterPolicyTemplateDir(t), "policy-"+block+".json.j2"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"file_cluster": map[string]any{
			"id":     cluster.ClusterId,
			"kind":   cluster.Kind,
			"first":  cluster.First,
			"bucket": cluster.Bucket,
		},
		"file_cluster_evs_segment_delete": false,
	}
	for name, value := range extraVars {
		vars[name] = value
	}
	policy, err := renderFileClusterPolicyTemplate(string(templateText), vars)
	if err != nil {
		t.Fatalf("%s's template: %v", block, err)
	}
	if !json.Valid([]byte(policy)) {
		t.Fatalf("%s's template rendered no JSON for %s:\n%s", block, cluster.ClusterId, policy)
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("sidecar-test-%s-%s-%x", block, cluster.ClusterId, suffix)
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	secretKey := hex.EncodeToString(secret)

	if err := self.do(ctx, http.MethodPut, "add-canned-policy", url.Values{"name": {name}}, []byte(policy)); err != nil {
		t.Fatalf("%s's policy for %s: %v", block, cluster.ClusterId, err)
	}
	userBody, err := json.Marshal(map[string]string{"secretKey": secretKey, "status": "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := fileClusterAdminEncrypt(self.secretKey, userBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := self.do(ctx, http.MethodPut, "add-user", url.Values{"accessKey": {name}}, encrypted); err != nil {
		t.Fatalf("%s's user: %v", block, err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		self.do(cleanupCtx, http.MethodDelete, "remove-user", url.Values{"accessKey": {name}}, nil)
		self.do(cleanupCtx, http.MethodDelete, "remove-canned-policy", url.Values{"name": {name}}, nil)
	})
	if err := self.do(ctx, http.MethodPut, "set-user-or-group-policy", url.Values{"policyName": {name}, "userOrGroup": {name}, "isGroup": {"false"}}, nil); err != nil {
		t.Fatalf("%s's policy attach: %v", block, err)
	}

	config := *cluster
	config.Endpoints = []string{fileClusterStackEndpoint}
	config.AccessKey = name
	config.SecretKey = secretKey
	return &config
}

// The clusters a test works in, fresh buckets on the stack's MinIO, each with the sentinel xops writes,
// and the root's client for checking what a refused request left.
func fileClusterPolicyStack(t *testing.T) (*fileClusterStackAdmin, *FileClusterConfig, *FileClusterConfig, *minio.Client) {
	t.Helper()
	ctx := t.Context()
	if err := localstack.CheckStackAddress(fileClusterStackEndpoint); err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	meta := &FileClusterConfig{
		ClusterId: "meta-1",
		Kind:      FileClusterKindMetadata,
		First:     true,
		Bucket:    fmt.Sprintf("sidecar-test-policy-meta-%x", suffix),
		Region:    "us-east-1",
	}
	segment := &FileClusterConfig{
		ClusterId: "seg-1",
		Kind:      FileClusterKindSegment,
		Bucket:    fmt.Sprintf("sidecar-test-policy-seg-%x", suffix),
		Region:    "us-east-1",
	}
	makeFileClusterStackBuckets(ctx, t, []*FileClusterConfig{meta, segment})
	root, err := minio.New(fileClusterStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(fileClusterStackAccessKey, fileClusterStackSecretKey, ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []*FileClusterConfig{meta, segment} {
		putFileClusterPolicyObject(t, root, config.Bucket, fileClusterSentinelKey)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for _, config := range []*FileClusterConfig{meta, segment} {
			for object := range root.ListObjects(cleanupCtx, config.Bucket, minio.ListObjectsOptions{Recursive: true}) {
				if object.Err == nil {
					root.RemoveObject(cleanupCtx, config.Bucket, object.Key, minio.RemoveObjectOptions{})
				}
			}
			root.RemoveBucket(cleanupCtx, config.Bucket)
		}
	})
	admin := &fileClusterStackAdmin{
		endpoint:  fileClusterStackEndpoint,
		accessKey: fileClusterStackAccessKey,
		secretKey: fileClusterStackSecretKey,
	}
	return admin, meta, segment, root
}

// An object the root writes, as another block would have.
func putFileClusterPolicyObject(t *testing.T, root *minio.Client, bucket string, key string) {
	t.Helper()
	body := []byte("synthetic")
	if _, err := root.PutObject(t.Context(), bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Whether the root still finds key.
func fileClusterPolicyObjectExists(t *testing.T, root *minio.Client, bucket string, key string) bool {
	t.Helper()
	_, err := root.StatObject(t.Context(), bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true
	}
	if response, ok := fileClusterErrorResponse(err); ok && response.StatusCode == http.StatusNotFound {
		return false
	}
	t.Fatalf("root stat of %s: %v", key, err)
	return false
}

// A block's clusters through the real client, checked once so writes may go.
func fileClusterPolicyClusters(t *testing.T, configs ...*FileClusterConfig) *FileClusters {
	t.Helper()
	clusters, err := NewFileClusters(t.Context(), configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	clusters.Check(t.Context())
	return clusters
}

// The store refused err for want of permission, as opposed to failing some other way.
func requireFileClusterAccessDenied(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: allowed, want access denied", what)
		return
	}
	if response, ok := fileClusterErrorResponse(err); !ok || response.Code != "AccessDenied" {
		t.Errorf("%s: %v, want access denied", what, err)
	}
}

// The store allowed what its row allows.
func requireFileClusterAllowed(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

// api reaches proj/, projrows/, rev/ and hold/ on the first cluster and nothing else: refused repo/, a
// LIST of the bucket, any DELETE, and the segment bucket.
func TestApiFileClusterUserReachesItsPrefixesAlone(t *testing.T) {
	ctx := t.Context()
	admin, meta, segment, root := fileClusterPolicyStack(t)
	apiMeta := admin.makeBlockUser(t, "api", meta, nil)
	clusters := fileClusterPolicyClusters(t, apiMeta)
	if !clusters.Ready() {
		t.Fatal("api's cluster is not ready: its user cannot read admin info or the sentinel")
	}
	cluster, err := clusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"proj/n1/00000000000000000001/_", "projrows/n1/digest/_", "rev/s1/_", "hold/n1/case1/_"} {
		_, err := cluster.PutIfNoneMatch(ctx, key, []byte("synthetic"), nil)
		requireFileClusterAllowed(t, "api PUT "+key, err)
		_, err = cluster.Get(ctx, key)
		requireFileClusterAllowed(t, "api GET "+key, err)
	}
	listed, _, err := cluster.List(ctx, "proj/n1/", "", 10)
	if err != nil || len(listed) != 1 {
		t.Errorf("api LIST proj/n1/: %d objects, %v", len(listed), err)
	}

	putFileClusterPolicyObject(t, root, meta.Bucket, "repo/r1")
	_, err = cluster.Get(ctx, "repo/r1")
	requireFileClusterAccessDenied(t, "api GET repo/", err)
	_, err = cluster.PutIfNoneMatch(ctx, "repo/r2", []byte("synthetic"), nil)
	requireFileClusterAccessDenied(t, "api PUT repo/", err)
	if fileClusterPolicyObjectExists(t, root, meta.Bucket, "repo/r2") {
		t.Error("api's refused PUT of repo/ wrote it")
	}
	_, _, err = cluster.List(ctx, "repo/", "", 10)
	requireFileClusterAccessDenied(t, "api LIST repo/", err)
	_, _, err = cluster.List(ctx, "", "", 10)
	requireFileClusterAccessDenied(t, "api LIST of the bucket", err)
	err = cluster.Delete(ctx, "rev/s1/_")
	requireFileClusterAccessDenied(t, "api DELETE rev/", err)
	if !fileClusterPolicyObjectExists(t, root, meta.Bucket, "rev/s1/_") {
		t.Error("api's refused DELETE removed rev/")
	}

	// api's user, sent at the segment bucket, which it holds no user on in production
	stray := *apiMeta
	stray.ClusterId = segment.ClusterId
	stray.Bucket = segment.Bucket
	stray.Kind = FileClusterKindMetadata
	strayClusters := fileClusterPolicyClusters(t, &stray)
	strayCluster, err := strayClusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	_, err = strayCluster.Get(ctx, fileClusterSentinelKey)
	requireFileClusterAccessDenied(t, "api GET of the segment bucket", err)
}

// evs reads, writes and deletes on metadata, and on the segment cluster reads and writes but deletes
// nothing until §11.13's purge turns its DELETE on.
func TestEvsFileClusterUserDeletesNoSegmentUntilThePurge(t *testing.T) {
	ctx := t.Context()
	admin, meta, segment, root := fileClusterPolicyStack(t)
	clusters := fileClusterPolicyClusters(t,
		admin.makeBlockUser(t, "evs", meta, nil),
		admin.makeBlockUser(t, "evs", segment, nil),
	)
	if !clusters.Ready() {
		t.Fatal("evs's clusters are not ready")
	}
	metaCluster, err := clusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	segmentCluster, err := clusters.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	for _, cluster := range []sidecar.BlobCluster{metaCluster, segmentCluster} {
		_, err := cluster.PutIfNoneMatch(ctx, "seg/r1/s1", []byte("synthetic"), nil)
		requireFileClusterAllowed(t, "evs PUT on "+cluster.ClusterId(), err)
		_, err = cluster.Get(ctx, "seg/r1/s1")
		requireFileClusterAllowed(t, "evs GET on "+cluster.ClusterId(), err)
		_, _, err = cluster.List(ctx, "seg/", "", 10)
		requireFileClusterAllowed(t, "evs LIST on "+cluster.ClusterId(), err)
	}
	requireFileClusterAllowed(t, "evs DELETE on metadata", metaCluster.Delete(ctx, "seg/r1/s1"))
	requireFileClusterAccessDenied(t, "evs DELETE on the segment bucket", segmentCluster.Delete(ctx, "seg/r1/s1"))
	if !fileClusterPolicyObjectExists(t, root, segment.Bucket, "seg/r1/s1") {
		t.Error("evs's refused segment DELETE removed the segment")
	}

	purging := fileClusterPolicyClusters(t,
		admin.makeBlockUser(t, "evs", meta, nil),
		admin.makeBlockUser(t, "evs", segment, map[string]any{"file_cluster_evs_segment_delete": true}),
	)
	purgingSegment, err := purging.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	requireFileClusterAllowed(t, "evs DELETE on the segment bucket with the purge on", purgingSegment.Delete(ctx, "seg/r1/s1"))
	if fileClusterPolicyObjectExists(t, root, segment.Bucket, "seg/r1/s1") {
		t.Error("evs's segment DELETE with the purge on left the segment")
	}
}

// gkm holds its metadata clusters whole, and the segment bucket refuses it.
func TestGkmFileClusterUserIsRefusedTheSegmentBucket(t *testing.T) {
	ctx := t.Context()
	admin, meta, segment, root := fileClusterPolicyStack(t)
	gkmMeta := admin.makeBlockUser(t, "gkm", meta, nil)
	clusters := fileClusterPolicyClusters(t, gkmMeta)
	cluster, err := clusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	_, err = cluster.PutIfNoneMatch(ctx, "gkm/g1/epoch/_", []byte("synthetic"), nil)
	requireFileClusterAllowed(t, "gkm PUT", err)
	_, _, err = cluster.List(ctx, "gkm/", "", 10)
	requireFileClusterAllowed(t, "gkm LIST", err)
	requireFileClusterAllowed(t, "gkm DELETE", cluster.Delete(ctx, "gkm/g1/epoch/_"))

	putFileClusterPolicyObject(t, root, segment.Bucket, "seg/r1/s1")
	stray := *gkmMeta
	stray.Bucket = segment.Bucket
	strayClusters := fileClusterPolicyClusters(t, &stray)
	strayCluster, err := strayClusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	_, err = strayCluster.Get(ctx, "seg/r1/s1")
	requireFileClusterAccessDenied(t, "gkm GET on the segment bucket", err)
	_, err = strayCluster.PutIfNoneMatch(ctx, "seg/r1/s2", []byte("synthetic"), nil)
	requireFileClusterAccessDenied(t, "gkm PUT on the segment bucket", err)
	_, _, err = strayCluster.List(ctx, "seg/", "", 10)
	requireFileClusterAccessDenied(t, "gkm LIST on the segment bucket", err)
}

// monitor reads each cluster's admin info and no object.
func TestMonitorFileClusterUserReadsNoObject(t *testing.T) {
	ctx := t.Context()
	admin, meta, segment, _ := fileClusterPolicyStack(t)
	clusters := fileClusterPolicyClusters(t,
		admin.makeBlockUser(t, "monitor", meta, nil),
		admin.makeBlockUser(t, "monitor", segment, nil),
	)
	for _, clusterId := range []string{meta.ClusterId, segment.ClusterId} {
		cluster := clusters.clusterIdClusters[clusterId]
		info, err := cluster.AdminInfo(ctx)
		if err != nil || info.PoolCount != 1 {
			t.Errorf("monitor's admin info on %s: %+v, %v", clusterId, info, err)
		}
		_, err = cluster.Get(ctx, fileClusterSentinelKey)
		requireFileClusterAccessDenied(t, "monitor GET on "+clusterId, err)
		_, _, err = cluster.List(ctx, "", "", 10)
		requireFileClusterAccessDenied(t, "monitor LIST on "+clusterId, err)
		_, err = cluster.PutIfNoneMatch(ctx, "monitor/_", []byte("synthetic"), nil)
		requireFileClusterAccessDenied(t, "monitor PUT on "+clusterId, err)
	}
}

// taskworker holds every cluster whole, the segment bucket's DELETE included.
func TestTaskworkerFileClusterUserDeletesSegments(t *testing.T) {
	ctx := t.Context()
	admin, meta, segment, root := fileClusterPolicyStack(t)
	clusters := fileClusterPolicyClusters(t,
		admin.makeBlockUser(t, "taskworker", meta, nil),
		admin.makeBlockUser(t, "taskworker", segment, nil),
	)
	segmentCluster, err := clusters.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	putFileClusterPolicyObject(t, root, segment.Bucket, "seg/r1/s1")
	_, _, err = segmentCluster.List(ctx, "seg/", "", 10)
	requireFileClusterAllowed(t, "taskworker LIST on the segment bucket", err)
	requireFileClusterAllowed(t, "taskworker DELETE on the segment bucket", segmentCluster.Delete(ctx, "seg/r1/s1"))
	if fileClusterPolicyObjectExists(t, root, segment.Bucket, "seg/r1/s1") {
		t.Error("taskworker's segment DELETE left the segment")
	}
	metaCluster, err := clusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	_, err = metaCluster.PutIfNoneMatch(ctx, "rev/s1/_", []byte("synthetic"), nil)
	requireFileClusterAllowed(t, "taskworker PUT on metadata", err)
	requireFileClusterAllowed(t, "taskworker DELETE on metadata", metaCluster.Delete(ctx, "rev/s1/_"))
}
