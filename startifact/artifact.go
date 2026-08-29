// Package startifact defines the immutable, content-addressed payout evidence
// published by each subnet network operator. It is deliberately independent of
// PostgreSQL so validators and third parties can rebuild artifacts byte-for-byte.
package startifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

const Schema = payoutartifact.Schema

// The server owns persistence, while the canonical schema and verifier live
// in sn so validators and offline analysis do not depend on this module.
type Boundary = payoutartifact.Boundary
type ProviderInput = payoutartifact.ProviderInput
type Leaf = payoutartifact.Leaf
type Artifact = payoutartifact.Artifact
type BuildInput = payoutartifact.BuildInput

var Build = payoutartifact.Build
var Sign = payoutartifact.Sign
var Verify = payoutartifact.Verify
var Bytes = payoutartifact.Bytes
var Decode = payoutartifact.Decode

const maximumArtifactBytes = 32 * 1024 * 1024

type Published struct {
	ContentHash string `json:"content_hash"`
	ContentKey  string `json:"content_key"`
	HistoryKey  string `json:"history_key"`
	Bucket      string `json:"bucket"`
}

func Publish(ctx context.Context, store server.BlobStore, a *Artifact) (*Published, error) {
	if store == nil {
		return nil, errors.New("server/blob store is unavailable")
	}
	b, err := Bytes(a)
	if err != nil {
		return nil, err
	}
	hashHex := strings.TrimPrefix(a.ContentHash, "sha256:")
	if len(hashHex) != 64 {
		return nil, errors.New("invalid content hash")
	}
	contentKey := filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "content", "sha256", hashHex+".json"))
	historyKey := filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "history", safeSegment(a.DeploymentID), fmt.Sprint(a.Netuid), fmt.Sprint(a.Epoch), fmt.Sprint(a.NoID), hashHex+".json"))
	if err := putImmutable(ctx, store, contentKey, b); err != nil {
		return nil, err
	}
	if err := putImmutable(ctx, store, historyKey, b); err != nil {
		return nil, err
	}
	return &Published{ContentHash: a.ContentHash, ContentKey: contentKey, HistoryKey: historyKey, Bucket: store.Bucket()}, nil
}

// Read resolves one content identity from server/blob and accepts only the
// exact canonical signed bytes. It is used by operator automation so deposit
// sizing consumes the same immutable statement validators audit.
func Read(ctx context.Context, store server.BlobStore, contentHash string) (*Artifact, []byte, error) {
	if store == nil {
		return nil, nil, errors.New("server/blob store is unavailable")
	}
	key, err := ContentKey(store, contentHash)
	if err != nil {
		return nil, nil, err
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()
	value, err := io.ReadAll(io.LimitReader(reader, maximumArtifactBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(value) > maximumArtifactBytes {
		return nil, nil, errors.New("payout artifact exceeds 32 MiB")
	}
	artifact, err := Decode(value)
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(artifact.ContentHash, contentHash) {
		return nil, nil, errors.New("payout artifact content identity mismatch")
	}
	return artifact, value, nil
}

func putImmutable(ctx context.Context, store server.BlobStore, key string, b []byte) error {
	objects, err := store.List(ctx, key)
	if err != nil {
		return fmt.Errorf("list immutable artifact key: %w", err)
	}
	exists := false
	for _, object := range objects {
		if object.Key == key {
			exists = true
			break
		}
	}
	if exists {
		reader, err := store.Get(ctx, key)
		if err != nil {
			return err
		}
		existing, readErr := io.ReadAll(io.LimitReader(reader, int64(len(b)+1)))
		reader.Close()
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, b) {
			return fmt.Errorf("immutable artifact key %s already contains different bytes", key)
		}
		return nil
	}
	tmp, err := os.CreateTemp("", "urnetwork-st-artifact-*.json")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return store.Put(ctx, key, path, "application/json")
}

func ContentKey(store server.BlobStore, hash string) (string, error) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hash)), "sha256:")
	if len(h) != 64 {
		return "", errors.New("content hash must be sha256 hex")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", errors.New("content hash must be sha256 hex")
	}
	return filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "content", "sha256", h+".json")), nil
}

func safeSegment(v string) string {
	v = strings.TrimSpace(v)
	v = strings.ReplaceAll(v, "/", "_")
	v = strings.ReplaceAll(v, "\\", "_")
	return v
}
