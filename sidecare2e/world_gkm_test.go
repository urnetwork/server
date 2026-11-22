//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's gkm helpers (world_test.go): a device's writes and reads through its own EvsClient, and the wait for a
// device's key client to hold a repo's generation, which the smoke's gkm step (world_smoke_test.go) and the real-keys
// case (world_keys_test.go) run on the world's real keys, each device's key client its own at gkm's block.

package sidecare2e

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/sidecart3"
)

// What the real-keys case runs with (QD-702).
type worldKeysSettings struct {
	// The sidecars of the one network.
	KeysClientCount int
	// The bound every phase of the case shares: the bootstrap and two joins, each of which may wait out a late ask's
	// minute (T3WorldSettings.KeyJoinTimeout), then the repo, the writes and the rotation, inside the case's 10 minutes.
	PhaseTimeout time.Duration
	// How long a sidecar may take to hold a generation the owner made, its key client learning of it by gkm's hint or
	// its resync, and how often it is asked meanwhile, since a key client's learning has no hook a test reads.
	GenerationTimeout      time.Duration
	GenerationPollInterval time.Duration
}

func defaultWorldKeysSettings() *worldKeysSettings {
	return &worldKeysSettings{
		KeysClientCount:        3,
		PhaseTimeout:           6 * time.Minute,
		GenerationTimeout:      2 * time.Minute,
		GenerationPollInterval: 200 * time.Millisecond,
	}
}

// Writes content at path as device, through its own EvsClient.
func worldKeysWrite(ctx context.Context, device *sidecart3.T3Device, repoId sidecar.Id, path string, content []byte) error {
	client, err := device.EvsClient()
	if err != nil {
		return err
	}
	_, err = client.Repo(repoId).Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: bytes.NewReader(content), Size: int64(len(content))})
	return err
}

// device reads path's head, which must be content.
func worldKeysRead(ctx context.Context, device *sidecart3.T3Device, repoId sidecar.Id, path string, content []byte) error {
	client, err := device.EvsClient()
	if err != nil {
		return err
	}
	var read bytes.Buffer
	if _, err := client.Repo(repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true}, &read); err != nil {
		return fmt.Errorf("%s's read of %s: %w", device.Name, path, err)
	}
	if !bytes.Equal(read.Bytes(), content) {
		return fmt.Errorf("%s reads %s as %q, not %q", device.Name, path, read.String(), content)
	}
	return nil
}

// Waits until device's key client holds repoId's generation at generation or past it, asking every poll interval.
func worldKeysAwaitGeneration(ctx context.Context, device *sidecart3.T3Device, repoId sidecar.Id, generation uint64, settings *worldKeysSettings) (*sidecar.EvsRepoKey, error) {
	ctx, cancel := context.WithTimeout(ctx, settings.GenerationTimeout)
	defer cancel()
	for {
		key, err := device.Gkm.CurrentRepoKey(ctx, repoId)
		if err == nil && generation <= key.Generation() {
			return key, nil
		}
		select {
		case <-ctx.Done():
			held := uint64(0)
			if key != nil {
				held = key.Generation()
			}
			return nil, fmt.Errorf("%s holds generation %d of %s, not %d: %v (%w)", device.Name, held, repoId, generation, err, ctx.Err())
		case <-time.After(settings.GenerationPollInterval):
		}
	}
}
