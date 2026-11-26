package evidencemove

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/server"
)

// One stored object's identity as a HEAD reports it.
type ObjectInfo struct {
	Key  string
	Size int64
	// without quotes, as the store reports it
	ETag string
}

// The object operations a move needs. Absent objects are errors that
// server.IsBlobNotFound reports. server.BlobStore deliberately has no delete,
// so this narrow store exists only for the mover.
type ObjectStore interface {
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// A server-side copy of src, as its HEAD reported it, to dstKey. Refused,
	// with an error wrapping errCopyPrecondition, when src's ETag changed.
	Copy(ctx context.Context, src ObjectInfo, dstKey string) error
	// Reads key, at most limit+1 bytes, so a caller sees a larger object as
	// one; info is the object's size and ETag as the read reported them.
	Read(ctx context.Context, key string, limit int64) (data []byte, info ObjectInfo, err error)
	Remove(ctx context.Context, key string) error
}

var errCopyPrecondition = errors.New("the copy source changed")

// The production store: one MinIO bucket through a client that sends each
// request once, so the mover's own backoff and gates own every retry (the
// SDK otherwise retries a 429 or 503 up to ten times, see
// server.SingleAttemptBlobStore).
type MinioObjectStore struct {
	client    *minio.Client
	bucket    string
	authority string
	tls       bool
}

// Connections are kept for at most connections requests in flight.
func NewMinioObjectStore(config *server.BlobStoreConfig, connections int) (*MinioObjectStore, error) {
	if config == nil || config.Local || config.Authority == "" || config.Bucket == "" {
		return nil, errors.New("the blob store is not a MinIO bucket")
	}
	transport, err := minio.DefaultTransport(config.Tls)
	if err != nil {
		return nil, err
	}
	transport.MaxIdleConns = max(transport.MaxIdleConns, 2*connections)
	transport.MaxIdleConnsPerHost = max(transport.MaxIdleConnsPerHost, 2*connections)
	client, err := minio.New(config.Authority, &minio.Options{
		Creds:      credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
		Secure:     config.Tls,
		Region:     config.Region,
		Transport:  transport,
		MaxRetries: 1,
	})
	if err != nil {
		return nil, err
	}
	return &MinioObjectStore{client: client, bucket: config.Bucket, authority: config.Authority, tls: config.Tls}, nil
}

func (self *MinioObjectStore) Bucket() string { return self.bucket }

// MinIO's cluster health route on the same server: 200 while it has write
// quorum (see MinioHealthGate).
func (self *MinioObjectStore) HealthURL() string {
	scheme := "http"
	if self.tls {
		scheme = "https"
	}
	return scheme + "://" + self.authority + "/minio/health/cluster"
}

// Refuses a bucket where a delete would not remove the flat object: with
// versioning enabled or suspended a delete leaves a delete marker in the same
// directory, and object lock keeps the version. Read only.
func (self *MinioObjectStore) Preflight(ctx context.Context) error {
	exists, err := self.client.BucketExists(ctx, self.bucket)
	if err != nil {
		return fmt.Errorf("bucket %s: %w", self.bucket, err)
	}
	if !exists {
		return fmt.Errorf("bucket %s does not exist", self.bucket)
	}
	versioning, err := self.client.GetBucketVersioning(ctx, self.bucket)
	if err != nil {
		return fmt.Errorf("bucket %s versioning: %w", self.bucket, err)
	}
	if versioning.Status != "" {
		return fmt.Errorf("bucket %s versioning is %s: a delete would leave a delete marker in the flat directory", self.bucket, versioning.Status)
	}
	lock, _, _, _, err := self.client.GetObjectLockConfig(ctx, self.bucket)
	if err != nil && minio.ToErrorResponse(err).Code != "ObjectLockConfigurationNotFoundError" {
		return fmt.Errorf("bucket %s object lock: %w", self.bucket, err)
	}
	if err == nil && lock != "" {
		return fmt.Errorf("bucket %s has object lock %s", self.bucket, lock)
	}
	return nil
}

func (self *MinioObjectStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := self.client.StatObject(ctx, self.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, normalizeMinioError(key, err)
	}
	return ObjectInfo{Key: key, Size: info.Size, ETag: strings.Trim(info.ETag, `"`)}, nil
}

func (self *MinioObjectStore) Copy(ctx context.Context, src ObjectInfo, dstKey string) error {
	_, err := self.client.CopyObject(
		ctx,
		minio.CopyDestOptions{Bucket: self.bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: self.bucket, Object: src.Key, MatchETag: src.ETag},
	)
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.StatusCode == http.StatusPreconditionFailed {
			return fmt.Errorf("copy %s to %s: %w: %w", src.Key, dstKey, errCopyPrecondition, err)
		}
		return normalizeMinioError(src.Key, err)
	}
	return nil
}

func (self *MinioObjectStore) Read(ctx context.Context, key string, limit int64) ([]byte, ObjectInfo, error) {
	object, err := self.client.GetObject(ctx, self.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, normalizeMinioError(key, err)
	}
	defer object.Close()
	// Stat before reading: from minio-go v7.4, an Object's Stat reports the
	// bytes left after the read offset, so after a full read it reports 0
	info, err := object.Stat()
	if err != nil {
		return nil, ObjectInfo{}, normalizeMinioError(key, err)
	}
	data, err := io.ReadAll(io.LimitReader(object, limit+1))
	if err != nil {
		return nil, ObjectInfo{}, normalizeMinioError(key, err)
	}
	return data, ObjectInfo{Key: key, Size: info.Size, ETag: strings.Trim(info.ETag, `"`)}, nil
}

// S3 answers a delete of an absent key with success, so a repeat is a no-op.
func (self *MinioObjectStore) Remove(ctx context.Context, key string) error {
	if err := self.client.RemoveObject(ctx, self.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return normalizeMinioError(key, err)
	}
	return nil
}

// NoSuchKey becomes fs.ErrNotExist; every other error is kept for
// classification (server.IsBlobThrottled, server.IsBlobRequestRefused).
func normalizeMinioError(key string, err error) error {
	if response := minio.ToErrorResponse(err); response.Code == minio.NoSuchKey {
		return errors.Join(errNotFound(key), err)
	}
	return err
}

// A request bound for the health and gate reads.
const gateRequestTimeout = 10 * time.Second
