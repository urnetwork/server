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
// a signal. What e2e-world's runs of 2026-11-21 found, each a product finding for a fix track:
//
//   - the join, found: in KeyDelivery mode a joining member device takes the config repo's generation only by a LATE
//     wrap (§10.6's late wraps row, KF2's wrapped row), which a holder writes on gkm's wrap-pending hint; gkm's own hint
//     writer resolves no sidecar id (plan-KEYS-hint-addressing's api route is not built), so in production that hint is
//     lost, the holders' hourly resync answers no late ask (GkmGroupClient's KeyDelivery resync runs SyncRepo,
//     RunPending and RenewDropKey alone), and no admission writes the first wrap either (DeliverFirstWrap has no caller
//     outside tests), so the joiner stands no sooner than the config group's next rotation, which a schedule makes
//     every 30 days (GkmSettings.ScheduleInterval). With the world's stand-in resolver and the exchange's push on (the
//     World's header) the hint reaches the owner, who delivers, and gkm's pending hint wakes the joiner, which stands;
//   - the join's minute: gkm caches repo/ 5 s (§10.6's checks row), so a gkm that read the config repo as keying at
//     KF1's group create, just before its activation, refuses 409 a late delivery judged inside those 5 s ("a keying
//     repo takes only its creation's deliveries"), DeliverLate drops a 409 for the next hint, and the joiner's next late
//     ask is LateAskInterval, a minute, away. Of five runs, the three whose owner delivered 1.7 s, 4.8 s and 4.8 s after
//     the activation stalled past the world's 60 s follow bound; the two at 12.3 s and 15.1 s joined at once. Under the
//     world's join bound (T3WorldSettings.KeyJoinTimeout) a sixth, refused 4.3 s after the activation, joined 124 s
//     after it, at its follower's retry past that minute: a joiner admitted at its network's start waits two minutes;
//   - the repo's group, open: CreateSharedRepo fails "evs: the repo's group: gkm refused: 404" in every run that got
//     that far. When the RepoCreate lands, every reader's group client, the creator's own included, syncs the new repo
//     (GkmGroupClient.syncNewReaderRepos: GET /gkm/repos/<id>/forms), before the creator's PUT /evs/v1/repos/<id> has
//     placed it; gkm's placement resolver caches that 404 for 5 s (§11.8's resolve row, EvsRepoPlaceSettings.NotFoundTtl)
//     and refuses KF1's POST /gkm/groups, sent right after the placement, with it, and EvsClient.CreateRepo does not
//     retry. The network's bootstrap meets the same 404 on the config repo and recovers only through its follower's 10 s
//     anchor retry ("config bootstrap: the config repo: evs: the repo's group: gkm refused: 404").
//
// Run it through the build's wrapper with -tags 'sidecar_e2e sidecar_pending sidecar_localstack sidecar_e2e_realkeys';
// the dump carries each sidecar's newest log.

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
