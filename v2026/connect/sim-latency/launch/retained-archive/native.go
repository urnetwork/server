// The only production transport is the existing server/blob protected client.
// No endpoint, credential, configuration write, or service is added here.
package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Both capabilities come from the same configured native store and authority.
type nativeArchiveTransport struct {
	Protected server.ProtectedBlobStore
	Versioned server.VersionedBlobStore
}

// Ordinary host-scoped settings remain the sole source of blob credentials.
// This constructor is unreachable from local prepare or default verification.
func newNativeTransport(policy archivePolicy) (archiveTransport, error) {
	server.GetSettings()
	config, present := server.LoadBlobStoreConfig()
	if !present || config.Local || config.Prefix != server.DefaultBlobPrefix {
		return nil, errors.New("ordinary native blob configuration unavailable")
	}
	selected := *config
	selected.Bucket = policy.Bucket
	store, err := server.NewBlobStore(&selected)
	if err != nil {
		return nil, errors.New("native archive store unavailable")
	}
	protected, protectedOk := store.(server.ProtectedBlobStore)
	versioned, versionedOk := store.(server.VersionedBlobStore)
	if !protectedOk || !versionedOk {
		return nil, errors.New("native protection and version-history capabilities required")
	}
	return &nativeArchiveTransport{Protected: protected, Versioned: versioned}, nil
}

// Native protection already validates enabled server-side replication targets.
func (self *nativeArchiveTransport) CheckProtection(ctx context.Context) (archiveProtection, error) {
	proof, err := self.Protected.CheckProtection(ctx)
	if err != nil || proof == nil {
		return archiveProtection{}, errors.New("native protection refused")
	}
	return archiveProtection{Authority: proof.Authority, Bucket: proof.Bucket,
		ObjectLock: proof.ObjectLock, Versioning: proof.Versioning,
		ReplicationTargets: append([]string{}, proof.ReplicationTargets...)}, nil
}

// Even a noncurrent version or delete marker spends a supposedly fresh prefix.
func (self *nativeArchiveTransport) ListVersions(ctx context.Context, prefix string, visit func() error) error {
	return self.Versioned.ListVersions(ctx, prefix, func(server.BlobObjectVersion) error { return visit() })
}

// Exactly one native retained-write call is made; SDK retries are not relabeled.
func (self *nativeArchiveTransport) PutRetained(ctx context.Context, key, localPath string, retainUntil time.Time) (archiveRetention, error) {
	proof, err := self.Protected.PutRetained(ctx, key, localPath, "application/x-tar", retainUntil)
	if err != nil || proof == nil {
		return archiveRetention{}, errors.New("native retained write refused or ambiguous")
	}
	return archiveRetention{Key: proof.Key, Size: proof.Size, VersionId: proof.VersionId,
		Mode: proof.Mode, RetainUntil: proof.RetainUntil}, nil
}

// No fallback to a current-object read exists.
func (self *nativeArchiveTransport) GetVersion(ctx context.Context, key, versionId string) (io.ReadCloser, error) {
	return self.Protected.GetVersion(ctx, key, versionId)
}
