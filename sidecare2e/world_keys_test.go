//go:build sidecar_e2e && sidecar_pending && sidecar_localstack && sidecar_e2e_realkeys

// The world on real keys (sidecart3's T3WorldSettings.RealKeys, world_keys.go): every device's key client is its own
// real one, ur daemon's composition, at the world's gkm block, the config repo's heads live on evs, and each network is
// bootstrapped by its first owner's follower as ur daemon's is. A network of KeysClientCount sidecars: the owner's
// follower bootstraps it (KF1's draft of the config group at gkm, the first head on evs, base 1), the others are
// admitted and join, and then:
//
//   - a gkm group formed: the owner creates a general repo through evs, whose KF1 forms its group at gkm's host, and
//     grants its member writer, so every sidecar reads generation 1 through its own key client;
//   - every sidecar writes its own path and reads every other's back;
//   - the group rotated: the owner rotates the repo (`ur keys rotate`), every sidecar reads generation 2, writes again
//     under it, and reads every other's second write back.
//
// It fails today, and stays behind its own tag, sidecar_e2e_realkeys, until it passes, so the world's default run stays
// a signal: in e2e-world's runs of 2026-11-21 the owner's follower bootstraps the network on real keys (the draft,
// base 1, the first head, standing at the admissions), but the first admitted sidecar never stands. Its follower
// reads "joining: the log applied rows-only to 3 waits for the config repo's key and a full re-read" until the phase's
// bound, and the dump shows its group client saying "pending: gkm unavailable: no verified state yet" throughout; the
// owner's log holds "config join: gkm GET /gkm/repos/{repo_id}/forms: no service token: sidecar api POST /oauth/token
// refused: 503 temporarily_unavailable" and "reader grant: gkm key missing: gkm holds no group for the repo", and the
// sidecars not yet following "ReaderKey upkeep: gkm: no repo scope to present". Whether the config group's KF1 ever
// reached gkm, and why the joiner's join never completes, is not found: the cause may be the world's composition
// (world_keys.go, ported from ur daemon's) or the clients'. Run it through the build's wrapper with -tags
// 'sidecar_e2e sidecar_pending sidecar_localstack sidecar_e2e_realkeys'; the dump carries each sidecar's newest log.

package sidecare2e

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server/sidecart3"
)

// What the real-keys case runs with (QD-702).
type worldKeysSettings struct {
	// The sidecars of the one network.
	KeysClientCount int
	// Each phase's bound.
	PhaseTimeout time.Duration
	// How long a sidecar may take to hold a generation the owner made, its key client learning of it by gkm's hint or
	// its resync, and how often it is asked meanwhile, since a key client's learning has no hook a test reads.
	GenerationTimeout      time.Duration
	GenerationPollInterval time.Duration
}

func defaultWorldKeysSettings() *worldKeysSettings {
	return &worldKeysSettings{
		KeysClientCount:        3,
		PhaseTimeout:           4 * time.Minute,
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

// See the file's header.
func TestE2eWorldRealKeys(t *testing.T) {
	settings := defaultWorldKeysSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		worldSettings := DefaultWorldSettings()
		worldSettings.T3 = sidecart3.DefaultT3WorldSettings()
		worldSettings.T3.RealKeys = true
		world := NewWorld(t, worldSettings)
		defer world.Close()

		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		defer cancel()
		network, err := world.AddNetwork(ctx, "k", settings.KeysClientCount, nil)
		if err != nil {
			world.FailProgress(t, "the real-keys network", err, nil)
		}

		// a gkm group formed: KF1 at gkm through the owner's key client, the member granted writer
		repoId, err := network.CreateSharedRepo(ctx, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER)
		if err != nil {
			world.FailProgress(t, "the shared repo", err, nil)
		}
		groupIds := map[sidecar.Id]bool{}
		for _, device := range network.Clients {
			key, err := worldKeysAwaitGeneration(ctx, device, repoId, 1, settings)
			if err != nil {
				world.FailProgress(t, "generation 1", err, nil)
			}
			groupIds[key.GroupId()] = true
		}
		if len(groupIds) != 1 {
			t.Fatalf("the sidecars hold the repo under %d groups", len(groupIds))
		}
		content := func(device *sidecart3.T3Device, round int) []byte {
			return []byte(fmt.Sprintf("%s's write %d\n", device.Name, round))
		}
		writeAndReadAll := func(round int) {
			for _, device := range network.Clients {
				if err := worldKeysWrite(ctx, device, repoId, device.Name+".txt", content(device, round)); err != nil {
					world.FailProgress(t, fmt.Sprintf("%s's write %d", device.Name, round), err, nil)
				}
			}
			for _, reader := range network.Clients {
				for _, writer := range network.Clients {
					if err := worldKeysRead(ctx, reader, repoId, writer.Name+".txt", content(writer, round)); err != nil {
						world.FailProgress(t, fmt.Sprintf("round %d's reads", round), err, nil)
					}
				}
			}
		}
		writeAndReadAll(1)

		// the group rotated: generation 2, held by every sidecar, written and read under
		rotated, err := network.Owner().Gkm.RotateRepo(ctx, repoId)
		if err != nil {
			world.FailProgress(t, "the rotation", err, nil)
		}
		if rotated.Generation() != 2 {
			t.Fatalf("the rotation made generation %d, not 2", rotated.Generation())
		}
		for _, device := range network.Clients {
			key, err := worldKeysAwaitGeneration(ctx, device, repoId, 2, settings)
			if err != nil {
				world.FailProgress(t, "generation 2", err, nil)
			}
			if key.GroupId() != rotated.GroupId() || !bytes.Equal(key.Commitment(), rotated.Commitment()) {
				t.Fatalf("%s holds generation 2 under group %s, commitment %x; the rotation made %s, %x", device.Name, key.GroupId(), key.Commitment(), rotated.GroupId(), rotated.Commitment())
			}
		}
		writeAndReadAll(2)
	})
}
