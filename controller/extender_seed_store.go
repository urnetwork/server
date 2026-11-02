package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Where the public seed bundle is kept (connect/EXTENDER-UPDATE2.md §2.3).
//
// The publisher writes the bundle to the blob store, which is the origin every
// mirror serves from: the api answers ExtenderSeedPath from it, and the
// operations-managed mirrors copy the api's answer. Two objects per network
// host, under the store's prefix:
//
//	<prefix>/extender-seed/<host>/latest.bin
//	<prefix>/extender-seed/<host>/sequence/<sequence, 20 digits>.bin
//
// latest.bin is the bundle the origin serves, replaced by each publish. A
// sequence object is the same bytes written once with PutIfAbsent before latest
// is replaced: it claims the sequence, so two publishers can never store two
// different bodies at one sequence, and the highest claimed sequence is a floor
// the next sequence must pass even when latest.bin was lost or rolled back. The
// sequence objects expire through the code-owned bucket lifecycle
// (ExtenderSeedHistoryTtl); latest.bin has no rule, since it is replaced long
// before any rule would reach it and a deleted latest would turn an expired
// bundle into a missing one.
//
// Nothing here holds a key: the store and the mirrors receive only signed
// artifacts.

// The largest artifact the origin keeps or serves, the envelope's limit
// (§2.2). A larger object is refused before it is read whole.
const ExtenderSeedMaxArtifactBytes = connect.ExtenderSeedMaxEnvelopeByteCount

// How long a claimed sequence object is kept. A week is several times the
// bundle's validity, which is what a floor has to outlive.
const ExtenderSeedHistoryTtl = 7 * 24 * time.Hour

const (
	extenderSeedKeyDirectory      = "extender-seed"
	extenderSeedLatestName        = "latest.bin"
	extenderSeedSequenceDirectory = "sequence"
	extenderSeedSequenceSuffix    = ".bin"
	// the digits of the largest uint64, so lexical order is numeric order
	extenderSeedSequenceDigits = 20
)

// A sequence some other publisher claimed first. The publish stops without
// replacing latest.bin; the next pass reads the claim back as its floor.
var ErrExtenderSeedSequenceClaimed = errors.New("the seed sequence was claimed by another publish")

// A latest.bin at or above the sequence being published. The publish stops
// rather than roll the origin back.
var ErrExtenderSeedSequenceSuperseded = errors.New("the stored seed bundle is at or above this sequence")

// An object larger than ExtenderSeedMaxArtifactBytes.
var ErrExtenderSeedArtifactTooLarge = errors.New("the seed artifact is larger than the envelope limit")

// NormalizeExtenderSeedNetworkHost is the network host in the form a seed body
// names and a key is built from: the existing host normalization (lower case,
// no trailing dot, as connect.ExtenderNetworkHostAllowed compares) and nothing
// that could leave the host's own key directory.
func NormalizeExtenderSeedNetworkHost(networkHost string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(networkHost), "."))
	if host == "" || 253 < len(host) || strings.Contains(host, "..") || strings.HasPrefix(host, ".") || strings.HasPrefix(host, "-") {
		return "", fmt.Errorf("the extender network host is not usable for a seed bundle")
	}
	for _, c := range host {
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.') {
			return "", fmt.Errorf("the extender network host is not usable for a seed bundle")
		}
	}
	return host, nil
}

func extenderSeedHostDirectory(store server.BlobStore, host string) string {
	return strings.Join([]string{store.Prefix(), extenderSeedKeyDirectory, host}, "/")
}

// The key of the bundle the origin serves.
func ExtenderSeedLatestKey(store server.BlobStore, host string) string {
	return extenderSeedHostDirectory(store, host) + "/" + extenderSeedLatestName
}

// The key prefix of the claimed sequences.
func ExtenderSeedSequencePrefix(store server.BlobStore, host string) string {
	return extenderSeedHostDirectory(store, host) + "/" + extenderSeedSequenceDirectory + "/"
}

// The key that claims one sequence.
func ExtenderSeedSequenceKey(store server.BlobStore, host string, sequence uint64) string {
	return fmt.Sprintf("%s%0*d%s", ExtenderSeedSequencePrefix(store, host), extenderSeedSequenceDigits, sequence, extenderSeedSequenceSuffix)
}

// The sequence a claim key names, or false for a key that is not one.
func parseExtenderSeedSequenceKey(prefix string, key string) (uint64, bool) {
	name, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return 0, false
	}
	digits, ok := strings.CutSuffix(name, extenderSeedSequenceSuffix)
	if !ok || len(digits) != extenderSeedSequenceDigits {
		return 0, false
	}
	sequence, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return sequence, true
}

// The lifecycle rule of the claimed sequences of the configured host, or none
// while the seed is off or the host is not usable. Joined into the code-owned
// bucket rules by ApplyBlobRetention.
func extenderSeedHistoryLifecycleRules(store server.BlobStore) []server.BlobLifecycleRule {
	if store == nil {
		return nil
	}
	config, err := EnvExtenderConfig()
	if err != nil || !config.Seed.Enabled {
		return nil
	}
	host, err := NormalizeExtenderSeedNetworkHost(config.NetworkHost)
	if err != nil {
		return nil
	}
	return []server.BlobLifecycleRule{{
		KeyPrefix: ExtenderSeedSequencePrefix(store, host),
		TTL:       ExtenderSeedHistoryTtl,
	}}
}

// ReadExtenderSeedLatest reads the bundle the origin serves, nil with no error
// when none is stored. An object larger than the envelope limit is an error
// after at most one byte past the limit is read.
func ReadExtenderSeedLatest(ctx context.Context, store server.BlobStore, host string) ([]byte, error) {
	return readExtenderSeedObject(ctx, store, ExtenderSeedLatestKey(store, host))
}

func readExtenderSeedObject(ctx context.Context, store server.BlobStore, key string) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		if server.IsBlobNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer reader.Close()
	artifact, err := io.ReadAll(io.LimitReader(reader, ExtenderSeedMaxArtifactBytes+1))
	if err != nil {
		if server.IsBlobNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if ExtenderSeedMaxArtifactBytes < len(artifact) {
		return nil, ErrExtenderSeedArtifactTooLarge
	}
	return artifact, nil
}

// ExtenderSeedClaimedSequenceFloor is the highest sequence any publish has
// claimed for the host, zero when none is kept.
func ExtenderSeedClaimedSequenceFloor(ctx context.Context, store server.BlobStore, host string) (uint64, error) {
	prefix := ExtenderSeedSequencePrefix(store, host)
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	floor := uint64(0)
	for _, object := range objects {
		if sequence, ok := parseExtenderSeedSequenceKey(prefix, object.Key); ok && floor < sequence {
			floor = sequence
		}
	}
	return floor, nil
}

// WriteExtenderSeed stores one signed bundle at its sequence: it claims the
// sequence, then replaces latest.bin, unless a sequence at or above it is
// already claimed or stored.
//
// readLatestSequence decodes the stored latest.bin's sequence; the store does
// not decode envelopes itself. A latest.bin it cannot decode does not stop the
// write: the claimed sequences are the floor then, and replacing an unreadable
// bundle with a valid one is the repair.
func WriteExtenderSeed(
	ctx context.Context,
	store server.BlobStore,
	host string,
	sequence uint64,
	artifact []byte,
	readLatestSequence func(artifact []byte) (uint64, bool),
) error {
	if len(artifact) == 0 || ExtenderSeedMaxArtifactBytes < len(artifact) {
		return ErrExtenderSeedArtifactTooLarge
	}
	stageDir, err := os.MkdirTemp("", "extender-seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)
	stagePath := filepath.Join(stageDir, extenderSeedLatestName)
	if err := os.WriteFile(stagePath, artifact, 0o600); err != nil {
		return err
	}

	created, err := store.PutIfAbsent(ctx, ExtenderSeedSequenceKey(store, host, sequence), stagePath, ExtenderSeedContentType)
	if err != nil {
		return err
	}
	if !created {
		return ErrExtenderSeedSequenceClaimed
	}

	// a concurrent publish that claimed a higher sequence, or wrote latest
	// with one, wins: never put a lower bundle over it
	floor, err := ExtenderSeedClaimedSequenceFloor(ctx, store, host)
	if err != nil {
		return err
	}
	if sequence < floor {
		return ErrExtenderSeedSequenceSuperseded
	}
	latest, err := ReadExtenderSeedLatest(ctx, store, host)
	if err != nil && !errors.Is(err, ErrExtenderSeedArtifactTooLarge) {
		return err
	}
	if latest != nil && readLatestSequence != nil {
		if latestSequence, ok := readLatestSequence(latest); ok && sequence <= latestSequence {
			return ErrExtenderSeedSequenceSuperseded
		}
	}
	return store.Put(ctx, ExtenderSeedLatestKey(store, host), stagePath, ExtenderSeedContentType)
}
