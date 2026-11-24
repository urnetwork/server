//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world on its real keys (the World's default, WorldSettings.FakeGkm false, and the T3World's; sidecart3's
// world_keys.go): every device's key client is its own real one, ur daemon's composition, at the world's gkm block, the
// config repo's heads live on evs, and each network is bootstrapped by its first owner's follower as ur daemon's is. A
// network of KeysClientCount sidecars: the owner's follower bootstraps it (KF1's draft of the config group at gkm, the
// first head on evs, base 1), the others are admitted and join, and then:
//
//   - a gkm group formed: the owner creates a general repo through evs, whose KF1 forms its group at gkm's host, and
//     grants its member writer, so every sidecar reads generation 1 through its own key client;
//   - every sidecar writes its own path and reads every other's back;
//   - the group rotated: the owner rotates the repo (`ur keys rotate`), every sidecar reads generation 2, writes again
//     under it, and reads every other's second write back.
//
// The smoke's gkm step (world_smoke_test.go) runs the same on each network of the smoke.
//
// e2e-world's runs of 2026-11-21 found three product bugs here, each fixed since, and this case, behind a tag of its own
// until then, now runs with the package:
//
//   - a joining member device took the config repo's generation only at the config group's next rotation: gkm's hint
//     writer resolved no sidecar id, the holders' resync answered no late ask, and nothing wrote a joiner's first wrap
//     (keys-late-delivery: gkm's hints by sidecar id reach their clients through api's internal identity route, every
//     sync answers late asks, and the signer delivers a joiner's first wrap at once);
//   - a late delivery judged inside gkm's 5 s repo/ cache of a keying repo was refused, so a joiner waited a minute or
//     two for its next late ask;
//   - a repo's creation was refused by gkm's cached 404 of its placement, which every reader's sync of the new repo
//     caused before the creator's PUT placed it (gkm-repo-create-race, with the one before: a write resolves placement
//     from a fresh read alone, and gkm's repo/ keying cache's TTL is a setting, 0 by default).
//
// Run it through the build's wrapper with -tags 'sidecar_e2e sidecar_pending sidecar_localstack'; the dump carries each
// sidecar's newest log.

package sidecare2e

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server/sidecart3"
)

// See the file's header.
func TestE2eWorldRealKeys(t *testing.T) {
	settings := defaultWorldKeysSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := NewWorld(t, DefaultWorldSettings())
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
