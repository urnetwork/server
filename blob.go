package server

// blob.go — abstract blob (object) storage. Backed by MinIO in real
// environments, or the local filesystem for dev/local. The rest of the
// codebase depends only on the BlobStore interface and LoadBlobStore, not on
// the MinIO client, so the backend is a config choice.
//
// Backend selection (LoadBlobStore):
//   - no vault `minio.yml`, local env -> local filesystem (default root)
//   - no vault `minio.yml`, other env -> disabled
//   - `minio.yml` authority "local"   -> local filesystem (root from `path`)
//     (or empty authority)
//   - `minio.yml` with an authority   -> MinIO
//
// The vault resource stays `minio.yml` (its credentials/endpoint), so ops and
// ansible are unaffected; only the Go surface is abstracted.

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"

	"github.com/urnetwork/glog"
)

// DefaultBlobPrefix is the general blob-storage namespace used when
// `minio.yml` sets no prefix. Subsystems scope themselves under it by
// stream (e.g. stats samples at `<prefix>/<env>/<stream>/...`), so one
// bucket serves as the org-wide blob store.
// It is the object-key namespace used when `minio.yml` sets no
// explicit prefix.
const DefaultBlobPrefix = "blob"

// blobPartialSuffix marks an in-progress local write (renamed into place on
// completion), so a List never returns a half-written object.
const blobPartialSuffix = ".partial"

// BlobObject is a stored object's key and size, as returned by List.
type BlobObject struct {
	Key  string
	Size int64
}

// BlobLifecycleRule expires (deletes) objects whose key starts with KeyPrefix
// once they are older than TTL. This is how per-"stream" retention is set: each
// stats stream maps to a key-prefix rule (see stats.RegisterStreamTTL).
type BlobLifecycleRule struct {
	KeyPrefix string
	TTL       time.Duration
}

// localReapInterval is how often the local backend scans for expired objects.
const localReapInterval = 1 * time.Hour

// DefaultLocalBlobMaxBytes prevents an explicitly selected local backend from
// consuming an unbounded service disk. The source stats spool has a separate
// cap; this bounds the successful-copy destination as well.
const DefaultLocalBlobMaxBytes int64 = 8 * 1024 * 1024 * 1024

// BlobStore is object storage keyed by string, safe for concurrent use. Keys
// are full object keys; callers compose any namespace under Prefix. Retention
// is the backing store's job (a MinIO ILM lifecycle rule; the local backend
// does not expire), so there is deliberately no Delete — the store only writes,
// reads, and lists.
type BlobStore interface {
	// Put uploads the file at localPath to key with the given content type.
	Put(ctx context.Context, key string, localPath string, contentType string) error
	// Get opens key for reading; the caller closes the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns every object whose key starts with keyPrefix.
	List(ctx context.Context, keyPrefix string) ([]BlobObject, error)
	// SetLifecycle declares per-key-prefix TTL expiry, replacing the store's
	// previously-declared rules. Idempotent. MinIO enforces it server-side via
	// an ILM lifecycle configuration; the local backend runs an in-process
	// reaper (ctx bounds its lifetime). An empty rule set is a no-op (it does
	// not wipe rules set out-of-band, e.g. by ops).
	SetLifecycle(ctx context.Context, rules []BlobLifecycleRule) error
	// Bucket is the backing bucket name (for manifests/logging).
	Bucket() string
	// Prefix is the configured object-key namespace (for key composition).
	Prefix() string
	// Authority is the backing endpoint (for logging).
	Authority() string
}

// BlobStoreConfig is the backing MinIO configuration (or local-backend
// selection) read from the vault `minio.yml` resource.
type BlobStoreConfig struct {
	Authority string
	AccessKey string
	SecretKey string
	Bucket    string
	Tls       bool
	// Prefix is the object-key namespace (default DefaultBlobPrefix).
	Prefix string
	// Local selects the local-filesystem backend (authority "local" or empty).
	Local bool
	// LocalPath is the filesystem root for the local backend.
	LocalPath string
	// LocalMaxBytes bounds all objects under LocalPath.
	LocalMaxBytes int64
}

// defaultLocalBlobRoot is the local backend's root when unset: a `blob`
// directory under the site home (alongside the stats tree).
func defaultLocalBlobRoot() string {
	return filepath.Join(SiteHomeRoot(), "blob")
}

// LoadBlobStore returns a ready blob store, choosing the backend from the vault
// `minio.yml` resource (see the backend-selection note above). An implicit
// local store is permitted only in WARP_ENV=local; elsewhere missing MinIO
// configuration disables upload instead of filling the service host's disk.
func LoadBlobStore() (store BlobStore, ok bool) {
	config, present := LoadBlobStoreConfig()
	if !present {
		env, _ := Env()
		if env != "local" {
			return nil, false
		}
		return NewLocalBlobStore(defaultLocalBlobRoot(), DefaultBlobPrefix), true
	}
	if config.Local {
		return NewLocalBlobStoreWithMaxBytes(
			config.LocalPath,
			config.Prefix,
			config.LocalMaxBytes,
		), true
	}
	minioStore, err := NewBlobStore(config)
	if err != nil {
		return nil, false
	}
	return minioStore, true
}

// LoadBlobStoreConfig reads the backing config from the vault `minio.yml`
// resource. present is false only when the resource is absent; a present
// resource with authority "local" or empty selects the local backend.
func LoadBlobStoreConfig() (config *BlobStoreConfig, present bool) {
	resource, err := Vault.SimpleResource("minio.yml")
	if err != nil {
		return nil, false
	}
	values := resource.Parse()
	str := func(key string) string {
		if v, ok := values[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	prefix := str("prefix")
	if prefix == "" {
		prefix = DefaultBlobPrefix
	}
	authority, ok := resolveBlobAuthority(str("authority"), Routes())
	if !ok {
		// a host without the interpolated env var has no blob store; the
		// stats subsystem is fail-safe and must not crash or silently fall
		// back to the local backend on a misconfigured prod host
		glog.Infof("[blob]store disabled: authority env interpolation unresolved\n")
		return nil, false
	}
	// local backend when explicitly "local" or when no endpoint is configured
	local := authority == "" || strings.EqualFold(authority, "local")
	localPath := str("path")
	if localPath == "" {
		localPath = defaultLocalBlobRoot()
	}
	tls := false
	if v, ok := values["tls"]; ok {
		if b, ok := v.(bool); ok {
			tls = b
		}
	}
	localMaxBytes := DefaultLocalBlobMaxBytes
	if value, ok := values["max_bytes"]; ok {
		switch value := value.(type) {
		case int:
			localMaxBytes = int64(value)
		case int64:
			localMaxBytes = value
		case float64:
			localMaxBytes = int64(value)
		}
	}
	if localMaxBytes <= 0 {
		localMaxBytes = DefaultLocalBlobMaxBytes
	}
	return &BlobStoreConfig{
		Authority:     authority,
		AccessKey:     str("access_key"),
		SecretKey:     str("secret_key"),
		Bucket:        str("bucket"),
		Tls:           tls,
		Prefix:        prefix,
		Local:         local,
		LocalPath:     localPath,
		LocalMaxBytes: localMaxBytes,
	}, true
}

// NewBlobStore builds a MinIO-backed store from config.
func NewBlobStore(config *BlobStoreConfig) (BlobStore, error) {
	client, err := minio.New(config.Authority, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
		Secure: config.Tls,
	})
	if err != nil {
		return nil, err
	}
	prefix := config.Prefix
	if prefix == "" {
		prefix = DefaultBlobPrefix
	}
	return &minioBlobStore{
		client:    client,
		bucket:    config.Bucket,
		prefix:    prefix,
		authority: config.Authority,
	}, nil
}

// minioBlobStore is the MinIO implementation of BlobStore.
type minioBlobStore struct {
	client    *minio.Client
	bucket    string
	prefix    string
	authority string
}

func (self *minioBlobStore) Put(ctx context.Context, key string, localPath string, contentType string) error {
	_, err := self.client.FPutObject(ctx, self.bucket, key, localPath, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

func (self *minioBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	object, err := self.client.GetObject(ctx, self.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return object, nil
}

func (self *minioBlobStore) List(ctx context.Context, keyPrefix string) ([]BlobObject, error) {
	objects := []BlobObject{}
	for object := range self.client.ListObjects(ctx, self.bucket, minio.ListObjectsOptions{
		Prefix:    keyPrefix,
		Recursive: true,
	}) {
		if object.Err != nil {
			return nil, object.Err
		}
		objects = append(objects, BlobObject{Key: object.Key, Size: object.Size})
	}
	return objects, nil
}

// resolveBlobAuthority resolves the configured authority to a dialable
// host:port: `{{ env:... }}` values interpolate per the vault convention
// (e.g. BRINGYOUR_MINIO_HOSTNAME, threaded from config settings.yml
// env_vars), and the host part then maps through the config settings
// `routes` (hostname -> the best route ip for this host) the same way
// services reach sibling hosts. A host that is not in routes passes through
// unchanged (a raw ip or an externally resolvable name). Returns ok=false
// when interpolation references an unset env var — the caller treats that
// as store-disabled rather than panicking (translateString raises).
func resolveBlobAuthority(authority string, routes map[string]string) (string, bool) {
	if authority == "" {
		return "", true
	}
	resolved := authority
	if strings.Contains(authority, "{{") {
		if err := HandleError(func() {
			resolved = translateString(authority)
		}); err != nil {
			return "", false
		}
	}
	host := resolved
	port := ""
	if i := strings.LastIndex(resolved, ":"); 0 <= i {
		host = resolved[:i]
		port = resolved[i+1:]
	}
	if route, ok := routes[host]; ok && route != "" {
		if port == "" {
			return route, true
		}
		return route + ":" + port, true
	}
	return resolved, true
}

// blobLifecycleRuleIdPrefix marks the ILM rules this code owns. SetLifecycle
// replaces exactly the rules carrying this prefix and preserves every other
// rule on the bucket (ops-set or another env's), so code-owned retention can
// coexist with out-of-band rules.
const blobLifecycleRuleIdPrefix = "urnetwork-ttl-"

// SetLifecycle installs the rules as MinIO ILM (server-side expiry) using a
// read-merge-write: current bucket rules are fetched, rules owned by this code
// (blobLifecycleRuleIdPrefix) are replaced, and all other rules are preserved
// byte-for-byte. MinIO expires by whole days, so a TTL is rounded up to at
// least one day. An empty input is a no-op (it does not remove previously
// installed rules). A failed read aborts rather than risking a blind
// replacement of rules this code does not own.
func (self *minioBlobStore) SetLifecycle(ctx context.Context, rules []BlobLifecycleRule) error {
	if len(rules) == 0 {
		return nil
	}
	owned := []lifecycle.Rule{}
	for _, rule := range rules {
		if rule.TTL <= 0 {
			continue
		}
		days := int(math.Ceil(rule.TTL.Hours() / 24))
		if days < 1 {
			days = 1
		}
		id := blobLifecycleRuleIdPrefix + strings.ReplaceAll(strings.Trim(rule.KeyPrefix, "/"), "/", "-")
		owned = append(owned, lifecycle.Rule{
			ID:         id,
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: rule.KeyPrefix},
			Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)},
		})
	}
	if len(owned) == 0 {
		return nil
	}

	existing, err := self.client.GetBucketLifecycle(ctx, self.bucket)
	if err != nil {
		if minio.ToErrorResponse(err).Code != "NoSuchLifecycleConfiguration" {
			// cannot see the current rules; do not risk clobbering them
			return err
		}
		existing = nil
	}

	config := lifecycle.NewConfiguration()
	config.Rules = mergeOwnedLifecycleRules(existing, owned)
	return self.client.SetBucketLifecycle(ctx, self.bucket, config)
}

// mergeOwnedLifecycleRules replaces existing rules by exact ID match with the
// owned set and preserves every other rule — ops-set rules and, in a shared
// bucket, another env's code-owned rules (their deterministic IDs differ).
// A code-owned rule for a stream that no longer registers a TTL lingers until
// removed out-of-band; it only expires objects under a prefix nothing writes.
func mergeOwnedLifecycleRules(existing *lifecycle.Configuration, owned []lifecycle.Rule) []lifecycle.Rule {
	ownedIds := map[string]bool{}
	for _, rule := range owned {
		ownedIds[rule.ID] = true
	}
	merged := []lifecycle.Rule{}
	if existing != nil {
		for _, rule := range existing.Rules {
			if !ownedIds[rule.ID] {
				merged = append(merged, rule)
			}
		}
	}
	return append(merged, owned...)
}

func (self *minioBlobStore) Bucket() string    { return self.bucket }
func (self *minioBlobStore) Prefix() string    { return self.prefix }
func (self *minioBlobStore) Authority() string { return self.authority }

// NewLocalBlobStore builds a filesystem-backed store rooted at root with the
// default aggregate byte cap.
func NewLocalBlobStore(root string, prefix string) BlobStore {
	return NewLocalBlobStoreWithMaxBytes(root, prefix, DefaultLocalBlobMaxBytes)
}

// NewLocalBlobStoreWithMaxBytes is NewLocalBlobStore with an explicit
// aggregate byte cap.
func NewLocalBlobStoreWithMaxBytes(root string, prefix string, maxBytes int64) BlobStore {
	if prefix == "" {
		prefix = DefaultBlobPrefix
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLocalBlobMaxBytes
	}
	return &localBlobStore{
		root:     root,
		prefix:   prefix,
		maxBytes: maxBytes,
	}
}

// localBlobStore is the local-filesystem implementation of BlobStore.
type localBlobStore struct {
	root     string
	prefix   string
	maxBytes int64

	putMu    sync.Mutex
	reapMu   sync.Mutex
	rules    []BlobLifecycleRule
	reapOnce sync.Once
}

func (self *localBlobStore) pathFor(key string) string {
	return filepath.Join(self.root, filepath.FromSlash(key))
}

func (self *localBlobStore) Put(ctx context.Context, key string, localPath string, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := self.pathFor(key)
	tmp := dst + blobPartialSuffix
	srcInfo, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	// Serialize capacity checks with writes and reaping. Otherwise concurrent
	// uploads can each observe the same free space and collectively exceed the
	// bound.
	self.putMu.Lock()
	defer self.putMu.Unlock()

	usage, err := self.usageBytes()
	if err != nil {
		return err
	}
	var replacedBytes int64
	if info, err := os.Stat(dst); err == nil {
		replacedBytes = info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	if info, err := os.Stat(tmp); err == nil {
		replacedBytes += info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	if self.maxBytes < usage-replacedBytes+srcInfo.Size() {
		return fmt.Errorf(
			"local blob capacity exceeded: usage=%d replacement=%d incoming=%d max=%d",
			usage,
			replacedBytes,
			srcInfo.Size(),
			self.maxBytes,
		)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	// write to a temp file then rename, so a List never sees a partial object
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (self *localBlobStore) usageBytes() (int64, error) {
	var total int64
	if _, err := os.Stat(self.root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	err := filepath.Walk(self.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (self *localBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return os.Open(self.pathFor(key))
}

func (self *localBlobStore) List(ctx context.Context, keyPrefix string) ([]BlobObject, error) {
	objects := []BlobObject{}
	if _, err := os.Stat(self.root); err != nil {
		if os.IsNotExist(err) {
			return objects, nil
		}
		return nil, err
	}
	err := filepath.Walk(self.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, blobPartialSuffix) {
			return nil
		}
		rel, err := filepath.Rel(self.root, path)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, keyPrefix) {
			objects = append(objects, BlobObject{Key: key, Size: info.Size()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return objects, nil
}

// SetLifecycle records the rules and starts (once) a background reaper bound to
// ctx that deletes expired objects. Objects are aged by file mtime (their local
// write/upload time), matching MinIO's object-age semantics.
func (self *localBlobStore) SetLifecycle(ctx context.Context, rules []BlobLifecycleRule) error {
	self.reapMu.Lock()
	self.rules = rules
	self.reapMu.Unlock()
	self.reapOnce.Do(func() {
		go self.reapLoop(ctx)
	})
	return nil
}

func (self *localBlobStore) reapLoop(ctx context.Context) {
	for {
		self.reapPass()
		select {
		case <-ctx.Done():
			return
		case <-time.After(localReapInterval):
		}
	}
}

func (self *localBlobStore) reapPass() {
	self.reapMu.Lock()
	rules := self.rules
	self.reapMu.Unlock()
	if len(rules) == 0 {
		return
	}
	if _, err := os.Stat(self.root); err != nil {
		return
	}
	self.putMu.Lock()
	defer self.putMu.Unlock()
	now := NowUtc()
	filepath.Walk(self.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(self.root, path)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		for _, rule := range rules {
			if rule.TTL <= 0 {
				continue
			}
			if strings.HasPrefix(key, rule.KeyPrefix) && rule.TTL <= now.Sub(info.ModTime()) {
				os.Remove(path)
				break
			}
		}
		return nil
	})
}

func (self *localBlobStore) Bucket() string    { return "local" }
func (self *localBlobStore) Prefix() string    { return self.prefix }
func (self *localBlobStore) Authority() string { return "local:" + self.root }
